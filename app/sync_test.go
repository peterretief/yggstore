package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// machine is a data file with its log, from the example design.
func machine(t *testing.T, name string) *data {
	t.Helper()
	path := filepath.Join(t.TempDir(), "d.db")
	if err := importDesign(path, "testdata/sample.sql", false, io.Discard); err != nil {
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

// meet gives two machines each other's entries.
func meet(t *testing.T, a, b *data) {
	t.Helper()
	ea, err := readEntries(a.db, "")
	if err != nil {
		t.Fatal(err)
	}
	eb, err := readEntries(b.db, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.sync.merge(eb); err != nil {
		t.Fatal(err)
	}
	if _, err := b.sync.merge(ea); err != nil {
		t.Fatal(err)
	}
}

// everything is a data file's tables and refusals, to compare.
func everything(t *testing.T, d *data) string {
	t.Helper()
	tables, err := d.design()
	if err != nil {
		t.Fatal(err)
	}
	tx, _ := d.db.Begin()
	defer tx.Rollback()
	out := map[string]any{}
	for _, tb := range tables {
		rows, err := readRows(tx, tb, "")
		if err != nil {
			t.Fatal(err)
		}
		byKey := map[string]any{}
		for _, r := range rows {
			byKey[text(r[tb.Key])] = r
		}
		out[tb.Name] = byKey
	}
	list, err := d.refusedList()
	if err != nil {
		t.Fatal(err)
	}
	out["refused"] = list
	b, _ := json.Marshal(out)
	return string(b)
}

func same(t *testing.T, a, b *data) {
	t.Helper()
	if x, y := everything(t, a), everything(t, b); x != y {
		t.Fatalf("the machines differ:\n%s\n%s", x, y)
	}
}

func put(t *testing.T, d *data, table, row string, f map[string]any) {
	t.Helper()
	if err := d.sync.write(entryRow, table, row, f); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
}

func value(t *testing.T, d *data, q string, args ...any) (s string) {
	t.Helper()
	if err := d.db.QueryRow(q, args...).Scan(&s); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return s
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func writeFile(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMachinesWorkApartAndMeet(t *testing.T) {
	a, b := machine(t, "desktop"), machine(t, "box")
	put(t, a, "clients", "c1", map[string]any{"name": "Acme"})
	put(t, b, "clients", "c2", map[string]any{"name": "Zed"})
	meet(t, a, b)
	same(t, a, b)
	if value(t, b, `SELECT COUNT(*) FROM clients`) != "2" {
		t.Fatal("rows from both")
	}

	// Different fields of one record: both stay. The same field: the later.
	put(t, a, "clients", "c1", map[string]any{"email": "a@example.org"})
	put(t, b, "clients", "c1", map[string]any{"notes": "from box"})
	put(t, a, "clients", "c2", map[string]any{"name": "Zed early"})
	put(t, b, "clients", "c2", map[string]any{"name": "Zed late"})
	meet(t, a, b)
	same(t, a, b)
	if value(t, a, `SELECT email || '/' || notes FROM clients WHERE id = 'c1'`) != "a@example.org/from box" {
		t.Fatal("an edit was lost")
	}
	if value(t, a, `SELECT name FROM clients WHERE id = 'c2'`) != "Zed late" {
		t.Fatal("the later edit should win")
	}
	// Meeting again changes nothing.
	before := everything(t, a)
	meet(t, a, b)
	if everything(t, a) != before {
		t.Fatal("meeting twice")
	}
	// The tables are what the log says.
	if err := a.sync.rebuild(); err != nil {
		t.Fatal(err)
	}
	if everything(t, a) != before {
		t.Fatal("a replay came out different")
	}
}

func TestRefusedTheSameEverywhere(t *testing.T) {
	a, b := machine(t, "desktop"), machine(t, "box")
	put(t, a, "clients", "c1", map[string]any{"name": "Acme"})
	put(t, a, "projects", "p1", map[string]any{"title": "Site", "client_id": "c1", "status": "open"})
	meet(t, a, b)

	// Both take invoice number 1: the earlier keeps it.
	put(t, a, "invoices", "i1", map[string]any{"number": "1", "project_id": "p1", "amount": "10"})
	put(t, b, "invoices", "i2", map[string]any{"number": "1", "project_id": "p1", "amount": "20"})
	meet(t, a, b)
	same(t, a, b)
	if value(t, a, `SELECT amount FROM invoices`) != "10" {
		t.Fatal("the earlier should keep it")
	}
	list, _ := a.refusedList()
	if len(list) != 1 || list[0].Node != "box" || !strings.Contains(list[0].Reason, "already") {
		t.Fatalf("%+v", list)
	}
	if !strings.Contains(strings.Join(list[0].Fields, ","), "Amount: 20") {
		t.Fatalf("what it tried: %+v", list[0])
	}

	// Dismissing it reaches the other machine.
	if err := a.sync.write(entryDismiss, "", "x", map[string]any{"node": list[0].Node, "seq": list[0].Seq}); err != nil {
		t.Fatal(err)
	}
	meet(t, a, b)
	same(t, a, b)
	if b.refusedCount() != 0 {
		t.Fatal("still listed")
	}

	// A record deleted on one and edited on the other stays deleted, and
	// the edit is listed.
	put(t, a, "clients", "c2", map[string]any{"name": "Zed"})
	meet(t, a, b)
	if err := a.sync.write(entryDelete, "clients", "c2", nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	put(t, b, "clients", "c2", map[string]any{"name": "Zed Ltd"})
	meet(t, a, b)
	same(t, a, b)
	if value(t, b, `SELECT COUNT(*) FROM clients WHERE id = 'c2'`) != "0" {
		t.Fatal("it came back")
	}
	list, _ = b.refusedList()
	if len(list) != 1 || !strings.Contains(list[0].Reason, "deleted") {
		t.Fatalf("%+v", list)
	}

	// A project added on one while its client goes on the other: whichever
	// came first decides, the same on both.
	put(t, a, "clients", "c3", map[string]any{"name": "Yes"})
	meet(t, a, b)
	if err := a.sync.write(entryDelete, "clients", "c3", nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	put(t, b, "projects", "p3", map[string]any{"title": "Late", "client_id": "c3", "status": "open"})
	meet(t, a, b)
	same(t, a, b)
}

func TestTimeNeverGoesBack(t *testing.T) {
	a, b := machine(t, "desktop"), machine(t, "box")
	// A machine with its clock an hour fast.
	future := time.Now().Add(time.Hour).UnixMilli()
	if _, err := b.sync.merge([]entry{{Node: "fast", Seq: 1, T: future, Kind: entryRow, Table: "clients", Row: "c1", Fields: map[string]any{"name": "Fast"}}}); err != nil {
		t.Fatal(err)
	}
	put(t, b, "clients", "c1", map[string]any{"name": "Seen it, and changed it"})
	meet(t, a, b)
	same(t, a, b)
	if value(t, a, `SELECT name FROM clients`) != "Seen it, and changed it" {
		t.Fatal("a save lost to one it had seen")
	}
}

func TestLogTakesInWhatWasThere(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	if err := importDesign(path, "testdata/sample.sql", false, io.Discard); err != nil {
		t.Fatal(err)
	}
	d, _ := openData(path)
	defer d.db.Close()
	d.db.Exec(`INSERT INTO clients (id, name) VALUES ('1', 'Old')`)
	d.db.Exec(`INSERT INTO projects (id, title, client_id, status) VALUES ('2', 'Old job', '1', 'open')`)
	var err error
	if d.sync, err = openSync(d, "desktop"); err != nil {
		t.Fatal(err)
	}
	before := everything(t, d)
	if err := d.sync.rebuild(); err != nil {
		t.Fatal(err)
	}
	if everything(t, d) != before {
		t.Fatalf("%s\n%s", before, everything(t, d))
	}
	if _, err := openSync(d, "desktop"); err != nil {
		t.Fatal(err)
	}
	if n := value(t, d, `SELECT COUNT(*) FROM _sync_entries WHERE kind = 'row'`); n != "2" {
		t.Fatalf("taken in twice: %s", n)
	}
	// A restored copy carries on under the new machine's name.
	l, err := openSync(d, "box")
	if err != nil {
		t.Fatal(err)
	}
	if err := l.write(entryRow, "clients", "c9", map[string]any{"name": "New"}); err != nil {
		t.Fatal(err)
	}
	if value(t, d, `SELECT seq FROM _sync_entries WHERE node = 'box'`) != "1" {
		t.Fatal("the new machine's numbers")
	}
}

// over joins two machines over HTTP, with one key.
func over(t *testing.T, ds ...*data) []string {
	t.Helper()
	key := make([]byte, 32)
	var urls []string
	for _, d := range ds {
		d.keep = &keeper{key: key, seen: map[string]string{}}
		srv := httptest.NewServer(http.HandlerFunc(d.serveSync))
		t.Cleanup(srv.Close)
		urls = append(urls, srv.URL)
	}
	return urls
}

func TestSharingOverTheNetwork(t *testing.T) {
	a, b := machine(t, "desktop"), machine(t, "box")
	urls := over(t, a, b)
	c := http.DefaultClient
	put(t, a, "clients", "c1", map[string]any{"name": "Acme"})
	put(t, b, "clients", "c2", map[string]any{"name": "Zed"})
	if n, err := a.pull(c, urls[1]); err != nil || n == 0 {
		t.Fatalf("%d %v", n, err)
	}
	if n, err := b.pull(c, urls[0]); err != nil || n == 0 {
		t.Fatalf("%d %v", n, err)
	}
	same(t, a, b)
	// Nothing new, nothing done.
	if n, err := a.pull(c, urls[1]); err != nil || n != 0 {
		t.Fatalf("%d %v", n, err)
	}

	// A machine with another key gets nothing, and says why.
	stranger := machine(t, "stranger")
	stranger.keep = &keeper{key: bytes.Repeat([]byte{1}, 32), seen: map[string]string{}}
	if _, err := stranger.pull(c, urls[0]); err == nil {
		t.Fatal("a stranger got in")
	}
	if value(t, stranger, `SELECT COUNT(*) FROM clients`) != "0" {
		t.Fatal("a stranger got rows")
	}
	// An old request recorded and sent again is refused.
	old, _ := json.Marshal(syncAsk{At: time.Now().Add(-time.Hour).UnixMilli(), ID: "x", Have: map[string]int64{}})
	sealed, _ := seal(a.keep.key, "ask", old)
	res, err := http.Post(urls[0], "application/octet-stream", bytes.NewReader(sealed))
	if err != nil || res.StatusCode != http.StatusForbidden {
		t.Fatalf("an old request: %v %v", res, err)
	}
	// An answer can't be passed off as a request.
	if _, err := unseal(a.keep.key, "ask", must2(seal(a.keep.key, "answer", []byte("{}")))); err == nil {
		t.Fatal("an answer opened as a request")
	}
}

func must2(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

func TestAnEmptyMachineCatchesUp(t *testing.T) {
	a := machine(t, "desktop")
	put(t, a, "clients", "c1", map[string]any{"name": "Acme"})
	if err := a.sync.write(entryAccount, "", "a@example.org", map[string]any{"hash": hashPassword("correct horse")}); err != nil {
		t.Fatal(err)
	}
	// A new machine: an empty file, with no design.
	path := filepath.Join(t.TempDir(), "new.db")
	b, err := openData(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.db.Close() })
	if b.sync, err = openSync(b, "box"); err != nil {
		t.Fatal(err)
	}
	urls := over(t, a, b)
	if _, err := b.pull(http.DefaultClient, urls[0]); err != nil {
		t.Fatal(err)
	}
	same(t, a, b)
	if value(t, b, `SELECT name FROM clients`) != "Acme" {
		t.Fatal("no rows")
	}
	// Someone can sign in there, with no session carried over.
	h := (&app{db: nil, data: b, tries: map[string][]time.Time{}})
	var hash string
	if err := h.accountsDB().QueryRow(`SELECT hash FROM ` + h.accounts() + ` WHERE name = 'a@example.org'`).Scan(&hash); err != nil || !matches(hash, "correct horse") {
		t.Fatalf("the account: %v", err)
	}
	// A design change goes round, and the rows stay.
	file := filepath.Join(t.TempDir(), "d.sql")
	writeFile(t, file, strings.Replace(readFile(t, "testdata/sample.sql"), "  email TEXT,", "  email TEXT,\n  phone TEXT,", 1))
	if err := importDesign(a.path, file, false, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := a.sync.noteDesign(); err != nil {
		t.Fatal(err)
	}
	put(t, a, "clients", "c1", map[string]any{"phone": "123"})
	if _, err := b.pull(http.DefaultClient, urls[0]); err != nil {
		t.Fatal(err)
	}
	same(t, a, b)
	if value(t, b, `SELECT phone FROM clients`) != "123" {
		t.Fatal("the new column didn't arrive")
	}
	// Restarting with the same design adds nothing.
	n := value(t, b, `SELECT COUNT(*) FROM _sync_entries WHERE kind = 'design'`)
	if _, err := openSync(b, "box"); err != nil {
		t.Fatal(err)
	}
	if err := b.sync.noteDesign(); err != nil {
		t.Fatal(err)
	}
	if value(t, b, `SELECT COUNT(*) FROM _sync_entries WHERE kind = 'design'`) != n {
		t.Fatal("the rebuilt table reads as a new design")
	}
}

func TestMoreThanOneAnswer(t *testing.T) {
	a := machine(t, "desktop")
	l := a.sync
	got, more, err := l.missing(map[string]int64{})
	if err != nil || more {
		t.Fatalf("%d %v %v", len(got), more, err)
	}
	var fake []entry
	for i := int64(1); i <= syncMaxSend+10; i++ {
		fake = append(fake, entry{Node: "z", Seq: i, T: i, Kind: entryRow, Table: "gone", Row: "r"})
	}
	if _, err := l.keepEntries(fake); err != nil {
		t.Fatal(err)
	}
	got, more, err = l.missing(map[string]int64{})
	if err != nil || !more || len(got) != syncMaxSend {
		t.Fatalf("%d %v %v", len(got), more, err)
	}
}

func TestNumberedDesignCantBeLogged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	d, _ := openData(path)
	defer d.db.Close()
	d.db.Exec(`CREATE TABLE a (id INTEGER PRIMARY KEY, x TEXT)`)
	if _, err := openSync(d, "desktop"); err == nil || !strings.Contains(err.Error(), "numbered") {
		t.Fatalf("%v", err)
	}
}

func TestImportKeepsTheLog(t *testing.T) {
	d := machine(t, "desktop")
	put(t, d, "clients", "c1", map[string]any{"name": "Acme", "email": "a@example.org"})
	path := value(t, d, `SELECT file FROM pragma_database_list WHERE name = 'main'`)
	file := filepath.Join(t.TempDir(), "d.sql")
	writeFile(t, file, strings.Replace(readFile(t, "testdata/sample.sql"), "  email TEXT,", "  email TEXT,\n  phone TEXT,", 1))
	if err := importDesign(path, file, false, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := d.sync.rebuild(); err != nil {
		t.Fatal(err)
	}
	if value(t, d, `SELECT email FROM clients`) != "a@example.org" {
		t.Fatal("a row was lost")
	}
	put(t, d, "clients", "c1", map[string]any{"phone": "123"})
	if err := d.sync.rebuild(); err != nil {
		t.Fatal(err)
	}
	if value(t, d, `SELECT phone FROM clients`) != "123" {
		t.Fatal("the new column")
	}
}

func TestRefusedPage(t *testing.T) {
	a, b := machine(t, "desktop"), machine(t, "box")
	put(t, a, "clients", "c1", map[string]any{"name": "Acme"})
	put(t, a, "projects", "p1", map[string]any{"title": "Site", "client_id": "c1", "status": "open"})
	meet(t, a, b)
	put(t, a, "invoices", "i1", map[string]any{"number": "1", "project_id": "p1", "amount": "10"})
	put(t, b, "invoices", "i2", map[string]any{"number": "1", "project_id": "p1", "amount": "20"})
	meet(t, a, b)
	mux := http.NewServeMux()
	admin := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			h(w, r.WithContext(context.WithValue(r.Context(), viewerKey{}, person{Name: "a@example.org", Admin: true})))
		}
	}
	a.routes(mux, admin, admin)
	do := func(method, path string, f url.Values) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(f.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	if s := do("GET", dataBase, nil).Body.String(); !strings.Contains(s, "1 to look at") {
		t.Fatal("the diagram doesn't say")
	}
	s := do("GET", dataBase+"_refused", nil).Body.String()
	if !strings.Contains(s, "Amount: 20") || !strings.Contains(s, "Project: Site") || !strings.Contains(s, "invoices/new?") {
		t.Fatalf("%s", s)
	}
	list, _ := a.refusedList()
	if w := do("POST", dataBase+"_refused", url.Values{"node": {list[0].Node}, "seq": {list[0].Seq}}); w.Code != http.StatusSeeOther {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if !strings.Contains(do("GET", dataBase+"_refused", nil).Body.String(), "Nothing to look at") {
		t.Fatal("still listed")
	}
	// A form's save only sends what changed.
	if w := do("POST", dataBase+"clients/c1", url.Values{"name": {"Acme"}, "notes": {"hi"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if s := value(t, a, `SELECT fields FROM _sync_entries WHERE tbl = 'clients' ORDER BY t DESC LIMIT 1`); s != `{"notes":"hi"}` {
		t.Fatalf("%s", s)
	}
}
