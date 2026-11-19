package app

import (
	"database/sql"
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

// With a log, groups travel in it like everything else, so every machine
// has the same groups, and a rule naming one ("allow: group:technicians
// write") means the same everywhere.

func applyGroup(tx *sql.Tx, e entry) error {
	removed, _ := e.Fields["removed"].(bool)
	if e.Kind == entryGroup {
		if removed {
			if _, err := tx.Exec(`DELETE FROM _group_members WHERE grp = ?`, e.Row); err != nil {
				return err
			}
			_, err := tx.Exec(`DELETE FROM _groups WHERE id = ?`, e.Row)
			return err
		}
		_, err := tx.Exec(`INSERT INTO _groups VALUES (?, ?) ON CONFLICT (id) DO UPDATE SET name = excluded.name`, e.Row, e.Fields["name"])
		return err
	}
	grp, _ := e.Fields["group"].(string)
	name, _ := e.Fields["name"].(string)
	if removed {
		_, err := tx.Exec(`DELETE FROM _group_members WHERE grp = ? AND name = ?`, grp, name)
		return err
	}
	_, err := tx.Exec(`INSERT OR IGNORE INTO _group_members SELECT id, ? FROM _groups WHERE id = ?`, name, grp)
	return err
}

// adoptGroups puts groups made before the log into it.
func (l *syncLog) adoptGroups() error {
	if _, err := l.d.db.Exec(groupsTables); err != nil {
		return err
	}
	var groups [][2]string
	var members [][2]string
	rows, err := l.d.db.Query(`SELECT id, name FROM _groups WHERE id NOT IN (SELECT row FROM _sync_entries WHERE kind = ?)`, entryGroup)
	if err != nil {
		return err
	}
	for rows.Next() {
		var g [2]string
		rows.Scan(&g[0], &g[1])
		groups = append(groups, g)
	}
	rows.Close()
	rows, err = l.d.db.Query(`SELECT grp, name FROM _group_members WHERE grp || '/' || name NOT IN (SELECT row FROM _sync_entries WHERE kind = ?)`, entryMember)
	if err != nil {
		return err
	}
	for rows.Next() {
		var m [2]string
		rows.Scan(&m[0], &m[1])
		members = append(members, m)
	}
	rows.Close()
	for _, g := range groups {
		if err := l.write(entryGroup, "", g[0], map[string]any{"name": g[1]}); err != nil {
			return err
		}
	}
	for _, m := range members {
		if err := l.write(entryMember, "", m[0]+"/"+m[1], map[string]any{"group": m[0], "name": m[1]}); err != nil {
			return err
		}
	}
	return nil
}

// groupName is how rules name a group: "Sign technicians" is
// group:sign_technicians.
func groupName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else if s := b.String(); s != "" && !strings.HasSuffix(s, "_") {
			b.WriteByte('_')
		}
	}
	return strings.TrimSuffix(b.String(), "_")
}

// groupsOf is the groups a person is in, as rules name them.
func groupsOf(db *sql.DB, name string) []string {
	rows, err := db.Query(`SELECT g.name FROM _groups g JOIN _group_members m ON m.grp = g.id WHERE m.name = ?`, name)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			out = append(out, groupName(n))
		}
	}
	return out
}

// groupMembers is everyone in the group a rule names.
func groupMembers(db *sql.DB, rule string) []string {
	rows, err := db.Query(`SELECT g.name, m.name FROM _groups g JOIN _group_members m ON m.grp = g.id`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var g, n string
		if rows.Scan(&g, &n) == nil && groupName(g) == rule {
			out = append(out, n)
		}
	}
	return out
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
	var l *syncLog
	if a.data != nil {
		l = a.data.sync
	}
	err := func() error {
		if l != nil { // through the log, to every machine
			switch r.PostFormValue("do") {
			case "new":
				n := strings.Join(strings.Fields(r.PostFormValue("name")), " ")
				if n == "" || len(n) > 100 {
					return errors.New("a group needs a name")
				}
				var taken int
				db.QueryRow(`SELECT COUNT(*) FROM _groups WHERE name = ?`, n).Scan(&taken)
				if taken > 0 {
					return errors.New("there's already a group called " + n)
				}
				return l.write(entryGroup, "", newID(), map[string]any{"name": n})
			case "add":
				if a.accountRole(name) == "" {
					return errors.New("there's no account for " + name)
				}
				return l.write(entryMember, "", id+"/"+name, map[string]any{"group": id, "name": name})
			case "remove":
				return l.write(entryMember, "", id+"/"+name, map[string]any{"group": id, "name": name, "removed": true})
			case "delete":
				db.Exec(`DELETE FROM _docs_access WHERE name = ?`, groupKey+id)
				return l.write(entryGroup, "", id, map[string]any{"removed": true})
			}
			return errors.New("nothing to do")
		}
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
