package site

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// HealthHost is the host name a web node answers "ok" to, so its own
// tunnel can tell it is serving. Cloudflare only forwards configured
// hostnames, so visitors can't reach it.
const HealthHost = "yggstore-health"

// Tunnel runs a Cloudflare Tunnel connector for a web node, but only while
// the node's web server answers. Cloudflare sends visitors to any connected
// connector without checking what is behind it, so a connector left running
// in front of a stopped web server would turn visitors away; tying the two
// together lets Cloudflare move them to the other web nodes instead.
type Tunnel struct {
	Bin       string // cloudflared
	TokenFile string // the tunnel's token
	Dir       string // for its (empty) settings file
	WebAddr   string // the web server, e.g. 127.0.0.1:8480
	Log       func(string, ...any)

	// How often the web server is checked, and after how many failed
	// checks in a row the connector is stopped.
	Every    time.Duration
	Failures int

	mu    sync.Mutex
	state string
}

// State is "connected", "starting", "stopped: why", or "" before Run.
func (t *Tunnel) State() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state
}

func (t *Tunnel) setState(s string) {
	t.mu.Lock()
	t.state = s
	t.mu.Unlock()
}

// Check reports whether the token and cloudflared are usable, so a mistake
// shows at start rather than in the log later.
func (t *Tunnel) Check() error {
	if _, err := exec.LookPath(t.Bin); err != nil {
		return errors.New("cloudflared not found: install it, or give its path with -cloudflared")
	}
	b, err := os.ReadFile(t.TokenFile)
	if err != nil {
		return err
	}
	if tok := strings.TrimSpace(string(b)); !strings.HasPrefix(tok, "eyJ") {
		return errors.New(t.TokenFile + " doesn't hold a tunnel token (it starts with eyJ; the tunnel's ID is not its token)")
	}
	return nil
}

func (t *Tunnel) healthy(ctx context.Context) bool {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+t.WebAddr+"/", nil)
	req.Host = HealthHost
	c := http.Client{Timeout: 5 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// Run keeps the connector up while the web server is healthy, until ctx ends.
func (t *Tunnel) Run(ctx context.Context) {
	if t.Every <= 0 {
		t.Every = 5 * time.Second
	}
	if t.Failures <= 0 {
		t.Failures = 2
	}
	conf := filepath.Join(t.Dir, "cloudflared.yml")
	// Its own settings file, so cloudflared doesn't also read the system's
	// /etc/cloudflared/config.yml and join another tunnel.
	os.MkdirAll(t.Dir, 0o700)
	os.WriteFile(conf, []byte("no-autoupdate: true\n"), 0o600)

	var cmd *exec.Cmd
	var exited chan struct{}
	stop := func(why string) {
		if cmd != nil {
			t.Log("tunnel: stopping the connector (%s)", why)
			cmd.Process.Signal(os.Interrupt)
			select {
			case <-exited:
			case <-time.After(10 * time.Second):
				cmd.Process.Kill()
				<-exited
			}
			cmd = nil
		}
		t.setState("stopped: " + why)
	}
	defer func() { stop("the node is stopping") }()

	failed, backoff := 0, time.Duration(0)
	var restartAt time.Time
	for {
		ok := t.healthy(ctx)
		if ok {
			failed = 0
		} else {
			failed++
		}
		switch {
		case cmd != nil && failed >= t.Failures:
			stop("the web server isn't answering")
		case cmd == nil && ok && !time.Now().Before(restartAt):
			cmd, exited = t.start(conf)
			if cmd == nil {
				backoff = min(max(2*backoff, 5*time.Second), time.Minute)
				restartAt = time.Now().Add(backoff)
			}
		case cmd == nil && !ok:
			t.setState("stopped: the web server isn't answering")
		}
		if exited != nil {
			select {
			case <-exited:
				if cmd != nil {
					t.Log("tunnel: the connector exited; starting it again soon")
					cmd = nil
					backoff = min(max(2*backoff, 5*time.Second), time.Minute)
					restartAt = time.Now().Add(backoff)
					t.setState("stopped: the connector exited")
				}
			default:
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(t.Every):
		}
	}
}

func (t *Tunnel) start(conf string) (*exec.Cmd, chan struct{}) {
	cmd := exec.Command(t.Bin, "--config", conf, "tunnel", "run", "--token-file", t.TokenFile)
	setChildAttrs(cmd)
	out, _ := cmd.StdoutPipe()
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Log("tunnel: could not start cloudflared: %v", err)
		t.setState("stopped: " + err.Error())
		return nil, nil
	}
	t.setState("starting")
	t.Log("tunnel: connector started")
	exited := make(chan struct{})
	go func() {
		t.watchOutput(out)
		cmd.Wait()
		close(exited)
	}()
	return cmd, exited
}

// watchOutput passes on cloudflared's warnings and errors, and notes when
// it has connected.
func (t *Tunnel) watchOutput(r io.Reader) {
	sc := bufio.NewScanner(r)
	announced := false
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.Contains(line, "Registered tunnel connection"):
			t.setState("connected")
			if !announced {
				t.Log("tunnel: connected to Cloudflare")
				announced = true
			}
		case strings.Contains(line, " ERR "), strings.Contains(line, " WRN "):
			t.Log("tunnel: %s", line)
		}
	}
}
