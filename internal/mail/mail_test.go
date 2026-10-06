package mail

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/files"
	"github.com/peterretief/yggstore/internal/localstore"
	"github.com/peterretief/yggstore/internal/msg"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/server"
	"github.com/peterretief/yggstore/internal/share"
	"github.com/peterretief/yggstore/internal/transport"
)

const sample = "From: =?UTF-8?Q?Ren=C3=A9e?= <renee@example.net>\r\n" +
	"To: you@example.org\r\n" +
	"Subject: =?UTF-8?B?Q2Fmw6kgb24gRnJpZGF5?=\r\n" +
	"Date: Tue, 06 Oct 2026 09:30:00 +0200\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=\"outer\"\r\n" +
	"\r\n" +
	"--outer\r\n" +
	"Content-Type: multipart/alternative; boundary=\"inner\"\r\n" +
	"\r\n" +
	"--inner\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"Content-Transfer-Encoding: quoted-printable\r\n" +
	"\r\n" +
	"See you at the caf=C3=A9 at ten.\r\n" +
	"--inner\r\n" +
	"Content-Type: text/html; charset=utf-8\r\n" +
	"\r\n" +
	"<p>See you at the <b>caf\xc3\xa9</b> at ten.</p>\r\n" +
	"--inner--\r\n" +
	"--outer\r\n" +
	"Content-Type: text/plain; name=\"menu.txt\"\r\n" +
	"Content-Disposition: attachment; filename=\"../menu.txt\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"Q29mZmVlLCB0ZWEu\r\n" +
	"--outer--\r\n"

func identity(t *testing.T) *share.Identity {
	id, err := share.LoadOrCreate(filepath.Join(t.TempDir(), "sharing.key"))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestSealOpen(t *testing.T) {
	me, other := identity(t), identity(t)
	sealed, err := Seal(me.Code(), []byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("caf")) {
		t.Fatal("sealed message shows its text")
	}
	if raw, err := Open(me, sealed); err != nil || string(raw) != sample {
		t.Fatalf("open: %v", err)
	}
	if _, err := Open(other, sealed); err != ErrNotForYou {
		t.Fatalf("someone else opened it: %v", err)
	}
	sealed[len(sealed)-1] ^= 1
	if _, err := Open(me, sealed); err != ErrNotForYou {
		t.Fatalf("altered message opened: %v", err)
	}
}

// The Worker seals in JavaScript; what it makes must open here.
func TestWorkerSealOpensInGo(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	worker, _ := filepath.Abs("../../deploy/mail-worker/worker.js")
	script := filepath.Join(t.TempDir(), "seal.mjs")
	os.WriteFile(script, []byte(`import { seal } from "file://`+worker+`";
const out = await seal(process.argv[2], new TextEncoder().encode(process.argv[3]));
process.stdout.write(Buffer.from(out).toString("base64"));`), 0o600)
	me := identity(t)
	out, err := exec.Command(node, script, me.Code(), sample).Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	sealed, _ := base64.StdEncoding.DecodeString(string(out))
	if raw, err := Open(me, sealed); err != nil || string(raw) != sample {
		t.Fatalf("open the Worker's message: %v", err)
	}
}

func TestParse(t *testing.T) {
	m, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if m.From != "Renée <renee@example.net>" || m.Subject != "Café on Friday" || m.Date == 0 {
		t.Fatalf("headers: %+v", m.Summary)
	}
	if strings.TrimSpace(m.Text) != "See you at the café at ten." || m.FromHTML {
		t.Fatalf("text: %q", m.Text)
	}
	if len(m.Attachments) != 1 || m.Attachments[0].Name != "menu.txt" || string(m.Attachments[0].data) != "Coffee, tea." {
		t.Fatalf("attachments: %+v", m.Attachments)
	}

	htmlOnly := "Subject: hi\r\nContent-Type: text/html; charset=windows-1252\r\n\r\n" +
		"<html><head><style>p{}</style></head><body><p>Price: \x80 5</p><p>Read <a href=\"https://example.net/x\">more</a></p><script>alert(1)</script></body></html>"
	m, _ = Parse([]byte(htmlOnly))
	if !m.FromHTML || m.Text != "Price: € 5\nRead more (https://example.net/x)" {
		t.Fatalf("html text: %q", m.Text)
	}
}

// postbox stands in for the nodes' messaging: each node's direct messages.
type postbox struct {
	mu    sync.Mutex
	boxes map[string][]msg.Stored
	wake  chan struct{}
}

type postNode struct {
	p    *postbox
	self string
}

func (n postNode) Send(to, typ, body string) (msg.Message, error) {
	n.p.mu.Lock()
	defer n.p.mu.Unlock()
	list := n.p.boxes[to]
	m := msg.Stored{N: int64(len(list) + 1), Message: msg.Message{From: n.self, To: to, Type: typ, Body: body}}
	n.p.boxes[to] = append(list, m)
	close(n.p.wake)
	n.p.wake = make(chan struct{})
	return m.Message, nil
}

func (n postNode) Wait(ctx context.Context, after int64, topic string) []msg.Stored {
	for {
		n.p.mu.Lock()
		var out []msg.Stored
		for _, m := range n.p.boxes[n.self] {
			if m.N > after {
				out = append(out, m)
			}
		}
		wake := n.p.wake
		n.p.mu.Unlock()
		if len(out) > 0 {
			return out
		}
		select {
		case <-wake:
		case <-ctx.Done():
			return nil
		}
	}
}

func TestMailReachesTheRecipient(t *testing.T) {
	var list []peers.Peer
	for i := range 6 {
		srv := httptest.NewServer(server.Handler(localstore.New(t.TempDir()), server.Options{
			Transport: transport.Loopback{}, Allowed: map[string]bool{"127.0.0.1": true}}))
		t.Cleanup(srv.Close)
		list = append(list, peers.Peer{Name: string(rune('a' + i)), Addr: strings.TrimPrefix(srv.URL, "http://")})
	}
	// The web node and the recipient's node: members, not storing here.
	list = append(list, peers.Peer{Name: "web", Addr: "127.0.0.2:9"}, peers.Peer{Name: "home", Addr: "127.0.0.3:9"})
	pb := &postbox{boxes: map[string][]msg.Stored{}, wake: make(chan struct{})}
	members := func() []peers.Peer { return list }
	c := client.New()
	web := &Node{Dir: t.TempDir(), Token: "secret", Self: "127.0.0.2", Msgs: postNode{pb, "127.0.0.2"}, Peers: members, Client: c}
	home := &Node{Dir: t.TempDir(), Box: t.TempDir(), Self: "127.0.0.3", Msgs: postNode{pb, "127.0.0.3"}, Peers: members, Client: c}
	for _, n := range []*Node{web, home} {
		if err := n.Start(); err != nil {
			t.Fatal(err)
		}
	}
	me := identity(t)
	sealed, _ := Seal(me.Code(), []byte(sample))

	post := func(token, node string, body []byte) int {
		r := httptest.NewRequest(http.MethodPost, "http://mail-in.example.org"+IngestPath+"?node="+node, bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		web.ServeHTTP(rec, r)
		return rec.Code
	}
	if code := post("wrong", "127.0.0.3", sealed); code != http.StatusForbidden {
		t.Fatalf("wrong token: %d", code)
	}
	if code := post("secret", "127.0.0.9", sealed); code != http.StatusBadRequest {
		t.Fatalf("not a member: %d", code)
	}
	if code := post("secret", "127.0.0.3", []byte(sample)); code != http.StatusBadRequest {
		t.Fatalf("unsealed: %d", code)
	}
	if code := post("secret", "127.0.0.3", sealed); code != http.StatusOK {
		t.Fatalf("post: %d", code)
	}
	var first *taken
	for _, tk := range web.st.Taken {
		first = tk
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go web.Run(ctx)
	go home.Run(ctx)
	deadline := time.Now().Add(20 * time.Second)
	for {
		web.mu.Lock()
		left := len(web.st.Taken)
		web.mu.Unlock()
		home.mu.Lock()
		todo := len(home.st.Todo)
		home.mu.Unlock()
		if left == 0 && todo == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not collected: web holds %d, home has %d to do", left, todo)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := files.Get(ctx, c, first.Stub, &bytes.Buffer{}, nil); err == nil {
		t.Fatal("the web node's copy is still there")
	}

	box := &Box{Dir: home.Box, ID: me}
	got, err := box.List()
	if err != nil || len(got) != 1 || got[0].Subject != "Café on Friday" || got[0].Received == 0 {
		t.Fatalf("list: %+v %v", got, err)
	}
	a, data, err := box.Attachment(got[0].ID, 0)
	if err != nil || a.Name != "menu.txt" || string(data) != "Coffee, tea." {
		t.Fatalf("attachment: %+v %q %v", a, data, err)
	}
	// The mailbox's own copy in the group can rebuild the message.
	own, err := files.ReadStub(filepath.Join(home.Box, got[0].ID+".ystub"))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := files.Get(ctx, c, own, &buf, nil); err != nil || !bytes.Equal(buf.Bytes(), sealed) {
		t.Fatalf("group copy: %v", err)
	}
	if err := box.Delete(ctx, c, got[0].ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := box.List(); len(got) != 0 {
		t.Fatalf("after delete: %+v", got)
	}
	if err := files.Get(ctx, c, own, &bytes.Buffer{}, nil); err == nil {
		t.Fatal("the group copy outlived the delete")
	}
}
