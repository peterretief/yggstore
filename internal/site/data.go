package site

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ncruces/go-sqlite3/driver"
	_ "github.com/ncruces/go-sqlite3/embed"
)

// A site's data on a web node (see docs/site-code.md, "Data that lasts"):
// every write is an entry in log.db, kept for good and shared with the
// other web nodes; read.db is built from them by replaying every entry in
// one order (time, web node, number) through the site's own SQL: its
// api/schema.sql makes the tables, and api/entries.sql says, for each kind
// of entry, what it does to them. A statement that breaks one of the
// tables' rules (UNIQUE, CHECK, a trigger's RAISE) turns its entry down,
// the same way on every web node, since they all replay the same entries
// in the same order. An entry that arrives out of order, or new SQL in a
// new version of the site, means read.db is built again from the start.
// So that a replay comes out the same everywhere and every time, the SQL's
// time now and random() come from the entry (see datafuncs.go).

// Entry is one write to a site's data.
type Entry struct {
	ID     string         `json:"id"`   // NODE/SEQ
	Time   int64          `json:"t"`    // unix ms, when it was taken
	Node   string         `json:"node"` // the web node that took it
	Seq    int64          `json:"seq"`  // its number there
	Kind   string         `json:"kind"`
	Fields map[string]any `json:"fields,omitempty"`
}

// Entry statuses in read.db's _entries table.
const (
	EntryApplied = "applied"
	EntryRefused = "refused"
)

const (
	maxEntryBytes = 64 << 10
	maxFields     = 64
)

var (
	entryKind  = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	kindHeader = regexp.MustCompile(`(?m)^--\s*kind:\s*(\S+)\s*$`)
	paramName  = regexp.MustCompile(`:([A-Za-z][A-Za-z0-9_]*)`)
	fieldName  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,62}$`)
)

// siteSQL is a site's api/schema.sql and api/entries.sql.
type siteSQL struct {
	schema string
	kinds  map[string][]string // kind → its statements
	hash   string
}

// readSiteSQL reads a version's api/schema.sql and api/entries.sql.
func readSiteSQL(root string) (siteSQL, error) {
	schema, err := os.ReadFile(filepath.Join(root, "api", "schema.sql"))
	if err != nil {
		return siteSQL{}, errors.New("the site has no api/schema.sql")
	}
	entries, err := os.ReadFile(filepath.Join(root, "api", "entries.sql"))
	if err != nil {
		return siteSQL{}, errors.New("the site has no api/entries.sql")
	}
	return parseSiteSQL(string(schema), string(entries))
}

// parseSiteSQL splits entries.sql into its kinds:
//
//	-- kind: register
//	INSERT INTO registrations (id, name, at) VALUES (:entry_id, :name, :entry_time);
func parseSiteSQL(schema, entries string) (siteSQL, error) {
	h := sha256.Sum256([]byte(schema + "\x00" + entries))
	s := siteSQL{schema: schema, kinds: map[string][]string{}, hash: hex.EncodeToString(h[:])}
	marks := kindHeader.FindAllStringSubmatchIndex(entries, -1)
	for i, m := range marks {
		kind := entries[m[2]:m[3]]
		if !entryKind.MatchString(kind) {
			return s, fmt.Errorf("api/entries.sql: %q can't be a kind (a-z, 0-9, - and _)", kind)
		}
		end := len(entries)
		if i+1 < len(marks) {
			end = marks[i+1][0]
		}
		for _, st := range splitStatements(entries[m[1]:end]) {
			s.kinds[kind] = append(s.kinds[kind], st)
		}
	}
	if len(s.kinds) == 0 {
		return s, errors.New("api/entries.sql has no kinds (a line \"-- kind: NAME\" before each kind's statements)")
	}
	return s, nil
}

// splitStatements splits SQL at semicolons that end a line.
func splitStatements(sqlText string) []string {
	var out []string
	for _, part := range regexp.MustCompile(`;[ \t]*(\r?\n|$)`).Split(sqlText, -1) {
		lines := []string{}
		for _, l := range strings.Split(part, "\n") {
			if t := strings.TrimSpace(l); t != "" && !strings.HasPrefix(t, "--") {
				lines = append(lines, l)
			}
		}
		if len(lines) > 0 {
			out = append(out, strings.TrimSpace(strings.Join(lines, "\n")))
		}
	}
	return out
}

// siteData is one site's data on this web node.
type siteData struct {
	dir string
	env *replayEnv // read.db's writing connections' time and chance

	mu    sync.Mutex // writes, replays and rebuilds
	log   *sql.DB
	read  *sql.DB // read.db, to write to
	ro    *sql.DB // read.db, for queries: read-only
	hash  string  // the site SQL read.db was built with
	dirty bool    // the log has entries read.db may not have
}

func openSiteData(dir string) (*siteData, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	log, err := sql.Open("sqlite3", "file:"+filepath.Join(dir, "log.db")+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(wal)")
	if err != nil {
		return nil, err
	}
	if _, err := log.Exec(`CREATE TABLE IF NOT EXISTS entries (
		id TEXT PRIMARY KEY, t INTEGER NOT NULL, node TEXT NOT NULL, seq INTEGER NOT NULL,
		kind TEXT NOT NULL, fields TEXT NOT NULL DEFAULT '{}', got INTEGER NOT NULL,
		UNIQUE (node, seq))`); err != nil {
		log.Close()
		return nil, err
	}
	log.Exec(`CREATE INDEX IF NOT EXISTS entries_order ON entries (t, node, seq)`)
	log.Exec(`CREATE INDEX IF NOT EXISTS entries_got ON entries (got)`)
	return &siteData{dir: dir, env: &replayEnv{}, log: log, dirty: true}, nil
}

func (d *siteData) close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, db := range []*sql.DB{d.ro, d.read, d.log} {
		if db != nil {
			db.Close()
		}
	}
	d.ro, d.read = nil, nil
	d.env.close()
}

// record takes a new entry at this web node.
func (d *siteData) record(node, kind string, fields map[string]any, now time.Time) (Entry, error) {
	if !entryKind.MatchString(kind) {
		return Entry{}, fmt.Errorf("%q can't be a kind", kind)
	}
	if len(fields) > maxFields {
		return Entry{}, fmt.Errorf("an entry holds at most %d fields", maxFields)
	}
	for k := range fields {
		if !fieldName.MatchString(k) || strings.HasPrefix(k, "entry_") {
			return Entry{}, fmt.Errorf("%q can't be a field's name (letters, digits and _, not starting entry_)", k)
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var seq, last sql.NullInt64
	d.log.QueryRow(`SELECT max(seq) FROM entries WHERE node = ?`, node).Scan(&seq)
	d.log.QueryRow(`SELECT max(t) FROM entries`).Scan(&last)
	// After every entry this node has, so its own entries don't land
	// before what it already replayed, whatever the clocks say.
	t := max(now.UnixMilli(), last.Int64+1)
	e := Entry{Node: node, Seq: seq.Int64 + 1, Time: t, Kind: kind, Fields: fields}
	e.ID = fmt.Sprintf("%s/%d", node, e.Seq)
	if _, err := d.addLocked([]Entry{e}, now); err != nil {
		return Entry{}, err
	}
	return e, nil
}

// add keeps entries from another web node, and gives those new here.
func (d *siteData) add(entries []Entry, now time.Time) ([]Entry, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.addLocked(entries, now)
}

func (d *siteData) addLocked(entries []Entry, now time.Time) ([]Entry, error) {
	tx, err := d.log.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var fresh []Entry
	for _, e := range entries {
		if e.ID != fmt.Sprintf("%s/%d", e.Node, e.Seq) || !entryKind.MatchString(e.Kind) || e.Node == "" {
			continue
		}
		f, _ := json.Marshal(e.Fields)
		if len(f) > maxEntryBytes {
			continue
		}
		r, err := tx.Exec(`INSERT OR IGNORE INTO entries (id, t, node, seq, kind, fields, got) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			e.ID, e.Time, e.Node, e.Seq, e.Kind, string(f), now.UnixMilli())
		if err != nil {
			return nil, err
		}
		if n, _ := r.RowsAffected(); n > 0 {
			fresh = append(fresh, e)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if len(fresh) > 0 {
		d.dirty = true
	}
	return fresh, nil
}

// since gives the entries this web node got after got (unix ms), oldest
// first, at most limit, and the time it got the last one: what another
// web node asks for to catch up.
func (d *siteData) since(got int64, limit int) ([]Entry, int64, error) {
	rows, err := d.log.Query(`SELECT id, t, node, seq, kind, fields, got FROM entries WHERE got > ? ORDER BY got, id LIMIT ?`, got, limit)
	if err != nil {
		return nil, got, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		var f string
		if err := rows.Scan(&e.ID, &e.Time, &e.Node, &e.Seq, &e.Kind, &f, &got); err != nil {
			return nil, got, err
		}
		json.Unmarshal([]byte(f), &e.Fields)
		out = append(out, e)
	}
	return out, got, rows.Err()
}

// replay brings read.db up to every entry in the log, with the site's SQL:
// new entries in order, or the whole log again when one came out of order
// or the SQL changed.
func (d *siteData) replay(s siteSQL) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.read == nil || d.hash != s.hash {
		return d.rebuildLocked(s)
	}
	if !d.dirty {
		return nil
	}
	todo, early, err := d.unappliedLocked(d.read)
	if err != nil {
		return err
	}
	if early {
		return d.rebuildLocked(s)
	}
	if err := applyEntries(d.read, d.env, s, todo); err != nil {
		return err
	}
	d.dirty = false
	return nil
}

// unappliedLocked is the log's entries read.db hasn't had, in order, and
// whether any of them comes before one it has.
func (d *siteData) unappliedLocked(read *sql.DB) ([]Entry, bool, error) {
	var lt, lseq sql.NullInt64
	var lnode sql.NullString
	read.QueryRow(`SELECT t, node, seq FROM _entries ORDER BY t DESC, node DESC, seq DESC LIMIT 1`).Scan(&lt, &lnode, &lseq)
	have := map[string]bool{}
	rows, err := read.Query(`SELECT id FROM _entries`)
	if err != nil {
		return nil, false, err
	}
	for rows.Next() {
		var id string
		rows.Scan(&id)
		have[id] = true
	}
	rows.Close()
	all, err := d.log.Query(`SELECT id, t, node, seq, kind, fields FROM entries ORDER BY t, node, seq`)
	if err != nil {
		return nil, false, err
	}
	defer all.Close()
	var todo []Entry
	early := false
	for all.Next() {
		var e Entry
		var f string
		if err := all.Scan(&e.ID, &e.Time, &e.Node, &e.Seq, &e.Kind, &f); err != nil {
			return nil, false, err
		}
		if have[e.ID] {
			continue
		}
		json.Unmarshal([]byte(f), &e.Fields)
		if lt.Valid && before(e, lt.Int64, lnode.String, lseq.Int64) {
			early = true
		}
		todo = append(todo, e)
	}
	return todo, early, all.Err()
}

func before(e Entry, t int64, node string, seq int64) bool {
	if e.Time != t {
		return e.Time < t
	}
	if e.Node != node {
		return e.Node < node
	}
	return e.Seq < seq
}

// rebuildLocked builds read.db again from the whole log, in a new file that
// then takes the old one's place, so queries never see half of it.
func (d *siteData) rebuildLocked(s siteSQL) error {
	path := filepath.Join(d.dir, "read.db")
	tmp := path + ".new"
	for _, p := range []string{tmp, tmp + "-wal", tmp + "-shm"} {
		os.Remove(p)
	}
	// Foreign keys on, whatever SQLite's default: REFERENCES is a rule too.
	db, err := driver.Open("file:"+tmp+"?_pragma=journal_mode(delete)&_pragma=foreign_keys(1)", d.env.register)
	if err != nil {
		return err
	}
	d.env.enterSchema()
	_, err = db.Exec(s.schema)
	d.env.leave()
	if err != nil {
		db.Close()
		return fmt.Errorf("api/schema.sql: %w", err)
	}
	if _, err := db.Exec(`CREATE TABLE _entries (id TEXT PRIMARY KEY, t INTEGER, node TEXT, seq INTEGER, kind TEXT, status TEXT, error TEXT)`); err != nil {
		db.Close()
		return fmt.Errorf("api/schema.sql can't have a table called _entries: %w", err)
	}
	todo, _, err := d.unappliedLocked(db)
	if err == nil {
		err = applyEntries(db, d.env, s, todo)
	}
	db.Close()
	if err != nil {
		os.Remove(tmp)
		return err
	}
	for _, c := range []*sql.DB{d.ro, d.read} {
		if c != nil {
			c.Close()
		}
	}
	d.ro, d.read = nil, nil
	for _, p := range []string{path + "-wal", path + "-shm"} {
		os.Remove(p)
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if d.read, err = driver.Open("file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(wal)&_pragma=foreign_keys(1)", d.env.register); err != nil {
		return err
	}
	if d.ro, err = sql.Open("sqlite3", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)&_pragma=query_only(1)"); err != nil {
		return err
	}
	d.hash, d.dirty = s.hash, false
	return nil
}

// applyEntries applies entries in order, each on its own: one whose
// statements fail is turned down and changes nothing.
func applyEntries(db *sql.DB, env *replayEnv, s siteSQL, entries []Entry) error {
	if len(entries) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, e := range entries {
		status, why := EntryApplied, ""
		stmts, ok := s.kinds[e.Kind]
		if !ok {
			status, why = EntryRefused, "the site has no kind "+e.Kind
		} else {
			if _, err := tx.Exec(`SAVEPOINT entry`); err != nil {
				return err
			}
			env.enter(e)
			for _, st := range stmts {
				if _, err := tx.Exec(st, params(st, e)...); err != nil {
					status, why = EntryRefused, err.Error()
					break
				}
			}
			env.leave()
			if status == EntryRefused {
				tx.Exec(`ROLLBACK TO entry`)
			}
			tx.Exec(`RELEASE entry`)
		}
		if _, err := tx.Exec(`INSERT INTO _entries (id, t, node, seq, kind, status, error) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			e.ID, e.Time, e.Node, e.Seq, e.Kind, status, why); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// params are the named parameters a statement uses: the entry's fields,
// and :entry_id, :entry_time, :entry_node, :entry_kind. A field the entry
// lacks is NULL.
func params(stmt string, e Entry) []any {
	seen := map[string]bool{}
	var out []any
	for _, m := range paramName.FindAllStringSubmatch(stmt, -1) {
		name := m[1]
		if seen[name] {
			continue
		}
		seen[name] = true
		var v any
		switch name {
		case "entry_id":
			v = e.ID
		case "entry_time":
			v = e.Time
		case "entry_node":
			v = e.Node
		case "entry_kind":
			v = e.Kind
		default:
			v = sqlValue(e.Fields[name])
		}
		out = append(out, sql.Named(name, v))
	}
	return out
}

// sqlValue is a JSON value as SQLite takes it: a whole number as an
// integer, a list or object as its JSON text.
func sqlValue(v any) any {
	switch x := v.(type) {
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1<<53 {
			return int64(x)
		}
		return x
	case bool:
		if x {
			return int64(1)
		}
		return int64(0)
	case nil, string, int64:
		return x
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

// Rows are a query's answer.
type Rows struct {
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
}

// query runs a read-only query on read.db.
func (d *siteData) query(q string, args []any, limit int) (Rows, error) {
	d.mu.Lock()
	ro := d.ro
	d.mu.Unlock()
	if ro == nil {
		return Rows{}, errors.New("the site's data isn't ready yet")
	}
	for i, a := range args {
		args[i] = sqlValue(a)
	}
	rows, err := ro.Query(q, args...)
	if err != nil {
		return Rows{}, err
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	out := Rows{Columns: cols, Rows: [][]any{}}
	for rows.Next() {
		if len(out.Rows) == limit {
			return out, fmt.Errorf("more than %d rows: add a LIMIT", limit)
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return out, err
		}
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				vals[i] = string(b)
			}
		}
		out.Rows = append(out.Rows, vals)
	}
	return out, rows.Err()
}

// status is how an entry fared in read.db: applied, refused (and why), or
// "" when it hasn't been replayed yet.
func (d *siteData) status(id string) (string, string) {
	d.mu.Lock()
	read := d.read
	d.mu.Unlock()
	if read == nil {
		return "", ""
	}
	var status, why sql.NullString
	read.QueryRow(`SELECT status, error FROM _entries WHERE id = ?`, id).Scan(&status, &why)
	return status.String, why.String
}
