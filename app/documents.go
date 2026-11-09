package app

import (
	"database/sql"
	"errors"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Documents: admins upload files and whole folders and share any of them
// with chosen people; sharing a folder shares what's in it. Everyone else
// sees only what's been shared with them. The files stay in one folder on
// this machine, each under its id, and the list of them is in the data file.
// With -copy-to, each one goes into yggstore a minute or so after it's
// uploaded, as does the data file (copy.go).
//
// Uploads come through the web nodes, so each file can be at most
// maxDocument (the most a request through them may carry).

const (
	docsBase    = base + "docs/"
	maxDocument = 10<<20 - 64<<10 // under the web nodes' limit, with room for the form
)

const docsTables = `
CREATE TABLE IF NOT EXISTS _docs (id TEXT PRIMARY KEY, parent TEXT NOT NULL, name TEXT NOT NULL,
  folder INTEGER NOT NULL, size INTEGER NOT NULL DEFAULT 0, by TEXT NOT NULL, at INTEGER NOT NULL, UNIQUE (parent, name));
CREATE TABLE IF NOT EXISTS _docs_access (doc TEXT NOT NULL, name TEXT NOT NULL, PRIMARY KEY (doc, name));` + groupsTables

type docs struct {
	db  *sql.DB
	dir string
}

type doc struct {
	ID, Parent, Name, By string
	Folder               bool
	Size, At             int64
}

func openDocs(db *sql.DB, dir string) (*docs, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if _, err := db.Exec(docsTables); err != nil {
		return nil, err
	}
	return &docs{db: db, dir: dir}, nil
}

// validDocName is a file or folder name that can't climb out or hide.
func validDocName(s string) bool {
	if s == "" || s == "." || s == ".." || len(s) > 200 || strings.ContainsAny(s, `/\`) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func (d *docs) all() (map[string]doc, error) {
	rows, err := d.db.Query(`SELECT id, parent, name, folder, size, by, at FROM _docs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]doc{}
	for rows.Next() {
		var x doc
		if err := rows.Scan(&x.ID, &x.Parent, &x.Name, &x.Folder, &x.Size, &x.By, &x.At); err != nil {
			return nil, err
		}
		out[x.ID] = x
	}
	return out, rows.Err()
}

func (d *docs) grants() (map[string][]string, error) {
	rows, err := d.db.Query(`SELECT doc, name FROM _docs_access ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = append(out[id], name)
	}
	return out, rows.Err()
}

// docVisible says whether a document reaches someone, given their keys
// (docs.keys): an admin always, anyone else if it, or a folder it's in, was
// shared with them or with a group they're in.
func docVisible(all map[string]doc, grants map[string][]string, keys map[string]bool, id string) bool {
	if keys[everyone] {
		return true
	}
	for seen := 0; id != "" && seen < len(all)+1; seen++ {
		for _, k := range grants[id] {
			if keys[k] {
				return true
			}
		}
		id = all[id].Parent
	}
	return false
}

// child finds a document by name in a folder.
func (d *docs) child(parent, name string) (doc, bool) {
	var x doc
	err := d.db.QueryRow(`SELECT id, parent, name, folder, size, by, at FROM _docs WHERE parent = ? AND name = ?`, parent, name).
		Scan(&x.ID, &x.Parent, &x.Name, &x.Folder, &x.Size, &x.By, &x.At)
	return x, err == nil
}

func (d *docs) folder(parent, name, by string) (string, error) {
	if !validDocName(name) {
		return "", errors.New("that isn't a name a folder can have")
	}
	if x, ok := d.child(parent, name); ok {
		if !x.Folder {
			return "", errors.New("there's already a file called " + name)
		}
		return x.ID, nil
	}
	id := newID()
	_, err := d.db.Exec(`INSERT INTO _docs (id, parent, name, folder, by, at) VALUES (?, ?, ?, 1, ?, ?)`, id, parent, name, by, time.Now().Unix())
	return id, err
}

// store saves an upload at path (folders made as needed) in parent. A file
// of the same name is replaced, and keeps who it's shared with.
func (d *docs) store(parent, path, by string, r io.Reader) error {
	parts := strings.Split(strings.Trim(filepath.ToSlash(path), "/"), "/")
	for _, p := range parts {
		if !validDocName(p) {
			return errors.New(path + " isn't a name a file can have")
		}
	}
	for _, p := range parts[:len(parts)-1] {
		var err error
		if parent, err = d.folder(parent, p, by); err != nil {
			return err
		}
	}
	name := parts[len(parts)-1]
	id := newID()
	old, replace := d.child(parent, name)
	if replace {
		if old.Folder {
			return errors.New("there's already a folder called " + name)
		}
		id = old.ID
	}
	tmp, err := os.CreateTemp(d.dir, ".upload-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	size, err := io.Copy(tmp, io.LimitReader(r, maxDocument+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if size > maxDocument {
		return errors.New(name + " is bigger than " + strconv.Itoa(maxDocument>>20) + " MB")
	}
	if err := os.Rename(tmp.Name(), filepath.Join(d.dir, id)); err != nil {
		return err
	}
	defer nudgeCopy() // into yggstore soon, not at the end of the day
	if replace {
		_, err = d.db.Exec(`UPDATE _docs SET size = ?, by = ?, at = ? WHERE id = ?`, size, by, time.Now().Unix(), id)
	} else {
		_, err = d.db.Exec(`INSERT INTO _docs (id, parent, name, folder, size, by, at) VALUES (?, ?, ?, 0, ?, ?, ?)`, id, parent, name, size, by, time.Now().Unix())
	}
	return err
}

// remove takes away a document, and everything in it if it's a folder. What
// was stored in yggstore stays there, as earlier copies of the data do.
func (d *docs) remove(all map[string]doc, id string) error {
	defer nudgeCopy()
	for _, x := range all {
		if x.Parent == id {
			if err := d.remove(all, x.ID); err != nil {
				return err
			}
		}
	}
	if _, err := d.db.Exec(`DELETE FROM _docs_access WHERE doc = ?`, id); err != nil {
		return err
	}
	if _, err := d.db.Exec(`DELETE FROM _docs WHERE id = ?`, id); err != nil {
		return err
	}
	if !all[id].Folder {
		os.Remove(filepath.Join(d.dir, id))
	}
	return nil
}

func (d *docs) routes(mux *http.ServeMux, in, admin func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET "+docsBase+"{$}", in(d.list))
	mux.HandleFunc("GET "+docsBase+"f/{id}", in(d.list))
	mux.HandleFunc("GET "+docsBase+"d/{id}", in(d.download))
	mux.HandleFunc("POST "+docsBase+"upload", admin(d.upload))
	mux.HandleFunc("POST "+docsBase+"folder", admin(d.newFolder))
	mux.HandleFunc("POST "+docsBase+"delete", admin(d.delete))
	mux.HandleFunc("GET "+docsBase+"share/{id}", admin(d.sharePage))
	mux.HandleFunc("POST "+docsBase+"share/{id}", admin(d.share))
}

type docItem struct {
	Link, Name, Size, When, Shared, ID string
	Folder                             bool
}

type docsPage struct {
	Admin    bool
	Here     string // the folder's id, "" at the top
	Title    string
	Crumbs   [][2]string // link, name
	Items    []docItem
	Error    string
	MaxMB    int
	Everyone bool
}

func folderLink(id string) string {
	if id == "" {
		return docsBase
	}
	return docsBase + "f/" + url.PathEscape(id)
}

func sizeText(n int64) string {
	switch {
	case n < 1<<10:
		return strconv.FormatInt(n, 10) + " B"
	case n < 1<<20:
		return strconv.FormatInt(n>>10, 10) + " KB"
	}
	return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MB"
}

func (d *docs) list(w http.ResponseWriter, r *http.Request) {
	d.show(w, r, http.StatusOK, "")
}

func (d *docs) show(w http.ResponseWriter, r *http.Request, code int, msg string) {
	who := viewer(r)
	all, err := d.all()
	grants, err2 := d.grants()
	if err != nil || err2 != nil {
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}
	keys := d.keys(who)
	here := r.PathValue("id")
	if here == "" {
		here = r.PostFormValue("parent")
	}
	if here != "" && (!all[here].Folder || !docVisible(all, grants, keys, here)) {
		http.NotFound(w, r)
		return
	}
	groups, _ := d.groups()
	p := docsPage{Admin: who.Admin, Here: here, Title: "Documents", Error: msg, MaxMB: maxDocument >> 20}
	for id := here; id != ""; id = all[id].Parent {
		if !docVisible(all, grants, keys, id) {
			break
		}
		p.Crumbs = append([][2]string{{folderLink(id), all[id].Name}}, p.Crumbs...)
	}
	if here != "" {
		p.Title = all[here].Name
	}
	for _, x := range all {
		show := x.Parent == here && docVisible(all, grants, keys, x.ID)
		// At the top, someone who isn't an admin sees what was shared with
		// them, wherever it is.
		if here == "" && !who.Admin {
			show = docVisible(all, grants, keys, x.ID) && !docVisible(all, grants, keys, x.Parent)
		}
		if !show {
			continue
		}
		it := docItem{ID: x.ID, Name: x.Name, Folder: x.Folder, When: time.Unix(x.At, 0).Format("2 Jan 2006")}
		if x.Folder {
			it.Link = folderLink(x.ID)
		} else {
			it.Link, it.Size = docsBase+"d/"+url.PathEscape(x.ID), sizeText(x.Size)
		}
		if who.Admin {
			it.Shared = sharedWith(grants[x.ID], groups)
		}
		p.Items = append(p.Items, it)
	}
	sort.Slice(p.Items, func(i, j int) bool {
		if p.Items[i].Folder != p.Items[j].Folder {
			return p.Items[i].Folder
		}
		return strings.ToLower(p.Items[i].Name) < strings.ToLower(p.Items[j].Name)
	})
	page(w, r, code, "docs", p)
}

func (d *docs) download(w http.ResponseWriter, r *http.Request) {
	all, err := d.all()
	grants, err2 := d.grants()
	id := r.PathValue("id")
	x, ok := all[id]
	if err != nil || err2 != nil || !ok || x.Folder || !docVisible(all, grants, d.keys(viewer(r)), id) {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(filepath.Join(d.dir, id))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	// Always a download, never a page of this site: a file can't run here.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(x.Name))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, "", time.Unix(x.At, 0), f)
}

func (d *docs) upload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxDocument+64<<10)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		d.answer(w, r, errors.New("that's more than "+strconv.Itoa(maxDocument>>20)+" MB at once: send fewer files, or smaller ones"))
		return
	}
	defer r.MultipartForm.RemoveAll()
	parent := r.FormValue("parent")
	all, err := d.all()
	if err != nil {
		d.answer(w, r, err)
		return
	}
	if parent != "" && !all[parent].Folder {
		d.answer(w, r, errors.New("that folder isn't there"))
		return
	}
	files, paths := r.MultipartForm.File["file"], r.MultipartForm.Value["path"]
	if len(files) == 0 {
		d.answer(w, r, errors.New("choose a file or a folder first"))
		return
	}
	for i, fh := range files {
		path := fh.Filename
		if i < len(paths) && paths[i] != "" {
			path = paths[i] // where it was in the folder that was chosen
		}
		f, err := fh.Open()
		if err == nil {
			err = d.store(parent, path, viewer(r).Name, f)
			f.Close()
		}
		if err != nil {
			d.answer(w, r, err)
			return
		}
	}
	d.answer(w, r, nil)
}

// answer is the end of a change: the folder again, or for the page's own
// script (which sends files one at a time) a word.
func (d *docs) answer(w http.ResponseWriter, r *http.Request, err error) {
	if r.Header.Get("X-Upload") != "" {
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Write([]byte("ok"))
		return
	}
	if err != nil {
		d.show(w, r, http.StatusBadRequest, "That couldn't be done: "+err.Error()+".")
		return
	}
	http.Redirect(w, r, folderLink(r.FormValue("parent")), http.StatusSeeOther)
}

func (d *docs) newFolder(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	parent := r.PostFormValue("parent")
	all, err := d.all()
	if err == nil && parent != "" && !all[parent].Folder {
		err = errors.New("that folder isn't there")
	}
	if err == nil {
		_, err = d.folder(parent, strings.TrimSpace(r.PostFormValue("name")), viewer(r).Name)
	}
	d.answer(w, r, err)
}

func (d *docs) delete(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	all, err := d.all()
	id := r.PostFormValue("id")
	if _, ok := all[id]; err == nil && !ok {
		err = errors.New("it isn't there")
	}
	if err == nil {
		err = d.remove(all, id)
	}
	d.answer(w, r, err)
}

type sharePage struct {
	Doc       doc
	Back      string
	Groups    []shareRow
	People    []shareRow
	Inherited []string // who sees it through a folder it's in
}

type shareRow struct {
	Key, Name string // Key is what's saved: an email, or groupKey and a group's id
	On        bool
}

// shareWith is filled in by the program: everyone who could be given access.
var shareWith func() ([]account, error)

func (d *docs) sharePage(w http.ResponseWriter, r *http.Request) {
	all, err := d.all()
	grants, err2 := d.grants()
	x, ok := all[r.PathValue("id")]
	if err != nil || err2 != nil || !ok {
		http.NotFound(w, r)
		return
	}
	people, err := shareWith()
	if err != nil {
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}
	groups, err := d.groups()
	if err != nil {
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}
	shared := map[string]bool{}
	for _, k := range grants[x.ID] {
		shared[k] = true
	}
	p := sharePage{Doc: x, Back: folderLink(x.Parent)}
	for _, g := range groups {
		label := g.Name
		if len(g.Members) > 0 {
			label += " (" + strings.Join(g.Members, ", ") + ")"
		} else {
			label += " (nobody yet)"
		}
		p.Groups = append(p.Groups, shareRow{groupKey + g.ID, label, shared[groupKey+g.ID]})
	}
	for _, a := range people {
		if a.Role == roleAdmin {
			continue // admins see everything anyway
		}
		on := false
		for _, n := range grants[x.ID] {
			on = on || n == a.Name
		}
		p.People = append(p.People, shareRow{a.Name, a.Name, on})
		if !on && docVisible(all, grants, d.keys(person{Name: a.Name}), x.Parent) {
			p.Inherited = append(p.Inherited, a.Name)
		}
	}
	page(w, r, http.StatusOK, "share", p)
}

func (d *docs) share(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	all, err := d.all()
	x, ok := all[r.PathValue("id")]
	if err != nil || !ok {
		http.NotFound(w, r)
		return
	}
	people, err := shareWith()
	if err != nil {
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}
	r.ParseForm()
	chosen := map[string]bool{}
	for _, n := range r.PostForm["name"] {
		chosen[n] = true
	}
	tx, err := d.db.Begin()
	if err != nil {
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	groups, err := d.groups()
	if err != nil {
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}
	tx.Exec(`DELETE FROM _docs_access WHERE doc = ?`, x.ID)
	for _, g := range groups { // only real groups, whatever the form says
		if chosen[groupKey+g.ID] {
			if _, err := tx.Exec(`INSERT INTO _docs_access VALUES (?, ?)`, x.ID, groupKey+g.ID); err != nil {
				http.Error(w, "something went wrong", http.StatusInternalServerError)
				return
			}
		}
	}
	for _, a := range people { // only real accounts, whatever the form says
		if chosen[a.Name] && a.Role != roleAdmin {
			if _, err := tx.Exec(`INSERT INTO _docs_access VALUES (?, ?)`, x.ID, a.Name); err != nil {
				http.Error(w, "something went wrong", http.StatusInternalServerError)
				return
			}
		}
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, folderLink(x.Parent), http.StatusSeeOther)
}

func init() {
	template.Must(dataPages.Parse(`
{{define "docs"}}{{template "top"}}<title>{{.Title}}</title>
<nav><a href="` + base + `">Home</a> · <a href="` + docsBase + `">Documents</a>{{range .Crumbs}} / <a href="{{index . 0}}">{{index . 1}}</a>{{end}}</nav>
<h1>{{.Title}}</h1>
{{if .Error}}<p class="err">{{.Error}}</p>{{end}}
{{if .Items}}<div class="scroll"><table><thead><tr><th>Name</th><th>Size</th><th>Added</th>{{if .Admin}}<th>Shared with</th><th></th>{{end}}</tr></thead><tbody>
{{$admin := .Admin}}{{$here := .Here}}{{range .Items}}<tr><td>{{if .Folder}}📁 {{end}}<a href="{{.Link}}">{{.Name}}</a></td><td>{{.Size}}</td><td>{{.When}}</td>
{{if $admin}}<td>{{.Shared}}</td><td><a href="` + docsBase + `share/{{.ID}}">Share</a>
<form method="post" action="` + docsBase + `delete" style="display:inline" onsubmit="return confirm('Delete {{.Name}}{{if .Folder}} and everything in it{{end}}?')"><input type="hidden" name="id" value="{{.ID}}"><input type="hidden" name="parent" value="{{$here}}"><button>Delete</button></form></td>{{end}}</tr>{{end}}
</tbody></table></div>{{else}}<p>{{if .Admin}}Nothing here yet.{{else}}Nothing has been shared with you here yet.{{end}}</p>{{end}}
{{if .Admin}}
<h2>Add</h2>
<form id="upload" class="edit" method="post" action="` + docsBase + `upload" enctype="multipart/form-data">
<input type="hidden" name="parent" value="{{.Here}}">
<label>Files <input type="file" name="file" multiple></label>
<label>Or a whole folder <input type="file" id="folder" webkitdirectory multiple></label>
<button>Upload</button> <span id="progress" class="muted">Up to {{.MaxMB}} MB a file.</span></form>
<form class="edit" method="post" action="` + docsBase + `folder"><input type="hidden" name="parent" value="{{.Here}}">
<label>New folder <input name="name" required></label><button>Make it</button></form>
<script>
// Files go one at a time, each with where it was in the chosen folder.
document.getElementById("upload").addEventListener("submit", async (e) => {
  e.preventDefault();
  const form = e.target, out = document.getElementById("progress");
  const files = [...form.querySelector("[name=file]").files, ...document.getElementById("folder").files];
  if (!files.length) { out.textContent = "Choose a file or a folder first."; return; }
  for (const [i, f] of files.entries()) {
    out.textContent = "Sending " + (i + 1) + " of " + files.length + ": " + f.name;
    const body = new FormData();
    body.append("parent", form.parent.value);
    body.append("file", f);
    body.append("path", f.webkitRelativePath || f.name);
    const res = await fetch(form.action, { method: "POST", body, headers: { "X-Upload": "1" } });
    if (!res.ok) { out.textContent = f.name + ": " + (await res.text()); return; }
  }
  location.reload();
});
</script>{{end}}{{end}}

{{define "share"}}{{template "top"}}<title>Share {{.Doc.Name}}</title>
<nav><a href="{{.Back}}">Back</a></nav><h1>Share {{.Doc.Name}}</h1>
<p>{{if .Doc.Folder}}Whoever you tick sees this folder and everything in it.{{else}}Whoever you tick can download this file.{{end}} Admins see everything anyway.</p>
{{if .Inherited}}<p class="muted">Already see it through a folder it's in: {{range $i, $n := .Inherited}}{{if $i}}, {{end}}{{$n}}{{end}}.</p>{{end}}
{{if or .People .Groups}}<form class="choices" method="post" action="` + docsBase + `share/{{.Doc.ID}}">
{{if .Groups}}<h2>Groups</h2>{{range .Groups}}
<label><input type="checkbox" name="name" value="{{.Key}}"{{if .On}} checked{{end}}> {{.Name}}</label>{{end}}{{end}}
{{if .People}}<h2>People</h2>{{range .People}}
<label><input type="checkbox" name="name" value="{{.Key}}"{{if .On}} checked{{end}}> {{.Name}}</label>{{end}}{{end}}
<p><button>Save</button></p></form>{{else}}<p>There's nobody to share with yet: add people on the <a href="` + base + `people">People</a> page.</p>{{end}}{{end}}
`))
}
