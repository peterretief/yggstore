package site

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

const testSchema = `
CREATE TABLE courses (id TEXT PRIMARY KEY, places INTEGER NOT NULL);
INSERT INTO courses VALUES ('intro', 2);
CREATE TABLE members (email TEXT PRIMARY KEY, name TEXT NOT NULL, joined INTEGER);
CREATE TABLE bookings (id TEXT PRIMARY KEY, course TEXT NOT NULL REFERENCES courses, email TEXT NOT NULL);
CREATE TRIGGER full BEFORE INSERT ON bookings
WHEN (SELECT count(*) FROM bookings WHERE course = NEW.course) >= (SELECT places FROM courses WHERE id = NEW.course)
BEGIN SELECT RAISE(ABORT, 'the course is full'); END;
`

const testEntries = `
-- kind: join
INSERT INTO members (email, name, joined) VALUES (lower(:email), :name, :entry_time);

-- kind: book
-- One place, if there is one left.
INSERT INTO bookings (id, course, email) VALUES (:entry_id, :course, lower(:email));

-- kind: rename
UPDATE members SET name = :name WHERE email = lower(:email);
`

func testSQL(t *testing.T) siteSQL {
	t.Helper()
	s, err := parseSiteSQL(testSchema, testEntries)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func dump(t *testing.T, d *siteData) string {
	t.Helper()
	var b strings.Builder
	for _, q := range []string{`SELECT email, name FROM members ORDER BY email`, `SELECT id, email FROM bookings ORDER BY id`,
		`SELECT id, status FROM _entries ORDER BY t, node, seq`} {
		r, err := d.query(q, nil, 1000)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintln(&b, r.Rows)
	}
	return b.String()
}

func TestSiteSQL(t *testing.T) {
	s := testSQL(t)
	if len(s.kinds["join"]) != 1 || len(s.kinds["book"]) != 1 || !strings.HasPrefix(s.kinds["book"][0], "INSERT INTO bookings") {
		t.Fatalf("kinds: %q", s.kinds)
	}
	if _, err := parseSiteSQL(testSchema, "INSERT INTO x VALUES (1);"); err == nil {
		t.Fatal("entries.sql without kinds was taken")
	}
	if _, err := parseSiteSQL(testSchema, "-- kind: Bad Kind\nSELECT 1;"); err == nil {
		t.Fatal("a bad kind was taken")
	}
}

// A web node's entries, replayed: rules turn entries down, the same way
// everywhere.
func TestSiteDataRules(t *testing.T) {
	d, err := openSiteData(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()
	s := testSQL(t)
	now := time.UnixMilli(1_800_000_000_000)
	rec := func(kind string, f map[string]any) Entry {
		e, err := d.record("web-a", kind, f, now)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	anna := rec("join", map[string]any{"email": "Anna@Example.org", "name": "Anna"})
	again := rec("join", map[string]any{"email": "anna@example.org", "name": "Anna again"})
	b1 := rec("book", map[string]any{"course": "intro", "email": "anna@example.org"})
	b2 := rec("book", map[string]any{"course": "intro", "email": "ben@example.org"})
	b3 := rec("book", map[string]any{"course": "intro", "email": "cy@example.org"})
	nope := rec("book", map[string]any{"course": "nope", "email": "dee@example.org"})
	if err := d.replay(s); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{anna.ID: EntryApplied, again.ID: EntryRefused, b1.ID: EntryApplied, b2.ID: EntryApplied, b3.ID: EntryRefused, nope.ID: EntryRefused} {
		if got, why := d.status(id); got != want {
			t.Errorf("%s: %s %q, want %s", id, got, why, want)
		}
	}
	if _, why := d.status(b3.ID); !strings.Contains(why, "the course is full") {
		t.Errorf("why the third booking was turned down: %q", why)
	}
	if b1.Time <= anna.Time || b2.Seq != b1.Seq+1 {
		t.Fatalf("entries out of order: %+v %+v %+v", anna, b1, b2)
	}

	// Queries can't write.
	if _, err := d.query(`DELETE FROM bookings`, nil, 10); err == nil {
		t.Fatal("a query wrote")
	}
	if r, err := d.query(`SELECT count(*) FROM bookings WHERE course = ?`, []any{"intro"}, 10); err != nil || r.Rows[0][0] != int64(2) {
		t.Fatalf("count: %v %v", r, err)
	}

	// New SQL in a new version: built again with it.
	s2, _ := parseSiteSQL(strings.Replace(testSchema, "('intro', 2)", "('intro', 3)", 1), testEntries)
	if err := d.replay(s2); err != nil {
		t.Fatal(err)
	}
	if got, _ := d.status(b3.ID); got != EntryApplied {
		t.Fatalf("with three places the third booking is %s", got)
	}
}

// Two web nodes take entries at the same time and get each other's later,
// in a different order: replay gives both the same data, the same
// bookings winning.
func TestSiteDataMerge(t *testing.T) {
	s := testSQL(t)
	open := func() *siteData {
		d, err := openSiteData(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(d.close)
		return d
	}
	a, b := open(), open()
	t0 := time.UnixMilli(1_800_000_000_000)
	ea1, _ := a.record("web-a", "join", map[string]any{"email": "anna@example.org", "name": "Anna"}, t0)
	ea2, _ := a.record("web-a", "book", map[string]any{"course": "intro", "email": "anna@example.org"}, t0.Add(30*time.Millisecond))
	// b was cut off: it took two bookings for the same course meanwhile,
	// the first a little before a's.
	eb1, _ := b.record("web-b", "book", map[string]any{"course": "intro", "email": "ben@example.org"}, t0.Add(20*time.Millisecond))
	eb2, _ := b.record("web-b", "book", map[string]any{"course": "intro", "email": "cy@example.org"}, t0.Add(40*time.Millisecond))
	a.replay(s)
	b.replay(s)
	if got, _ := a.status(ea2.ID); got != EntryApplied {
		t.Fatalf("before meeting, a's booking is %s", got)
	}

	// They meet: each gets the other's entries.
	if fresh, err := a.add([]Entry{eb2, eb1}, t0.Add(time.Second)); err != nil || len(fresh) != 2 {
		t.Fatalf("a took %v %v", fresh, err)
	}
	if fresh, _ := a.add([]Entry{eb1}, t0.Add(time.Second)); len(fresh) != 0 {
		t.Fatal("an entry was taken twice")
	}
	b.add([]Entry{ea1, ea2}, t0.Add(time.Second))
	if err := a.replay(s); err != nil {
		t.Fatal(err)
	}
	if err := b.replay(s); err != nil {
		t.Fatal(err)
	}
	da, db := dump(t, a), dump(t, b)
	if da != db {
		t.Fatalf("the copies differ:\n%s\n%s", da, db)
	}
	// In time order: ben (20 ms), anna (30 ms), cy (40 ms): cy is too late.
	for id, want := range map[string]string{eb1.ID: EntryApplied, ea2.ID: EntryApplied, eb2.ID: EntryRefused} {
		if got, _ := a.status(id); got != want {
			t.Errorf("%s: %s, want %s", id, got, want)
		}
	}

	// A later entry from one comes after everything the other has, even if
	// its clock is behind.
	late, _ := b.record("web-b", "rename", map[string]any{"email": "anna@example.org", "name": "Anna K"}, t0)
	if late.Time <= ea2.Time {
		t.Fatalf("an entry from a slow clock went back in time: %d", late.Time)
	}

	// Catching up: what a got after a time, oldest first.
	got, last, err := a.since(0, 10)
	if err != nil || len(got) != 4 || last == 0 {
		t.Fatalf("since: %d entries, %d, %v", len(got), last, err)
	}
	if more, _, _ := a.since(last, 10); len(more) != 0 {
		t.Fatalf("since the last: %v", more)
	}
	if !reflect.DeepEqual(got[0].Fields, ea1.Fields) {
		t.Fatalf("fields came back as %v", got[0].Fields)
	}
}
