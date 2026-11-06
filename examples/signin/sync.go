package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// With -machine, a data file keeps a log (docs/local-apps.md): every save is
// an entry, kept for good, and the tables are what replaying the entries in
// one order (time, machine, number) gives. So two machines that worked apart
// can merge their logs and come out the same: the later edit of a field wins,
// and an entry that breaks a rule of the design is refused on both, and
// listed for a person to put right.
//
// A machine's time never goes back past what it has seen, so a save always
// comes after everything it could have known about.

const syncTables = `
CREATE TABLE IF NOT EXISTS _sync_entries (node TEXT NOT NULL, seq INTEGER NOT NULL, t INTEGER NOT NULL,
  kind TEXT NOT NULL, tbl TEXT NOT NULL, row TEXT NOT NULL, fields TEXT NOT NULL, PRIMARY KEY (node, seq));
CREATE INDEX IF NOT EXISTS _sync_order ON _sync_entries (t, node, seq);
CREATE INDEX IF NOT EXISTS _sync_rows ON _sync_entries (tbl, row);
CREATE TABLE IF NOT EXISTS _sync_refused (node TEXT NOT NULL, seq INTEGER NOT NULL, reason TEXT NOT NULL, PRIMARY KEY (node, seq));
CREATE TABLE IF NOT EXISTS _sync_deleted (tbl TEXT NOT NULL, row TEXT NOT NULL, PRIMARY KEY (tbl, row));
CREATE TABLE IF NOT EXISTS _sync_accounts (name TEXT PRIMARY KEY, hash TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS _sync_state (key TEXT PRIMARY KEY, value TEXT NOT NULL);`

// Kinds of entry.
const (
	entryRow     = "row"     // set these fields of this row, making it if need be
	entryDelete  = "delete"  // remove this row for good
	entryDismiss = "dismiss" // a person has dealt with a refused entry
	entryAccount = "account" // who can sign in, and their password's hash
	entryDesign  = "design"  // the tables' statements: see noteDesign
)

type entry struct {
	Node   string         `json:"node"`
	Seq    int64          `json:"seq"`
	T      int64          `json:"t"` // unix ms
	Kind   string         `json:"kind"`
	Table  string         `json:"table"`
	Row    string         `json:"row"`
	Fields map[string]any `json:"fields,omitempty"`
}

var errDeleted = errors.New("this record was deleted")

type syncLog struct {
	d    *data
	node string
	mu   sync.Mutex
}

// syncMachine is the name a data file's log goes by: "" if it has no log.
func syncMachine(db *sql.DB) string {
	var name string
	db.QueryRow(`SELECT value FROM _sync_state WHERE key = 'machine'`).Scan(&name)
	return name
}

// openSync turns the log on, taking in any rows that aren't in it yet. A copy
// restored on another machine goes on under that machine's name.
func openSync(d *data, node string) (*syncLog, error) {
	tables, err := d.design()
	if err != nil {
		return nil, err
	}
	if notes := designNotes(tables); len(notes) > 0 {
		return nil, errors.New("this design can't be shared yet: " + strings.Join(notes, "; "))
	}
	if _, err := d.db.Exec(syncTables); err != nil {
		return nil, err
	}
	if _, err := d.db.Exec(`INSERT INTO _sync_state VALUES ('machine', ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, node); err != nil {
		return nil, err
	}
	l := &syncLog{d: d, node: node}
	if err := l.adopt(tables); err != nil {
		return nil, err
	}
	// A design already in the log is only replaced by an import.
	if latest, _, err := l.latestDesign(); err != nil {
		return nil, err
	} else if latest == "" {
		if err := l.noteDesign(); err != nil {
			return nil, err
		}
	}
	return l, nil
}

// parentsFirst orders tables so a row's parents are in the log before it.
func parentsFirst(tables []table) []table {
	level := levels(tables)
	out := slices.Clone(tables)
	sort.SliceStable(out, func(i, j int) bool { return level[out[i].Name] < level[out[j].Name] })
	return out
}

// next is the time and number of this machine's next entry.
func (l *syncLog) next(tx *sql.Tx) (t, seq int64, err error) {
	var last, mine sql.NullInt64
	if err = tx.QueryRow(`SELECT MAX(t) FROM _sync_entries`).Scan(&last); err != nil {
		return
	}
	if err = tx.QueryRow(`SELECT MAX(seq) FROM _sync_entries WHERE node = ?`, l.node).Scan(&mine); err != nil {
		return
	}
	return max(time.Now().UnixMilli(), last.Int64+1), mine.Int64 + 1, nil
}

func addEntry(tx *sql.Tx, e entry) error {
	fields, err := json.Marshal(e.Fields)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT OR IGNORE INTO _sync_entries VALUES (?, ?, ?, ?, ?, ?, ?)`, e.Node, e.Seq, e.T, e.Kind, e.Table, e.Row, string(fields))
	return err
}

// adopt puts rows that were made without the log into it.
func (l *syncLog) adopt(tables []table) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	tx, err := l.d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, t := range parentsFirst(tables) {
		rows, err := readRows(tx, t, `WHERE CAST(`+quote(t.Key)+` AS TEXT) NOT IN (SELECT row FROM _sync_entries WHERE tbl = ?)`, t.Name)
		if err != nil {
			return err
		}
		for _, fields := range rows {
			when, seq, err := l.next(tx)
			if err != nil {
				return err
			}
			row := text(fields[t.Key])
			delete(fields, t.Key)
			if err := addEntry(tx, entry{Node: l.node, Seq: seq, T: when, Kind: entryRow, Table: t.Name, Row: row, Fields: fields}); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// readRows reads rows as fields, in the form an entry keeps them.
func readRows(tx *sql.Tx, t table, where string, args ...any) ([]map[string]any, error) {
	var names []string
	for _, c := range t.Cols {
		names = append(names, quote(c.Name))
	}
	rows, err := tx.Query("SELECT "+strings.Join(names, ", ")+" FROM "+quote(t.Name)+" "+where+" ORDER BY rowid", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		vals := make([]any, len(t.Cols))
		ptrs := make([]any, len(vals))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		fields := map[string]any{}
		for i, c := range t.Cols {
			if vals[i] != nil {
				fields[c.Name] = text(vals[i])
			} else {
				fields[c.Name] = nil
			}
		}
		out = append(out, fields)
	}
	return out, rows.Err()
}

// apply does what an entry says to the tables. An error is why it's refused.
func apply(tx *sql.Tx, tables []table, e entry) error {
	if e.Kind == entryDismiss {
		_, err := tx.Exec(`DELETE FROM _sync_refused WHERE node = ? AND seq = ?`, e.Fields["node"], e.Fields["seq"])
		return err
	}
	if e.Kind == entryAccount {
		_, err := tx.Exec(`INSERT INTO _sync_accounts VALUES (?, ?) ON CONFLICT (name) DO UPDATE SET hash = excluded.hash`, e.Row, e.Fields["hash"])
		return err
	}
	t := find(tables, e.Table)
	if t == nil {
		return nil // a table the design no longer has
	}
	var gone int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM _sync_deleted WHERE tbl = ? AND row = ?`, e.Table, e.Row).Scan(&gone); err != nil {
		return err
	}
	if gone > 0 {
		return errDeleted
	}
	if e.Kind == entryDelete {
		if _, err := tx.Exec("DELETE FROM "+quote(t.Name)+" WHERE "+quote(t.Key)+" = ?", e.Row); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO _sync_deleted VALUES (?, ?)`, e.Table, e.Row)
		return err
	}
	var names, marks, sets []string
	var args []any
	for _, c := range t.Cols { // in the design's order; a field it dropped is passed over
		if v, ok := e.Fields[c.Name]; ok && c.Name != t.Key {
			names = append(names, quote(c.Name))
			marks = append(marks, "?")
			sets = append(sets, quote(c.Name)+" = ?")
			args = append(args, v)
		}
	}
	var there int
	if err := tx.QueryRow("SELECT COUNT(*) FROM "+quote(t.Name)+" WHERE "+quote(t.Key)+" = ?", e.Row).Scan(&there); err != nil {
		return err
	}
	if there > 0 {
		if len(sets) == 0 {
			return nil
		}
		_, err := tx.Exec("UPDATE "+quote(t.Name)+" SET "+strings.Join(sets, ", ")+" WHERE "+quote(t.Key)+" = ?", append(args, e.Row)...)
		return err
	}
	names = append(names, quote(t.Key))
	marks = append(marks, "?")
	_, err := tx.Exec("INSERT INTO "+quote(t.Name)+" ("+strings.Join(names, ", ")+") VALUES ("+strings.Join(marks, ", ")+")", append(args, e.Row)...)
	return err
}

// write saves one entry from this machine. If the design refuses it, it's
// said at once and isn't kept.
func (l *syncLog) write(kind, table, row string, fields map[string]any) error {
	tables, err := l.d.design()
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	tx, err := l.d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	when, seq, err := l.next(tx)
	if err != nil {
		return err
	}
	e := entry{Node: l.node, Seq: seq, T: when, Kind: kind, Table: table, Row: row, Fields: fields}
	if err := apply(tx, tables, e); err != nil {
		return err
	}
	if err := addEntry(tx, e); err != nil {
		return err
	}
	return tx.Commit()
}

func readEntries(q interface {
	Query(string, ...any) (*sql.Rows, error)
}, where string, args ...any) ([]entry, error) {
	rows, err := q.Query(`SELECT node, seq, t, kind, tbl, row, fields FROM _sync_entries `+where+` ORDER BY t, node, seq`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []entry
	for rows.Next() {
		var e entry
		var fields string
		if err := rows.Scan(&e.Node, &e.Seq, &e.T, &e.Kind, &e.Table, &e.Row, &fields); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(fields), &e.Fields); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// merge takes entries from another machine, and replays if any were new.
func (l *syncLog) merge(from []entry) (added int, err error) {
	if added, err = l.keepEntries(from); err != nil || added == 0 {
		return added, err
	}
	if err := l.catchDesign(); err != nil {
		return added, err
	}
	return added, l.rebuild()
}

func (l *syncLog) keepEntries(from []entry) (added int, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	tx, err := l.d.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for _, e := range from {
		fields, err := json.Marshal(e.Fields)
		if err != nil {
			return 0, err
		}
		res, err := tx.Exec(`INSERT OR IGNORE INTO _sync_entries VALUES (?, ?, ?, ?, ?, ?, ?)`, e.Node, e.Seq, e.T, e.Kind, e.Table, e.Row, string(fields))
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		added += int(n)
	}
	return added, tx.Commit()
}

// replay builds the tables again from the log.
func replay(tx *sql.Tx, tables []table) error {
	// Emptied in any order, with foreign keys back on before the entries.
	if _, err := tx.Exec(`PRAGMA defer_foreign_keys = ON`); err != nil {
		return err
	}
	for _, t := range tables {
		if _, err := tx.Exec("DELETE FROM " + quote(t.Name)); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`PRAGMA defer_foreign_keys = OFF; DELETE FROM _sync_refused; DELETE FROM _sync_deleted; DELETE FROM _sync_accounts`); err != nil {
		return err
	}
	entries, err := readEntries(tx, "")
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := apply(tx, tables, e); err != nil {
			if _, err := tx.Exec(`INSERT INTO _sync_refused VALUES (?, ?, ?)`, e.Node, e.Seq, refusal(err)); err != nil {
				return err
			}
		}
	}
	return nil
}

func refusal(err error) string {
	if errors.Is(err, errDeleted) {
		return "This record was deleted on another machine."
	}
	return plainError(err)
}

// rebuild replays the log: after a design change, or to check it.
func (l *syncLog) rebuild() error {
	tables, err := l.d.design()
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	tx, err := l.d.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := replay(tx, tables); err != nil {
		return err
	}
	return tx.Commit()
}

// changed is what a form changes in a row, so edits to different fields on
// different machines both stay.
func (d *data) changed(t table, id string, fields map[string]any) (map[string]any, error) {
	tx, err := d.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := readRows(tx, t, "WHERE "+quote(t.Key)+" = ?", id)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	out := map[string]any{}
	for k, v := range fields {
		if k != t.Key && text(rows[0][k]) != text(v) {
			out[k] = v
		}
	}
	return out, nil
}

// saveEntry is the form's save, with the log on.
func (d *data) saveEntry(t table, id string, fields map[string]any) error {
	if id != "" {
		change, err := d.changed(t, id, fields)
		if err != nil || len(change) == 0 {
			return err
		}
		return d.sync.write(entryRow, t.Name, id, change)
	}
	row := newID()
	if c := t.col(t.Key); !c.Random {
		row = text(fields[t.Key])
		delete(fields, t.Key)
	}
	return d.sync.write(entryRow, t.Name, row, fields)
}

// The refused list.

type refused struct {
	Node, Seq, When, Table, Reason, Fix string
	Fields                              []string
}

func (d *data) refusedCount() int {
	if d.sync == nil {
		return 0
	}
	var n int
	d.db.QueryRow(`SELECT COUNT(*) FROM _sync_refused`).Scan(&n)
	return n
}

func (d *data) refusedList() ([]refused, error) {
	tables, err := d.design()
	if err != nil {
		return nil, err
	}
	rows, err := d.db.Query(`SELECT e.node, e.seq, e.t, e.kind, e.tbl, e.row, e.fields, r.reason
		FROM _sync_refused r JOIN _sync_entries e ON e.node = r.node AND e.seq = r.seq ORDER BY e.t DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []refused
	for rows.Next() {
		var x refused
		var seq, when int64
		var kind, row, stored string
		if err := rows.Scan(&x.Node, &seq, &when, &kind, &x.Table, &row, &stored, &x.Reason); err != nil {
			return nil, err
		}
		x.Seq = strconv.FormatInt(seq, 10)
		x.When = time.UnixMilli(when).Format("2006-01-02 15:04")
		var fields map[string]any
		json.Unmarshal([]byte(stored), &fields)
		q := url.Values{}
		for k, v := range fields {
			shown := text(v)
			if t := find(tables, x.Table); t != nil {
				if c := t.col(k); c != nil && c.Ref != nil {
					if _, labels := d.picks(tables, *c.Ref); labels[shown] != "" {
						shown = labels[shown]
					}
				}
			}
			x.Fields = append(x.Fields, human(k)+": "+shown)
			q.Set(k, text(v))
		}
		sort.Strings(x.Fields)
		if kind == entryDelete {
			x.Fields = []string{"Delete a record"}
			x.Fix = tableLink(x.Table) + url.PathEscape(row)
		} else {
			x.Fix = tableLink(x.Table) + "new?" + q.Encode()
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (d *data) refusedPage(w http.ResponseWriter, r *http.Request) {
	list, err := d.refusedList()
	if err != nil {
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}
	page(w, http.StatusOK, "refused", list)
}

func (d *data) dismiss(w http.ResponseWriter, r *http.Request) {
	seq, err := strconv.ParseInt(r.PostFormValue("seq"), 10, 64)
	if err != nil {
		http.Error(w, "no such entry", http.StatusBadRequest)
		return
	}
	fields := map[string]any{"node": r.PostFormValue("node"), "seq": seq}
	if err := d.sync.write(entryDismiss, "", fmt.Sprint(fields["node"], "/", seq), fields); err != nil {
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, dataBase+"_refused", http.StatusSeeOther)
}
