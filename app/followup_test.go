package app

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// A sign owner's people all see the owner's tickets ("own through owner")
// and write on them; follow-ups are kept in the history and emailed to the
// owner's people and staff, not the writer, and the customers' side isn't
// told which of the staff wrote.
func TestFollowUps(t *testing.T) {
	if p := DesignProblems(t, "testdata/signs.sql"); len(p) > 0 {
		t.Fatalf("problems: %v", p)
	}
	s := NewTestSite(t, "testdata/signs.sql", map[string][2]string{
		"sam": {"sam@example.org", roleStaff}, "tim": {"tim@saldana.example", roleClient},
		"tia": {"tia@saldana.example", roleClient}, "mo": {"mo@metro.example", roleClient}})
	bag := &mailbag{}
	d := s.App.data
	d.notify.send = bag.sendFrom
	d.notify.people = func(role string) []string {
		if role == roleStaff {
			return []string{"sam@example.org"}
		}
		return nil
	}
	for _, q := range []string{
		`INSERT INTO sign_owners VALUES ('saldana', 'Saldana'), ('metro', 'Metro')`,
		`INSERT INTO sign_owner_people VALUES ('p1', 'saldana', 'tim@saldana.example'), ('p2', 'saldana', 'tia@saldana.example'), ('p3', 'metro', 'mo@metro.example')`,
	} {
		if _, err := s.DB().Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	// The signs page opens the list with the sign filled in for Add.
	list := s.Get("tim", tableLink("signs_support")+"?fill.sign=s1&fill.owner=saldana")
	if !strings.Contains(list, `new?owner=saldana&amp;sign=s1"`) {
		t.Fatalf("Add doesn't carry the sign:\n%s", list)
	}
	if w := s.Post("tim", tableLink("signs_support")+"new", url.Values{"owner": {"saldana"}, "sign": {"s1"}, "subject": {"Half dark"}}); w.Code != 303 {
		t.Fatalf("tim's ticket: %d %s", w.Code, w.Body)
	}
	if w := s.Post("mo", tableLink("signs_support")+"new", url.Values{"owner": {"saldana"}, "subject": {"Not mine"}}); w.Code != 403 {
		t.Fatalf("mo made one for saldana: %d", w.Code)
	}
	var id, login string
	s.DB().QueryRow(`SELECT id, login FROM signs_support`).Scan(&id, &login)
	if login != "tim@saldana.example" {
		t.Fatalf("login %q", login)
	}
	// Tia, Tim's colleague, sees it; Mo doesn't.
	if page := s.Get("tia", tableLink("signs_support")); !strings.Contains(page, "Half dark") {
		t.Fatal("tia doesn't see her owner's ticket")
	}
	if page := s.Get("mo", tableLink("signs_support")); strings.Contains(page, "Half dark") {
		t.Fatal("mo sees another owner's ticket")
	}
	if w := s.Post("mo", tableLink("signs_support")+id+"/follow-up", url.Values{"text": {"x"}}); w.Code != 404 {
		t.Fatalf("mo wrote on it: %d", w.Code)
	}

	wait := func(n int) {
		t.Helper()
		for end := time.Now().Add(3 * time.Second); bag.count() < n && time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		}
	}
	before := bag.count()
	if w := s.Post("tia", tableLink("signs_support")+id+"/follow-up", url.Values{"text": {"Still dark this morning."}}); w.Code != 303 {
		t.Fatalf("tia's follow-up: %d", w.Code)
	}
	wait(before + 2)
	to := map[string]string{}
	bag.mu.Lock()
	for _, m := range bag.sent[before:] {
		to[m.To] = m.Body
	}
	bag.mu.Unlock()
	if len(to) != 2 || !strings.Contains(to["tim@saldana.example"], "Still dark") || !strings.Contains(to["sam@example.org"], "from tia@saldana.example") {
		t.Fatalf("told %v", to)
	}
	if _, ok := to["tia@saldana.example"]; ok {
		t.Fatal("the writer was told")
	}

	before = bag.count()
	s.Post("sam", tableLink("signs_support")+id+"/follow-up", url.Values{"text": {"We come on Tuesday."}})
	wait(before + 2)
	bag.mu.Lock()
	for _, m := range bag.sent[before:] {
		if strings.Contains(m.Body, "sam@") {
			t.Errorf("the customer's side heard which of the staff wrote: %s", m.Body)
		}
	}
	bag.mu.Unlock()
	page := s.Get("tim", tableLink("signs_support")+id)
	for _, want := range []string{"Follow-up", "Still dark this morning.", "We come on Tuesday.", "Add a follow-up"} {
		if !strings.Contains(page, want) {
			t.Fatalf("tim's page lacks %q:\n%s", want, page)
		}
	}
	if strings.Contains(page, "sam@example.org") {
		t.Fatal("tim's page names the staff")
	}
}

// Signed out, a link to a page goes to sign in, and the sign-in goes back
// to it; never to somewhere else.
func TestBackAfterSignIn(t *testing.T) {
	s := NewTestSite(t, "testdata/signs.sql", nil)
	w := s.Do("", "GET", tableLink("signs_support")+"?fill.sign=p2", nil, "")
	var back *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == backCookie {
			back = c
		}
	}
	if w.Code != 303 || back == nil {
		t.Fatalf("%d, cookie %v", w.Code, back)
	}
	to := func(c *http.Cookie) string {
		r := httptest.NewRequest("POST", base+"login", nil)
		if c != nil {
			r.AddCookie(c)
		}
		return signedInTo(httptest.NewRecorder(), r)
	}
	if got := to(back); got != tableLink("signs_support")+"?fill.sign=p2" {
		t.Fatalf("back to %q", got)
	}
	for _, bad := range []string{"https://evil.example/", "//evil.example/app/", "/elsewhere"} {
		if got := to(&http.Cookie{Name: backCookie, Value: url.QueryEscape(bad)}); got != base {
			t.Errorf("%s: to %q", bad, got)
		}
	}
	if to(nil) != base {
		t.Fatal("no cookie")
	}
}
