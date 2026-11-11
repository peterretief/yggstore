package app

import (
	"crypto/rand"
	"errors"
	"html/template"
	"net/http"
	"strings"
)

// People: an admin adds someone by email and passes on the temporary
// password the page shows once; everyone can change their own password.

type account struct {
	Name, Role string
}

type peoplePage struct {
	Me      string
	People  []account
	Groups  []circle
	HasDocs bool
	Told    string // a temporary password to pass on, shown once
	ToldFor string
	Error   string
}

// tempPassword is easy to read out over the phone: no 0/O or 1/l.
func tempPassword() string {
	const letters = "abcdefghjkmnpqrstuvwxyz23456789"
	b := make([]byte, 12)
	rand.Read(b)
	var s strings.Builder
	for i, c := range b {
		if i > 0 && i%4 == 0 {
			s.WriteByte('-')
		}
		s.WriteByte(letters[int(c)%len(letters)])
	}
	return s.String()
}

func (a *app) listAccounts() ([]account, error) {
	rows, err := a.accountsDB().Query(`SELECT name, role FROM ` + a.accounts() + ` ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []account
	for rows.Next() {
		var x account
		if err := rows.Scan(&x.Name, &x.Role); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (a *app) accountRole(name string) string {
	var role string
	a.accountsDB().QueryRow(`SELECT role FROM `+a.accounts()+` WHERE name = ?`, name).Scan(&role)
	return role
}

// setAccount saves an account, in the log if there is one.
func (a *app) setAccount(name, hash, role string) error {
	if a.data != nil && a.data.sync != nil {
		return a.data.sync.write(entryAccount, "", name, map[string]any{"hash": hash, "role": role})
	}
	_, err := a.db.Exec(`INSERT INTO accounts (name, hash, role) VALUES (?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET hash = excluded.hash, role = excluded.role`, name, hash, role)
	return err
}

func (a *app) accountHash(name string) string {
	var hash string
	a.accountsDB().QueryRow(`SELECT hash FROM `+a.accounts()+` WHERE name = ?`, name).Scan(&hash)
	return hash
}

// removeAccount takes away an account, its sessions and what was shared with it.
func (a *app) removeAccount(name string) error {
	var err error
	if a.data != nil && a.data.sync != nil {
		err = a.data.sync.write(entryAccount, "", name, map[string]any{"removed": true})
	} else {
		_, err = a.db.Exec(`DELETE FROM accounts WHERE name = ?`, name)
	}
	if err != nil {
		return err
	}
	a.db.Exec(`DELETE FROM sessions WHERE name = ?`, name)
	if a.docs != nil {
		a.docs.db.Exec(`DELETE FROM _docs_access WHERE name = ?`, name)
		a.docs.db.Exec(`DELETE FROM _group_members WHERE name = ?`, name)
	}
	return nil
}

func (a *app) peoplePage(w http.ResponseWriter, r *http.Request) {
	a.showPeople(w, r, http.StatusOK, peoplePage{})
}

func (a *app) showPeople(w http.ResponseWriter, r *http.Request, code int, p peoplePage) {
	var err error
	if p.People, err = a.listAccounts(); err != nil {
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}
	p.Me = viewer(r).Name
	if a.docs != nil {
		p.HasDocs = true
		if p.Groups, err = a.docs.groups(); err != nil {
			http.Error(w, "something went wrong", http.StatusInternalServerError)
			return
		}
	}
	page(w, r, code, "people", p)
}

func (a *app) changePeople(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	who := viewer(r)
	name := cleanEmail(r.PostFormValue("email"))
	role := r.PostFormValue("role")
	if role != roleAdmin && role != roleStaff {
		role = roleClient
	}
	fail := func(msg string) { a.showPeople(w, r, http.StatusBadRequest, peoplePage{Error: msg}) }
	if !strings.Contains(name, "@") || strings.ContainsAny(name, " <>\"") || len(name) > 200 || strings.HasPrefix(name, groupKey) {
		fail("That isn't an email address.")
		return
	}
	exists := a.accountRole(name) != ""
	switch r.PostFormValue("do") {
	case "add":
		if exists {
			fail(name + " already has an account. To give them a new password, use Reset.")
			return
		}
		pw := tempPassword()
		if err := a.setAccount(name, hashPassword(pw), role); err != nil {
			fail("That couldn't be saved.")
			return
		}
		a.showPeople(w, r, http.StatusOK, peoplePage{Told: pw, ToldFor: name})
		return
	case "reset":
		if !exists {
			fail("There's no account for " + name + ".")
			return
		}
		pw := tempPassword()
		if err := a.setAccount(name, hashPassword(pw), a.accountRole(name)); err != nil {
			fail("That couldn't be saved.")
			return
		}
		a.db.Exec(`DELETE FROM sessions WHERE name = ?`, name)
		a.showPeople(w, r, http.StatusOK, peoplePage{Told: pw, ToldFor: name})
		return
	case "role":
		if name == who.Name {
			fail("You can't change your own role: ask another admin.")
			return
		}
		if !exists {
			fail("There's no account for " + name + ".")
			return
		}
		if err := a.setAccount(name, a.accountHash(name), role); err != nil {
			fail("That couldn't be saved.")
			return
		}
	case "remove":
		if name == who.Name {
			fail("You can't remove yourself: ask another admin.")
			return
		}
		if err := a.removeAccount(name); err != nil {
			fail("That couldn't be done.")
			return
		}
	default:
		fail("Nothing to do.")
		return
	}
	http.Redirect(w, r, base+"people", http.StatusSeeOther)
}

type passwordPage struct{ Error, Done string }

func (a *app) passwordPage(w http.ResponseWriter, r *http.Request) {
	page(w, r, http.StatusOK, "password", passwordPage{})
}

func (a *app) changePassword(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	who := viewer(r)
	now, next := r.PostFormValue("current"), r.PostFormValue("new")
	err := func() error {
		if a.tooMany("name:" + who.Name) {
			return errors.New("Too many tries. Try again later.")
		}
		if !matches(a.accountHash(who.Name), now) {
			a.wrong("name:" + who.Name)
			return errors.New("Your current password isn't right.")
		}
		if len(next) < 10 {
			return errors.New("The new password needs at least 10 characters.")
		}
		if next != r.PostFormValue("again") {
			return errors.New("The two new passwords aren't the same.")
		}
		return a.setAccount(who.Name, hashPassword(next), a.accountRole(who.Name))
	}()
	if err != nil {
		page(w, r, http.StatusBadRequest, "password", passwordPage{Error: err.Error()})
		return
	}
	page(w, r, http.StatusOK, "password", passwordPage{Done: "Your password is changed."})
}

func init() {
	template.Must(dataPages.Parse(`
{{define "people"}}{{template "top"}}<title>People</title>
<h1>People</h1>
{{if .Error}}<p class="err">{{.Error}}</p>{{end}}
{{if .Told}}<p class="told">The temporary password for <b>{{.ToldFor}}</b> is <code>{{.Told}}</code>. Give it to them by phone or in person, not by email. It isn't shown again.</p>{{end}}
<h2>Add someone</h2>
<form class="edit" method="post" action="` + base + `people"><input type="hidden" name="do" value="add">
<label>Email <input name="email" type="email" required></label>
<label>Role <select name="role"><option value="client">Client: sees only their own, and what's shared with them</option><option value="staff">Staff: sees and changes what the design allows staff</option><option value="admin">Admin: sees and changes everything</option></select></label>
<button>Add</button></form>
<h2>Everyone</h2>
<div class="scroll" tabindex="0" role="region" aria-label="Everyone"><table><thead><tr><th>Email</th><th>Role</th><th></th></tr></thead><tbody>
{{$me := .Me}}{{range $i, $p := .People}}<tr><td>{{.Name}}{{if eq .Name $me}} (you){{end}}</td><td data-label="Role">{{.Role}}</td><td>
<form method="post" action="` + base + `people" style="display:inline"><input type="hidden" name="email" value="{{.Name}}"><input type="hidden" name="do" value="reset"><button>Reset password</button></form>
{{if ne .Name $me}}<form method="post" action="` + base + `people" style="display:inline"><input type="hidden" name="email" value="{{.Name}}"><input type="hidden" name="do" value="role"><select name="role" aria-label="Role for {{.Name}}">{{$r := .Role}}{{range $x := roles}}<option value="{{$x}}"{{if eq $x $r}} selected{{end}}>{{$x}}</option>{{end}}</select> <button>Change role</button></form>
<button type="button" commandfor="remove-{{$i}}" command="show-modal">Remove</button>
<dialog id="remove-{{$i}}"><p>Remove {{.Name}}? They can't sign in any more.</p><form class="bar" method="post" action="` + base + `people"><input type="hidden" name="email" value="{{.Name}}"><input type="hidden" name="do" value="remove"><button type="button" commandfor="remove-{{$i}}" command="close">Cancel</button><button>Remove</button></form></dialog>{{end}}
</td></tr>{{end}}</tbody></table></div>
{{if .HasDocs}}<h2 id="groups">Groups</h2>
<p class="muted">Share a document with a group, and everyone in it sees it, including whoever joins later.</p>
{{$people := .People}}{{range .Groups}}{{$g := .}}<h3>{{.Name}}</h3>
{{if .Members}}<ul class="members">{{range .Members}}<li>{{.}} <form method="post" action="` + base + `groups" style="display:inline"><input type="hidden" name="do" value="remove"><input type="hidden" name="group" value="{{$g.ID}}"><input type="hidden" name="email" value="{{.}}"><button>Take out</button></form></li>{{end}}</ul>{{else}}<p class="muted">Nobody yet.</p>{{end}}
<form method="post" action="` + base + `groups" style="display:inline"><input type="hidden" name="do" value="add"><input type="hidden" name="group" value="{{.ID}}">
<select name="email" aria-label="Someone to add">{{range $people}}<option value="{{.Name}}">{{.Name}}</option>{{end}}</select> <button>Add to {{.Name}}</button></form>
<button type="button" commandfor="delete-group-{{.ID}}" command="show-modal">Delete group</button>
<dialog id="delete-group-{{.ID}}"><p>Delete the group {{.Name}}? Its people stay, and lose only what was shared with the group.</p><form class="bar" method="post" action="` + base + `groups"><input type="hidden" name="do" value="delete"><input type="hidden" name="group" value="{{.ID}}"><button type="button" commandfor="delete-group-{{.ID}}" command="close">Cancel</button><button>Delete group</button></form></dialog>{{end}}
<form class="edit" method="post" action="` + base + `groups"><input type="hidden" name="do" value="new">
<label>New group <input name="name" required maxlength="100"></label><button>Make it</button></form>{{end}}{{end}}

{{define "password"}}{{template "top"}}<title>Your password</title>
<h1>Your password</h1>
{{if .Error}}<p class="err">{{.Error}}</p>{{end}}{{if .Done}}<p class="told">{{.Done}}</p>{{end}}
<form class="edit" method="post" action="` + base + `password">
<label>Current password <input name="current" type="password" autocomplete="current-password" required></label>
<label>New password, at least 10 characters <input name="new" type="password" autocomplete="new-password" minlength="10" required></label>
<label>New password again <input name="again" type="password" autocomplete="new-password" minlength="10" required></label>
<button>Change it</button></form>{{end}}
`))
}
