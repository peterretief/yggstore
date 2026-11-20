package app

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Steps (docs/app-workflow.md in yggstore): a table's rows move from status
// to status, and the design says who may move them, who is told, and when
// someone is reminded. Like the rules, they are comments inside the table:
//
//	CREATE TABLE leave (
//	  -- step: draft -> owner submits -> submitted
//	  -- step: submitted -> role:admin approves -> approved
//	  -- tell: submitted to role:admin
//	  -- tell: approved to owner
//	  -- remind: submitted after 3 days
//	  login TEXT NOT NULL,
//	  status TEXT NOT NULL DEFAULT 'draft'
//	);
//
// The status column is never typed in: only a step changes it, and only the
// steps listed can happen. Who acts is "owner" (the row's login), a role
// ("role:staff", "role:admin"), or a column holding an email ("column:
// manager"); an admin may take any step. Taking a step needs only that the
// person may read the row: so a client can submit their own request, though
// clients don't write. Every step is kept, with who took it and why, as the
// row's history.
//
//	  -- follow-ups: owner, through:owner, role:staff
//
// lets anyone who may read a row write on it, a thread kept in its history
// (a customer's "it's still dark", staff's "we're coming Tuesday"), and
// emails the people named, but not the writer. "through:COLUMN" is the
// people of the record COLUMN points to: a sign owner's people.

const statusColumn = "status"

type step struct {
	From, Who, Action, To string
}

type tell struct {
	At  []string // statuses
	Who []string
}

type remind struct {
	At    []string
	After time.Duration
}

var (
	stepLine   = regexp.MustCompile(`(?i)--\s*(step|tell|remind|follow-ups):\s*(.+)$`)
	afterWords = regexp.MustCompile(`(?i)^(.+?)\s+after\s+(\d+)\s*(day|days|hour|hours)$`)
)

// label is a step's button: "approves" is Approve.
func (s step) label() string {
	a := s.Action
	switch {
	case strings.HasSuffix(a, "shes"), strings.HasSuffix(a, "ches"), strings.HasSuffix(a, "xes"), strings.HasSuffix(a, "sses"), strings.HasSuffix(a, "zes"):
		a = strings.TrimSuffix(a, "es")
	case strings.HasSuffix(a, "s") && !strings.HasSuffix(a, "ss"):
		a = strings.TrimSuffix(a, "s")
	}
	return strings.ToUpper(a[:1]) + a[1:]
}

func validWho(t *table, who string) error {
	kind, name, _ := strings.Cut(who, ":")
	switch {
	case who == "mail": // a member's mail that names the step (mailin.go)
	case who == "owner":
		if t.col(loginColumn) == nil {
			return fmt.Errorf("%q needs a %s column, saying whose each row is", who, loginColumn)
		}
	case kind == "role" && slices.Contains([]string{roleAdmin, roleStaff, roleClient}, name):
	case kind == "group" && ruleWord.MatchString(name):
	case kind == "column" && name != "":
		if t.col(name) == nil {
			return fmt.Errorf("%q: there's no column %s", who, name)
		}
	case kind == "through" && name != "":
		if c := t.col(name); c == nil || c.Ref == nil {
			return fmt.Errorf("%q: %s isn't a column pointing to another table", who, name)
		}
	default:
		return fmt.Errorf("%q isn't owner, role:staff, role:admin, role:client, group:NAME, column:NAME, through:COLUMN or mail", who)
	}
	return nil
}

// readStep takes one of a table's step, tell or remind lines.
func readStep(t *table, kind, text string) error {
	text = strings.TrimSpace(text)
	list := func(s string) []string {
		var out []string
		for _, x := range strings.Split(s, ",") {
			if x = strings.TrimSpace(x); x != "" {
				out = append(out, x)
			}
		}
		return out
	}
	switch strings.ToLower(kind) {
	case "step":
		parts := strings.Split(text, "->")
		if len(parts) != 3 {
			return fmt.Errorf("%q isn't a step like \"draft -> owner submits -> submitted\"", text)
		}
		middle := strings.Fields(parts[1])
		s := step{From: strings.TrimSpace(parts[0]), To: strings.TrimSpace(parts[2])}
		if len(middle) != 2 || s.From == "" || s.To == "" {
			return fmt.Errorf("%q isn't a step like \"draft -> owner submits -> submitted\"", text)
		}
		s.Who, s.Action = strings.ToLower(middle[0]), strings.ToLower(middle[1])
		if !ruleWord.MatchString(s.Action) {
			return fmt.Errorf("%q: the action is one word, like submits", text)
		}
		if err := validWho(t, s.Who); err != nil {
			return err
		}
		t.Steps = append(t.Steps, s)
	case "tell":
		i := strings.LastIndex(strings.ToLower(text), " to ")
		if i < 0 {
			return fmt.Errorf("%q isn't like \"tell: submitted to role:admin\"", text)
		}
		tl := tell{At: list(text[:i])}
		for _, w := range list(strings.ToLower(text[i+4:])) {
			if err := validWho(t, w); err != nil {
				return err
			}
			tl.Who = append(tl.Who, w)
		}
		t.Tells = append(t.Tells, tl)
	case "follow-ups":
		for _, w := range list(strings.ToLower(strings.TrimPrefix(strings.TrimSpace(text), "tell "))) {
			if err := validWho(t, w); err != nil {
				return err
			}
			t.FollowUps = append(t.FollowUps, w)
		}
	case "remind":
		m := afterWords.FindStringSubmatch(text)
		if m == nil {
			return fmt.Errorf("%q isn't like \"remind: submitted after 3 days\"", text)
		}
		n, _ := strconv.Atoi(m[2])
		unit := 24 * time.Hour
		if strings.HasPrefix(strings.ToLower(m[3]), "hour") {
			unit = time.Hour
		}
		if n < 1 {
			return fmt.Errorf("%q: remind after at least 1 %s", text, m[3])
		}
		t.Reminds = append(t.Reminds, remind{At: list(m[1]), After: time.Duration(n) * unit})
	}
	return nil
}

// settleSteps checks a table's steps once all its lines are read, and makes
// its status column the steps' own.
func settleSteps(t *table) {
	if t.Steps == nil {
		if t.Tells != nil || t.Reminds != nil || t.FollowUps != nil {
			t.Problems = append(t.Problems, t.Name+": tell:, remind: and follow-ups: need steps")
		}
		return
	}
	c := t.col(statusColumn)
	if c == nil {
		t.Problems = append(t.Problems, t.Name+": steps need a status column")
		t.Steps, t.Tells, t.Reminds = nil, nil, nil
		return
	}
	known := map[string]bool{}
	var all []string
	for _, s := range t.Steps {
		for _, x := range []string{s.From, s.To} {
			if !known[x] {
				known[x] = true
				all = append(all, x)
			}
		}
	}
	for _, tl := range t.Tells {
		for _, x := range tl.At {
			if !known[x] {
				t.Problems = append(t.Problems, fmt.Sprintf("%s: tell: no step reaches %q", t.Name, x))
			}
		}
	}
	for _, r := range t.Reminds {
		for _, x := range r.At {
			if !known[x] {
				t.Problems = append(t.Problems, fmt.Sprintf("%s: remind: no step leaves %q", t.Name, x))
			}
		}
	}
	t.Start = t.Steps[0].From
	if c.Default != "" {
		t.Start = c.Default
	}
	c.Auto, c.Required, c.Choices = true, false, all
}

// mayAct says whether a person may take a step on a row they can read.
func mayAct(p person, row map[string]string, s step) bool {
	if p.Admin {
		return true
	}
	if p.Name == "" {
		return false
	}
	kind, name, _ := strings.Cut(s.Who, ":")
	switch {
	case s.Who == "owner":
		return strings.EqualFold(strings.TrimSpace(row[loginColumn]), p.Name)
	case kind == "role":
		return p.Role == name
	case kind == "group":
		return p.is(s.Who)
	case kind == "column":
		return strings.EqualFold(strings.TrimSpace(row[name]), p.Name)
	}
	return false
}

// History.

const stepsTable = `CREATE TABLE IF NOT EXISTS _steps (tbl TEXT NOT NULL, row TEXT NOT NULL, t INTEGER NOT NULL,
  node TEXT NOT NULL DEFAULT '', who TEXT NOT NULL, action TEXT NOT NULL, from_s TEXT NOT NULL, to_s TEXT NOT NULL,
  comment TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS _steps_rows ON _steps (tbl, row, t);`

const (
	entryStep    = "step" // a row moved on (or was made, or someone was reminded)
	actionMade   = "_made"
	actionRemind = "_remind"
	actionMail   = "_mail" // a message about the row came in
	actionNote   = "_note" // a follow-up written on the row
)

var errMovedOn = errors.New("someone had already moved this on")

// applyStep does a step entry: the status changes, if the row is still where
// the step starts, and the history keeps it.
func applyStep(tx *sql.Tx, t table, e entry) error {
	f := func(k string) string { s, _ := e.Fields[k].(string); return s }
	from, to, action := f("from"), f("to"), f("action")
	if action != actionMade && action != actionRemind && action != actionMail && action != actionNote {
		var now sql.NullString
		if err := tx.QueryRow("SELECT "+quote(statusColumn)+" FROM "+quote(t.Name)+" WHERE "+quote(t.Key)+" = ?", e.Row).Scan(&now); err != nil {
			if err == sql.ErrNoRows {
				return errDeleted
			}
			return err
		}
		if now.String != from {
			return errMovedOn
		}
		if _, err := tx.Exec("UPDATE "+quote(t.Name)+" SET "+quote(statusColumn)+" = ? WHERE "+quote(t.Key)+" = ?", to, e.Row); err != nil {
			return err
		}
	}
	_, err := tx.Exec(`INSERT INTO _steps (tbl, row, t, node, who, action, from_s, to_s, comment) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.Name, e.Row, e.T, e.Node, f("by"), action, from, to, f("comment"))
	return err
}

// takeStep keeps a step: through the log if there is one.
func (d *data) takeStep(t table, row, by, action, from, to, comment string) error {
	fields := map[string]any{"by": by, "action": action, "from": from, "to": to}
	if comment != "" {
		fields["comment"] = comment
	}
	if d.sync != nil {
		return d.sync.write(entryStep, t.Name, row, fields)
	}
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := applyStep(tx, t, entry{T: time.Now().UnixMilli(), Kind: entryStep, Table: t.Name, Row: row, Fields: fields}); err != nil {
		return err
	}
	return tx.Commit()
}

type historyLine struct {
	When, Who, What, Comment string
}

func (d *data) history(t table, row string) []historyLine {
	rows, err := d.db.Query(`SELECT t, who, action, from_s, to_s, comment FROM _steps WHERE tbl = ? AND row = ? ORDER BY t, rowid`, t.Name, row)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []historyLine
	for rows.Next() {
		var at int64
		var h historyLine
		var action, from, to string
		if rows.Scan(&at, &h.Who, &action, &from, &to, &h.Comment) != nil {
			return out
		}
		h.When = time.UnixMilli(at).Format("2 Jan 2006 15:04")
		switch action {
		case actionMade:
			h.What = "Made, " + to
		case actionRemind:
			h.What, h.Who = "Reminded ("+to+")", ""
		case actionMail:
			h.What = "Mail came"
		case actionNote:
			h.What = "Follow-up"
		default:
			h.What = (step{Action: action}).label() + ": " + from + " → " + to
		}
		out = append(out, h)
	}
	return out
}

// The step a person presses.

type stepButton struct{ Action, Label string }

// stepsFor is the steps a person may take on a row now.
func stepsFor(t table, p person, row map[string]string) []stepButton {
	var out []stepButton
	for _, s := range t.Steps {
		if s.From == row[statusColumn] && mayAct(p, row, s) && !slices.ContainsFunc(out, func(b stepButton) bool { return b.Action == s.Action }) {
			out = append(out, stepButton{s.Action, s.label()})
		}
	}
	return out
}

// readRow is one row, if the person may read it, as text.
func (d *data) readRow(tables []table, t table, p person, id string) (map[string]string, bool) {
	where, args := quote(t.Key)+" = ?", []any{id}
	if limit, limitArgs, ok := t.rows(tables, p); !ok {
		return nil, false
	} else if limit != "" {
		where += " AND " + limit
		args = append(args, limitArgs...)
	}
	tx, err := d.db.Begin()
	if err != nil {
		return nil, false
	}
	defer tx.Rollback()
	rows, err := readRows(tx, t, "WHERE "+where, args...)
	if err != nil || len(rows) != 1 {
		return nil, false
	}
	out := map[string]string{}
	for k, v := range rows[0] {
		out[k] = text(v)
	}
	return out, true
}

func (d *data) step(w http.ResponseWriter, r *http.Request) {
	tables, t := d.tableFor(w, r)
	if t == nil {
		return
	}
	who, id := viewer(r), r.PathValue("id")
	row, ok := d.readRow(tables, *t, who, id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	action := r.PostFormValue("action")
	var s *step
	for i := range t.Steps {
		if x := t.Steps[i]; x.From == row[statusColumn] && x.Action == action && mayAct(who, row, x) {
			s = &x
			break
		}
	}
	if s == nil {
		http.Error(w, "you can't do that to this now", http.StatusForbidden)
		return
	}
	comment := strings.TrimSpace(r.PostFormValue("comment"))
	if len(comment) > 4000 {
		comment = comment[:4000]
	}
	if err := d.takeStep(*t, id, who.Name, s.Action, s.From, s.To, comment); err != nil {
		msg := plainError(err)
		if errors.Is(err, errMovedOn) {
			msg = "Someone had already moved this on. Have a look at it again."
		}
		page(w, r, http.StatusConflict, "message", formPage{Name: t.Name, Action: tableLink(t.Name) + url.PathEscape(id), Error: msg})
		return
	}
	row[statusColumn] = s.To
	d.notice(r)
	go d.tellAbout(*t, id, row, who.Name, s, comment)
	http.Redirect(w, r, tableLink(t.Name)+url.PathEscape(id), http.StatusSeeOther)
}

// made keeps a new row's start in its history, and tells whoever is told.
func (d *data) made(t table, id, by string) {
	if t.Steps == nil {
		return
	}
	if err := d.takeStep(t, id, by, actionMade, "", t.Start, ""); err != nil {
		log.Printf("steps: %s %s: %v", t.Name, id, err)
	}
	tables, err := d.design()
	if err != nil {
		return
	}
	row, ok := d.readRow(tables, t, person{Admin: true}, id)
	if !ok {
		return
	}
	go d.tellAbout(t, id, row, by, &step{Action: actionMade, To: t.Start}, "")
}

// Telling people.

// notifier is how data reaches people: set by the app.
type notifier struct {
	send    func(from string, to []string, subject, body string) error // nil: nobody is emailed; from "" is the node's own address
	people  func(role string) []string                                 // the accounts with a role
	site    atomic.Value                                               // string: the site's address, for links
	address func(r *http.Request) string                               // the site's address, from a request
}

// notice remembers the site's address from a request, for links made later.
func (d *data) notice(r *http.Request) {
	if d.notify.site.Load() == nil && d.notify.address != nil {
		d.notify.site.Store(d.notify.address(r))
	}
}

func (d *data) link(t table, id string) string {
	site, _ := d.notify.site.Load().(string)
	return site + tableLink(t.Name) + url.PathEscape(id)
}

// whoIs is the emails a who means for a row.
func (d *data) whoIs(who string, row map[string]string) []string {
	kind, name, _ := strings.Cut(who, ":")
	var out []string
	switch {
	case who == "owner":
		out = []string{row[loginColumn]}
	case kind == "column":
		out = []string{row[name]}
	case kind == "role" && d.notify.people != nil:
		out = d.notify.people(name)
	case kind == "group":
		out = groupMembers(d.db, name)
	case kind == "through":
		out = d.peopleThrough(name, row)
	}
	return slices.DeleteFunc(out, func(s string) bool { return !strings.Contains(s, "@") })
}

// rowTitle is how a row is named in an email.
func rowTitle(t table, row map[string]string) string {
	label := t.Label
	for _, c := range t.Cols { // a message's subject says more than who sent it
		if c.Mail == "subject" || strings.EqualFold(c.Name, "subject") || strings.EqualFold(c.Name, "title") {
			label = c.Name
			break
		}
	}
	if s := strings.TrimSpace(row[label]); s != "" && label != t.Key {
		if len(s) > 80 {
			s = s[:80] + "…"
		}
		return s
	}
	return "a " + strings.ToLower(human(t.Name)) + " record"
}

// tellAbout emails the people the design tells when a row reaches a status,
// except whoever moved it there. People told by their role (staff, admins)
// hear who moved it; the row's owner or the person a column names hear what
// happened, not which of the staff did it.
func (d *data) tellAbout(t table, id string, row map[string]string, by string, s *step, comment string) {
	if d.notify.send == nil {
		return
	}
	var inside, others []string
	for _, tl := range t.Tells {
		if slices.Contains(tl.At, s.To) {
			for _, w := range tl.Who {
				if (strings.HasPrefix(w, "role:") && w != "role:client") || strings.HasPrefix(w, "group:") {
					inside = append(inside, d.whoIs(w, row)...)
				} else {
					others = append(others, d.whoIs(w, row)...)
				}
			}
		}
	}
	inside = cleanList(inside, by)
	others = slices.DeleteFunc(cleanList(others, by), func(a string) bool { return slices.Contains(inside, a) })
	title := rowTitle(t, row)
	subject := fmt.Sprintf("%s %s: %s", human(t.Name), s.To, title)
	for _, group := range []struct {
		to   []string
		said bool
	}{{inside, true}, {others, false}} {
		if len(group.to) == 0 {
			continue
		}
		body := fmt.Sprintf("%s: %s\n\nIt's now %s.", human(t.Name), title, s.To)
		if group.said {
			if s.Action == actionMade {
				body += fmt.Sprintf(" %s made it.", by)
			} else {
				body += fmt.Sprintf(" %s pressed %s.", by, s.label())
			}
		}
		body += "\n"
		if comment != "" {
			body += "\n" + comment + "\n"
		}
		body += "\nOpen it: " + d.link(t, id) + "\n"
		d.sendEach(t.mailFrom(), group.to, subject, body)
	}
}

// sendEach emails each person on their own, so one who can't be reached
// just now (someone whose box is off, say) doesn't stop the others' mail,
// and nobody sees who else was told.
func (d *data) sendEach(from string, to []string, subject, body string) {
	for _, a := range to {
		if err := d.notify.send(from, []string{a}, subject, body); err != nil {
			log.Printf("steps: emailing %s (%s): %v", a, subject, err)
		}
	}
}

// cleanList is a list of addresses once each, without one.
func cleanList(list []string, without string) []string {
	seen := map[string]bool{cleanEmail(without): true}
	var out []string
	for _, a := range list {
		if a = cleanEmail(a); a != "" && !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	return out
}

// Reminders.

// remindAll reminds, once, the people who may move a row that has waited
// longer than its design says. With a log, only the machine that moved the
// row there reminds, and the reminder is kept, so it's sent once.
func (d *data) remindAll(now time.Time) {
	if d.notify.send == nil {
		return
	}
	tables, err := d.design()
	if err != nil {
		return
	}
	self := ""
	if d.sync != nil {
		self = d.sync.node
	}
	for _, t := range tables {
		for _, rm := range t.Reminds {
			for _, status := range rm.At {
				d.remindAt(tables, t, status, rm.After, self, now)
			}
		}
	}
}

func (d *data) remindAt(tables []table, t table, status string, after time.Duration, self string, now time.Time) {
	// Each row in the status, when it got there and on which machine, and
	// whether it has been reminded since.
	rows, err := d.db.Query(`SELECT CAST(x.`+quote(t.Key)+` AS TEXT), s.t, s.node,
		EXISTS (SELECT 1 FROM _steps r WHERE r.tbl = s.tbl AND r.row = s.row AND r.action = ? AND r.t >= s.t)
		FROM `+quote(t.Name)+` x JOIN _steps s ON s.tbl = ? AND s.row = CAST(x.`+quote(t.Key)+` AS TEXT) AND s.to_s = ? AND s.action != ?
		WHERE x.`+quote(statusColumn)+` = ? AND s.t = (SELECT MAX(t) FROM _steps WHERE tbl = s.tbl AND row = s.row AND action != ?)`,
		actionRemind, t.Name, status, actionRemind, status, actionRemind)
	if err != nil {
		log.Printf("steps: reminders for %s: %v", t.Name, err)
		return
	}
	var list []string
	for rows.Next() {
		var id, node string
		var at int64
		var reminded bool
		if rows.Scan(&id, &at, &node, &reminded) != nil {
			continue
		}
		if !reminded && node == self && now.Sub(time.UnixMilli(at)) >= after {
			list = append(list, id)
		}
	}
	rows.Close()
	for _, id := range list {
		row, ok := d.readRow(tables, t, person{Admin: true}, id)
		if !ok {
			continue
		}
		var to []string
		for _, s := range t.Steps {
			if s.From == status {
				to = append(to, d.whoIs(s.Who, row)...)
			}
		}
		if len(to) == 0 && d.notify.people != nil { // only admins may move it
			to = d.notify.people(roleAdmin)
		}
		to = cleanList(to, "")
		if err := d.takeStep(t, id, "", actionRemind, status, status, ""); err != nil {
			log.Printf("steps: %s %s: %v", t.Name, id, err)
			continue
		}
		if len(to) == 0 {
			continue
		}
		title := rowTitle(t, row)
		body := fmt.Sprintf("%s: %s\n\nIt has been %s since %s, waiting for one of you.\n\nOpen it: %s\n",
			human(t.Name), title, status, d.enteredAt(t, id), d.link(t, id))
		d.sendEach(t.mailFrom(), to, fmt.Sprintf("Waiting: %s %s: %s", human(t.Name), status, title), body)
	}
}

func (d *data) enteredAt(t table, id string) string {
	var at int64
	d.db.QueryRow(`SELECT MAX(t) FROM _steps WHERE tbl = ? AND row = ? AND action != ?`, t.Name, id, actionRemind).Scan(&at)
	return time.UnixMilli(at).Format("2 January 15:04")
}

// remindEvery looks for rows to remind about, every so often, for good.
func (d *data) remindEvery(every time.Duration) {
	for {
		time.Sleep(every)
		d.remindAll(time.Now())
	}
}

// peopleThrough is the people of the record a row's column points to: the
// logins of a table that links people to that record (sign_owner_people,
// for a sign owner), or that record's own login.
func (d *data) peopleThrough(col string, row map[string]string) []string {
	tables, err := d.design()
	if err != nil {
		return nil
	}
	var t *table
	for i := range tables {
		if tables[i].col(col) != nil && tables[i].col(col).Ref != nil {
			if _, ok := row[col]; ok {
				t = &tables[i]
				break
			}
		}
	}
	if t == nil || row[col] == "" {
		return nil
	}
	ref := t.col(col).Ref
	parent := find(tables, ref.Table)
	if parent == nil {
		return nil
	}
	var out []string
	add := func(q string, args ...any) {
		rows, err := d.db.Query(q, args...)
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var s sql.NullString
			if rows.Scan(&s) == nil && s.String != "" {
				out = append(out, s.String)
			}
		}
	}
	if parent.col(loginColumn) != nil {
		add("SELECT "+quote(loginColumn)+" FROM "+quote(parent.Name)+" WHERE "+quote(ref.Column)+" = ?", row[col])
	}
	for _, o := range tables {
		if o.col(loginColumn) == nil {
			continue
		}
		for _, c := range o.Cols {
			if c.Ref != nil && c.Ref.Table == parent.Name {
				add("SELECT "+quote(loginColumn)+" FROM "+quote(o.Name)+" WHERE "+quote(c.Name)+" = ?", row[col])
			}
		}
	}
	return out
}

// followUp is a follow-up written on a row by someone who may read it.
func (d *data) followUp(w http.ResponseWriter, r *http.Request) {
	tables, t := d.tableFor(w, r)
	if t == nil {
		return
	}
	who, id := viewer(r), r.PathValue("id")
	if t.FollowUps == nil {
		http.NotFound(w, r)
		return
	}
	row, ok := d.readRow(tables, *t, who, id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	text := strings.TrimSpace(r.PostFormValue("text"))
	if text == "" {
		http.Redirect(w, r, tableLink(t.Name)+url.PathEscape(id), http.StatusSeeOther)
		return
	}
	if len(text) > 4000 {
		text = text[:4000]
	}
	if err := d.takeStep(*t, id, who.Name, actionNote, "", "", text); err != nil {
		page(w, r, http.StatusConflict, "message", formPage{Name: t.Name, Action: tableLink(t.Name) + url.PathEscape(id), Error: plainError(err)})
		return
	}
	d.notice(r)
	go d.tellFollowUp(*t, id, row, who, text)
	http.Redirect(w, r, tableLink(t.Name)+url.PathEscape(id), http.StatusSeeOther)
}

// tellFollowUp emails a follow-up to the people the design names, not the
// writer. Staff and groups hear who wrote; the customers' side hears what
// was written, not which of the staff wrote it.
func (d *data) tellFollowUp(t table, id string, row map[string]string, by person, text string) {
	if d.notify.send == nil {
		return
	}
	var inside, others []string
	for _, w := range t.FollowUps {
		if (strings.HasPrefix(w, "role:") && w != "role:client") || strings.HasPrefix(w, "group:") {
			inside = append(inside, d.whoIs(w, row)...)
		} else {
			others = append(others, d.whoIs(w, row)...)
		}
	}
	inside = cleanList(inside, by.Name)
	others = slices.DeleteFunc(cleanList(others, by.Name), func(a string) bool { return slices.Contains(inside, a) })
	title := rowTitle(t, row)
	subject := "Follow-up: " + title
	for _, group := range []struct {
		to   []string
		said bool
	}{{inside, true}, {others, false}} {
		if len(group.to) == 0 {
			continue
		}
		head := "A follow-up on " + human(t.Name) + ": " + title
		if group.said || by.Role == roleClient {
			head += ", from " + by.Name
		}
		body := head + "\n\n" + text + "\n\nOpen it, or answer there: " + d.link(t, id) + "\n"
		d.sendEach(t.mailFrom(), group.to, subject, body)
	}
}
