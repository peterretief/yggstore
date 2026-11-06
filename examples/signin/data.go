package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"encoding/csv"
	"fmt"
	"html/template"
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
	Ref        *ref
	Choices    []string
}

type table struct {
	Name  string
	Cols  []column
	Key   string // what identifies a row
	Label string // what a row is called in drop-downs and reports
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
	case strings.Contains(strings.ToLower(c.Name), "email"):
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
	rows, err := d.db.Query(`SELECT name, sql FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite%' AND name NOT LIKE '\_sync%' ESCAPE '\' ORDER BY name`)
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
	t.Label = t.Key
	for _, c := range t.Cols {
		if k := c.kind(); (k == "text" || k == "email") && !c.Auto {
			t.Label = c.Name
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

func (d *data) routes(mux *http.ServeMux, in func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET "+dataBase+"{$}", in(d.diagram))
	if d.sync != nil {
		mux.HandleFunc("GET "+dataBase+"_refused", in(d.refusedPage))
		mux.HandleFunc("POST "+dataBase+"_refused", in(d.dismiss))
	}
	mux.HandleFunc("GET "+dataBase+"{table}/{$}", in(d.report))
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
	p := layout(tables)
	p.Refused = d.refusedCount()
	p.Machines = d.machines()
	page(w, http.StatusOK, "diagram", p)
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
	p := find(tables, r.Table)
	if p == nil {
		return nil, nil
	}
	rows, err := d.db.Query(fmt.Sprintf(`SELECT %s, %s FROM %s ORDER BY 2 LIMIT %d`, quote(r.Column), quote(p.Label), quote(r.Table), maxPick))
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
		filter = human(field) + ": " + value
		if c.Ref != nil {
			if _, labels := d.picks(tables, *c.Ref); labels[value] != "" {
				filter = human(field) + ": " + labels[value]
			}
		}
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
			out.Link = tableLink(c.Ref.Table) + url.PathEscape(s)
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
	p := reportPage{Name: t.Name, Link: tableLink(t.Name), Q: q, By: by, Filter: filter, Total: total, CSV: link(map[string]string{"csv": "1", "page": ""})}
	for _, c := range cols {
		h := head{Name: c.Name, Title: human(c.Name), Num: c.summed()}
		flip := ""
		if order == c.Name && !desc {
			flip = "1"
		}
		h.Link = link(map[string]string{"sort": c.Name, "desc": flip, "page": ""})
		p.Heads = append(p.Heads, h)
		if c.Ref != nil || c.Choices != nil || c.kind() == "checkbox" {
			p.GroupBy = append(p.GroupBy, head{Name: c.Name, Title: human(c.Name), Link: link(map[string]string{"by": c.Name})})
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
	if c := t.col(by); c != nil {
		d.groups(&p, *t, *c, from, args, show)
	}
	page(w, http.StatusOK, "report", p)
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
	Name, Title, Kind, Value string
	Required                 bool
	Options                  [][2]string
}

type child struct {
	Table, Title, Link, Add string
	Count                   int64
}

type formPage struct {
	Name, Link, Action, Delete, Error string
	Fields                            []field
	Children                          []child
}

func (d *data) fields(tables []table, t table, values func(string) string) []field {
	var out []field
	for _, c := range t.Cols {
		if c.Auto {
			continue
		}
		f := field{Name: c.Name, Title: human(c.Name), Kind: c.kind(), Value: values(c.Name), Required: c.Required}
		switch f.Kind {
		case "ref":
			f.Options, _ = d.picks(tables, *c.Ref)
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
	p := formPage{Name: t.Name, Link: tableLink(t.Name), Action: tableLink(t.Name) + "new"}
	if id == "" {
		p.Fields = d.fields(tables, *t, r.URL.Query().Get) // a link may fill some in
		page(w, http.StatusOK, "form", p)
		return
	}
	vals := make([]any, len(t.Cols))
	ptrs := make([]any, len(vals))
	var names []string
	for i, c := range t.Cols {
		ptrs[i] = &vals[i]
		names = append(names, quote(c.Name))
	}
	err := d.db.QueryRow("SELECT "+strings.Join(names, ", ")+" FROM "+quote(t.Name)+" WHERE "+quote(t.Key)+" = ?", id).Scan(ptrs...)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	got := map[string]string{}
	for i, c := range t.Cols {
		got[c.Name] = text(vals[i])
	}
	p.Fields = d.fields(tables, *t, func(n string) string { return got[n] })
	p.Action = tableLink(t.Name) + url.PathEscape(id)
	p.Delete = p.Action + "/delete"
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
			d.db.QueryRow("SELECT COUNT(*) FROM "+quote(o.Name)+" WHERE "+quote(c.Name)+" = ?", v).Scan(&k.Count)
			k.Link = tableLink(o.Name) + "?" + url.Values{"f": {c.Name}, "v": {v}}.Encode()
			k.Add = tableLink(o.Name) + "new?" + url.Values{c.Name: {v}}.Encode()
			p.Children = append(p.Children, k)
		}
	}
	page(w, http.StatusOK, "form", p)
}

func (d *data) save(w http.ResponseWriter, r *http.Request) {
	tables, t := d.tableFor(w, r)
	if t == nil {
		return
	}
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
		}
		names = append(names, quote(c.Name))
		marks = append(marks, "?")
		args = append(args, v)
		fields[c.Name] = v
	}
	var err error
	if d.sync != nil {
		err = d.saveEntry(*t, id, fields)
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
			Fields: d.fields(tables, *t, r.PostFormValue)}
		page(w, http.StatusBadRequest, "form", p)
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
	var err error
	if d.sync != nil {
		err = d.sync.write(entryDelete, t.Name, id, nil)
	} else {
		_, err = d.db.Exec("DELETE FROM "+quote(t.Name)+" WHERE "+quote(t.Key)+" = ?", id)
	}
	if err != nil {
		p := formPage{Name: t.Name, Link: tableLink(t.Name), Action: tableLink(t.Name) + url.PathEscape(id), Error: plainError(err),
			Fields: d.fields(tables, *t, r.PostFormValue)}
		page(w, http.StatusConflict, "message", p)
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

func page(w http.ResponseWriter, code int, name string, p any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	dataPages.ExecuteTemplate(w, name, p)
}

var dataPages = template.Must(template.New("").Funcs(template.FuncMap{"human": human, "tableLink": tableLink, "add": func(a, b int) int { return a + b }}).Parse(`
{{define "top"}}<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<style>
:root{--ink:#1c1c1c;--soft:#666;--line:#c9c9c9;--bg:#fff;--box:#f4f4f1;--accent:#0b5cad}
@media (prefers-color-scheme:dark){:root{--ink:#eee;--soft:#aaa;--line:#555;--bg:#161616;--box:#232323;--accent:#7db8f0}}
body{font:16px/1.5 system-ui,sans-serif;color:var(--ink);background:var(--bg);max-width:70rem;margin:0 auto;padding:1rem}
a{color:var(--accent)}nav{margin-bottom:1rem}.scroll{overflow-x:auto}
table{border-collapse:collapse;width:100%}th,td{text-align:left;padding:.35rem .6rem;border-bottom:1px solid var(--line);white-space:nowrap}
.num{text-align:right}tfoot td{font-weight:600}.err{color:#c62828}
form.edit{max-width:30rem}form.edit label{display:block;margin-bottom:1rem}
form.edit input:not([type=checkbox]),form.edit select,form.edit textarea{display:block;width:100%;box-sizing:border-box;padding:.5rem;font:inherit}
button{padding:.4rem 1rem;font:inherit}.bar{display:flex;gap:1rem;flex-wrap:wrap;align-items:center;margin:1rem 0}
svg text{fill:var(--ink);font:13px system-ui,sans-serif}svg .type{fill:var(--soft);font-size:11px}svg .title{font-weight:600}
svg rect{fill:var(--box);stroke:var(--line)}svg path{fill:none;stroke:var(--accent);stroke-width:1.5}svg .end{fill:var(--accent);font-size:12px}
@media print{nav,.bar,form{display:none}}
</style>{{end}}

{{define "diagram"}}{{template "top"}}<title>Your data</title>
<nav><a href="` + base + `">Home</a></nav><h1>Your data</h1>
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

{{define "report"}}{{template "top"}}<title>{{human .Name}}</title>
<nav><a href="` + dataBase + `">Your data</a></nav><h1>{{human .Name}}</h1>
{{if .Filter}}<p>{{.Filter}} · <a href="{{.Link}}">Show all</a></p>{{end}}
<div class="bar"><a href="{{.Link}}new">Add</a>
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
<form class="edit" method="post" action="{{.Action}}">{{range .Fields}}<label>{{.Title}}{{if .Required}} *{{end}}
{{if or (eq .Kind "ref") (eq .Kind "choice")}}{{$v := .Value}}<select name="{{.Name}}"{{if .Required}} required{{end}}><option value=""></option>{{range .Options}}<option value="{{index . 0}}"{{if eq (index . 0) $v}} selected{{end}}>{{index . 1}}</option>{{end}}</select>
{{else if eq .Kind "textarea"}}<textarea name="{{.Name}}" rows="5"{{if .Required}} required{{end}}>{{.Value}}</textarea>
{{else if eq .Kind "checkbox"}}<input type="checkbox" name="{{.Name}}" value="1"{{if and .Value (ne .Value "0")}} checked{{end}}>
{{else if eq .Kind "decimal"}}<input type="number" step="any" name="{{.Name}}" value="{{.Value}}"{{if .Required}} required{{end}}>
{{else}}<input type="{{.Kind}}" name="{{.Name}}" value="{{.Value}}"{{if .Required}} required{{end}}>{{end}}</label>{{end}}
<button>Save</button></form>
{{if .Delete}}<form method="post" action="{{.Delete}}" onsubmit="return confirm('Delete this?')"><p><button>Delete</button></p></form>{{end}}
{{if .Children}}<h2>Related</h2><ul>{{range .Children}}<li><a href="{{.Link}}">{{.Title}}</a> ({{.Count}}) · <a href="{{.Add}}">Add</a></li>{{end}}</ul>{{end}}{{end}}

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
