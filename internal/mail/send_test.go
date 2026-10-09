package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	netmail "net/mail"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterretief/yggstore/internal/peers"
)

func TestCompose(t *testing.T) {
	raw, rcpts, err := Draft{From: "me@example.org", Name: "Me\r\nBcc: evil@example.net", To: "A <a@example.net>, b@example.net",
		Cc: "c@example.net, a@example.net", Bcc: "hidden@example.net", Subject: "Grüße\r\nX-Evil: 1",
		Text:      "line one\nline two, long enough to be wrapped by quoted-printable " + strings.Repeat("x", 100),
		InReplyTo: "<orig@example.net>", References: "<first@example.net>"}.Compose()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(rcpts, " "); got != "a@example.net b@example.net c@example.net hidden@example.net" {
		t.Fatalf("recipients: %s", got)
	}
	m, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if m.Subject != "Grüße X-Evil: 1" || !strings.Contains(m.Text, "line one\r\nline two") || strings.Contains(string(raw), "hidden@") {
		t.Fatalf("parsed %+v\n%s", m, raw)
	}
	msg, _ := netmail.ReadMessage(bytes.NewReader(raw))
	if msg.Header.Get("Bcc") != "" || msg.Header.Get("X-Evil") != "" {
		t.Fatalf("a header got in:\n%s", raw)
	}
	if msg.Header.Get("References") != "<first@example.net> <orig@example.net>" || msg.Header.Get("In-Reply-To") != "<orig@example.net>" {
		t.Fatalf("threading:\n%s", raw)
	}
	if !strings.HasSuffix(msg.Header.Get("Message-ID"), "@example.org>") {
		t.Fatalf("message ID %q", msg.Header.Get("Message-ID"))
	}
	if _, _, err := (Draft{From: "me@example.org", Subject: "nobody"}).Compose(); err == nil {
		t.Fatal("composed a message to no one")
	}
}

func TestRelay(t *testing.T) {
	const me, other = "200::1", "200::2"
	var delivered []string
	r := &Relay{Senders: map[string]string{"me@example.org": me, "*@group.example": other},
		Deliver: func(ctx context.Context, from string, to []string, msg []byte) error {
			delivered = append(delivered, from+" "+strings.Join(to, ","))
			return nil
		}}
	send := func(caller string, from string, rcpt []string, extra string) int {
		raw, _, err := Draft{From: from, To: "x@example.net", Subject: "hi", Text: "hello"}.Compose()
		if err != nil {
			t.Fatal(err)
		}
		body, _ := json.Marshal(sendRequest{Rcpt: rcpt, Message: append([]byte(extra), raw...)})
		w := httptest.NewRecorder()
		if !r.Serve(w, httptest.NewRequest(http.MethodPost, SendPath, bytes.NewReader(body)), caller) {
			t.Fatal("not served")
		}
		return w.Code
	}
	to := []string{"x@example.net"}
	if c := send(me, "me@example.org", to, ""); c != http.StatusOK {
		t.Fatalf("own address: %d", c)
	}
	if c := send(other, "anyone@group.example", to, ""); c != http.StatusOK {
		t.Fatalf("domain: %d", c)
	}
	if c := send(other, "me@example.org", to, ""); c != http.StatusForbidden {
		t.Fatalf("someone else's address: %d", c)
	}
	if c := send(me, "Me@Example.org", to, "Sender: boss@example.org\r\n"); c != http.StatusForbidden {
		t.Fatalf("another Sender: %d", c)
	}
	if c := send(me, "me@example.org", to, "Bcc: y@example.net\r\n"); c != http.StatusBadRequest {
		t.Fatalf("Bcc header: %d", c)
	}
	if c := send(me, "me@example.org", []string{"x@example.net\r\nRCPT TO:<z@example.net>"}, ""); c != http.StatusBadRequest {
		t.Fatalf("bad recipient: %d", c)
	}
	if c := send(me, "me@example.org", nil, ""); c != http.StatusBadRequest {
		t.Fatalf("no recipients: %d", c)
	}
	if len(delivered) != 2 || delivered[0] != "me@example.org x@example.net" {
		t.Fatalf("delivered %q", delivered)
	}
	for i := 0; i < MaxPerHour; i++ {
		send(me, "me@example.org", to, "")
	}
	if c := send(me, "me@example.org", to, ""); c != http.StatusTooManyRequests {
		t.Fatalf("over the limit: %d", c)
	}
	if c := send(other, "anyone@group.example", to, ""); c != http.StatusOK {
		t.Fatalf("one member's limit stopped another: %d", c)
	}
}

func TestLoadRelay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mail-out.json")
	os.WriteFile(path, []byte(RelayExample), 0o600)
	if _, err := LoadRelay(path); err == nil {
		t.Fatal("loaded a relay with no login")
	}
	os.WriteFile(path, []byte(`{"smtp":"mail.example.net:587","user":"u","password":"p","senders":{"Me@Example.org":"200:0:0::1"}}`), 0o644)
	os.Chmod(path, 0o644)
	if _, err := LoadRelay(path); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("loaded a password others can read: %v", err)
	}
	os.Chmod(path, 0o600)
	r, err := LoadRelay(path)
	if err != nil {
		t.Fatal(err)
	}
	if !r.may("200::1", "me@example.org") {
		t.Fatalf("senders %v", r.Senders)
	}
}

func TestBoxAddress(t *testing.T) {
	b := &Box{Dir: t.TempDir()}
	if b.Address() != "" {
		t.Fatal("an address before one was set")
	}
	if err := b.SetAddress("me@example.org"); err != nil || b.Address() != "me@example.org" {
		t.Fatalf("address %q: %v", b.Address(), err)
	}
	if list, err := b.List(); err != nil || len(list) != 0 {
		t.Fatalf("the address file showed as mail: %v %v", list, err)
	}
}

func TestRelayNames(t *testing.T) {
	const anna, bob = "200::2", "200::3"
	list := []peers.Peer{
		{Name: "desktop", Addr: "[200::1]:7400", Admin: true, Domain: "group.example"},
		{Name: "anna", Addr: "[" + anna + "]:7400", Names: []string{"anna.group.example"}, Code: "ys1a"},
		{Name: "bob", Addr: "[" + bob + "]:7400"},
	}
	r := &Relay{Peers: func() []peers.Peer { return list }}
	if !r.may(anna, "Anna@group.example") {
		t.Fatal("anna may not send as her name")
	}
	if r.may(bob, "anna@group.example") || r.may(anna, "bob@group.example") || r.may(anna, "anna@other.example") {
		t.Fatal("sent as a name that isn't the node's")
	}
}

func TestMailboxLookup(t *testing.T) {
	list := []peers.Peer{
		{Name: "anna", Addr: "[200::2]:7400", Names: []string{"anna.group.example"}, Code: "ys1a"},
		{Name: "gw", Addr: "[200::4]:7400", Gateway: true, Names: []string{"gw.group.example"}, Code: "ys1g"},
	}
	n := &Node{Token: "secret", Peers: func() []peers.Peer { return list }}
	ask := func(to, token string) (int, string) {
		req := httptest.NewRequest(http.MethodGet, MailboxPath+"?to="+to, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		n.ServeHTTP(w, req)
		return w.Code, strings.TrimSpace(w.Body.String())
	}
	if c, body := ask("anna@group.example", "secret"); c != http.StatusOK || body != `{"code":"ys1a","node":"200::2"}` {
		t.Fatalf("anna: %d %s", c, body)
	}
	if c, _ := ask("anna@group.example", "wrong"); c != http.StatusForbidden {
		t.Fatalf("wrong token: %d", c)
	}
	for _, to := range []string{"bob@group.example", "gw@group.example", "anna"} {
		if c, _ := ask(to, "secret"); c != http.StatusNotFound {
			t.Fatalf("%s: %d", to, c)
		}
	}
}
