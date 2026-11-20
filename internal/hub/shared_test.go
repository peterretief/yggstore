package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	netmail "net/mail"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/mail"
	"github.com/peterretief/yggstore/internal/peers"
)

func TestEnsureShared(t *testing.T) {
	var s HubState
	s.AddPerson("Sales", t.TempDir()) // a person has the name already
	added, taken := s.EnsureShared(SharedNames(DefaultShared), "users", time.Now())
	if strings.Join(added, ",") != "support,accounts" || strings.Join(taken, ",") != "sales" {
		t.Fatalf("added %v, taken %v", added, taken)
	}
	if added, taken = s.EnsureShared(SharedNames(DefaultShared), "users", time.Now()); added != nil || len(taken) != 1 {
		t.Fatalf("again: added %v, taken %v", added, taken)
	}
	if n := len(s.SharedPeople()); n != 2 {
		t.Fatalf("%d shared", n)
	}
	// Removed by the box's admin: it stays off.
	s.People[s.person("support")].Removed = true
	if added, _ = s.EnsureShared([]string{"support"}, "users", time.Now()); added != nil || len(s.SharedPeople()) != 1 {
		t.Fatal("a removed shared mailbox came back")
	}
	if got := SharedNames(" Support, ,sa/les,accounts "); strings.Join(got, ",") != "support,accounts" {
		t.Fatalf("names: %v", got)
	}
}

// Anyone on the box's network opens a shared mailbox from the info page,
// with no password; nobody does from anywhere else, or through a proxy,
// and a person's own mailbox isn't opened that way.
func TestSharedMailboxes(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "hub.json")
	Update(context.Background(), statePath, func(s *HubState) error {
		s.AddPerson("Kath", filepath.Join(dir, "users"))
		s.EnsureShared(SharedNames(DefaultShared), filepath.Join(dir, "users"), time.Now())
		return nil
	})
	peersPath := filepath.Join(dir, "peers.json")
	peers.Write(peersPath, []peers.Peer{{Name: "admin", Addr: "[200::1]:7400", Admin: true, Domain: "group.example"}, {Name: "localbox-004", Addr: "[200::2]:7400"}})
	ps := NewPeopleSites(PeopleSitesConfig{StatePath: statePath, PeersPath: peersPath, Self: "200::2", Dir: filepath.Join(dir, "people"), Logf: t.Logf})
	u := &Users{cfg: UsersConfig{StatePath: statePath, PeersPath: peersPath, People: ps}}
	f := &filesPage{u: u, self: "200::2"}
	do := func(path, remote string, hdr ...string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		r.RemoteAddr, r.Host = remote, "mail.local"
		for i := 0; i+1 < len(hdr); i += 2 {
			r.Header.Set(hdr[i], hdr[i+1])
		}
		w := httptest.NewRecorder()
		if !f.ServeWebmail(w, r) {
			t.Fatalf("%s wasn't webmail's", path)
		}
		return w
	}
	const lan = "192.168.1.20:5555"
	w := do("/info", lan)
	for _, want := range []string{"Mail on localbox-004", "support@local.4", "/webmail/shared/sales", "/webmail/shared/accounts", "Open your own mail"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Fatalf("info page lacks %q: %s", want, w.Body)
		}
	}
	if strings.Contains(w.Body.String(), "kath") {
		t.Fatal("the info page lists a person's own mailbox")
	}
	if w := do("/webmail/shared/support", lan); w.Code != 200 || !strings.Contains(w.Body.String(), "<title>Your mail</title>") {
		t.Fatalf("support: %d", w.Code)
	}
	w = do("/webmail/shared/support?op=status", lan)
	var st sitePageState
	if json.Unmarshal(w.Body.Bytes(), &st); st.Person != "support" || st.Address != "support@local.4" {
		t.Fatalf("status: %s", w.Body)
	}
	for _, c := range []struct {
		path, remote string
		hdr          []string
		code         int
	}{
		{"/webmail/shared/support", "41.1.2.3:5555", nil, 403},                                      // the internet
		{"/info", "41.1.2.3:5555", nil, 403},                                                        //
		{"/webmail/shared/support", "127.0.0.1:5555", []string{"X-Forwarded-For", "41.1.2.3"}, 403}, // a proxy on the box
		{"/webmail/shared/support", lan, []string{"Cf-Connecting-Ip", "41.1.2.3"}, 403},             // a tunnel
		{"/webmail/shared/kath", lan, nil, 404},                                                     // a person's own
		{"/webmail/shared/nobody", lan, nil, 404},
		{"/webmail/shared/support?op=pushkey", lan, nil, 403},
	} {
		if w := do(c.path, c.remote, c.hdr...); w.Code != c.code {
			t.Errorf("%s from %s %v: %d, not %d", c.path, c.remote, c.hdr, w.Code, c.code)
		}
	}
	// A shared mailbox has no password to sign in with.
	if st, _ := Load(statePath); st.People[1].Password != nil {
		t.Fatal("a shared mailbox has a password")
	}
}

func TestSharedCopiesFlag(t *testing.T) {
	c, err := SharedCopies(" Support=Signs-Support@example.internal, sales = signs-sales@example.org ")
	if err != nil || c["support"] != "signs-support@example.internal" || c["sales"] != "signs-sales@example.org" || len(c) != 2 {
		t.Fatalf("%v %v", c, err)
	}
	if _, err := SharedCopies("support"); err == nil {
		t.Fatal("no address taken")
	}
	var s HubState
	if _, err := s.AddPerson("Kath", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	s.EnsureShared([]string{"support", "sales"}, "users", time.Now())
	if ch := s.SetCopies(c, time.Now()); len(ch) != 2 {
		t.Fatalf("changed %v", ch)
	}
	if ch := s.SetCopies(c, time.Now()); len(ch) != 0 {
		t.Fatalf("changed again %v", ch)
	}
	if s.People[0].CopyTo != "" {
		t.Fatal("a person's own mail is passed on")
	}
	if ch := s.SetCopies(map[string]string{"support": c["support"]}, time.Now()); len(ch) != 1 || s.People[2].CopyTo != "" {
		t.Fatalf("sales still passes on: %v", ch)
	}
	if !sameMailbox("a@example.internal", "A@example.org") || sameMailbox("a@example.org", "b@example.org") || sameMailbox("a@example.org", "a@other.org") {
		t.Fatal("sameMailbox")
	}
}

// A shared mailbox passes what people wrote on to its CopyTo, once each,
// keeping who wrote it; not what a program sent, nor what came from there,
// so the app's emails back to the box don't loop.
func TestCopyShared(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "hub.json")
	ctx := context.Background()
	st, _ := Update(ctx, statePath, func(s *HubState) error {
		s.EnsureShared([]string{"support"}, filepath.Join(dir, "users"), time.Now())
		s.SetCopies(map[string]string{"support": "signs-support@group.internal"}, time.UnixMilli(100))
		return nil
	})
	support := st.People[0]
	key, err := PersonKey(statePath, support)
	if err != nil {
		t.Fatal(err)
	}
	list := []peers.Peer{{Name: "admin", Addr: "[200::1]:7400", Admin: true, Domain: "group.example"}, {Name: "localbox-004", Addr: "[200::2]:7400"}}
	peersPath := filepath.Join(dir, "peers.json")
	peers.Write(peersPath, list)
	ps := NewPeopleSites(PeopleSitesConfig{StatePath: statePath, PeersPath: peersPath, Self: "200::2", Dir: filepath.Join(dir, "people"), Logf: t.Logf,
		Online: func(context.Context) ([]peers.Peer, error) { return list, nil }})
	mdir := ps.mailDir(support)
	os.MkdirAll(mdir, 0o700)
	put := func(id string, received int, head string) {
		sealed, _ := mail.Seal(key.Code(), []byte(head+"\r\n\r\nThe sign at the mall is dark.\r\n"))
		os.WriteFile(filepath.Join(mdir, id+".sealed"), sealed, 0o600)
		os.WriteFile(filepath.Join(mdir, id+".json"), []byte(`{"received":`+strconv.Itoa(received)+`}`), 0o600)
	}
	put("person", 200, "From: Kath <kath@local.5>\r\nTo: support@local.4\r\nSubject: Sign dark")
	put("old", 50, "From: kath@local.5\r\nSubject: Before copies began")
	put("auto", 200, "From: notes@group.example\r\nAuto-Submitted: auto-generated\r\nSubject: Ticket solved")
	put("back", 200, "From: signs-support@group.example\r\nSubject: Re: Sign dark")
	var sent [][]byte
	sendShared = func(_ context.Context, _ client.Client, _ []peers.Peer, raw []byte, rcpts []string) error {
		if strings.Join(rcpts, ",") != "signs-support@group.internal" {
			t.Errorf("sent to %v", rcpts)
		}
		sent = append(sent, raw)
		return nil
	}
	defer func() { sendShared = mail.Send }()
	ps.copyShared(ctx, list, support, key)
	ps.copyShared(ctx, list, support, key) // once each
	if len(sent) != 1 {
		t.Fatalf("%d sent", len(sent))
	}
	msg, _ := netmail.ReadMessage(bytes.NewReader(sent[0]))
	h := msg.Header
	for k, want := range map[string]string{"From": "support@local.4", "Reply-To": "kath@local.5", "Subject": "Sign dark",
		"Auto-Submitted": "auto-forwarded", "X-Box": "localbox-004", "X-Original-From": "kath@local.5"} {
		if !strings.Contains(h.Get(k), want) {
			t.Errorf("%s: %q, not %q", k, h.Get(k), want)
		}
	}
	if body, _ := io.ReadAll(msg.Body); !strings.Contains(string(body), "The sign at the mall is dark.") {
		t.Errorf("body: %s", body)
	}
}
