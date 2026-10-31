package site

import (
	"database/sql"
	"errors"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/atomicfile"
)

// Notices are short posts the owner writes on their box (news, specials,
// opening hours) for their website's visitors to read. They're kept in
// the site's folder as a SQLite file, published with the site and
// read-only to visitors (an app site can read it as it reads its own
// database), and written into the home page between markers, so a plain
// page shows them too.
const (
	NoticesFile  = "notices.db"
	noticesStart = "<!-- yggstore: notices -->"
	noticesEnd   = "<!-- /yggstore: notices -->"
	// NoticesShown is how many of the newest a home page shows.
	NoticesShown = 10
	maxTitle     = 120
	maxBody      = 2000
)

type Notice struct {
	ID    int64
	At    time.Time
	Title string
	Body  string
}

func openNotices(dir string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(dir, NoticesFile)+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(delete)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS notices (
		id    INTEGER PRIMARY KEY,
		at    TEXT NOT NULL,  -- UTC, 2026-10-31T14:05:00Z
		title TEXT NOT NULL,
		body  TEXT NOT NULL
	)`); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// Notices are the site's notices in dir, newest first.
func Notices(dir string) ([]Notice, error) {
	if _, err := os.Stat(filepath.Join(dir, NoticesFile)); err != nil {
		return nil, nil
	}
	db, err := openNotices(dir)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id, at, title, body FROM notices ORDER BY at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Notice
	for rows.Next() {
		var n Notice
		var at string
		if err := rows.Scan(&n.ID, &at, &n.Title, &n.Body); err != nil {
			return nil, err
		}
		n.At, _ = time.Parse(time.RFC3339, at)
		out = append(out, n)
	}
	return out, rows.Err()
}

// AddNotice adds a notice to the site in dir, and puts it on its home page.
func AddNotice(dir, title, body string, at time.Time) error {
	title, body = strings.TrimSpace(title), strings.TrimSpace(body)
	switch {
	case title == "" && body == "":
		return errors.New("a notice needs a heading or some text")
	case len(title) > maxTitle:
		return fmt.Errorf("a heading is at most %d characters", maxTitle)
	case len(body) > maxBody:
		return fmt.Errorf("a notice is at most %d characters", maxBody)
	}
	db, err := openNotices(dir)
	if err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO notices (at, title, body) VALUES (?, ?, ?)`, at.UTC().Format(time.RFC3339), title, body)
	db.Close()
	if err != nil {
		return err
	}
	return KeepNotices(dir)
}

// RemoveNotice takes a notice off the site in dir.
func RemoveNotice(dir string, id int64) error {
	db, err := openNotices(dir)
	if err != nil {
		return err
	}
	_, err = db.Exec(`DELETE FROM notices WHERE id = ?`, id)
	db.Close()
	if err != nil {
		return err
	}
	return KeepNotices(dir)
}

// KeepNotices makes the home page in dir show the site's notices, after
// the owner replaced the page, say. It only writes when the page differs.
func KeepNotices(dir string) error {
	home := filepath.Join(dir, "index.html")
	b, err := os.ReadFile(home)
	if err != nil {
		return nil // no home page to show them on
	}
	list, err := Notices(dir)
	if err != nil {
		return err
	}
	page := WithNotices(string(b), list)
	if page == string(b) {
		return nil
	}
	return atomicfile.Replace(home, []byte(page), 0o644)
}

// WithNotices puts the newest notices on a home page, after its first
// heading (or at the top of the body), in place of those put there
// before. With none, the section goes.
func WithNotices(page string, list []Notice) string {
	page = withoutMarked(page, noticesStart, noticesEnd)
	if len(list) == 0 {
		return page
	}
	var b strings.Builder
	b.WriteString(noticesStart + "\n<section style=\"margin:1.5em 0\">\n")
	for i, n := range list {
		if i == NoticesShown {
			break
		}
		b.WriteString("<article style=\"margin:0 0 1.2em\">\n")
		if n.Title != "" {
			b.WriteString("<h2 style=\"margin:0 0 .2em;font-size:1.15em\">" + html.EscapeString(n.Title) + "</h2>\n")
		}
		if !n.At.IsZero() {
			b.WriteString("<p style=\"margin:0 0 .4em;font-size:.85em;opacity:.7\">" + n.At.Format("2 January 2006") + "</p>\n")
		}
		if n.Body != "" {
			b.WriteString("<p style=\"margin:0\">" + strings.ReplaceAll(html.EscapeString(n.Body), "\n", "<br>\n") + "</p>\n")
		}
		b.WriteString("</article>\n")
	}
	b.WriteString("</section>\n" + noticesEnd + "\n")
	section := b.String()
	lower := strings.ToLower(page)
	if i := strings.Index(lower, "</h1>"); i >= 0 {
		i += len("</h1>")
		if i < len(page) && page[i] == '\n' {
			i++
		}
		return page[:i] + section + page[i:]
	}
	if i := strings.Index(lower, "<body"); i >= 0 {
		if j := strings.Index(page[i:], ">"); j >= 0 {
			i += j + 1
			if i < len(page) && page[i] == '\n' {
				i++
			}
			return page[:i] + section + page[i:]
		}
	}
	return section + page
}
