package app

import (
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func leaveSite(t *testing.T) (*TestSite, *mailbag) {
	t.Helper()
	s := NewTestSite(t, "testdata/leave.sql", map[string][2]string{
		"boss": {"boss@example.org", roleAdmin},
		"anna": {"anna@example.org", roleStaff},
		"bob":  {"bob@example.org", roleStaff},
		"cat":  {"cat@example.org", roleClient},
	})
	bag := &mailbag{}
	s.App.data.notify.send = bag.sendFrom
	return s, bag
}

func leaveID(t *testing.T, s *TestSite, who string) string {
	t.Helper()
	var id string
	if err := s.DB().QueryRow(`SELECT id FROM leave WHERE login = ? ORDER BY rowid DESC LIMIT 1`, who).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func status(t *testing.T, s *TestSite, id string) string {
	t.Helper()
	var st string
	s.DB().QueryRow(`SELECT status FROM leave WHERE id = ?`, id).Scan(&st)
	return st
}

func TestLeaveSteps(t *testing.T) {
	if p := DesignProblems(t, "testdata/leave.sql"); len(p) > 0 {
		t.Fatalf("problems: %v", p)
	}
	s, bag := leaveSite(t)
	// A status sent with the form is ignored: a new row starts as a draft.
	w := s.Post("anna", tableLink("leave")+"new", url.Values{"login": {"anna@example.org"}, "first_day": {"2026-12-02"},
		"last_day": {"2026-12-06"}, "reason": {"Family"}, "status": {"approved"}})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("new: %d %s", w.Code, w.Body)
	}
	id := leaveID(t, s, "anna@example.org")
	if st := status(t, s, id); st != "draft" {
		t.Fatalf("starts as %q", st)
	}
	page := s.Get("anna", tableLink("leave")+id)
	if !strings.Contains(page, `value="submits">Submit</button>`) || strings.Contains(page, "Approve") || !strings.Contains(page, "Made, draft") {
		t.Fatalf("anna's page:\n%s", page)
	}
	if strings.Contains(page, `name="status"`) {
		t.Fatal("the status can be typed in")
	}
	step := func(as, action, comment string) int {
		return s.Post(as, tableLink("leave")+id+"/step", url.Values{"action": {action}, "comment": {comment}}).Code
	}
	if c := step("anna", "approves", ""); c != http.StatusForbidden {
		t.Fatalf("anna approved her own: %d", c)
	}
	if c := step("bob", "submits", ""); c != http.StatusNotFound {
		t.Fatalf("bob moved anna's: %d", c)
	}
	if c := step("cat", "submits", ""); c != http.StatusNotFound {
		t.Fatalf("a client moved it: %d", c)
	}
	if c := step("boss", "cancels", ""); c != http.StatusForbidden {
		t.Fatalf("a step that doesn't start here: %d", c)
	}
	// Saving the form can't move it either.
	s.Post("anna", tableLink("leave")+id, url.Values{"login": {"anna@example.org"}, "first_day": {"2026-12-02"}, "last_day": {"2026-12-07"}, "status": {"approved"}})
	if st := status(t, s, id); st != "draft" {
		t.Fatalf("the form moved it to %q", st)
	}
	if c := step("anna", "submits", ""); c != http.StatusSeeOther {
		t.Fatalf("submit: %d", c)
	}
	waitFor(t, "the admins told", func() bool { return bag.count() == 1 })
	to, subject, body := bag.last()
	if to != "boss@example.org" || subject != "Leave submitted: anna@example.org" || !strings.Contains(body, "anna@example.org pressed Submit") ||
		!strings.Contains(body, "https://example.com/app/data/leave/"+id) {
		t.Fatalf("told: %s / %s / %s", to, subject, body)
	}
	if c := step("boss", "approves", "Enjoy it"); c != http.StatusSeeOther {
		t.Fatalf("approve: %d", c)
	}
	waitFor(t, "anna told", func() bool { return bag.count() == 2 })
	if to, subject, body := bag.last(); to != "anna@example.org" || !strings.Contains(subject, "approved") || !strings.Contains(body, "Enjoy it") {
		t.Fatalf("told: %s / %s / %s", to, subject, body)
	}
	if c := step("boss", "declines", ""); c != http.StatusForbidden {
		t.Fatalf("declined after approving: %d", c)
	}
	page = s.Get("anna", tableLink("leave")+id)
	for _, want := range []string{"Status: <b>approved</b>", "Submit: draft → submitted", "Approve: submitted → approved · boss@example.org", "<q>Enjoy it</q>", `value="cancels">Cancel</button>`} {
		if !strings.Contains(page, want) {
			t.Fatalf("no %q in:\n%s", want, page)
		}
	}
	// The report groups by status.
	if r := s.Get("boss", tableLink("leave")+"?by=status"); !strings.Contains(r, "<td>approved</td>") {
		t.Fatalf("report:\n%s", r)
	}
}

func TestLeaveReminder(t *testing.T) {
	s, bag := leaveSite(t)
	s.Post("anna", tableLink("leave")+"new", url.Values{"login": {"anna@example.org"}, "first_day": {"2026-12-02"}, "last_day": {"2026-12-06"}})
	id := leaveID(t, s, "anna@example.org")
	s.Post("anna", tableLink("leave")+id+"/step", url.Values{"action": {"submits"}})
	waitFor(t, "the admins told", func() bool { return bag.count() == 1 })
	d := s.App.data
	d.remindAll(time.Now().Add(47 * time.Hour))
	if bag.count() != 1 {
		t.Fatal("reminded too soon")
	}
	d.remindAll(time.Now().Add(73 * time.Hour))
	if bag.count() != 2 {
		t.Fatal("not reminded")
	}
	if to, subject, _ := bag.last(); to != "boss@example.org" || !strings.HasPrefix(subject, "Waiting: Leave submitted") {
		t.Fatalf("reminded: %s %s", to, subject)
	}
	d.remindAll(time.Now().Add(200 * time.Hour))
	if bag.count() != 2 {
		t.Fatal("reminded twice")
	}
	if page := s.Get("boss", tableLink("leave")+id); !strings.Contains(page, "Reminded (submitted)") {
		t.Fatalf("no reminder in the history:\n%s", page)
	}
}

func TestStepProblems(t *testing.T) {
	dir := t.TempDir()
	for design, want := range map[string]string{
		"CREATE TABLE a (\n -- step: x -> owner sends -> y\n id TEXT PRIMARY KEY, login TEXT);":                                  "need a status column",
		"CREATE TABLE a (\n -- step: x -> someone sends -> y\n id TEXT PRIMARY KEY, status TEXT);":                               "isn't owner",
		"CREATE TABLE a (\n -- step: x -> owner sends -> y\n id TEXT PRIMARY KEY, status TEXT);":                                 "needs a login column",
		"CREATE TABLE a (\n -- step: x -> role:staff sends -> y\n -- tell: z to role:admin\n id TEXT PRIMARY KEY, status TEXT);": "no step reaches \"z\"",
		"CREATE TABLE a (\n -- tell: z to role:admin\n id TEXT PRIMARY KEY, status TEXT);":                                       "need steps",
		"CREATE TABLE a (\n -- step: x -> role:staff sends -> y\n -- remind: x after soon\n id TEXT PRIMARY KEY, status TEXT);":  "isn't like",
	} {
		path := filepath.Join(dir, "d.sql")
		os.WriteFile(path, []byte(design), 0o600)
		got := strings.Join(DesignProblems(t, path), "; ")
		if !strings.Contains(got, want) {
			t.Errorf("%s\n  got %q, want %q", design, got, want)
		}
	}
}

// Steps travel in the log; two machines that move a row at once agree on
// which counted.
func TestStepsAcrossMachines(t *testing.T) {
	open := func(name string) *data {
		path := filepath.Join(t.TempDir(), "d.db")
		if err := importDesign(path, "testdata/leave.sql", false, io.Discard); err != nil {
			t.Fatal(err)
		}
		d, err := openData(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { d.db.Close() })
		if d.sync, err = openSync(d, name); err != nil {
			t.Fatal(err)
		}
		return d
	}
	a, b := open("a"), open("b")
	tables, _ := a.design()
	leave := *find(tables, "leave")
	id, err := a.saveEntry(leave, "", map[string]any{"login": "anna@example.org", "first_day": "2026-12-02", "last_day": "2026-12-06", "status": "draft"})
	if err != nil {
		t.Fatal(err)
	}
	a.made(leave, id, "anna@example.org")
	if err := a.takeStep(leave, id, "anna@example.org", "submits", "draft", "submitted", ""); err != nil {
		t.Fatal(err)
	}
	meet(t, a, b)
	if got := b.history(leave, id); len(got) != 2 || got[1].What != "Submit: draft → submitted" {
		t.Fatalf("b's history: %+v", got)
	}
	// Approved on one, declined on the other, before they meet.
	if err := a.takeStep(leave, id, "boss@example.org", "approves", "submitted", "approved", ""); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := b.takeStep(leave, id, "kim@example.org", "declines", "submitted", "declined", ""); err != nil {
		t.Fatal(err)
	}
	meet(t, a, b)
	var sa, sb string
	a.db.QueryRow(`SELECT status FROM leave WHERE id = ?`, id).Scan(&sa)
	b.db.QueryRow(`SELECT status FROM leave WHERE id = ?`, id).Scan(&sb)
	if sa != "approved" || sb != "approved" {
		t.Fatalf("a %q, b %q", sa, sb)
	}
	if a.refusedCount() != 1 || b.refusedCount() != 1 {
		t.Fatalf("refused: %d %d", a.refusedCount(), b.refusedCount())
	}
	if ha, hb := a.history(leave, id), b.history(leave, id); len(ha) != 3 || len(hb) != 3 {
		t.Fatalf("histories: %+v / %+v", ha, hb)
	}
}
