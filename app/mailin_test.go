package app

import (
	"encoding/json"
	netmail "net/mail"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterretief/yggstore/internal/mail"
	"github.com/peterretief/yggstore/internal/share"
)

// drop puts a message in an app folder's inbox, as the machine's node does.
func drop(t *testing.T, dir string, key *share.Identity, id, raw, inGroup string) {
	t.Helper()
	sealed, err := mail.Seal(key.Code(), []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(dir, "inbox"), 0o700)
	meta, _ := json.Marshal(mail.Meta{Received: 1764662400000, InGroup: inGroup})
	os.WriteFile(filepath.Join(dir, "inbox", id+".json"), meta, 0o600)
	os.WriteFile(filepath.Join(dir, "inbox", id+".sealed"), sealed, 0o600)
}

func TestMailIn(t *testing.T) {
	if p := DesignProblems(t, "testdata/helpdesk.sql"); len(p) > 0 {
		t.Fatalf("problems: %v", p)
	}
	s := NewTestSite(t, "testdata/helpdesk.sql", map[string][2]string{
		"boss": {"boss@example.org", roleAdmin},
		"sam":  {"sam@example.org", roleStaff},
		"cust": {"customer@example.net", roleClient},
	})
	bag := &mailbag{}
	d := s.App.data
	d.notify.send = bag.send
	dir := t.TempDir()
	key, err := share.LoadOrCreate(filepath.Join(dir, "sharing.key"))
	if err != nil {
		t.Fatal(err)
	}
	drop(t, dir, key, "m1", "Delivered-To: support@client.example\r\nFrom: Cee Customer <Customer@Example.net>\r\nTo: support@client.example\r\n"+
		"Subject: The printer\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain\r\n\r\nIt won't print.\r\n"+
		"--b\r\nContent-Type: text/plain; name=log.txt\r\nContent-Disposition: attachment; filename=\"../log.txt\"\r\n\r\npaper jam\r\n--b--\r\n", "")
	// Outside mail claiming to be a member, to a members-only address.
	drop(t, dir, key, "m2", "Delivered-To: leave@group.example\r\nFrom: sam@example.org\r\nSubject: Leave\r\n\r\nMe\r\n", "")
	// The same, from a member through the group's mail.
	drop(t, dir, key, "m3", "Delivered-To: leave@group.example\r\nFrom: sam@example.org\r\nSubject: Leave\r\n\r\n2 to 6 December\r\n", "sam@example.org")
	drop(t, dir, key, "m4", "Delivered-To: nobody@group.example\r\nFrom: x@example.net\r\nSubject: ?\r\n\r\n?\r\n", "")
	// Sealed for someone else.
	other, _ := share.LoadOrCreate(filepath.Join(t.TempDir(), "other.key"))
	drop(t, dir, other, "m5", "Delivered-To: support@client.example\r\nFrom: x@example.net\r\nSubject: ?\r\n\r\n?\r\n", "")

	n, err := d.takeMail(dir, s.App.docs)
	if err != nil || n != 2 {
		t.Fatalf("took %d: %v", n, err)
	}
	var id, login, name, subject, body, received, status string
	var member bool
	if err := s.DB().QueryRow(`SELECT id, login, sender_name, subject, body, received, member, status FROM tickets`).
		Scan(&id, &login, &name, &subject, &body, &received, &member, &status); err != nil {
		t.Fatal(err)
	}
	if login != "customer@example.net" || name != "Cee Customer" || subject != "The printer" || !strings.Contains(body, "won't print") ||
		!strings.HasPrefix(received, "2025-12-02") || member || status != "new" {
		t.Fatalf("ticket: %q %q %q %q %q %v %q", login, name, subject, body, received, member, status)
	}
	var leave string
	s.DB().QueryRow(`SELECT login FROM leave_requests`).Scan(&leave)
	if leave != "sam@example.org" {
		t.Fatalf("leave: %q", leave)
	}
	refused, _ := filepath.Glob(filepath.Join(dir, "refused", "*.why"))
	if len(refused) != 3 {
		t.Fatalf("refused: %v", refused)
	}
	why, _ := os.ReadFile(filepath.Join(dir, "refused", "m2.why"))
	if !strings.Contains(string(why), "members' mail only") {
		t.Fatalf("m2: %s", why)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "inbox", "*")); len(left) != 0 {
		t.Fatalf("left in the inbox: %v", left)
	}
	waitFor(t, "staff told, the customer answered", func() bool { return bag.count() == 2 })
	var toStaff, toCustomer bool
	for _, m := range bag.sent {
		toStaff = toStaff || (m.To == "sam@example.org" && strings.Contains(m.Subject, "Tickets new: The printer"))
		toCustomer = toCustomer || (m.To == "customer@example.net" && m.Subject == "Re: The printer")
	}
	if !toStaff || !toCustomer {
		t.Fatalf("sent: %+v", bag.sent)
	}
	// The customer sees their ticket, and its attachment is kept.
	if page := s.Get("cust", tableLink("tickets")+id); !strings.Contains(page, "The printer") || !strings.Contains(page, "Status: <b>new</b>") {
		t.Fatalf("customer's page:\n%s", page)
	}
	if docs := s.Get("boss", "/app/docs/"); !strings.Contains(docs, "Mail in") {
		t.Fatalf("documents:\n%s", docs)
	}
	var kept string
	s.DB().QueryRow(`SELECT name FROM _docs WHERE folder = 0`).Scan(&kept)
	if kept != ".._log.txt" && kept != "log.txt" {
		t.Fatalf("attachment kept as %q", kept)
	}
	// Staff take it on and solve it; the customer is told.
	for _, a := range []string{"takes", "solves"} {
		if c := s.Post("sam", tableLink("tickets")+id+"/step", map[string][]string{"action": {a}}).Code; c != 303 {
			t.Fatalf("%s: %d", a, c)
		}
	}
	waitFor(t, "the customer told", func() bool { return bag.count() == 3 })
	if to, subject, _ := bag.last(); to != "customer@example.net" || subject != "Tickets solved: The printer" {
		t.Fatalf("told %s: %s", to, subject)
	}
	if c := s.Post("cust", tableLink("tickets")+id+"/step", map[string][]string{"action": {"reopens"}}).Code; c != 303 {
		t.Fatalf("customer reopens: %d", c)
	}
}

func TestNoAnswer(t *testing.T) {
	for raw, want := range map[string]bool{
		"From: a@example.net\r\n\r\n":                                 false,
		"From: a@example.net\r\nAuto-Submitted: auto-replied\r\n\r\n": true,
		"From: a@example.net\r\nPrecedence: bulk\r\n\r\n":             true,
		"From: a@example.net\r\nList-Id: <x.example.net>\r\n\r\n":     true,
		"From: noreply@example.net\r\n\r\n":                           true,
	} {
		m, _ := netmailRead(raw)
		if got := noAnswer(m, fromOf(raw)); got != want {
			t.Errorf("%q: %v", raw, got)
		}
	}
}

func netmailRead(raw string) (netmail.Header, error) {
	m, err := netmail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		return nil, err
	}
	return m.Header, nil
}

func fromOf(raw string) string {
	h, _ := netmailRead(raw)
	a, _ := netmail.ParseAddress(h.Get("From"))
	if a == nil {
		return ""
	}
	return a.Address
}
