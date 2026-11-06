// Command signin is a starting point for a local app (docs/local-apps.md):
// sign-in and a private page, run on your own machine behind a published
// site. Accounts are in a SQLite file beside it and never leave the machine.
//
//	go run ./examples/signin -add alice@example.org   # asks for a password
//	go run ./examples/signin                          # listens on 127.0.0.1:3000
//
// The site's folder says which paths come here, in .yggstore.json:
//
//	{ "local": { "/app/": "http://127.0.0.1:3000" } }
//
// Anything private has to come from here, after session says who's asking:
// a published page is public, whatever a form in front of it says.
package main

import (
	"bufio"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver"
	_ "github.com/ncruces/go-sqlite3/embed"
)

const (
	cookieName = "session"
	base       = "/app/" // the path in .yggstore.json
	sessionFor = 14 * 24 * time.Hour
	rounds     = 600_000
	maxTries   = 5 // wrong tries, per address and per email, in tryWindow
	tryWindow  = 15 * time.Minute
)

type app struct {
	db    *sql.DB
	data  *data // nil without -data
	mu    sync.Mutex
	tries map[string][]time.Time
}

func main() {
	addr := flag.String("addr", "127.0.0.1:3000", "where to listen: keep it on this machine")
	dbPath := flag.String("db", "signin.db", "the accounts file")
	add := flag.String("add", "", "add an account by its email, or set its password, and stop")
	dataPath := flag.String("data", "", "a data file to show, with its diagram, forms and reports")
	design := flag.String("import", "", "make the data file match this .sql file, and stop")
	copyTo := flag.String("copy-to", "", "a folder in the yggstore outbox for a daily copy of the data")
	var copyAlso []string
	flag.Func("copy-also", "copy-to: a file to copy too, such as the design or the site's .yggstore.json (can be repeated)", func(s string) error {
		copyAlso = append(copyAlso, s)
		return nil
	})
	copyNow := flag.Bool("copy-now", false, "copy-to: make a copy now, and stop")
	machine := flag.String("machine", "", "keep a log of the data's changes, under this machine's name")
	keyPath := flag.String("sync-key", "", "the key your machines share")
	newKey := flag.String("sync-newkey", "", "make a key your machines can share, and stop")
	site := flag.String("site", "", "the site this program serves, for -sync-with")
	var syncAddrs []string
	flag.Func("sync-with", "another machine's node, as [ADDRESS]:7400 (can be repeated)", func(s string) error {
		syncAddrs = append(syncAddrs, s)
		return nil
	})
	drop := flag.Bool("drop", false, "import: also remove tables and columns the .sql file no longer has")
	flag.Parse()

	if *newKey != "" {
		if err := newSyncKey(*newKey); err != nil {
			log.Fatal(err)
		}
		fmt.Println("made", *newKey, "- copy it to your other machines by hand")
		return
	}
	db, err := sql.Open("sqlite3", "file:"+*dbPath+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(wal)")
	if err != nil {
		log.Fatal(err)
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS accounts (name TEXT PRIMARY KEY, hash TEXT NOT NULL);
		CREATE TABLE IF NOT EXISTS sessions (token TEXT PRIMARY KEY, name TEXT NOT NULL, ends INTEGER NOT NULL);`); err != nil {
		log.Fatal(err)
	}
	if *design != "" {
		if *dataPath == "" {
			log.Fatal("-import needs -data, the data file")
		}
		if err := importDesign(*dataPath, *design, *drop, os.Stdout); err != nil {
			log.Fatal(err)
		}
		d, err := openData(*dataPath)
		if err != nil {
			log.Fatal(err)
		}
		if name := syncMachine(d.db); name != "" {
			l, err := openSync(d, name) // takes in rows from the file
			if err == nil {
				err = l.rebuild()
			}
			if err == nil {
				err = l.noteDesign()
			}
			if err != nil {
				log.Fatal(err)
			}
		}
		return
	}
	sources := []source{{db, filepath.Base(*dbPath)}}
	var dataFile *data
	if *dataPath != "" {
		if dataFile, err = openData(*dataPath); err != nil {
			log.Fatal(err)
		}
		sources = append(sources, source{dataFile.db, filepath.Base(*dataPath)})
		name := *machine
		if name == "" {
			name = syncMachine(dataFile.db)
		}
		if name != "" {
			if dataFile.sync, err = openSync(dataFile, name); err != nil {
				log.Fatal(err)
			}
			if err := dataFile.sync.adoptAccounts(db); err != nil {
				log.Fatal(err)
			}
		}
	}
	if *add != "" {
		var l *syncLog
		if dataFile != nil {
			l = dataFile.sync
		}
		if err := addAccount(db, l, *add); err != nil {
			log.Fatal(err)
		}
		fmt.Println("saved", *add)
		return
	}
	if *copyNow {
		if *copyTo == "" {
			log.Fatal("-copy-now needs -copy-to")
		}
		if err := copyAll(sources, copyAlso, *copyTo); err != nil {
			log.Fatal(err)
		}
		fmt.Println("copied to", *copyTo)
		return
	}
	if host, _, err := net.SplitHostPort(*addr); err != nil || !isLocal(host) {
		log.Fatalf("%s isn't on this machine only", *addr)
	}
	if *keyPath != "" {
		if dataFile == nil || dataFile.sync == nil {
			log.Fatal("-sync-key needs -data and -machine")
		}
		key, err := readSyncKey(*keyPath)
		if err != nil {
			log.Fatal(err)
		}
		dataFile.keep = &keeper{key: key, seen: map[string]string{}}
		if len(syncAddrs) > 0 {
			if *site == "" {
				log.Fatal("-sync-with needs -site")
			}
			go dataFile.syncWith(*site, syncAddrs)
		}
	}
	a := &app{db: db, data: dataFile, tries: map[string][]time.Time{}}
	if *copyTo != "" {
		go keepCopies(sources, copyAlso, *copyTo)
	}
	srv := &http.Server{Addr: *addr, Handler: a.routes(), ReadHeaderTimeout: 10 * time.Second}
	log.Printf("listening on %s", *addr)
	log.Fatal(srv.ListenAndServe())
}

func isLocal(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || ip != nil && ip.IsLoopback()
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+base+"{$}", a.private)
	mux.HandleFunc("GET "+base+"login", a.loginPage)
	mux.HandleFunc("POST "+base+"login", a.login)
	mux.HandleFunc("POST "+base+"logout", a.logout)
	mux.HandleFunc("GET "+base+"me", a.me)
	if a.data != nil {
		a.data.routes(mux, a.signedIn)
		if a.data.keep != nil {
			mux.HandleFunc("POST "+syncPath, a.data.serveSync)
		}
	}
	return mux
}

// session is who's asking: "" if nobody signed in.
func (a *app) session(r *http.Request) string {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return ""
	}
	var name string
	err = a.db.QueryRow(`SELECT name FROM sessions WHERE token = ? AND ends > ?`, hashToken(c.Value), time.Now().Unix()).Scan(&name)
	if err != nil {
		return ""
	}
	return name
}

// signedIn keeps a page to people signed in.
func (a *app) signedIn(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.session(r) == "" {
			http.Redirect(w, r, base+"login", http.StatusSeeOther)
			return
		}
		h(w, r)
	}
}

// private is your private page: replace it with your own. Every handler
// with something private in it starts like this one.
func (a *app) private(w http.ResponseWriter, r *http.Request) {
	name := a.session(r)
	if name == "" {
		http.Redirect(w, r, base+"login", http.StatusSeeOther)
		return
	}
	render(w, http.StatusOK, privatePage, struct {
		Name string
		Data bool
	}{name, a.data != nil})
}

func (a *app) me(w http.ResponseWriter, r *http.Request) {
	name := a.session(r)
	if name == "" {
		http.Error(w, "not signed in", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintln(w, name)
}

func (a *app) loginPage(w http.ResponseWriter, r *http.Request) {
	render(w, http.StatusOK, loginPage, "")
}

func (a *app) login(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	name, pw := cleanEmail(r.PostFormValue("email")), r.PostFormValue("password")
	ip := r.Header.Get("X-Forwarded-For")
	if a.tooMany(ip) || a.tooMany("name:"+name) {
		render(w, http.StatusTooManyRequests, loginPage, "Too many tries. Try again later.")
		return
	}
	var hash string
	err := a.accountsDB().QueryRow(`SELECT hash FROM `+a.accounts()+` WHERE name = ?`, name).Scan(&hash)
	if ok := matches(hash, pw); err != nil || !ok { // checked either way, to take as long
		a.wrong(ip)
		a.wrong("name:" + name)
		render(w, http.StatusUnauthorized, loginPage, "That email or password isn't right.")
		return
	}
	b := make([]byte, 32)
	rand.Read(b)
	token := hex.EncodeToString(b)
	ends := time.Now().Add(sessionFor)
	a.db.Exec(`DELETE FROM sessions WHERE ends <= ?`, time.Now().Unix())
	if _, err := a.db.Exec(`INSERT INTO sessions VALUES (?, ?, ?)`, hashToken(token), name, ends.Unix()); err != nil {
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}
	setCookie(w, r, token, ends)
	http.Redirect(w, r, base, http.StatusSeeOther)
}

func (a *app) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		a.db.Exec(`DELETE FROM sessions WHERE token = ?`, hashToken(c.Value))
	}
	setCookie(w, r, "", time.Unix(1, 0))
	http.Redirect(w, r, base+"login", http.StatusSeeOther)
}

// setCookie keeps the session to this site's local paths. Secure, except
// in yggstore site dev, which isn't https.
func setCookie(w http.ResponseWriter, r *http.Request, value string, ends time.Time) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: value, Path: base, Expires: ends, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: r.Header.Get("X-Forwarded-Proto") == "https"})
}

func (a *app) tooMany(key string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	var recent []time.Time
	for _, t := range a.tries[key] {
		if time.Since(t) < tryWindow {
			recent = append(recent, t)
		}
	}
	if len(recent) == 0 {
		delete(a.tries, key)
		return false
	}
	a.tries[key] = recent
	return len(recent) >= maxTries
}

func (a *app) wrong(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tries[key] = append(a.tries[key], time.Now())
}

// cleanEmail is an email as accounts are kept: Alice@Example.org is
// alice@example.org.
func cleanEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// With a log, accounts are in it, so they're the same on every machine.
func (a *app) accountsDB() *sql.DB {
	if a.data != nil && a.data.sync != nil {
		return a.data.db
	}
	return a.db
}

func (a *app) accounts() string {
	if a.data != nil && a.data.sync != nil {
		return "_sync_accounts"
	}
	return "accounts"
}

func addAccount(db *sql.DB, l *syncLog, email string) error {
	name := cleanEmail(email)
	if !strings.Contains(name, "@") {
		return errors.New("an account is an email address")
	}
	fmt.Print("Password (at least 10 characters): ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	pw := strings.TrimRight(line, "\r\n")
	if err != nil && pw == "" {
		return err
	}
	if len(pw) < 10 {
		return errors.New("at least 10 characters")
	}
	hash := hashPassword(pw)
	if l != nil {
		return l.write(entryAccount, "", name, map[string]any{"hash": hash})
	}
	_, err = db.Exec(`INSERT INTO accounts VALUES (?, ?) ON CONFLICT (name) DO UPDATE SET hash = excluded.hash`, name, hash)
	return err
}

func hashToken(t string) string {
	s := sha256.Sum256([]byte(t))
	return hex.EncodeToString(s[:])
}

func derive(pw string, salt []byte) []byte {
	k, _ := pbkdf2.Key(sha256.New, pw, salt, rounds, 32)
	return k
}

func hashPassword(pw string) string {
	salt := make([]byte, 16)
	rand.Read(salt)
	return hex.EncodeToString(salt) + "$" + hex.EncodeToString(derive(pw, salt))
}

// matches always does the work, even for an account that isn't there.
func matches(hash, pw string) bool {
	s, k, _ := strings.Cut(hash, "$")
	salt, err1 := hex.DecodeString(s)
	want, err2 := hex.DecodeString(k)
	got := derive(pw, salt)
	return err1 == nil && err2 == nil && len(want) > 0 && subtle.ConstantTimeCompare(got, want) == 1
}

func render(w http.ResponseWriter, code int, t *template.Template, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	t.Execute(w, data)
}

const style = `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<style>body{font:16px/1.5 system-ui,sans-serif;max-width:24rem;margin:3rem auto;padding:0 1rem}
label,input,button{display:block;width:100%;box-sizing:border-box}input{padding:.5rem;margin:.25rem 0 1rem}
button{padding:.5rem}.err{color:#b00020}</style>`

var loginPage = template.Must(template.New("login").Parse(style + `<title>Sign in</title><h1>Sign in</h1>
{{if .}}<p class="err">{{.}}</p>{{end}}
<form method="post" action="` + base + `login">
<label>Email <input name="email" type="email" autocomplete="username" required autofocus></label>
<label>Password <input name="password" type="password" autocomplete="current-password" required></label>
<button>Sign in</button></form>`))

var privatePage = template.Must(template.New("private").Parse(style + `<title>Private</title><h1>Hello, {{.Name}}</h1>
<p>Only people signed in see this page. It came from your own machine.</p>
{{if .Data}}<p><a href="` + dataBase + `">Your data</a></p>{{end}}
<form method="post" action="` + base + `logout"><button>Sign out</button></form>`))
