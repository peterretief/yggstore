package app

import (
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"encoding/csv"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The design of a data file is the file's own tables: foreign keys are the
// relationships, and column types, NOT NULL and CHECK (x IN (...)) say what
// each form field is. So the diagram, forms and reports below are worked
// out from the file each time, and change when the design does.
//
//	go run ./examples/signin -data clients.db -import design.sql   # see import.go
//	go run ./examples/signin -data clients.db
//
// Everyone signed in can see and change everything here.

const (
	dataBase = base + "data/"
	pageRows = 100
	maxPick  = 1000 // the most rows a drop-down offers
)

var (
	checkIn = regexp.MustCompile(`(?i)CHECK\s*\(\s*"?(\w+)"?\s+IN\s*\(([^)]*)\)\s*\)`)
	quoted  = regexp.MustCompile(`'((?:[^']|'')*)'`)
)

type ref struct{ Table, Column string }

type column struct {
	Name, Type string
	Required   bool
	Auto       bool // given when a row is made, not asked for
	Random     bool // Auto, and a random id: see newID
	Maker      bool // Auto, and who made the row: "fill: who made it" (permissions.go)
	Ref        *ref
	Choices    []string
	HasDefault bool            // the design gives it a value when it's left out
	Hide       map[string]bool // roles it's hidden from (permissions.go)
	Label      string          // how it's named to people, if not by its name
	Help       string          // a line under its field in forms
}

type table struct {
	Name     string
	Cols     []column
	Key      string   // what identifies a row
	Label    string   // what a row is called in drop-downs and reports
	Rules    []rule   // who may do what (permissions.go)
	Problems []string // rules that couldn't be read
}

// visible is the columns people see in reports: a random id means nothing
// to anyone.
func (t table) visible() []column {
	var out []column
	for _, c := range t.Cols {
		if !c.Random {
			out = append(out, c)
		}
	}
	return out
}

// newID is the id of a new row in a table with id TEXT PRIMARY KEY. Numbers
// would clash between machines that work apart, so ids are random. The
// letter in front keeps it from ever reading as a number.
func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return "r" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}

// loginColumn, in a table's design, is the email of the account a row belongs
// to. What's linked to that row by foreign keys belongs to them too.
const loginColumn = "login"

// scope is what limits a table to a client's own rows, and whether they can
// see the table at all: not if it has no way back to a login column.
func scope(tables []table, t table, p person) (cond string, args []any, ok bool) {
	return scopeBy(tables, t, p, loginColumn)
}

// scopeBy limits a table to the rows that reach a column naming this person
// (by): in the row, in a row it refers to (and so on up), or in a row of
// another table that refers to it, such as a course's teachers in
// course_teachers.
func scopeBy(tables []table, t table, p person, by string) (string, []any, bool) {
	return scopeFrom(tables, t, p, by, map[string]bool{})
}

func scopeFrom(tables []table, t table, p person, by string, seen map[string]bool) (string, []any, bool) {
	if p.Name == "" || seen[t.Name] {
		return "", nil, false
	}
	seen[t.Name] = true
	if t.col(by) != nil {
		return "lower(" + quote(by) + ") = ?", []any{cleanEmail(p.Name)}, true
	}
	for _, c := range t.Cols {
		if c.Ref == nil {
			continue
		}
		parent := find(tables, c.Ref.Table)
		if parent == nil {
			continue
		}
		if cond, args, ok := scopeFrom(tables, *parent, p, by, seen); ok {
			return quote(c.Name) + " IN (SELECT " + quote(c.Ref.Column) + " FROM " + quote(parent.Name) + " WHERE " + cond + ")", args, true
		}
	}
	// A table that links people to this one: its rows naming them.
	for _, o := range tables {
		if o.col(by) == nil {
			continue
		}
		for _, c := range o.Cols {
			if c.Ref != nil && c.Ref.Table == t.Name {
				return quote(c.Ref.Column) + " IN (SELECT " + quote(c.Name) + " FROM " + quote(o.Name) + " WHERE lower(" + quote(by) + ") = ?)", []any{cleanEmail(p.Name)}, true
			}
		}
	}
	return "", nil, false
}

// allowed says whether a person may do an action (read, write or delete) to
// a row as it is, and, for a write, to the row as it would be with the
// changes: so nobody moves a row into or out of what's theirs. id "" is a
// new row.
func (d *data) allowed(tables []table, t table, p person, action, id string, changes map[string]any) bool {
	cond, args, ok := t.rowsFor(tables, p, action)
	if !ok {
		return false
	}
	if cond == "" {
		return true
	}
	values := map[string]any{}
	if id != "" {
		var n int
		if d.db.QueryRow("SELECT COUNT(*) FROM "+quote(t.Name)+" WHERE "+quote(t.Key)+" = ? AND "+cond, append([]any{id}, args...)...).Scan(&n); n != 1 {
			return false
		}
		if changes == nil {
			return true
		}
		tx, err := d.db.Begin()
		if err != nil {
			return false
		}
		rows, err := readRows(tx, t, "WHERE "+quote(t.Key)+" = ?", id)
		tx.Rollback()
		if err != nil || len(rows) != 1 {
			return false
		}
		values = rows[0]
	}
	for k, v := range changes {
		values[k] = v
	}
	if _, ok := values[t.Key]; !ok {
		values[t.Key] = id
	}
	// The row as it would be, as a one-row table the same condition can read.
	var cols []string
	var vals []any
	for _, c := range t.Cols {
		cols = append(cols, "? AS "+quote(c.Name))
		vals = append(vals, values[c.Name])
	}
	var n int
	d.db.QueryRow("SELECT COUNT(*) FROM (SELECT "+strings.Join(cols, ", ")+") WHERE "+cond, append(vals, args...)...).Scan(&n)
	return n == 1
}

func (t table) col(name string) *column {
	for i := range t.Cols {
		if t.Cols[i].Name == name {
			return &t.Cols[i]
		}
	}
	return nil
}

// kind is the form field for a column.
func (c column) kind() string {
	t := strings.ToUpper(c.Type)
	has := func(s ...string) bool {
		return slices.ContainsFunc(s, func(x string) bool { return strings.Contains(t, x) })
	}
	switch {
	case c.Ref != nil:
		return "ref"
	case c.Choices != nil:
		return "choice"
	case has("BOOL"):
		return "checkbox"
	case has("DATETIME"):
		return "datetime-local"
	case has("DATE"):
		return "date"
	case has("INT"):
		return "number"
	case has("REAL", "FLOA", "DOUB", "NUM", "DEC"):
		return "decimal"
	case has("LONGTEXT"):
		return "textarea"
	case strings.Contains(strings.ToLower(c.Name), "email") || strings.EqualFold(c.Name, loginColumn):
		return "email"
	}
	return "text"
}

// summed is whether a report adds a column up.
func (c column) summed() bool {
	k := c.kind()
	return !c.Auto && (k == "decimal" || k == "number")
}

func quote(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

// human is a column's name as a person would say it.
func human(s string) string {
	s = strings.ReplaceAll(strings.TrimSuffix(s, "_id"), "_", " ")
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func tableLink(name string) string { return dataBase + url.PathEscape(name) + "/" }

type data struct {
	db   *sql.DB
	path string
	sync *syncLog // nil without -machine
	keep *keeper  // nil without -sync-key
}

func openData(path string) (*data, error) {
	db, err := sql.Open("sqlite3", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(wal)&_pragma=foreign_keys(1)&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	return &data{db: db, path: path}, nil
}

// design reads the file's tables, in name order.
func (d *data) design() ([]table, error) {
	rows, err := d.db.Query(`SELECT name, sql FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite%' AND name NOT LIKE '\_%' ESCAPE '\' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var out []table
	stmts := map[string]string{}
	for rows.Next() {
		var t table
		var stmt string
		if err := rows.Scan(&t.Name, &stmt); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, t)
		stmts[t.Name] = stmt
	}
	rows.Close()
	for i := range out {
		if err := d.readTable(&out[i], stmts[out[i].Name]); err != nil {
			return nil, err
		}
	}
	for i := range out { // a foreign key may leave out which column it means
		for j := range out[i].Cols {
			if r := out[i].Cols[j].Ref; r != nil && r.Column == "" {
				for _, p := range out {
					if p.Name == r.Table {
						r.Column = p.Key
					}
				}
			}
		}
	}
	return out, nil
}

func (d *data) readTable(t *table, stmt string) error {
	rows, err := d.db.Query(`SELECT name, type, "notnull", dflt_value, pk FROM pragma_table_info(?)`, t.Name)
	if err != nil {
		return err
	}
	var keys []string
	for rows.Next() {
		var c column
		var notNull bool
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&c.Name, &c.Type, &notNull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		c.Required = notNull && !dflt.Valid
		c.HasDefault = dflt.Valid
		if pk > 0 {
			keys = append(keys, c.Name)
		}
		t.Cols = append(t.Cols, c)
	}
	rows.Close()
	t.Key = "rowid"
	if len(keys) == 1 {
		t.Key = keys[0]
		c := t.col(t.Key)
		switch {
		case strings.EqualFold(c.Type, "INTEGER"):
			c.Auto, c.Required = true, false
		case strings.EqualFold(c.Type, "TEXT") && strings.EqualFold(c.Name, "id"):
			c.Auto, c.Random, c.Required = true, true, false
		}
	}
	fks, err := d.db.Query(`SELECT "from", "table", "to" FROM pragma_foreign_key_list(?)`, t.Name)
	if err != nil {
		return err
	}
	for fks.Next() {
		var from, parent string
		var to sql.NullString
		if err := fks.Scan(&from, &parent, &to); err != nil {
			fks.Close()
			return err
		}
		if c := t.col(from); c != nil {
			c.Ref = &ref{parent, to.String}
		}
	}
	fks.Close()
	for _, m := range checkIn.FindAllStringSubmatch(stmt, -1) {
		if c := t.col(m[1]); c != nil {
			for _, q := range quoted.FindAllStringSubmatch(m[2], -1) {
				c.Choices = append(c.Choices, strings.ReplaceAll(q[1], "''", "'"))
			}
		}
	}
	// A row is called by its first short text, or failing that its first
	// long text, in the order the design gives them.
	readRules(t, stmt) // first: a column hidden from anyone can't name a row
	t.Label = t.Key
	for _, kinds := range [][]string{{"text", "email"}, {"textarea"}} {
		if i := slices.IndexFunc(t.Cols, func(c column) bool {
			return slices.Contains(kinds, c.kind()) && !c.Auto && c.Ref == nil && len(c.Hide) == 0
		}); i >= 0 {
			t.Label = t.Cols[i].Name
			break
		}
	}
	return nil
}

func find(tables []table, name string) *table {
	for i := range tables {
		if tables[i].Name == name {
			return &tables[i]
		}
	}
	return nil
}

// tableFor is the table a request names, answering if it can't be had.
func (d *data) tableFor(w http.ResponseWriter, r *http.Request) ([]table, *table) {
	tables, err := d.design()
	if err != nil {
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return nil, nil
	}
	t := find(tables, r.PathValue("table"))
	if t == nil {
		http.NotFound(w, r)
	}
	return tables, t
}

// routes keeps seeing to people signed in, and changing to admins. What a
// client may see is limited in the handlers, by scope.
func (d *data) routes(mux *http.ServeMux, in, admin func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET "+dataBase+"{$}", in(d.diagram))
	if d.sync != nil {
		mux.HandleFunc("GET "+dataBase+"_refused", admin(d.refusedPage))
		mux.HandleFunc("POST "+dataBase+"_refused", admin(d.dismiss))
	}
	mux.HandleFunc("GET "+dataBase+"{table}/{$}", in(d.report))
	// Signed in is all a route asks: each handler then asks table.access.
	mux.HandleFunc("GET "+dataBase+"{table}/new", in(d.form))
	mux.HandleFunc("POST "+dataBase+"{table}/new", in(d.save))
	mux.HandleFunc("GET "+dataBase+"{table}/{id}", in(d.form))
	mux.HandleFunc("POST "+dataBase+"{table}/{id}", in(d.save))
	mux.HandleFunc("POST "+dataBase+"{table}/{id}/delete", in(d.remove))
}

// The diagram: parents on the left, what refers to them to their right.

const boxW, boxGap, headH, rowH = 220, 90, 28, 20 // boxW is in the page too

type boxRow struct {
	Name, Type string
	Y          int
	Mark       string
}

type box struct {
	Name, Link string
	X, Y, H    int
	Rows       []boxRow
}

type line struct {
	D            string
	OneX, OneY   int
	ManyX, ManyY int
}

type diagramPage struct {
	Tables   []table
	Refused  int
	Machines []string
	W, H     int
	Boxes    []box
	Lines    []line
}

// levels is how many tables deep each table's parents go.
func levels(tables []table) map[string]int {
	level := map[string]int{}
	for range tables { // cycles stop when the loop does
		for _, t := range tables {
			for _, c := range t.Cols {
				if c.Ref != nil && c.Ref.Table != t.Name && find(tables, c.Ref.Table) != nil {
					level[t.Name] = min(max(level[t.Name], level[c.Ref.Table]+1), len(tables))
				}
			}
		}
	}
	return level
}

func layout(tables []table) diagramPage {
	level := levels(tables)
	p := diagramPage{Tables: tables}
	nextY := map[int]int{}
	rowY := map[string]int{} // "table.column" to the middle of its row
	at := map[string]*box{}
	for _, t := range tables {
		l := level[t.Name]
		b := box{Name: t.Name, Link: tableLink(t.Name), X: 20 + l*(boxW+boxGap), Y: 20 + nextY[l], H: headH + len(t.Cols)*rowH + 6}
		for i, c := range t.Cols {
			row := boxRow{Name: c.Name, Type: strings.ToLower(c.Type), Y: b.Y + headH + i*rowH + 15}
			switch {
			case c.Name == t.Key:
				row.Mark = "key"
			case c.Ref != nil:
				row.Mark = "ref"
			}
			b.Rows = append(b.Rows, row)
			rowY[t.Name+"."+c.Name] = row.Y - 4
		}
		nextY[l] += b.H + 30
		p.Boxes = append(p.Boxes, b)
		p.W, p.H = max(p.W, b.X+boxW+20), max(p.H, b.Y+b.H+20)
	}
	for i := range p.Boxes {
		at[p.Boxes[i].Name] = &p.Boxes[i]
	}
	for _, t := range tables {
		for _, c := range t.Cols {
			if c.Ref == nil || at[c.Ref.Table] == nil {
				continue
			}
			child, parent := at[t.Name], at[c.Ref.Table]
			x1, y1 := parent.X+boxW, rowY[c.Ref.Table+"."+c.Ref.Column]
			x2, y2 := child.X, rowY[t.Name+"."+c.Name]
			mid := (x1 + x2) / 2
			if x2 <= x1 { // the same column of boxes: go round the right
				x2 = child.X + boxW
				mid = x1 + 45
			}
			p.Lines = append(p.Lines, line{
				D:    fmt.Sprintf("M %d %d C %d %d, %d %d, %d %d", x1, y1, mid, y1, mid, y2, x2, y2),
				OneX: x1 + 6, OneY: y1 - 4, ManyX: x2 - 12, ManyY: y2 - 4})
		}
	}
	return p
}

func (d *data) diagram(w http.ResponseWriter, r *http.Request) {
	tables, err := d.design()
	if err != nil {
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}
	if who := viewer(r); !who.Admin {
		var mine []table
		for _, t := range tables {
			if t.access(who).Read {
				mine = append(mine, t)
			}
		}
		page(w, r, http.StatusOK, "mine", mine)
		return
	}
	p := layout(tables)
	p.Refused = d.refusedCount()
	p.Machines = d.machines()
	page(w, r, http.StatusOK, "diagram", p)
}

// Reports.

type cell struct {
	Text, Link string
	Num        bool
}

type rowOut struct {
	Link  string
	Cells []cell
}

type head struct {
	Name, Title, Link string
	Num               bool
}

type group struct {
	Name  string
	Count int64
	Sums  []string
}

type reportPage struct {
	Name, Link, Q, By, Filter string
	Admin                     bool
	Total                     int64
	Heads                     []head
	Rows                      []rowOut
	Sums                      []cell
	HasSums                   bool
	Groups                    []group
	GroupHeads                []string
	GroupBy                   []head
	Prev, Next, CSV           string
}

// picks are a table's rows as a foreign key offers them: value to label.
func (d *data) picks(tables []table, r ref) ([][2]string, map[string]string) {
	return d.picksFor(tables, r, person{Admin: true})
}

// picksFor is picks, of only the rows who may read: what their drop-downs
// offer.
func (d *data) picksFor(tables []table, r ref, who person) ([][2]string, map[string]string) {
	p := find(tables, r.Table)
	if p == nil {
		return nil, nil
	}
	cond, args, ok := p.rows(tables, who)
	if !ok {
		return nil, nil
	}
	if cond != "" {
		cond = " WHERE " + cond
	}
	rows, err := d.db.Query(fmt.Sprintf(`SELECT %s, %s FROM %s%s ORDER BY 2 LIMIT %d`, quote(r.Column), quote(p.Label), quote(r.Table), cond, maxPick), args...)
	if err != nil {
		return nil, nil
	}
	defer rows.Close()
	var list [][2]string
	labels := map[string]string{}
	for rows.Next() {
		var v, l any
		if rows.Scan(&v, &l) == nil {
			list = append(list, [2]string{text(v), text(l)})
			labels[text(v)] = text(l)
		}
	}
	return list, labels
}

func text(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case []byte:
		return string(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case time.Time: // the driver's reading of a DATE or DATETIME column
		if x.Hour() == 0 && x.Minute() == 0 && x.Second() == 0 {
			return x.Format(time.DateOnly)
		}
		return x.Format("2006-01-02T15:04")
	case bool:
		if x {
			return "1"
		}
		return "0"
	}
	return fmt.Sprint(v)
}

func amount(v float64, isInt bool) string {
	if isInt {
		return strconv.FormatFloat(v, 'f', 0, 64)
	}
	return strconv.FormatFloat(v, 'f', 2, 64)
}

func (d *data) report(w http.ResponseWriter, r *http.Request) {
	tables, t := d.tableFor(w, r)
	if t == nil {
		return
	}
	who := viewer(r)
	acc := t.access(who)
	if !acc.Read {
		http.NotFound(w, r)
		return
	}
	full := *t
	*t = t.forViewer(who) // from here on, only what this person may see
	cols := t.visible()
	view := *t
	view.Cols = cols
	form := r.URL.Query()
	q, by, field, value := form.Get("q"), form.Get("by"), form.Get("f"), form.Get("v")

	// Column names only ever come from the design; what's typed is a value.
	var where []string
	var args []any
	if q != "" {
		var any []string
		for _, c := range cols {
			if k := c.kind(); k == "text" || k == "email" || k == "textarea" || k == "choice" {
				any = append(any, quote(c.Name)+` LIKE ?`)
				args = append(args, "%"+q+"%")
			}
		}
		if any != nil {
			where = append(where, "("+strings.Join(any, " OR ")+")")
		}
	}
	filter := ""
	if c := t.col(field); c != nil {
		where = append(where, quote(field)+" = ?")
		args = append(args, value)
		filter = c.title() + ": " + value
		if p := find(tables, c.Ref.Table); c.Ref != nil && p != nil && p.allRows(tables, who) {
			if _, labels := d.picks(tables, *c.Ref); labels[value] != "" {
				filter = c.title() + ": " + labels[value]
			}
		}
	}
	if limit, limitArgs, ok := full.rows(tables, who); !ok {
		http.NotFound(w, r)
		return
	} else if limit != "" {
		where = append(where, limit)
		args = append(args, limitArgs...)
	}
	cond := ""
	if where != nil {
		cond = " WHERE " + strings.Join(where, " AND ")
	}
	from := " FROM " + quote(t.Name) + cond

	order := t.Label
	sortBy, desc := form.Get("sort"), form.Get("desc") == "1"
	if t.col(sortBy) != nil {
		order = sortBy
	}
	dir := ""
	if desc {
		dir = " DESC"
	}
	names := []string{quote(t.Key) + ` AS "__key"`}
	for _, c := range cols {
		names = append(names, quote(c.Name))
	}
	query := "SELECT " + strings.Join(names, ", ") + from + " ORDER BY " + quote(order) + dir

	labels := map[string]map[string]string{}
	for _, c := range cols {
		if c.Ref != nil {
			_, labels[c.Name] = d.picks(tables, *c.Ref)
		}
	}
	show := func(c column, v any) cell {
		s := text(v)
		out := cell{Text: s, Num: c.summed()}
		switch {
		case c.kind() == "decimal" && s != "":
			if f, err := strconv.ParseFloat(s, 64); err == nil {
				out.Text = amount(f, false)
			}
		case c.kind() == "checkbox" && s != "":
			out.Text = map[bool]string{true: "Yes", false: "No"}[s != "0"]
		case c.Ref != nil && s != "":
			if l := labels[c.Name][s]; l != "" {
				out.Text = l
			}
			if p := find(tables, c.Ref.Table); p != nil && canSee(tables, *p, who) {
				out.Link = tableLink(c.Ref.Table) + url.PathEscape(s)
			}
		}
		return out
	}

	if form.Get("csv") == "1" {
		d.writeCSV(w, view, query, args, show)
		return
	}
	var total int64
	if err := d.db.QueryRow("SELECT COUNT(*)"+from, args...).Scan(&total); err != nil {
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}
	pageNo, _ := strconv.Atoi(form.Get("page"))
	pageNo = max(pageNo, 0)
	rows, err := d.db.Query(query+fmt.Sprintf(" LIMIT %d OFFSET %d", pageRows, pageNo*pageRows), args...)
	if err != nil {
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	link := func(change map[string]string) string {
		v := url.Values{}
		for k, x := range form {
			v[k] = x
		}
		for k, x := range change {
			if x == "" {
				v.Del(k)
			} else {
				v.Set(k, x)
			}
		}
		if len(v) == 0 {
			return tableLink(t.Name)
		}
		return tableLink(t.Name) + "?" + v.Encode()
	}
	p := reportPage{Admin: acc.Write, Name: t.Name, Link: tableLink(t.Name), Q: q, By: by, Filter: filter, Total: total, CSV: link(map[string]string{"csv": "1", "page": ""})}
	for _, c := range cols {
		h := head{Name: c.Name, Title: c.title(), Num: c.summed()}
		flip := ""
		if order == c.Name && !desc {
			flip = "1"
		}
		h.Link = link(map[string]string{"sort": c.Name, "desc": flip, "page": ""})
		p.Heads = append(p.Heads, h)
		if c.Ref != nil || c.Choices != nil || c.kind() == "checkbox" {
			p.GroupBy = append(p.GroupBy, head{Name: c.Name, Title: c.title(), Link: link(map[string]string{"by": c.Name})})
		}
	}
	for rows.Next() {
		vals := make([]any, len(cols)+1)
		ptrs := make([]any, len(vals))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			http.Error(w, "something went wrong", http.StatusInternalServerError)
			return
		}
		out := rowOut{Link: tableLink(t.Name) + url.PathEscape(text(vals[0]))}
		for i, c := range cols {
			out.Cells = append(out.Cells, show(c, vals[i+1]))
		}
		p.Rows = append(p.Rows, out)
	}
	rows.Close()
	if pageNo > 0 {
		p.Prev = link(map[string]string{"page": strconv.Itoa(pageNo - 1)})
	}
	if int64(pageNo+1)*pageRows < total {
		p.Next = link(map[string]string{"page": strconv.Itoa(pageNo + 1)})
	}
	for _, c := range cols {
		var s cell
		if c.summed() {
			var sum sql.NullFloat64
			d.db.QueryRow("SELECT SUM("+quote(c.Name)+")"+from, args...).Scan(&sum)
			s = cell{Text: amount(sum.Float64, c.kind() == "number"), Num: true}
			p.HasSums = true
		}
		p.Sums = append(p.Sums, s)
	}
	if c := view.col(by); c != nil {
		d.groups(&p, view, *c, from, args, show)
	}
	page(w, r, http.StatusOK, "report", p)
}

func canSee(tables []table, t table, p person) bool {
	_, _, ok := t.rows(tables, p)
	return ok
}

// groups adds the report's totals for each value of a column.
func (d *data) groups(p *reportPage, t table, by column, from string, args []any, show func(column, any) cell) {
	cols := []string{quote(by.Name), "COUNT(*)"}
	var summed []column
	for _, c := range t.Cols {
		if c.summed() {
			cols = append(cols, "SUM("+quote(c.Name)+")")
			summed = append(summed, c)
			p.GroupHeads = append(p.GroupHeads, human(c.Name))
		}
	}
	rows, err := d.db.Query("SELECT "+strings.Join(cols, ", ")+from+" GROUP BY 1 ORDER BY 2 DESC", args...)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var key any
		var g group
		sums := make([]sql.NullFloat64, len(summed))
		ptrs := []any{&key, &g.Count}
		for i := range sums {
			ptrs = append(ptrs, &sums[i])
		}
		if rows.Scan(ptrs...) != nil {
			return
		}
		if g.Name = show(by, key).Text; g.Name == "" {
			g.Name = "(none)"
		}
		for i, s := range sums {
			g.Sums = append(g.Sums, amount(s.Float64, summed[i].kind() == "number"))
		}
		p.Groups = append(p.Groups, g)
	}
}

func (d *data) writeCSV(w http.ResponseWriter, t table, query string, args []any, show func(column, any) cell) {
	rows, err := d.db.Query(query, args...)
	if err != nil {
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=report.csv")
	w.Header().Set("Cache-Control", "no-store")
	out := csv.NewWriter(w)
	defer out.Flush()
	var heads []string
	for _, c := range t.Cols {
		heads = append(heads, human(c.Name))
	}
	out.Write(heads)
	for rows.Next() {
		vals := make([]any, len(t.Cols)+1)
		ptrs := make([]any, len(vals))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if rows.Scan(ptrs...) != nil {
			return
		}
		var line []string
		for i, c := range t.Cols {
			s := show(c, vals[i+1]).Text
			// A spreadsheet would run this as a formula.
			if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) && !c.summed() {
				s = "'" + s
			}
			line = append(line, s)
		}
		out.Write(line)
	}
}

// Forms.

type field struct {
	Name, Title, Kind, Value, Help string
	Required                       bool
	Options                        [][2]string
}

type child struct {
	Table, Title, Link, Add string
	Count                   int64
}

type formPage struct {
	Name, Link, Action, Delete, Error string
	Fields                            []field
	Shown                             [][2]string // a client's view: title, value
	Children                          []child
}

// shown is a record as a client reads it. Nothing else is offered, so a
// drop-down can't list other people's records.
func (d *data) shown(tables []table, t table, got map[string]string) [][2]string {
	var out [][2]string
	for _, c := range t.Cols {
		if c.Auto {
			continue
		}
		s := got[c.Name]
		switch {
		case c.kind() == "checkbox" && s != "":
			s = map[bool]string{true: "Yes", false: "No"}[s != "0"]
		case c.Ref != nil:
			if _, labels := d.picks(tables, *c.Ref); labels[s] != "" {
				s = labels[s]
			}
		}
		out = append(out, [2]string{c.title(), s})
	}
	return out
}

func (d *data) fields(tables []table, t table, who person, values func(string) string) []field {
	var out []field
	for _, c := range t.Cols {
		if c.Auto {
			continue
		}
		f := field{Name: c.Name, Title: c.title(), Help: c.Help, Kind: c.kind(), Value: values(c.Name), Required: c.Required}
		switch f.Kind {
		case "ref":
			f.Options, _ = d.picksFor(tables, *c.Ref, who)
		case "choice":
			for _, x := range c.Choices {
				f.Options = append(f.Options, [2]string{x, x})
			}
		}
		out = append(out, f)
	}
	return out
}

func (d *data) form(w http.ResponseWriter, r *http.Request) {
	tables, t := d.tableFor(w, r)
	if t == nil {
		return
	}
	id := r.PathValue("id")
	who := viewer(r)
	acc := t.access(who)
	full := *t
	*t = t.forViewer(who)
	p := formPage{Name: t.Name, Link: tableLink(t.Name), Action: tableLink(t.Name) + "new"}
	if id == "" {
		if !acc.Write {
			http.Error(w, "you can't add to this", http.StatusForbidden)
			return
		}
		p.Fields = d.fields(tables, *t, viewer(r), r.URL.Query().Get) // a link may fill some in
		page(w, r, http.StatusOK, "form", p)
		return
	}
	vals := make([]any, len(t.Cols))
	ptrs := make([]any, len(vals))
	var names []string
	for i, c := range t.Cols {
		ptrs[i] = &vals[i]
		names = append(names, quote(c.Name))
	}
	where, args := quote(t.Key)+" = ?", []any{id}
	if limit, limitArgs, ok := full.rows(tables, who); !ok {
		http.NotFound(w, r)
		return
	} else if limit != "" {
		where += " AND " + limit
		args = append(args, limitArgs...)
	}
	err := d.db.QueryRow("SELECT "+strings.Join(names, ", ")+" FROM "+quote(t.Name)+" WHERE "+where, args...).Scan(ptrs...)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	got := map[string]string{}
	for i, c := range t.Cols {
		got[c.Name] = text(vals[i])
	}
	canWrite := acc.Write && d.allowed(tables, full, who, "write", id, nil)
	if canWrite {
		p.Fields = d.fields(tables, *t, viewer(r), func(n string) string { return got[n] })
		p.Action = tableLink(t.Name) + url.PathEscape(id)
	}
	if acc.Delete && d.allowed(tables, full, who, "delete", id, nil) {
		p.Delete = tableLink(t.Name) + url.PathEscape(id) + "/delete"
	}
	if !canWrite {
		p.Shown = d.shown(tables, *t, got)
	}
	for _, o := range tables {
		for _, c := range o.Cols {
			if c.Ref == nil || c.Ref.Table != t.Name {
				continue
			}
			v := got[c.Ref.Column]
			if c.Ref.Column == t.Key {
				v = id
			}
			k := child{Table: o.Name, Title: human(o.Name)}
			count, countArgs := quote(c.Name)+" = ?", []any{v}
			if limit, limitArgs, ok := o.rows(tables, who); !ok {
				continue
			} else if limit != "" {
				count += " AND " + limit
				countArgs = append(countArgs, limitArgs...)
			}
			d.db.QueryRow("SELECT COUNT(*) FROM "+quote(o.Name)+" WHERE "+count, countArgs...).Scan(&k.Count)
			k.Link = tableLink(o.Name) + "?" + url.Values{"f": {c.Name}, "v": {v}}.Encode()
			if o.access(who).Write {
				k.Add = tableLink(o.Name) + "new?" + url.Values{c.Name: {v}}.Encode()
			}
			p.Children = append(p.Children, k)
		}
	}
	page(w, r, http.StatusOK, "form", p)
}

func (d *data) save(w http.ResponseWriter, r *http.Request) {
	tables, t := d.tableFor(w, r)
	if t == nil {
		return
	}
	who := viewer(r)
	if !t.access(who).Write {
		http.Error(w, "you can't change this", http.StatusForbidden)
		return
	}
	full := *t
	*t = t.forViewer(who) // a column they can't see stays as it is
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	id := r.PathValue("id")
	var names, marks []string
	var args []any
	fields := map[string]any{}
	for _, c := range t.Cols {
		if c.Random && id == "" {
			names = append(names, quote(c.Name))
			marks = append(marks, "?")
			args = append(args, newID())
		}
		if c.Auto {
			continue
		}
		var v any
		if s := r.PostFormValue(c.Name); c.kind() == "checkbox" {
			v = map[bool]int{true: 1, false: 0}[s != ""]
		} else if s != "" {
			v = s
		} else if id == "" && c.HasDefault {
			continue // left blank on a new row: the design's default
		}
		names = append(names, quote(c.Name))
		marks = append(marks, "?")
		args = append(args, v)
		fields[c.Name] = v
	}
	// Who made a new row, whatever was sent, even into a column they can't
	// see; on a change it stays as it was.
	for _, c := range full.Cols {
		if c.Maker && id == "" {
			names = append(names, quote(c.Name))
			marks = append(marks, "?")
			args = append(args, who.Name)
			fields[c.Name] = who.Name
		}
	}
	if !d.allowed(tables, full, who, "write", id, fields) {
		http.Error(w, "you can't change this", http.StatusForbidden)
		return
	}
	var err error
	if d.sync != nil {
		err = d.saveEntry(full, id, fields)
	} else if id == "" {
		_, err = d.db.Exec("INSERT INTO "+quote(t.Name)+" ("+strings.Join(names, ", ")+") VALUES ("+strings.Join(marks, ", ")+")", args...)
	} else {
		sets := make([]string, len(names))
		for i, n := range names {
			sets[i] = n + " = ?"
		}
		_, err = d.db.Exec("UPDATE "+quote(t.Name)+" SET "+strings.Join(sets, ", ")+" WHERE "+quote(t.Key)+" = ?", append(args, id)...)
	}
	if err != nil {
		p := formPage{Name: t.Name, Link: tableLink(t.Name), Action: r.URL.Path, Error: plainError(err),
			Fields: d.fields(tables, *t, viewer(r), r.PostFormValue)}
		page(w, r, http.StatusBadRequest, "form", p)
		return
	}
	http.Redirect(w, r, tableLink(t.Name), http.StatusSeeOther)
}

func (d *data) remove(w http.ResponseWriter, r *http.Request) {
	tables, t := d.tableFor(w, r)
	if t == nil {
		return
	}
	id := r.PathValue("id")
	if !t.access(viewer(r)).Delete || !d.allowed(tables, *t, viewer(r), "delete", id, nil) {
		http.Error(w, "you can't delete this", http.StatusForbidden)
		return
	}
	var err error
	if d.sync != nil {
		err = d.sync.write(entryDelete, t.Name, id, nil)
	} else {
		_, err = d.db.Exec("DELETE FROM "+quote(t.Name)+" WHERE "+quote(t.Key)+" = ?", id)
	}
	if err != nil {
		p := formPage{Name: t.Name, Link: tableLink(t.Name), Action: tableLink(t.Name) + url.PathEscape(id), Error: plainError(err),
			Fields: d.fields(tables, *t, viewer(r), r.PostFormValue)}
		page(w, r, http.StatusConflict, "message", p)
		return
	}
	http.Redirect(w, r, tableLink(t.Name), http.StatusSeeOther)
}

func plainError(err error) string {
	s := err.Error()
	switch {
	case strings.Contains(s, "FOREIGN KEY"):
		return "Other records depend on this, or it refers to one that isn't there."
	case strings.Contains(s, "NOT NULL"):
		return "Please fill in everything that's required."
	case strings.Contains(s, "UNIQUE"):
		return "There's already a record with that value."
	case strings.Contains(s, "CHECK"):
		return "One of those values isn't allowed."
	}
	return "That couldn't be saved."
}

type menuItem struct {
	Title, Href, Icon string
	On                bool
}

type menu struct {
	Site, Name string
	Items      []menuItem
}

// menuFor is the menu for whoever a request is from: none if nobody's
// signed in. The program fills it in (main.go).
var menuFor = func(r *http.Request) menu { return menu{} }

// page writes a page: the style, the menu for whoever's signed in, and the
// page itself.
func page(w http.ResponseWriter, r *http.Request, code int, name string, p any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	dataPages.ExecuteTemplate(w, "head", nil)
	if m := menuFor(r); m.Name != "" {
		dataPages.ExecuteTemplate(w, "menu", m)
	}
	io.WriteString(w, "<main>")
	defer io.WriteString(w, "</main>")
	if err := dataPages.ExecuteTemplate(w, name, p); err != nil {
		log.Printf("page %s: %v", name, err) // the page stops where it went wrong
	}
}

var dataPages = template.Must(template.New("").Funcs(template.FuncMap{"human": human, "tableLink": tableLink, "extraStyle": func() template.CSS { return template.CSS(extraStyle) }, "roles": func() []string { return []string{roleClient, roleStaff, roleAdmin} }, "add": func(a, b int) int { return a + b }}).Parse(`
{{define "head"}}<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<style>
[hidden]{display:none!important}
:root{--ink:#1c1c1c;--soft:#666;--line:#c9c9c9;--bg:#fff;--box:#f4f4f1;--accent:#0b5cad}
@media (prefers-color-scheme:dark){:root{--ink:#eee;--soft:#aaa;--line:#555;--bg:#161616;--box:#232323;--accent:#7db8f0}}
body{font:16px/1.5 system-ui,sans-serif;color:var(--ink);background:var(--bg);max-width:70rem;margin:0 auto;padding:1rem}
a{color:var(--accent)}nav{margin-bottom:1rem}.scroll{overflow-x:auto}
table{border-collapse:collapse;width:100%}th,td{text-align:left;padding:.35rem .6rem;border-bottom:1px solid var(--line);white-space:nowrap}
.num{text-align:right}tfoot td{font-weight:600}.err{color:#c62828}
form.edit{max-width:30rem}form.edit label{display:block;margin-bottom:1rem}
form.edit input:not([type=checkbox]),form.edit select,form.edit textarea{display:block;width:100%;box-sizing:border-box;padding:.5rem;font:inherit}
button{padding:.4rem 1rem;font:inherit}.bar{display:flex;gap:1rem;flex-wrap:wrap;align-items:center;margin:1rem 0}
.told{background:#e7f4ec;color:#14532d;padding:.6rem .8rem;border-radius:6px}
@media (prefers-color-scheme:dark){.told{background:#14321f;color:#c7f0d4}}
.muted{color:var(--soft)}label input[type=checkbox]{margin-right:.4rem}
.choices label{display:block;margin:.35rem 0}.members{padding-left:1.2rem}.members li{margin:.3rem 0}
.members button{padding:.1rem .6rem;font-size:.9em}
svg text{fill:var(--ink);font:13px system-ui,sans-serif}svg .type{fill:var(--soft);font-size:11px}svg .title{font-weight:600}
svg rect{fill:var(--box);stroke:var(--line)}svg path{fill:none;stroke:var(--accent);stroke-width:1.5}svg .end{fill:var(--accent);font-size:12px}
@media print{nav,.bar,form{display:none}}
header.app{display:flex;flex-wrap:wrap;align-items:center;gap:.4rem 1.2rem;padding:.2rem 0 .8rem;margin-bottom:1.2rem;border-bottom:1px solid var(--line)}
header.app .site{font-weight:700;font-size:1.1rem;color:var(--ink);text-decoration:none;margin-right:auto}
nav.menu{display:flex;flex-wrap:wrap;gap:.2rem;margin:0}nav.menu a{color:var(--ink);text-decoration:none;padding:.35rem .7rem;border-radius:6px}
nav.menu a:hover{background:var(--box)}nav.menu a[aria-current]{background:var(--box);font-weight:600;color:var(--accent)}
details.who{position:relative}details.who summary{cursor:pointer;list-style:none;padding:.35rem .7rem;border:1px solid var(--line);border-radius:6px;font-size:.9rem;color:var(--soft)}
details.who summary::-webkit-details-marker{display:none}details.who[open] summary{background:var(--box)}
details.who .drop{position:absolute;right:0;top:2.4rem;z-index:5;background:var(--bg);border:1px solid var(--line);border-radius:8px;padding:.5rem;min-width:13rem;box-shadow:0 8px 16px #0000001a}
details.who .drop a,details.who .drop button{display:block;width:100%;text-align:left;padding:.4rem .6rem;border:0;background:none;font:inherit;color:var(--ink);text-decoration:none;border-radius:6px;cursor:pointer}
details.who .drop a:hover,details.who .drop button:hover{background:var(--box)}
main > nav{font-size:.9rem;color:var(--soft)}
.tiles{display:grid;grid-template-columns:repeat(auto-fit,minmax(180px,1fr));gap:1rem;margin:1.5rem 0}
.tiles a{display:block;padding:1.2rem;border:1px solid var(--line);border-radius:8px;text-decoration:none;color:var(--ink);background:var(--box)}
.tiles a:hover{border-color:var(--accent)}.tiles b{display:block;font-size:1.6rem;margin-bottom:.4rem}
form.signin{max-width:24rem;margin:3rem auto}
{{extraStyle}}</style>{{end}}

{{define "top"}}{{end}}

{{define "menu"}}<header class="app"><a class="site" href="/">{{.Site}}</a>
<nav class="menu" aria-label="Menu">{{range .Items}}<a href="{{.Href}}"{{if .On}} aria-current="page"{{end}}>{{.Title}}</a>{{end}}</nav>
<details class="who"><summary>{{.Name}} ▾</summary><div class="drop">
<a href="` + base + `password">Change your password</a>
<form method="post" action="` + base + `logout"><button>Sign out</button></form></div></details></header>{{end}}

{{define "home"}}<title>{{.Site}}</title>
<h1>Hello, {{.Name}}</h1>
<div class="tiles">{{range .Items}}{{if ne .Href "` + base + `"}}<a href="{{.Href}}"><b>{{.Icon}}</b>{{.Title}}</a>{{end}}{{end}}
<a href="` + base + `password"><b>🔑</b>Change your password</a></div>{{end}}

{{define "login"}}<title>Sign in</title>
<form class="edit signin" method="post" action="` + base + `login"><h1>Sign in</h1>
{{if .}}<p class="err">{{.}}</p>{{end}}
<label>Email <input name="email" type="email" autocomplete="username" required autofocus></label>
<label>Password <input name="password" type="password" autocomplete="current-password" required></label>
<button>Sign in</button></form>{{end}}

{{define "diagram"}}{{template "top"}}<title>Your data</title>
<h1>Your data</h1>
{{range .Machines}}<p>{{.}}</p>{{end}}
{{if .Refused}}<p class="err"><a href="` + dataBase + `_refused">{{.Refused}} to look at</a>: saves that couldn't be merged.</p>{{end}}
{{if .Tables}}<div class="scroll"><svg width="{{.W}}" height="{{.H}}" viewBox="0 0 {{.W}} {{.H}}" role="img" aria-label="Tables and how they relate">
{{range .Lines}}<path d="{{.D}}"/><text class="end" x="{{.OneX}}" y="{{.OneY}}">1</text><text class="end" x="{{.ManyX}}" y="{{.ManyY}}">n</text>{{end}}
{{range .Boxes}}{{$b := .}}<a href="{{.Link}}"><rect x="{{.X}}" y="{{.Y}}" width="220" height="{{.H}}" rx="6"/>
<text class="title" x="{{.X | add 10}}" y="{{.Y | add 19}}">{{.Name}}</text></a>
{{range .Rows}}<text x="{{$b.X | add 10}}" y="{{.Y}}">{{if .Mark}}{{if eq .Mark "key"}}# {{else}}→ {{end}}{{end}}{{.Name}}</text><text class="type" x="{{$b.X | add 150}}" y="{{.Y}}">{{.Type}}</text>{{end}}{{end}}
</svg></div>
<h2>Tables</h2><ul>{{range .Tables}}<li><a href="{{tableLink .Name}}">{{human .Name}}</a></li>{{end}}</ul>
{{else}}<p>There are no tables yet. Write your tables in a .sql file and import it with -import.</p>{{end}}{{end}}

{{define "mine"}}{{template "top"}}<title>Your records</title>
<h1>Your records</h1>
{{if .}}<ul>{{range .}}<li><a href="{{tableLink .Name}}">{{human .Name}}</a></li>{{end}}</ul>{{else}}<p>There's nothing here for you yet.</p>{{end}}{{end}}

{{define "report"}}{{template "top"}}<title>{{human .Name}}</title>
<nav><a href="` + dataBase + `">Your data</a></nav><h1>{{human .Name}}</h1>
{{if .Filter}}<p>{{.Filter}} · <a href="{{.Link}}">Show all</a></p>{{end}}
<div class="bar">{{if .Admin}}<a href="{{.Link}}new">Add</a>{{end}}
<form method="get" action="{{.Link}}"><input name="q" value="{{.Q}}" aria-label="Search"> <button>Search</button></form>
<span>{{.Total}} in all</span><a href="{{.CSV}}">Download CSV</a></div>
{{if .GroupBy}}<p>Totals by: {{range .GroupBy}}<a href="{{.Link}}">{{.Title}}</a> {{end}}</p>{{end}}
{{if .Groups}}<div class="scroll"><table><thead><tr><th></th><th class="num">Count</th>{{range .GroupHeads}}<th class="num">{{.}}</th>{{end}}</tr></thead>
<tbody>{{range .Groups}}<tr><td>{{.Name}}</td><td class="num">{{.Count}}</td>{{range .Sums}}<td class="num">{{.}}</td>{{end}}</tr>{{end}}</tbody></table></div><br>{{end}}
<div class="scroll"><table><thead><tr><th></th>{{range .Heads}}<th{{if .Num}} class="num"{{end}}><a href="{{.Link}}">{{.Title}}</a></th>{{end}}</tr></thead>
<tbody>{{range .Rows}}<tr><td><a href="{{.Link}}">Open</a></td>{{range .Cells}}<td{{if .Num}} class="num"{{end}}>{{if .Link}}<a href="{{.Link}}">{{.Text}}</a>{{else}}{{.Text}}{{end}}</td>{{end}}</tr>{{end}}</tbody>
{{if .HasSums}}<tfoot><tr><td>Total</td>{{range .Sums}}<td{{if .Num}} class="num"{{end}}>{{.Text}}</td>{{end}}</tr></tfoot>{{end}}</table></div>
<div class="bar">{{if .Prev}}<a href="{{.Prev}}">Previous</a>{{end}}{{if .Next}}<a href="{{.Next}}">Next</a>{{end}}</div>{{end}}

{{define "form"}}{{template "top"}}<title>{{human .Name}}</title>
<nav><a href="{{.Link}}">{{human .Name}}</a></nav><h1>{{human .Name}}</h1>
{{if .Error}}<p class="err">{{.Error}}</p>{{end}}
{{if .Shown}}<table>{{range .Shown}}<tr><th>{{index . 0}}</th><td>{{index . 1}}</td></tr>{{end}}</table>{{else}}
<form class="edit" method="post" action="{{.Action}}">{{range .Fields}}<label>{{.Title}}{{if .Required}} *{{end}}
{{if or (eq .Kind "ref") (eq .Kind "choice")}}{{$v := .Value}}<select name="{{.Name}}"{{if .Required}} required{{end}}><option value=""></option>{{range .Options}}<option value="{{index . 0}}"{{if eq (index . 0) $v}} selected{{end}}>{{index . 1}}</option>{{end}}</select>
{{else if eq .Kind "textarea"}}<textarea name="{{.Name}}" rows="5"{{if .Required}} required{{end}}>{{.Value}}</textarea>
{{else if eq .Kind "checkbox"}}<input type="checkbox" name="{{.Name}}" value="1"{{if and .Value (ne .Value "0")}} checked{{end}}>
{{else if eq .Kind "decimal"}}<input type="number" step="any" name="{{.Name}}" value="{{.Value}}"{{if .Required}} required{{end}}>
{{else}}<input type="{{.Kind}}" name="{{.Name}}" value="{{.Value}}"{{if .Required}} required{{end}}>{{end}}{{if .Help}}<small class="muted">{{.Help}}</small>{{end}}</label>{{end}}
<button>Save</button></form>{{end}}
{{if .Delete}}<form method="post" action="{{.Delete}}" onsubmit="return confirm('Delete this?')"><p><button>Delete</button></p></form>{{end}}
{{if .Children}}<h2>Related</h2><ul>{{range .Children}}<li><a href="{{.Link}}">{{.Title}}</a> ({{.Count}}){{if .Add}} · <a href="{{.Add}}">Add</a>{{end}}</li>{{end}}</ul>{{end}}{{end}}

{{define "refused"}}{{template "top"}}<title>To look at</title>
<nav><a href="` + dataBase + `">Your data</a></nav><h1>To look at</h1>
{{if .}}<p>These saves were made on one machine and couldn't be merged with another's. Nothing was lost: each is here until someone deals with it.</p>
<div class="scroll"><table><thead><tr><th>When</th><th>Machine</th><th>Table</th><th>What it tried</th><th>Why not</th><th></th></tr></thead><tbody>
{{range .}}<tr><td>{{.When}}</td><td>{{.Node}}</td><td>{{human .Table}}</td><td>{{range .Fields}}{{.}}<br>{{end}}</td><td>{{.Reason}}</td>
<td><a href="{{.Fix}}">Put it right</a> <form method="post" action="` + dataBase + `_refused" style="display:inline"><input type="hidden" name="node" value="{{.Node}}"><input type="hidden" name="seq" value="{{.Seq}}"><button>Dismiss</button></form></td></tr>{{end}}
</tbody></table></div>{{else}}<p>Nothing to look at.</p>{{end}}{{end}}

{{define "message"}}{{template "top"}}<title>{{human .Name}}</title>
<nav><a href="{{.Action}}">Back</a></nav><p class="err">{{.Error}}</p>{{end}}
`))
