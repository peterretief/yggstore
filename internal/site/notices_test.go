package site

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNotices(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "index.html")
	page := "<html><body>\n<h1>Harbour Kitchen</h1>\n<p>Fish.</p>\n</body></html>"
	os.WriteFile(home, []byte(page), 0o644)
	read := func() string { b, _ := os.ReadFile(home); return string(b) }

	if err := AddNotice(dir, "", "", time.Now()); err == nil {
		t.Fatal("an empty notice was taken")
	}
	day := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	if err := AddNotice(dir, "Open on Sunday", "From 10 until 4.\nBook ahead.", day); err != nil {
		t.Fatal(err)
	}
	if err := AddNotice(dir, "Kingklip <today>", "", day.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	list, err := Notices(dir)
	if err != nil || len(list) != 2 || list[0].Title != "Kingklip <today>" {
		t.Fatalf("newest first: %+v %v", list, err)
	}
	got := read()
	h1 := strings.Index(got, "</h1>")
	first, second := strings.Index(got, "Kingklip &lt;today&gt;"), strings.Index(got, "Open on Sunday")
	if h1 < 0 || first < h1 || second < first || !strings.Contains(got, "From 10 until 4.<br>") || !strings.Contains(got, "31 October 2026") {
		t.Fatalf("home page:\n%s", got)
	}
	if strings.Count(got, noticesStart) != 1 {
		t.Fatal("the section twice")
	}

	// The owner replaces the page: the notices go back on it.
	os.WriteFile(home, []byte("<html><body><h1>New look</h1></body></html>"), 0o644)
	if err := KeepNotices(dir); err != nil || !strings.Contains(read(), "Open on Sunday") {
		t.Fatalf("not kept: %v\n%s", err, read())
	}
	again := read()
	if KeepNotices(dir); read() != again {
		t.Fatal("rewritten with nothing changed")
	}

	// Taking them all off takes the section away.
	for _, n := range list {
		if err := RemoveNotice(dir, n.ID); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Contains(read(), noticesStart) {
		t.Fatalf("section left:\n%s", read())
	}
}
