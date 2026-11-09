package app

import (
	"errors"
	"net/http"
	"sort"
	"strings"
)

// Groups of people, such as a client's staff: share a document with a group
// and everyone in it sees it, and whoever joins later does too. In the
// documents' sharing a group is "group:" and its id, beside people's emails.
// (The type is circle: group is already a report's total, in data.go.)

const (
	groupsTables = `
CREATE TABLE IF NOT EXISTS _groups (id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE);
CREATE TABLE IF NOT EXISTS _group_members (grp TEXT NOT NULL, name TEXT NOT NULL, PRIMARY KEY (grp, name));`
	groupKey = "group:"
	everyone = "*" // an admin's key: they see everything
)

type circle struct {
	ID, Name string
	Members  []string
}

func (d *docs) groups() ([]circle, error) {
	rows, err := d.db.Query(`SELECT g.id, g.name, m.name FROM _groups g LEFT JOIN _group_members m ON m.grp = g.id ORDER BY g.name, m.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []circle
	for rows.Next() {
		var id, name string
		var member *string
		if err := rows.Scan(&id, &name, &member); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].ID != id {
			out = append(out, circle{ID: id, Name: name})
		}
		if member != nil {
			out[len(out)-1].Members = append(out[len(out)-1].Members, *member)
		}
	}
	return out, rows.Err()
}

// keys are what a document can be shared with that reach this person: their
// email and their groups. An admin's include everyone.
func (d *docs) keys(p person) map[string]bool {
	if p.Admin {
		return map[string]bool{everyone: true}
	}
	if p.Name == "" {
		return nil
	}
	keys := map[string]bool{p.Name: true}
	rows, err := d.db.Query(`SELECT grp FROM _group_members WHERE name = ?`, p.Name)
	if err != nil {
		return keys
	}
	defer rows.Close()
	for rows.Next() {
		var g string
		if rows.Scan(&g) == nil {
			keys[groupKey+g] = true
		}
	}
	return keys
}

// changeGroups is the People page's groups: make, add to, take from, delete.
func (a *app) changeGroups(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if a.docs == nil {
		http.NotFound(w, r)
		return
	}
	db := a.docs.db
	id, name := r.PostFormValue("group"), cleanEmail(r.PostFormValue("email"))
	err := func() error {
		switch r.PostFormValue("do") {
		case "new":
			n := strings.Join(strings.Fields(r.PostFormValue("name")), " ")
			if n == "" || len(n) > 100 {
				return errors.New("a group needs a name")
			}
			if _, err := db.Exec(`INSERT INTO _groups VALUES (?, ?)`, newID(), n); err != nil {
				return errors.New("there's already a group called " + n)
			}
		case "add":
			if a.accountRole(name) == "" {
				return errors.New("there's no account for " + name)
			}
			if _, err := db.Exec(`INSERT OR IGNORE INTO _group_members SELECT id, ? FROM _groups WHERE id = ?`, name, id); err != nil {
				return err
			}
		case "remove":
			if _, err := db.Exec(`DELETE FROM _group_members WHERE grp = ? AND name = ?`, id, name); err != nil {
				return err
			}
		case "delete":
			for _, q := range []string{`DELETE FROM _docs_access WHERE name = ?`} {
				if _, err := db.Exec(q, groupKey+id); err != nil {
					return err
				}
			}
			if _, err := db.Exec(`DELETE FROM _group_members WHERE grp = ?`, id); err != nil {
				return err
			}
			if _, err := db.Exec(`DELETE FROM _groups WHERE id = ?`, id); err != nil {
				return err
			}
		default:
			return errors.New("nothing to do")
		}
		return nil
	}()
	if err != nil {
		a.showPeople(w, r, http.StatusBadRequest, peoplePage{Error: "That couldn't be done: " + err.Error() + "."})
		return
	}
	http.Redirect(w, r, base+"people#groups", http.StatusSeeOther)
}

// sharedWith is who a document is shared with, as people read it.
func sharedWith(grants []string, groups []circle) string {
	names := map[string]string{}
	for _, g := range groups {
		names[groupKey+g.ID] = g.Name
	}
	var out []string
	for _, k := range grants {
		if n, ok := names[k]; ok {
			out = append(out, n+" (group)")
		} else if !strings.HasPrefix(k, groupKey) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}
