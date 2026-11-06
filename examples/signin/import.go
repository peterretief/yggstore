package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Importing makes a data file match a .sql file you edit: the file is tried
// on its own first, and if it's good, new tables and columns are added and
// changed tables rebuilt with their rows, all or nothing. Nothing is
// removed without -drop, and a copy of the data file is kept first.
//
// Rows in the file are added too, unless they clash with one already there.

var createTable = regexp.MustCompile("(?is)^\\s*CREATE\\s+TABLE\\s+(\"[^\"]+\"|\\[[^\\]]+\\]|`[^`]+`|[^\\s(]+)\\s*\\(")

// tableBody is a table's definition without its name.
func tableBody(stmt string) string {
	return strings.Join(strings.Fields(createTable.ReplaceAllString(stmt, "(")), " ")
}

type object struct{ kind, name, stmt string }

// objects lists what a schema holds ("main", or "design" when attached).
func objects(ctx context.Context, c *sql.Conn, schema string) ([]object, error) {
	rows, err := c.QueryContext(ctx, `SELECT type, name, sql FROM `+schema+`.sqlite_master
		WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite%' AND name NOT LIKE '\_sync%' ESCAPE '\' ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []object
	for rows.Next() {
		var o object
		if err := rows.Scan(&o.kind, &o.name, &o.stmt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func columns(ctx context.Context, c *sql.Conn, schema, table string) ([]string, error) {
	rows, err := c.QueryContext(ctx, `SELECT name FROM pragma_table_info(?, ?)`, table, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func quoteAll(names []string) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = quote(n)
	}
	return strings.Join(out, ", ")
}

func importDesign(path, file string, drop bool, out io.Writer) error {
	ctx := context.Background()
	stmts, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	// Tried on its own first: a mistake in the file changes nothing.
	scratch := path + ".import-tmp"
	os.Remove(scratch)
	defer os.Remove(scratch)
	sdb, err := sql.Open("sqlite3", "file:"+scratch)
	if err != nil {
		return err
	}
	_, err = sdb.Exec(string(stmts))
	sdb.Close()
	if err != nil {
		return fmt.Errorf("%s: %w (nothing was changed)", file, err)
	}

	db, err := sql.Open("sqlite3", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(wal)")
	if err != nil {
		return err
	}
	defer db.Close()
	c, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := c.ExecContext(ctx, `ATTACH DATABASE ? AS design`, scratch); err != nil {
		return err
	}
	have, err := objects(ctx, c, "main")
	if err != nil {
		return err
	}
	want, err := objects(ctx, c, "design")
	if err != nil {
		return err
	}
	byName := func(list []object, o object) *object {
		for i := range list {
			if list[i].kind == o.kind && list[i].name == o.name {
				return &list[i]
			}
		}
		return nil
	}

	// What would be lost is said first, and needs -drop.
	var lost []string
	for _, o := range have {
		if o.kind == "table" && byName(want, o) == nil {
			lost = append(lost, "table "+o.name)
		}
	}
	type change struct {
		o               object
		keep, add, gone []string
	}
	var changes []change
	var added []object
	for _, o := range want {
		if o.kind != "table" {
			continue
		}
		h := byName(have, o)
		if h == nil {
			added = append(added, o)
			continue
		}
		if tableBody(h.stmt) == tableBody(o.stmt) {
			continue
		}
		oldCols, err := columns(ctx, c, "main", o.name)
		if err != nil {
			return err
		}
		newCols, err := columns(ctx, c, "design", o.name)
		if err != nil {
			return err
		}
		ch := change{o: o}
		for _, n := range newCols {
			if slices.Contains(oldCols, n) {
				ch.keep = append(ch.keep, n)
			} else {
				ch.add = append(ch.add, n)
			}
		}
		for _, n := range oldCols {
			if !slices.Contains(newCols, n) {
				ch.gone = append(ch.gone, n)
				lost = append(lost, "column "+o.name+"."+n)
			}
		}
		changes = append(changes, ch)
	}
	if len(lost) > 0 && !drop {
		return fmt.Errorf("this would remove %s and what's in it. Nothing was changed. "+
			"If that's what you want, import again with -drop. If you renamed something, it counts as removed and added", strings.Join(lost, ", "))
	}

	if len(have) > 0 && (len(added)+len(changes)+len(lost) > 0) {
		keep := path + ".before-import-" + time.Now().Format("20060102-150405.000")
		if _, err := c.ExecContext(ctx, `VACUUM main INTO ?`, keep); err != nil {
			return err
		}
		fmt.Fprintln(out, "Kept a copy of the data as", keep)
	}

	// Foreign keys wait while tables are swapped, and are checked at the end.
	if _, err := c.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return err
	}
	if _, err := c.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	done := false
	defer func() {
		if !done {
			c.ExecContext(ctx, `ROLLBACK`)
		}
	}()
	exec := func(q string) error {
		_, err := c.ExecContext(ctx, q)
		return err
	}
	// Indexes, views and triggers are made again from the file.
	for _, o := range have {
		if o.kind == "table" {
			continue
		}
		if w := byName(want, o); w == nil || w.stmt != o.stmt {
			if err := exec("DROP " + strings.ToUpper(o.kind) + " " + quote(o.name)); err != nil {
				return err
			}
		}
	}
	for _, o := range added {
		if err := exec(o.stmt); err != nil {
			return err
		}
		fmt.Fprintln(out, "Added table", o.name)
	}
	for _, ch := range changes {
		tmp := ch.o.name + "__import"
		if err := exec(createTable.ReplaceAllString(ch.o.stmt, "CREATE TABLE "+strings.ReplaceAll(quote(tmp), "$", "$$")+" (")); err != nil {
			return err
		}
		if len(ch.keep) > 0 {
			names := quoteAll(ch.keep)
			if err := exec("INSERT INTO " + quote(tmp) + " (" + names + ") SELECT " + names + " FROM main." + quote(ch.o.name)); err != nil {
				return fmt.Errorf("%s: the rows there don't fit the new design: %w (nothing was changed)", ch.o.name, err)
			}
		}
		if err := exec("DROP TABLE main." + quote(ch.o.name)); err != nil {
			return err
		}
		if err := exec("ALTER TABLE " + quote(tmp) + " RENAME TO " + quote(ch.o.name)); err != nil {
			return err
		}
		what := "its rules"
		if len(ch.add)+len(ch.gone) > 0 {
			var parts []string
			if ch.add != nil {
				parts = append(parts, "added "+strings.Join(ch.add, ", "))
			}
			if ch.gone != nil {
				parts = append(parts, "removed "+strings.Join(ch.gone, ", "))
			}
			what = strings.Join(parts, "; ")
		}
		fmt.Fprintf(out, "Changed table %s: %s\n", ch.o.name, what)
	}
	for _, o := range have {
		if o.kind == "table" && byName(want, o) == nil {
			if err := exec("DROP TABLE main." + quote(o.name)); err != nil {
				return err
			}
			fmt.Fprintln(out, "Removed table", o.name)
		}
	}
	now, err := objects(ctx, c, "main")
	if err != nil {
		return err
	}
	for _, o := range want {
		if o.kind != "table" && byName(now, o) == nil {
			if err := exec(o.stmt); err != nil {
				return err
			}
		}
	}
	for _, o := range want {
		if o.kind != "table" {
			continue
		}
		names, err := columns(ctx, c, "design", o.name)
		if err != nil {
			return err
		}
		list := quoteAll(names)
		res, err := c.ExecContext(ctx, "INSERT OR IGNORE INTO main."+quote(o.name)+" ("+list+") SELECT "+list+" FROM design."+quote(o.name))
		if err != nil {
			return fmt.Errorf("%s: its rows: %w (nothing was changed)", o.name, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			fmt.Fprintf(out, "Added %d rows to %s\n", n, o.name)
		}
	}
	var table string
	var rowid sql.NullInt64
	var parent string
	var fk int
	err = c.QueryRowContext(ctx, `PRAGMA main.foreign_key_check`).Scan(&table, &rowid, &parent, &fk)
	if err == nil {
		return fmt.Errorf("a row in %s refers to one in %s that isn't there (nothing was changed)", table, parent)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err := exec(`COMMIT`); err != nil {
		return err
	}
	done = true
	if len(added)+len(changes)+len(lost) == 0 {
		fmt.Fprintln(out, "The design already matches.")
	}
	tables, err := (&data{db: db}).design()
	if err != nil {
		return err
	}
	for _, s := range designNotes(tables) {
		fmt.Fprintln(out, "Note:", s)
	}
	return nil
}

// designNotes says what in a design won't work with data on more than one
// machine (docs/local-apps.md), which needs random ids, not numbers.
func designNotes(tables []table) []string {
	var out, numbered []string
	for _, t := range tables {
		if c := t.col(t.Key); c != nil && c.Auto && !c.Random {
			numbered = append(numbered, t.Name)
		} else if c == nil {
			out = append(out, t.Name+" needs a primary key of one column")
		}
		for _, c := range t.Cols {
			if c.Ref == nil {
				continue
			}
			if p := find(tables, c.Ref.Table); p != nil {
				if pc := p.col(c.Ref.Column); pc != nil && !strings.EqualFold(pc.Type, c.Type) {
					out = append(out, fmt.Sprintf("%s.%s is %s but %s.%s is %s: make them the same", t.Name, c.Name, c.Type, p.Name, pc.Name, pc.Type))
				}
			}
		}
	}
	if numbered != nil {
		out = append(out, "numbered rows in "+strings.Join(numbered, ", ")+" can't be shared between machines: "+
			"use id TEXT PRIMARY KEY, and TEXT for what refers to them, then import. Rows and links are kept")
	}
	return out
}
