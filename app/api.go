// Package app is the private part of a site on yggstore (docs/app-patterns.md
// and docs/local-apps.md): sign-in, people and their roles, groups,
// documents, the data pages (a diagram, forms and reports, all from the
// site's design.sql, limited by its rules), the log and sync between
// machines, and the daily copy into yggstore.
//
// A site's own program is a few lines:
//
//	package main
//
//	import "github.com/peterretief/yggstore/app"
//
//	func main() { app.Main() }
//
// Pages of its own go beside it, in files that register them with AddPages
// and AddLink, and ask the same checks the data pages do: Viewer for who's
// asking, Table.Access and Table.Rows for what they may do and see,
// Data.Allowed for one row. Their tests start a site with NewTestSite, and
// TestSite.NothingLeaks walks every page as each person to show the rules
// hold. The learning example (an online learning platform, with a
// step-by-step tutorial) shows how.
package app

import (
	"database/sql"
	"html/template"
	"net/http"
	"strings"
)

// The addresses the package's pages answer under.
const (
	Base     = base     // the private part, /app/
	DataBase = dataBase // the data pages
	DocsBase = docsBase // documents
)

// The roles.
const (
	RoleAdmin  = roleAdmin
	RoleStaff  = roleStaff
	RoleClient = roleClient
)

type (
	// App is the running program.
	App = app
	// Person is who's asking: Name (their email), Role, and Admin.
	Person = person
	// Data is the site's data file, with its design and rules.
	Data = data
	// Table is one table of the design, with its columns and rules.
	Table = table
	// Column is one column of a table.
	Column = column
)

// AddPages adds an app's own pages. f is called once, when the program
// starts; wrap each handler in a.SignedIn or a.AdminOnly.
func AddPages(f func(a *App, mux *http.ServeMux)) { ownPages = append(ownPages, f) }

// AddLink puts a link in the menu, and a tile on the home page.
func AddLink(title, href, icon string) {
	ownLinks = append(ownLinks, struct{ Title, Href, Icon string }{title, href, icon})
}

// AddStyle adds CSS to every page.
func AddStyle(css string) { extraStyle += css }

var extraStyle string

// AddTemplates adds an app's own page templates, which Page can then show.
// Call it from an init function. A template can use the functions given, and
// the package's own (human, tableLink).
func AddTemplates(text string, funcs template.FuncMap) {
	template.Must(dataPages.Funcs(funcs).Parse(text))
}

// Page writes a page: the style, the menu and the template called name.
func Page(w http.ResponseWriter, r *http.Request, code int, name string, p any) {
	page(w, r, code, name, p)
}

// Viewer is who a request is from, inside a.SignedIn or a.AdminOnly.
func Viewer(r *http.Request) Person { return viewer(r) }

// SignedIn keeps a page to people signed in; AdminOnly to admins.
func (a *App) SignedIn(h http.HandlerFunc) http.HandlerFunc  { return a.signedIn(h) }
func (a *App) AdminOnly(h http.HandlerFunc) http.HandlerFunc { return a.adminOnly(h) }

// Data is the site's data file: nil without -data.
func (a *App) Data() *Data { return a.data }

// DB is the data file, to read from. Change it through Add, Change and
// Remove, so the log (if there is one) has every change.
func (d *Data) DB() *sql.DB { return d.db }

// Design is the data file's tables, with their rules.
func (d *Data) Design() ([]Table, error) { return d.design() }

// Allowed says whether someone may do an action (read, write, delete) to a
// row: as it is, and for a write, as it would be with the changes.
func (d *Data) Allowed(tables []Table, t Table, p Person, action, id string, changes map[string]any) bool {
	return d.allowed(tables, t, p, action, id, changes)
}

// Find is the table of that name, or nil.
func Find(tables []Table, name string) *Table { return find(tables, name) }

// Access is what a person may do with a table; Rows the rows they may see
// ("" for all of them), and RowsFor those they may write or delete.
func (t Table) Access(p Person) access { return t.access(p) }
func (t Table) Rows(tables []Table, p Person) (cond string, args []any, ok bool) {
	return t.rows(tables, p)
}
func (t Table) RowsFor(tables []Table, p Person, action string) (cond string, args []any, ok bool) {
	return t.rowsFor(tables, p, action)
}

// Quote quotes a name for SQL; NewID is a new row's id; Text is a value as
// text; CleanEmail an email as accounts keep it.
func Quote(name string) string   { return quote(name) }
func NewID() string              { return newID() }
func Text(v any) string          { return text(v) }
func CleanEmail(s string) string { return cleanEmail(s) }

// Add saves a new row and says its id; Change changes fields of one; Remove
// deletes one, and RemoveWhere every row with a column's value. They go
// through the log, if the data has one. They don't check who's asking: an
// app's own page checks first (Allowed, Rows), then changes.
func (d *Data) Add(table string, fields map[string]any) (string, error) {
	id := newID()
	if d.sync != nil {
		return id, d.sync.write(entryRow, table, id, fields)
	}
	names, marks, args := []string{"id"}, []string{"?"}, []any{id}
	for k, v := range fields {
		names = append(names, quote(k))
		marks = append(marks, "?")
		args = append(args, v)
	}
	_, err := d.db.Exec(`INSERT INTO `+quote(table)+` (`+strings.Join(names, ", ")+`) VALUES (`+strings.Join(marks, ", ")+`)`, args...)
	return id, err
}

func (d *Data) Change(table, id string, fields map[string]any) error {
	if d.sync != nil {
		return d.sync.write(entryRow, table, id, fields)
	}
	var sets []string
	var args []any
	for k, v := range fields {
		sets = append(sets, quote(k)+" = ?")
		args = append(args, v)
	}
	_, err := d.db.Exec(`UPDATE `+quote(table)+` SET `+strings.Join(sets, ", ")+` WHERE id = ?`, append(args, id)...)
	return err
}

func (d *Data) Remove(table, id string) error {
	if d.sync != nil {
		return d.sync.write(entryDelete, table, id, nil)
	}
	_, err := d.db.Exec(`DELETE FROM `+quote(table)+` WHERE id = ?`, id)
	return err
}

func (d *Data) RemoveWhere(table, column, value string) error {
	rows, err := d.db.Query(`SELECT id FROM `+quote(table)+` WHERE `+quote(column)+` = ?`, value)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		if err := d.Remove(table, id); err != nil {
			return err
		}
	}
	return nil
}
