package app

import (
	"bufio"
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
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
	docs  *docs // nil without -data
	mu    sync.Mutex
	tries map[string][]time.Time
}

// Main runs an app: flags, sign-in, people, documents, the data pages and
// any pages added with AddPages, until it's stopped.
func Main() {
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
	flag.Func("sync-with", "another machine's node, as [ADDRESS]:7400, or in development another copy, as http://127.0.0.1:PORT (can be repeated)", func(s string) error {
		syncAddrs = append(syncAddrs, s)
		return nil
	})
	syncEveryFlag := flag.Duration("sync-every", syncEvery, "how often to ask the other machines for what's new")
	check := flag.Bool("check", false, "import: only say what it would do")
	docsDir := flag.String("docs", "app-documents", "with -data: the folder the uploaded documents are kept in")
	asAdmin := flag.Bool("admin", false, "add: the account can see and change everything")
	asStaff := flag.Bool("staff", false, "add: the account is staff: it may do what the design allows staff")
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
	// Accounts from before there were roles could see everything, and still can.
	if !*check {
		db.Exec(`ALTER TABLE accounts ADD COLUMN role TEXT NOT NULL DEFAULT 'admin'`)
	}
	if *design != "" {
		if *dataPath == "" {
			log.Fatal("-import needs -data, the data file")
		}
		if *check {
			if err := checkDesign(*dataPath, *design, os.Stdout); err != nil {
				log.Fatal(err)
			}
			return
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
		role := roleClient
		if *asStaff {
			role = roleStaff
		}
		if *asAdmin {
			role = roleAdmin
		}
		if err := addAccount(db, l, *add, role); err != nil {
			log.Fatal(err)
		}
		fmt.Println("saved", *add, "as", role)
		return
	}
	if dataFile != nil && *copyTo != "" {
		copyAlso = append(copyAlso, *docsDir) // file by file: see copyFolder
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
			for _, a := range syncAddrs {
				if *site == "" && !strings.HasPrefix(a, "http://") {
					log.Fatal("-sync-with a node needs -site")
				}
			}
			go dataFile.syncWith(*site, syncAddrs, *syncEveryFlag)
		}
	}
	a := &app{db: db, data: dataFile, tries: map[string][]time.Time{}}
	if dataFile != nil {
		if a.docs, err = openDocs(dataFile.db, *docsDir); err != nil {
			log.Fatal(err)
		}
	}
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
	mux.HandleFunc("GET "+base+"{$}", a.signedIn(a.private))
	mux.HandleFunc("GET "+base+"login", a.loginPage)
	menuFor = a.menu
	mux.HandleFunc("POST "+base+"login", a.login)
	mux.HandleFunc("POST "+base+"logout", a.logout)
	mux.HandleFunc("GET "+base+"me", a.me)
	mux.HandleFunc("GET "+base+"people", a.adminOnly(a.peoplePage))
	mux.HandleFunc("POST "+base+"people", a.adminOnly(a.changePeople))
	mux.HandleFunc("POST "+base+"groups", a.adminOnly(a.changeGroups))
	mux.HandleFunc("GET "+base+"password", a.signedIn(a.passwordPage))
	mux.HandleFunc("POST "+base+"password", a.signedIn(a.changePassword))
	shareWith = a.listAccounts
	if a.docs != nil {
		a.docs.routes(mux, a.signedIn, a.adminOnly)
	}
	for _, add := range ownPages {
		add(a, mux)
	}
	if a.data != nil {
		a.data.routes(mux, a.signedIn, a.adminOnly)
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

// Who can do what. An admin sees and changes everything. A client sees only
// what's theirs (see scope), and changes nothing.
const (
	roleAdmin  = "admin"
	roleClient = "client"
)

type person struct {
	Name  string
	Role  string // roleAdmin, roleStaff or roleClient
	Admin bool
}

type viewerKey struct{}

// viewer is who a request is from: nobody, and no admin, if it doesn't say.
func viewer(r *http.Request) person {
	p, _ := r.Context().Value(viewerKey{}).(person)
	return p
}

// person is who's asking: the zero person if nobody's signed in. Their role
// is read each time, so a change takes effect at once.
func (a *app) person(r *http.Request) person {
	name := a.session(r)
	if name == "" {
		return person{}
	}
	var role string
	a.accountsDB().QueryRow(`SELECT role FROM `+a.accounts()+` WHERE name = ?`, name).Scan(&role)
	if role != roleAdmin && role != roleStaff {
		role = roleClient // anything else is the least
	}
	return person{Name: name, Role: role, Admin: role == roleAdmin}
}

// ownPages are an app's own pages: a file adds its own with
//
//	func init() { ownPages = append(ownPages, func(a *app, mux *http.ServeMux) { ... }) }
//
// and wraps each in a.signedIn or a.adminOnly, then asks table.access and
// table.rows inside, as the data pages do.
var ownPages []func(a *app, mux *http.ServeMux)

// ownLinks are links an app's own pages put on the home page at /app/.
var ownLinks []struct{ Title, Href, Icon string }

// signedIn keeps a page to people signed in.
func (a *app) signedIn(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := a.person(r)
		if p.Name == "" {
			http.Redirect(w, r, base+"login", http.StatusSeeOther)
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), viewerKey{}, p)))
	}
}

// adminOnly keeps a page to admins.
func (a *app) adminOnly(h http.HandlerFunc) http.HandlerFunc {
	return a.signedIn(func(w http.ResponseWriter, r *http.Request) {
		if !viewer(r).Admin {
			http.Error(w, "that's for admins", http.StatusForbidden)
			return
		}
		h(w, r)
	})
}

// menu is what someone signed in may go to, the page they're on marked.
func (a *app) menu(r *http.Request) menu {
	who := viewer(r)
	if who.Name == "" {
		return menu{}
	}
	m := menu{Site: strings.Split(r.Host, ":")[0], Name: who.Name}
	if m.Site == "" {
		m.Site = "Home"
	}
	m.Items = append(m.Items, menuItem{Title: "Home", Href: base, Icon: "🏠"})
	for _, l := range ownLinks {
		m.Items = append(m.Items, menuItem{Title: l.Title, Href: l.Href, Icon: l.Icon})
	}
	if a.docs != nil {
		m.Items = append(m.Items, menuItem{Title: "Documents", Href: docsBase, Icon: "📁"})
	}
	if a.data != nil {
		title := "Your data"
		if who.Role == roleClient {
			title = "Your records"
		}
		m.Items = append(m.Items, menuItem{Title: title, Href: dataBase, Icon: "🗂️"})
	}
	if who.Admin {
		m.Items = append(m.Items, menuItem{Title: "People", Href: base + "people", Icon: "👥"})
	}
	// The page they're on: the longest link its address starts with.
	on := -1
	for i, it := range m.Items {
		if (r.URL.Path == it.Href || it.Href != base && strings.HasPrefix(r.URL.Path, it.Href)) && (on < 0 || len(it.Href) > len(m.Items[on].Href)) {
			on = i
		}
	}
	if on >= 0 {
		m.Items[on].On = true
	}
	return m
}

// private is your private page: replace it with your own. Every handler
// with something private in it starts like this one.
func (a *app) private(w http.ResponseWriter, r *http.Request) {
	page(w, r, http.StatusOK, "home", a.menu(r))
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
	page(w, r, http.StatusOK, "login", "")
}

func (a *app) login(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	name, pw := cleanEmail(r.PostFormValue("email")), r.PostFormValue("password")
	ip := r.Header.Get("X-Forwarded-For")
	if a.tooMany(ip) || a.tooMany("name:"+name) {
		page(w, r, http.StatusTooManyRequests, "login", "Too many tries. Try again later.")
		return
	}
	var hash string
	err := a.accountsDB().QueryRow(`SELECT hash FROM `+a.accounts()+` WHERE name = ?`, name).Scan(&hash)
	if ok := matches(hash, pw); err != nil || !ok { // checked either way, to take as long
		a.wrong(ip)
		a.wrong("name:" + name)
		page(w, r, http.StatusUnauthorized, "login", "That email or password isn't right.")
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

func addAccount(db *sql.DB, l *syncLog, email, role string) error {
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
		return l.write(entryAccount, "", name, map[string]any{"hash": hash, "role": role})
	}
	_, err = db.Exec(`INSERT INTO accounts (name, hash, role) VALUES (?, ?, ?) ON CONFLICT (name) DO UPDATE SET hash = excluded.hash, role = excluded.role`, name, hash, role)
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
