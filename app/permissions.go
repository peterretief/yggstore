package app

import (
	"fmt"
	"regexp"
	"strings"
)

// Who may do what with a table (docs/app-patterns.md in yggstore). An admin
// may do everything. Anyone else may do only what the table's rules allow,
// and a table without rules is for admins only, so a mistake hides too much
// rather than too little.
//
// The rules are comments inside the table in design.sql (SQLite keeps those
// with the table, and drops comments before it):
//
//	CREATE TABLE projects (
//	  -- allow: staff read, staff write
//	  -- allow: client read own
//	  id TEXT PRIMARY KEY,
//	  budget REAL,    -- hide from: client
//	  notes LONGTEXT  -- label: Notes for us; help: Anything we should know
//	  made_by TEXT    -- fill: who made it
//	);
//
// "fill: who made it" puts the email of whoever makes a row in that column,
// whatever is sent, and keeps it; "staff read own by made_by" is then what
// they made.
//
// Drawn in drawDB, the same go in its comment boxes: a table's rules in the
// table's comment, which drawDB writes above the table and the import moves
// inside it (commentsInside), and a column's notes in the field's comment,
// which it writes on the line above the column.
//
// A rule can be limited: "own" is a person's own rows, through a login
// column (scope), "own by COLUMN" through another column naming them, such
// as a course's teacher in course_teachers ("staff write own by teacher"),
// and "if COLUMN" (reads only) the rows where that column is set, such as
// "client read if published". Rules add up. Clients only read, for now.

const roleStaff = "staff"

type rule struct {
	Role, Action string // staff or client; read, write or delete
	Own          bool   // only their own rows
	By           string // the column that names them: login, unless "own by ..."
	If           string // only rows where this column is set
}

var ruleWord = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// access is what one person may do with one table. Which rows they may read
// is rows' to say.
type access struct {
	Read, Write, Delete bool
}

var (
	allowLine  = regexp.MustCompile(`(?i)--\s*allow:\s*(.+)$`)
	columnLine = regexp.MustCompile(`^\s*("[^"]+"|\[[^\]]+\]|` + "`[^`]+`" + `|[A-Za-z_][A-Za-z0-9_]*)\s.*?--\s*(.+)$`)
	// columnStart is a line that may define a column, with no comment of
	// its own: its name, then its type.
	columnStart = regexp.MustCompile(`^\s*("[^"]+"|\[[^\]]+\]|` + "`[^`]+`" + `|[A-Za-z_][A-Za-z0-9_]*)\s+\S`)
	directive   = regexp.MustCompile(`(?i)^\s*(hide from|label|help|fill)\s*:\s*(.+?)\s*$`)
)

// readRule reads one rule, such as "client read if published".
func readRule(t *table, clause string) (rule, error) {
	words := strings.Fields(strings.ToLower(clause))
	bad := fmt.Errorf("%s: %q isn't a rule like \"staff write\", \"client read own\" or \"client read if published\"", t.Name, strings.TrimSpace(clause))
	if len(words) < 2 || (words[0] != roleStaff && words[0] != roleClient) {
		return rule{}, bad
	}
	r := rule{Role: words[0], Action: words[1]}
	switch r.Action {
	case "read":
	case "write", "delete":
		if r.Role == roleClient {
			return rule{}, fmt.Errorf("%s: clients can only read for now (%q)", t.Name, strings.TrimSpace(clause))
		}
	default:
		return rule{}, bad
	}
	rest := words[2:]
	if len(rest) > 0 && rest[0] == "own" {
		r.Own, r.By, rest = true, loginColumn, rest[1:]
		if len(rest) >= 2 && rest[0] == "by" {
			if !ruleWord.MatchString(rest[1]) {
				return rule{}, bad
			}
			r.By, rest = rest[1], rest[2:]
		}
	}
	if len(rest) == 2 && rest[0] == "if" {
		c := t.col(rest[1])
		if c == nil {
			return rule{}, fmt.Errorf("%s: %q: there's no column %s", t.Name, strings.TrimSpace(clause), rest[1])
		}
		r.If, rest = c.Name, nil
	}
	if len(rest) > 0 || (r.Action != "read" && r.If != "") {
		return rule{}, bad
	}
	return r, nil
}

// readRules takes a table's rules, and its columns' hiding, labels and help,
// from its statement. What it can't read is kept in t.Problems.
//
// A column's notes go at the end of its line, or on comment lines just above
// it, which is where drawDB puts a field's comment when it exports SQL.
func readRules(t *table, stmt string) {
	var above []string // comment lines waiting for the column below them
	for _, line := range strings.Split(stmt, "\n") {
		if m := allowLine.FindStringSubmatch(line); m != nil {
			for _, clause := range strings.Split(m[1], ",") {
				if r, err := readRule(t, clause); err != nil {
					t.Problems = append(t.Problems, err.Error())
				} else {
					t.Rules = append(t.Rules, r)
				}
			}
			continue
		}
		if c, ok := strings.CutPrefix(strings.TrimSpace(line), "--"); ok {
			above = append(above, c)
			continue
		}
		notes := above
		above = nil
		var c *column
		if m := columnLine.FindStringSubmatch(line); m != nil {
			c = t.col(strings.Trim(m[1], "\"[]`"))
			notes = append(notes, m[2])
		} else if m := columnStart.FindStringSubmatch(line); m != nil {
			c = t.col(strings.Trim(m[1], "\"[]`"))
		}
		if c != nil {
			for _, n := range notes {
				if err := applyNotes(c, n); err != nil {
					t.Problems = append(t.Problems, t.Name+"."+c.Name+": "+err.Error())
				}
			}
		}
	}
}

// applyNotes takes a column's hiding, label, help and filling from a
// comment, such as "hide from: client; label: Notes for us".
func applyNotes(c *column, comment string) error {
	for _, part := range strings.Split(comment, ";") {
		d := directive.FindStringSubmatch(part)
		if d == nil {
			continue
		}
		switch strings.ToLower(d[1]) {
		case "hide from":
			for _, role := range strings.Split(strings.ToLower(d[2]), ",") {
				if c.Hide == nil {
					c.Hide = map[string]bool{}
				}
				c.Hide[strings.TrimSpace(role)] = true
			}
		case "label":
			c.Label = d[2]
		case "help":
			c.Help = d[2]
		case "fill":
			if !strings.EqualFold(strings.Join(strings.Fields(d[2]), " "), "who made it") {
				return fmt.Errorf("%q: the only filling is \"fill: who made it\"", strings.TrimSpace(part))
			}
			// The signed-in person's email, when the row is made (see
			// data.save); never asked for, never changed.
			c.Maker, c.Auto, c.Required = true, true, false
		}
	}
	return nil
}

// access is the one check: what a person may do with this table.
func (t table) access(p person) access {
	if p.Admin {
		return access{Read: true, Write: true, Delete: true}
	}
	var a access
	if p.Name == "" {
		return a
	}
	for _, r := range t.Rules {
		if r.Role != p.Role {
			continue
		}
		switch r.Action {
		case "read":
			a.Read = true
		case "write":
			a.Read, a.Write = true, true
		case "delete":
			a.Read, a.Delete = true, true
		}
	}
	return a
}

// forViewer is the table as one person may see it: without the columns
// hidden from their role. Everything shown, searched, sorted, totalled,
// downloaded or saved goes through it.
func (t table) forViewer(p person) table {
	if p.Admin {
		return t
	}
	v := t
	v.Cols = nil
	for _, c := range t.Cols {
		if !c.Hide[p.Role] {
			v.Cols = append(v.Cols, c)
		}
	}
	return v
}

// title is how a column is named to people.
func (c column) title() string {
	if c.Label != "" {
		return c.Label
	}
	return human(c.Name)
}

// rows limits a query to what a person may see of a table: "" for all of
// it, or a condition; ok is false if they may see none of it. Each rule
// gives some rows, and they add up: what someone may change, they may see.
func (t table) rows(tables []table, p person) (cond string, args []any, ok bool) {
	return t.rowsFor(tables, p, "read")
}

// rowsFor is rows, for one action: the rows a person may read, write or
// delete.
func (t table) rowsFor(tables []table, p person, action string) (cond string, args []any, ok bool) {
	if p.Admin {
		return "", nil, true
	}
	var any []string
	for _, r := range t.Rules {
		if r.Role != p.Role || (action != "read" && r.Action != action) {
			continue
		}
		var parts []string
		if r.Own {
			c, a, ok := scopeBy(tables, t, p, r.By)
			if !ok {
				continue // no way back to a login: none of it is theirs
			}
			parts = append(parts, c)
			args = append(args, a...)
		}
		if r.If != "" {
			parts = append(parts, "COALESCE("+quote(r.If)+", 0) NOT IN (0, '0', '', 'false')")
		}
		if parts == nil {
			return "", nil, true // all of it
		}
		any = append(any, "("+strings.Join(parts, " AND ")+")")
	}
	if any == nil {
		return "", nil, false
	}
	return "(" + strings.Join(any, " OR ") + ")", args, true
}

// allRows says whether a person may see every row of a table.
func (t table) allRows(tables []table, p person) bool {
	cond, _, ok := t.rows(tables, p)
	return ok && cond == ""
}
