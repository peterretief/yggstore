package mailbridge

import (
	"context"
	"io"
	"net"
	"net/smtp"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap"
	imapclient "github.com/emersion/go-imap/client"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/mail"
	"github.com/peterretief/yggstore/internal/share"
)

const incoming = "From: Someone <someone@example.net>\r\nTo: me@example.org\r\nSubject: Hello there\r\nMessage-ID: <1@example.net>\r\nDate: Mon, 5 Oct 2026 10:00:00 +0000\r\n\r\nHi, how are you?\r\n"

func freeAddr(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

type fixture struct {
	box     *mail.Box
	b       *Bridge
	imap    string
	smtp    string
	mu      sync.Mutex
	sent    [][]string
	deleted []string
}

func setup(t *testing.T) *fixture {
	dir := t.TempDir()
	id, err := share.LoadOrCreate(filepath.Join(dir, "sharing.key"))
	if err != nil {
		t.Fatal(err)
	}
	box := &mail.Box{Dir: filepath.Join(dir, "mail"), ID: id}
	box.SetAddress("me@example.org")
	// A message the node collected.
	sealed, _ := mail.Seal(id.Code(), []byte(incoming))
	os.WriteFile(filepath.Join(box.Dir, "abc123.sealed"), sealed, 0o600)
	os.WriteFile(filepath.Join(box.Dir, "abc123.json"), []byte(`{"received":1791000000000}`), 0o600)

	f := &fixture{box: box, imap: freeAddr(t), smtp: freeAddr(t)}
	f.b = &Bridge{Box: box, Password: "correct horse battery staple 1234",
		Keep: func(ctx context.Context, raw []byte, folder string, flags []string, date time.Time) error {
			_, err := box.Keep(ctx, client.New(), nil, raw, folder, flags, date)
			if err != nil && strings.Contains(err.Error(), "kept here") {
				return nil
			}
			return err
		},
		Delete: func(ctx context.Context, id string) error {
			f.mu.Lock()
			f.deleted = append(f.deleted, id)
			f.mu.Unlock()
			return box.Delete(ctx, client.New(), id)
		},
		Send: func(ctx context.Context, raw []byte, rcpts []string) error {
			f.mu.Lock()
			f.sent = append(f.sent, append(rcpts, string(raw)))
			f.mu.Unlock()
			return nil
		},
		Log: t.Logf}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go f.b.ServeIMAP(ctx, f.imap)
	go f.b.ServeSMTP(ctx, f.smtp)
	for _, a := range []string{f.imap, f.smtp} {
		for i := 0; ; i++ {
			if c, err := net.Dial("tcp", a); err == nil {
				c.Close()
				break
			} else if i > 100 {
				t.Fatal(err)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	return f
}

func fetchAll(t *testing.T, c *imapclient.Client, items ...imap.FetchItem) []*imap.Message {
	seq, _ := imap.ParseSeqSet("1:*")
	ch := make(chan *imap.Message, 100)
	if err := c.Fetch(seq, items, ch); err != nil {
		t.Fatal(err)
	}
	var out []*imap.Message
	for m := range ch {
		out = append(out, m)
	}
	return out
}

func TestIMAP(t *testing.T) {
	f := setup(t)
	c, err := imapclient.Dial(f.imap)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Logout()
	if err := c.Login("me@example.org", "wrong"); err == nil {
		t.Fatal("logged in with a wrong password")
	}
	if err := c.Login("Me@Example.org", f.b.Password); err != nil {
		t.Fatal(err)
	}

	// The password is what counts: a mail program may give another user
	// name (an identity's address).
	if c2, err := imapclient.Dial(f.imap); err != nil {
		t.Fatal(err)
	} else if err := c2.Login("me@another.example", f.b.Password); err != nil {
		t.Fatalf("refused another user name: %v", err)
	} else {
		c2.Logout()
	}

	ch := make(chan *imap.MailboxInfo, 20)
	if err := c.List("", "*", ch); err != nil {
		t.Fatal(err)
	}
	var names []string
	for mi := range ch {
		names = append(names, mi.Name)
	}
	for _, want := range []string{"INBOX", "Sent", "Drafts", "Trash"} {
		if !slices.Contains(names, want) {
			t.Fatalf("folders %v lack %s", names, want)
		}
	}

	st, err := c.Select("INBOX", false)
	if err != nil || st.Messages != 1 {
		t.Fatalf("INBOX: %+v %v", st, err)
	}
	section := &imap.BodySectionName{}
	msgs := fetchAll(t, c, imap.FetchEnvelope, imap.FetchFlags, imap.FetchUid, section.FetchItem())
	if len(msgs) != 1 || msgs[0].Envelope.Subject != "Hello there" {
		t.Fatalf("fetched %+v", msgs)
	}
	body, _ := io.ReadAll(msgs[0].GetBody(section))
	if string(body) != incoming {
		t.Fatalf("body %q", body)
	}
	if msgs := fetchAll(t, c, imap.FetchFlags); !slices.Contains(msgs[0].Flags, imap.SeenFlag) {
		t.Fatalf("reading didn't mark it seen: %v", msgs[0].Flags)
	}
	uid := msgs[0].Uid

	// Drafts are kept like any message.
	draft := "From: me@example.org\r\nTo: x@example.net\r\nSubject: Draft\r\n\r\nNot yet.\r\n"
	if err := c.Append("Drafts", []string{imap.DraftFlag}, time.Now(), strings.NewReader(draft)); err != nil {
		t.Fatal(err)
	}
	if st, err := c.Status("Drafts", []imap.StatusItem{imap.StatusMessages}); err != nil || st.Messages != 1 {
		t.Fatalf("Drafts: %+v %v", st, err)
	}

	// Delete: move to Trash, then empty Trash.
	seq := new(imap.SeqSet)
	seq.AddNum(uid)
	if err := c.UidMove(seq, "Trash"); err != nil {
		t.Fatal(err)
	}
	if st, _ := c.Status("INBOX", []imap.StatusItem{imap.StatusMessages}); st.Messages != 0 {
		t.Fatalf("INBOX still holds %d", st.Messages)
	}
	if _, err := c.Select("Trash", false); err != nil {
		t.Fatal(err)
	}
	all, _ := imap.ParseSeqSet("1:*")
	if err := c.Store(all, imap.FormatFlagsOp(imap.AddFlags, true), []any{imap.DeletedFlag}, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Expunge(nil); err != nil {
		t.Fatal(err)
	}
	if len(f.deleted) != 1 || f.deleted[0] != "abc123" {
		t.Fatalf("deleted %v", f.deleted)
	}
	if _, err := os.Stat(filepath.Join(f.box.Dir, "abc123.sealed")); !os.IsNotExist(err) {
		t.Fatal("the message is still in the mailbox")
	}
}

func TestSMTP(t *testing.T) {
	f := setup(t)
	msg := "From: me@example.org\r\nTo: a@example.net\r\nSubject: Hi\r\n\r\nHello\r\n"
	auth := smtp.PlainAuth("", "me@example.org", f.b.Password, "127.0.0.1")
	if err := smtp.SendMail(f.smtp, auth, "me@example.org", []string{"a@example.net", "b@example.net"}, []byte(msg)); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 1 || f.sent[0][0] != "a@example.net" || f.sent[0][1] != "b@example.net" || !strings.Contains(f.sent[0][2], "Subject: Hi") {
		t.Fatalf("sent %q", f.sent)
	}
	other := smtp.PlainAuth("", "me@another.example", f.b.Password, "127.0.0.1")
	if err := smtp.SendMail(f.smtp, other, "me@example.org", []string{"a@example.net"}, []byte(msg)); err != nil {
		t.Fatalf("refused another user name: %v", err)
	}
	bad := smtp.PlainAuth("", "me@example.org", "wrong", "127.0.0.1")
	if err := smtp.SendMail(f.smtp, bad, "me@example.org", []string{"a@example.net"}, []byte(msg)); err == nil {
		t.Fatal("sent with a wrong password")
	}
	if err := smtp.SendMail(f.smtp, nil, "me@example.org", []string{"a@example.net"}, []byte(msg)); err == nil {
		t.Fatal("sent without logging in")
	}
}

func TestOnlyThisMachine(t *testing.T) {
	for _, a := range []string{"0.0.0.0:2143", "192.168.0.5:2143", "[::]:2587"} {
		if Loopback(a) == nil {
			t.Errorf("%s allowed", a)
		}
	}
	for _, a := range []string{"127.0.0.1:2143", "[::1]:2587", "localhost:2143"} {
		if err := Loopback(a); err != nil {
			t.Errorf("%s: %v", a, err)
		}
	}
}

// Mail the node collects and messages deleted from the dashboard reach a
// mail program with the folder open, and UIDNEXT never goes down.
func TestChangesElsewhere(t *testing.T) {
	f := setup(t)
	c, err := imapclient.Dial(f.imap)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Logout()
	updates := make(chan imapclient.Update, 20)
	c.Updates = updates
	if err := c.Login("me@example.org", f.b.Password); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Select("INBOX", false); err != nil {
		t.Fatal(err)
	}
	next := func() uint32 {
		st, err := c.Status("Archives", []imap.StatusItem{imap.StatusUidNext})
		if err != nil {
			t.Fatal(err)
		}
		return st.UidNext
	}

	// The node collects a second message.
	sealed, _ := mail.Seal(f.box.ID.Code(), []byte(strings.Replace(incoming, "Hello there", "Second", 1)))
	os.WriteFile(filepath.Join(f.box.Dir, "def456.json"), []byte(`{"received":1791000001000}`), 0o600)
	os.WriteFile(filepath.Join(f.box.Dir, "def456.sealed"), sealed, 0o600)
	f.b.sync("INBOX")
	c.Noop()
	if n := c.Mailbox().Messages; n != 2 {
		t.Fatalf("told of %d messages, want 2", n)
	}

	// Plain SEARCH by flags alone, numbered as the client knows them.
	crit := imap.NewSearchCriteria()
	crit.WithoutFlags = []string{imap.SeenFlag}
	if seqs, err := c.Search(crit); err != nil || len(seqs) != 2 {
		t.Fatalf("unseen: %v %v", seqs, err)
	}

	// The dashboard deletes the first; the client hears of it.
	if err := f.box.Delete(context.Background(), client.New(), "abc123"); err != nil {
		t.Fatal(err)
	}
	for len(updates) > 0 {
		<-updates
	}
	f.b.sync("INBOX")
	c.Noop()
	var expunged []uint32
	for len(updates) > 0 {
		if u, ok := (<-updates).(*imapclient.ExpungeUpdate); ok {
			expunged = append(expunged, u.SeqNum)
		}
	}
	if !slices.Equal(expunged, []uint32{1}) {
		t.Fatalf("told of expunged %v, want [1]", expunged)
	}
	msgs := fetchAll(t, c, imap.FetchEnvelope)
	if len(msgs) != 1 || msgs[0].SeqNum != 1 || msgs[0].Envelope.Subject != "Second" {
		t.Fatalf("after the delete: %+v", msgs)
	}

	// Archive it and take it back out: Archives' UIDNEXT only rises.
	before := next()
	all, _ := imap.ParseSeqSet("1:*")
	if err := c.Move(all, "Archives"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Select("Archives", false); err != nil {
		t.Fatal(err)
	}
	if err := c.Move(all, "INBOX"); err != nil {
		t.Fatal(err)
	}
	if after := next(); after <= before {
		t.Fatalf("UIDNEXT went from %d to %d", before, after)
	}
}

func TestNeedsContent(t *testing.T) {
	c := imap.NewSearchCriteria()
	c.WithoutFlags = []string{imap.SeenFlag}
	if needsContent(c) {
		t.Error("a flag search opens messages")
	}
	sub := imap.NewSearchCriteria()
	sub.Text = []string{"invoice"}
	c.Not = []*imap.SearchCriteria{sub}
	if !needsContent(c) {
		t.Error("NOT TEXT doesn't open messages")
	}
}
