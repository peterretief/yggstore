package mail

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/http"
	netmail "net/mail"
	"net/smtp"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/peers"
)

// Sending: a member's dashboard (or yggstore mail send) writes the message
// and posts it to a web node with -mail-out, which checks the From address
// is the member's and hands it to the group's SMTP relay. Only web nodes
// hold the relay's password.

// SendPath is where a node with -mail-out takes members' messages.
const SendPath = "/v1/mail/send"

// MaxRecipients is the most addresses one message goes to.
const MaxRecipients = 50

// Draft is a message to write.
type Draft struct {
	From    string // the sender's own address
	Name    string // shown with From, optional
	To      string // address lists, comma-separated
	Cc      string
	Bcc     string
	Subject string
	Text    string
	// InReplyTo and References thread a reply (from the message replied to).
	InReplyTo  string
	References string
}

// Compose writes a plain-text message and lists everyone it goes to,
// Bcc included (Bcc is not in the headers).
func (d Draft) Compose() (raw []byte, rcpts []string, err error) {
	from, err := netmail.ParseAddress(d.From)
	if err != nil {
		return nil, nil, fmt.Errorf("your address %q: %w", d.From, err)
	}
	if d.Name != "" {
		from.Name = oneLine(d.Name)
	}
	seen := map[string]bool{}
	var hdr bytes.Buffer
	put := func(k, v string) { fmt.Fprintf(&hdr, "%s: %s\r\n", k, v) }
	addrs := func(field, list string, inHeader bool) error {
		if strings.TrimSpace(list) == "" {
			return nil
		}
		parsed, err := netmail.ParseAddressList(list)
		if err != nil {
			return fmt.Errorf("%s: %w", field, err)
		}
		var shown []string
		for _, a := range parsed {
			a.Name = oneLine(a.Name)
			shown = append(shown, a.String())
			if k := strings.ToLower(a.Address); !seen[k] {
				seen[k] = true
				rcpts = append(rcpts, a.Address)
			}
		}
		if inHeader {
			put(field, strings.Join(shown, ", "))
		}
		return nil
	}
	put("From", from.String())
	if err := addrs("To", d.To, true); err != nil {
		return nil, nil, err
	}
	if err := addrs("Cc", d.Cc, true); err != nil {
		return nil, nil, err
	}
	if err := addrs("Bcc", d.Bcc, false); err != nil {
		return nil, nil, err
	}
	if len(rcpts) == 0 {
		return nil, nil, errors.New("no one to send it to")
	}
	if len(rcpts) > MaxRecipients {
		return nil, nil, fmt.Errorf("at most %d recipients", MaxRecipients)
	}
	put("Subject", mime.QEncoding.Encode("utf-8", oneLine(d.Subject)))
	put("Date", time.Now().Format(time.RFC1123Z))
	_, domain, _ := strings.Cut(from.Address, "@")
	put("Message-ID", "<"+randomID()+"@"+domain+">")
	if id := oneLine(d.InReplyTo); id != "" {
		put("In-Reply-To", id)
		put("References", strings.TrimSpace(oneLine(d.References)+" "+id))
	}
	put("MIME-Version", "1.0")
	put("Content-Type", "text/plain; charset=utf-8")
	put("Content-Transfer-Encoding", "quoted-printable")
	hdr.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&hdr)
	qp.Write([]byte(strings.ReplaceAll(strings.ReplaceAll(d.Text, "\r\n", "\n"), "\n", "\r\n")))
	qp.Close()
	return hdr.Bytes(), rcpts, nil
}

// oneLine keeps header values from starting new header lines.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func randomID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Send posts a message to a web node that relays mail, trying each until
// one takes it.
func Send(ctx context.Context, c client.Client, list []peers.Peer, raw []byte, rcpts []string) error {
	type relay struct {
		p   peers.Peer
		rtt time.Duration
	}
	var mu sync.Mutex
	var relays []relay
	var wg sync.WaitGroup
	for _, p := range list {
		if p.Gateway {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			ictx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			start := time.Now()
			if info, err := c.Info(ictx, p.Addr); err == nil && info.MailOut {
				mu.Lock()
				relays = append(relays, relay{p, time.Since(start)})
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(relays) == 0 {
		return errors.New("no web node that sends mail is online (see docs/mail.md, Sending)")
	}
	sort.Slice(relays, func(i, j int) bool { return relays[i].rtt < relays[j].rtt })
	var errs []string
	for _, r := range relays {
		err := post(ctx, c, r.p.Addr, raw, rcpts)
		if err == nil {
			return nil
		}
		var refused refusedError
		if errors.As(err, &refused) {
			return err // the next web node would say the same
		}
		errs = append(errs, r.p.Name+": "+err.Error())
	}
	return errors.New("not sent: " + strings.Join(errs, "; "))
}

// refusedError is a web node turning the message down, not failing.
type refusedError string

func (e refusedError) Error() string { return string(e) }

func post(ctx context.Context, c client.Client, addr string, raw []byte, rcpts []string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	body, _ := json.Marshal(sendRequest{Rcpt: rcpts, Message: raw})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+SendPath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	// The relay can take a while (it waits on the SMTP server); ctx bounds
	// it, not the client's usual timeout.
	hc := &http.Client{}
	if c.HTTP != nil {
		hc.Transport = c.HTTP.Transport
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	switch {
	case resp.StatusCode == http.StatusOK:
		return nil
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusTooManyRequests:
		return refusedError(strings.TrimSpace(string(msg)))
	}
	return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
}

type sendRequest struct {
	Rcpt    []string `json:"rcpt"`
	Message []byte   `json:"message"`
}

// Relay is a web node's side: it takes members' messages and sends them
// through the SMTP relay in its config file.
type Relay struct {
	SMTP     string `json:"smtp"` // host:port; 465 is TLS from the start, others use STARTTLS
	User     string `json:"user"`
	Password string `json:"password"`
	// Senders says which node may send as which address: "you@example.org"
	// or "*@example.org" (any address there) to a node ID.
	Senders map[string]string `json:"senders"`

	Log func(string, ...any) `json:"-"`
	// Deliver sends a message; tests replace it.
	Deliver func(ctx context.Context, from string, to []string, msg []byte) error `json:"-"`

	mu   sync.Mutex
	sent map[string][]time.Time // by caller, for the rate limit
}

// RelayExample is a config file to fill in.
const RelayExample = `{
  "smtp": "mail.smtp2go.com:587",
  "user": "",
  "password": "",
  "senders": {}
}
`

// MaxPerHour is how many messages a member may send in an hour.
const MaxPerHour = 60

// LoadRelay reads a relay config file. It holds a password, so it must not
// be readable by others.
func LoadRelay(path string) (*Relay, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s holds a password: make it private (chmod 600 %s)", path, path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	r := &Relay{}
	if err := json.Unmarshal(b, r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if _, _, err := net.SplitHostPort(r.SMTP); err != nil {
		return nil, fmt.Errorf("%s: smtp should be host:port, e.g. mail.smtp2go.com:587", path)
	}
	if r.User == "" || r.Password == "" {
		return nil, fmt.Errorf("%s: fill in the relay's user and password", path)
	}
	if len(r.Senders) == 0 {
		return nil, fmt.Errorf("%s: no senders (yggstore mail address prints a node's entry)", path)
	}
	senders := map[string]string{}
	for addr, node := range r.Senders {
		ip := net.ParseIP(node)
		if ip == nil {
			return nil, fmt.Errorf("%s: %s: %q is not a node ID", path, addr, node)
		}
		senders[strings.ToLower(addr)] = ip.String()
	}
	r.Senders = senders
	return r, nil
}

// may is whether node may send as addr.
func (r *Relay) may(node, addr string) bool {
	addr = strings.ToLower(addr)
	_, domain, ok := strings.Cut(addr, "@")
	if !ok {
		return false
	}
	return r.Senders[addr] == node || r.Senders["*@"+domain] == node
}

func (r *Relay) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log(format, args...)
	}
}

func (r *Relay) allow(caller string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sent == nil {
		r.sent = map[string][]time.Time{}
	}
	now := time.Now()
	keep := r.sent[caller][:0]
	for _, t := range r.sent[caller] {
		if now.Sub(t) < time.Hour {
			keep = append(keep, t)
		}
	}
	if len(keep) >= MaxPerHour {
		r.sent[caller] = keep
		return false
	}
	r.sent[caller] = append(keep, now)
	return true
}

// Serve answers SendPath for a member node (caller); it reports whether the
// request was its to answer.
func (r *Relay) Serve(w http.ResponseWriter, req *http.Request, caller string) bool {
	if req.URL.Path != SendPath {
		return false
	}
	if req.Method != http.MethodPost {
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return true
	}
	var in sendRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, MaxSize*4/3+64<<10)).Decode(&in); err != nil {
		http.Error(w, "bad request (or a message over 32 MiB)", http.StatusBadRequest)
		return true
	}
	msg, err := netmail.ReadMessage(bytes.NewReader(in.Message))
	if err != nil {
		http.Error(w, "not an email message", http.StatusBadRequest)
		return true
	}
	from, err := netmail.ParseAddressList(msg.Header.Get("From"))
	if err != nil || len(from) != 1 {
		http.Error(w, "the message needs one From address", http.StatusBadRequest)
		return true
	}
	if !r.may(caller, from[0].Address) || (msg.Header.Get("Sender") != "" && msg.Header.Get("Sender") != msg.Header.Get("From")) {
		r.logf("mail: %s may not send as %s", caller, from[0].Address)
		http.Error(w, fmt.Sprintf("this node may not send as %s (the web nodes' -mail-out file lists who may)", from[0].Address), http.StatusForbidden)
		return true
	}
	if msg.Header.Get("Bcc") != "" {
		http.Error(w, "Bcc belongs in the recipients, not the headers", http.StatusBadRequest)
		return true
	}
	if len(in.Rcpt) == 0 || len(in.Rcpt) > MaxRecipients {
		http.Error(w, fmt.Sprintf("1 to %d recipients", MaxRecipients), http.StatusBadRequest)
		return true
	}
	for _, a := range in.Rcpt {
		if p, err := netmail.ParseAddress(a); err != nil || p.Address != a {
			http.Error(w, "bad recipient "+a, http.StatusBadRequest)
			return true
		}
	}
	if !r.allow(caller) {
		http.Error(w, fmt.Sprintf("at most %d messages an hour", MaxPerHour), http.StatusTooManyRequests)
		return true
	}
	deliver := r.Deliver
	if deliver == nil {
		deliver = r.deliver
	}
	ctx, cancel := context.WithTimeout(req.Context(), 2*time.Minute)
	defer cancel()
	if err := deliver(ctx, from[0].Address, in.Rcpt, in.Message); err != nil {
		r.logf("mail: sending for %s: %v", caller, err)
		http.Error(w, "the mail relay: "+err.Error(), http.StatusBadGateway)
		return true
	}
	r.logf("mail: sent for %s from %s to %d recipient(s) (%d bytes)", caller, from[0].Address, len(in.Rcpt), len(in.Message))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"message_id": msg.Header.Get("Message-ID")})
	return true
}

// deliver sends one message over SMTP, always encrypted.
func (r *Relay) deliver(ctx context.Context, from string, to []string, msg []byte) error {
	host, port, _ := net.SplitHostPort(r.SMTP)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	tlsCfg := &tls.Config{ServerName: host}
	var conn net.Conn
	var err error
	if port == "465" {
		conn, err = (&tls.Dialer{Config: tlsCfg}).DialContext(ctx, "tcp", r.SMTP)
	} else {
		conn, err = (&net.Dialer{}).DialContext(ctx, "tcp", r.SMTP)
	}
	if err != nil {
		return err
	}
	deadline, _ := ctx.Deadline()
	conn.SetDeadline(deadline)
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if port != "465" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("the relay doesn't offer STARTTLS; not sending the password in the clear")
		}
		if err := c.StartTLS(tlsCfg); err != nil {
			return err
		}
	}
	if err := c.Auth(smtp.PlainAuth("", r.User, r.Password, host)); err != nil {
		return fmt.Errorf("logging in: %w", err)
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	for _, a := range to {
		if err := c.Rcpt(a); err != nil {
			return fmt.Errorf("%s: %w", a, err)
		}
	}
	wc, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := wc.Write(msg); err != nil {
		return err
	}
	if err := wc.Close(); err != nil {
		return err
	}
	return c.Quit()
}
