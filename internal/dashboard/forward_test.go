package dashboard

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/mail"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/share"
)

// A site's contact messages go on to an address once it has confirmed with
// the code sent to it; the rest of the mail, and older messages, don't.
func TestForwardSiteMessages(t *testing.T) {
	dir := t.TempDir()
	id, err := share.LoadOrCreate(filepath.Join(dir, "sharing.key"))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var sent []string // the messages the relay took
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/info":
			w.Write([]byte(`{"mail_out":true}`))
		case mail.SendPath:
			var req struct {
				Rcpt    []string `json:"rcpt"`
				Message []byte   `json:"message"`
			}
			json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req)
			mu.Lock()
			sent = append(sent, strings.Join(req.Rcpt, ",")+"\n"+string(req.Message))
			mu.Unlock()
		default:
			http.NotFound(w, r)
		}
	}))
	defer relay.Close()
	peersPath := filepath.Join(dir, "peers.json")
	peers.Write(peersPath, []peers.Peer{{Name: "web", Addr: strings.TrimPrefix(relay.URL, "http://")}})
	d := New(Config{PeersPath: peersPath, StubDir: dir, Client: client.New(), Identity: id,
		MailDir: filepath.Join(dir, "mail"), SitesDir: filepath.Join(dir, "sites")})
	box := &mail.Box{Dir: d.cfg.MailDir, ID: id}
	if err := box.SetAddress("peter@example.org"); err != nil {
		t.Fatal(err)
	}
	n := 0
	arrive := func(from, subject string, at time.Time) {
		n++
		raw := "From: " + from + "\r\nTo: " + from + "\r\nReply-To: visitor@example.net\r\nSubject: " + subject +
			"\r\nDate: " + at.Format(time.RFC1123Z) + "\r\n\r\nname: A visitor\r\nmessage: hello\r\n"
		sealed, err := mail.Seal(id.Code(), []byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		mid := "m" + string(rune('0'+n))
		os.MkdirAll(d.cfg.MailDir, 0o700)
		os.WriteFile(filepath.Join(d.cfg.MailDir, mid+".sealed"), sealed, 0o600)
		meta, _ := json.Marshal(mail.Meta{Received: at.UnixMilli()})
		os.WriteFile(filepath.Join(d.cfg.MailDir, mid+".json"), meta, 0o600)
	}
	ctx := context.Background()
	const site = "ngo.example.org"
	sentCount := func() int { mu.Lock(); defer mu.Unlock(); return len(sent) }

	arrive("Contact form on "+site+" <contact-form@"+site+">", "["+site+"] before", time.Now().Add(-time.Hour))
	if err := d.sendForwardCode(ctx, site, "not an address"); err == nil {
		t.Fatal("a code went to something that isn't an address")
	}
	if err := d.sendForwardCode(ctx, site, "kath@example.net"); err != nil {
		t.Fatal(err)
	}
	if sentCount() != 1 || !strings.HasPrefix(sent[0], "kath@example.net\n") {
		t.Fatalf("the code went %q", sent)
	}
	code := sent[0][strings.Index(sent[0], "Your code is ")+13:][:6]
	d.forwardAll(ctx)
	if sentCount() != 1 {
		t.Fatal("forwarded before the address confirmed")
	}
	if err := d.confirmForward(site, "000000x", time.Now()); err == nil {
		t.Fatal("a wrong code was taken")
	}
	if err := d.confirmForward(site, code, time.Now()); err != nil {
		t.Fatal(err)
	}

	arrive("Contact form on "+site+" <contact-form@"+site+">", "["+site+"] a question", time.Now().Add(time.Second))
	arrive("Contact form on other.example.org <contact-form@other.example.org>", "[other.example.org] not this one", time.Now().Add(time.Second))
	arrive("Someone <someone@example.net>", "ordinary mail", time.Now().Add(time.Second))
	d.forwardAll(ctx)
	d.forwardAll(ctx) // once only
	if sentCount() != 2 {
		t.Fatalf("forwarded %d, want the one new message from %s:\n%s", sentCount()-1, site, strings.Join(sent[1:], "\n----\n"))
	}
	got := sent[1]
	for _, want := range []string{"kath@example.net\n", "a question", "Reply-To: <visitor@example.net>", "hello", "peter@example.org"} {
		if !strings.Contains(got, want) {
			t.Errorf("forwarded message lacks %q:\n%s", want, got)
		}
	}

	// Stopped, nothing more goes.
	if err := d.saveForward(site, nil); err != nil {
		t.Fatal(err)
	}
	arrive("Contact form on "+site+" <contact-form@"+site+">", "["+site+"] after", time.Now().Add(2*time.Second))
	d.forwardAll(ctx)
	if sentCount() != 2 {
		t.Fatal("forwarded after it was stopped")
	}
}
