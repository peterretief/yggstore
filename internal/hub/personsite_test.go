package hub

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/mail"
	"github.com/peterretief/yggstore/internal/peers"
)

func TestForwardCode(t *testing.T) {
	now := time.Now()
	f := &Forward{To: "anna@example.com", CodeHash: tokenHash("123456"), SentAt: now.Unix()}
	if Confirm(f, "654321", now) == nil || f.Tries != 1 {
		t.Fatal("a wrong code was taken")
	}
	if err := Confirm(f, " 123456 ", now); err != nil || !f.Confirmed || f.On() {
		t.Fatalf("confirmed %v, on %v: %v", f.Confirmed, f.On(), err)
	}
	old := &Forward{CodeHash: tokenHash("1"), SentAt: now.Add(-2 * time.Hour).Unix()}
	if Confirm(old, "1", now) == nil {
		t.Fatal("an old code was taken")
	}
	tried := &Forward{CodeHash: tokenHash("1"), SentAt: now.Unix(), Tries: codeTries}
	if Confirm(tried, "1", now) == nil {
		t.Fatal("taken after too many tries")
	}

	var s HubState
	s.AddPerson("Anna", t.TempDir())
	if s.AllowForward("Anna", true) == nil {
		t.Fatal("allowed with no confirmed address")
	}
	s.People[0].Forward = f
	if err := s.AllowForward("anna", true); err != nil || !s.People[0].Forward.On() {
		t.Fatalf("allow: %v", err)
	}
	s.AllowForward("Anna", false)
	if s.People[0].Forward != nil {
		t.Fatal("still forwarding")
	}
}

// Asking again for a forwarding code sends the same one, not too often,
// so two emails never hold different codes.
func TestForwardCodeAgain(t *testing.T) {
	now := time.Now()
	code, at, err := codeToSend(nil, "anna@example.com", now)
	if err != nil || len(code) != 6 || at != now.Unix() {
		t.Fatalf("first: %q %d %v", code, at, err)
	}
	f := &Forward{To: "anna@example.com", Code: code, CodeHash: tokenHash(code), SentAt: at}
	if _, _, err := codeToSend(f, "anna@example.com", now.Add(time.Minute)); err == nil || !strings.Contains(err.Error(), "spam") {
		t.Fatalf("again at once: %v", err)
	}
	if c, a, err := codeToSend(f, "Anna@example.com", now.Add(codeEvery)); err != nil || c != code || a != at {
		t.Fatalf("again later: %q %d %v", c, a, err)
	}
	f.Resent = now.Add(codeEvery).Unix()
	if _, _, err := codeToSend(f, "anna@example.com", now.Add(codeEvery+time.Minute)); err == nil {
		t.Fatal("sent again soon after resending")
	}
	if c, _, _ := codeToSend(f, "other@example.com", now.Add(time.Minute)); c == "" {
		t.Fatal("no code for another address")
	}
	if c, a, _ := codeToSend(f, "anna@example.com", now.Add(codeFor)); a == at {
		t.Fatalf("an almost run-out code was sent again: %q", c)
	}
}

// Mail sealed for someone on the box moves from the node's mailbox into
// theirs, once collected; the box owner's own mail stays.
func TestSortMail(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "hub.json")
	nodeMail := filepath.Join(dir, "nodemail")
	os.MkdirAll(nodeMail, 0o700)
	ctx := context.Background()
	st, _ := Update(ctx, statePath, func(s *HubState) error {
		_, err := s.AddPerson("Anna", filepath.Join(dir, "users"))
		return err
	})
	anna := st.People[0]
	key, err := PersonKey(statePath, anna)
	if err != nil {
		t.Fatal(err)
	}
	peersPath := filepath.Join(dir, "peers.json")
	peers.Write(peersPath, []peers.Peer{{Name: "box", Addr: "[200::2]:7400"}})
	put := func(id, code string, collected bool) {
		sealed, err := mail.Seal(code, []byte("Subject: hi\r\n\r\nhello\r\n"))
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(nodeMail, id+".sealed"), sealed, 0o600)
		os.WriteFile(filepath.Join(nodeMail, id+".json"), []byte(`{"received":1}`), 0o600)
		if collected {
			os.WriteFile(filepath.Join(nodeMail, id+".ystub"), []byte(`{}`), 0o600)
		}
	}
	other, _ := PersonKey(filepath.Join(dir, "elsewhere", "hub.json"), Person{Folder: "owner"})
	put("forAnna", key.Code(), true)
	put("forOwner", other.Code(), true)
	put("stillComing", key.Code(), false)

	ps := NewPeopleSites(PeopleSitesConfig{StatePath: statePath, PeersPath: peersPath, Self: "200::2", NodeMail: nodeMail,
		Dir: filepath.Join(dir, "people"), Logf: t.Logf})
	ps.Step(ctx)

	box, _ := ps.Mailbox(anna)
	msgs, err := box.List()
	if err != nil || len(msgs) != 1 || msgs[0].ID != "forAnna" || msgs[0].Subject != "hi" {
		t.Fatalf("Anna's mailbox: %+v %v", msgs, err)
	}
	for _, id := range []string{"forOwner", "stillComing"} {
		if _, err := os.Stat(filepath.Join(nodeMail, id+".sealed")); err != nil {
			t.Errorf("%s left the node's mailbox", id)
		}
	}
	if _, err := os.Stat(filepath.Join(nodeMail, "forAnna.sealed")); err == nil {
		t.Error("Anna's message is still in the node's mailbox")
	}
	if st, _ := Load(statePath); st.People[0].Code != key.Code() {
		t.Fatal("her code isn't on the box's state")
	}
}
