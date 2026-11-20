package app

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"strings"
	"time"
)

// Registering, vetting and invites. With -register, anyone may ask for an
// account on the site's /app/register; they wait, unable to sign in, until
// an admin lets them in on People, or turns them down. Letting someone in
// emails them a link to a page where they choose their password. The same
// page sets a forgotten password ("Forgot your password?").
//
// A link is worked out from the account (an HMAC, under the key the app's
// machines share), not stored: so it works on every machine, asking again
// sends the same link, and it stops working once the password is set, or
// after inviteFor.

const (
	roleApplied = "applied" // registered, not let in yet: can't sign in
	inviteFor   = 7 * 24 * time.Hour
	resendAfter = 10 * time.Minute // asking again sooner sends nothing
	maxWaiting  = 500              // registrations waiting at once
)

// accountRec is all an account holds.
type accountRec struct {
	Name, Hash, Role string
	About            string // a registration's details, as JSON
	Invited          int64  // unix seconds: when its link was made; 0 for none
}

// registration is what someone filled in on the form.
type registration struct {
	Name  string `json:"name"`
	About string `json:"about,omitempty"`
	At    int64  `json:"at"`
}

// prepare gives an accounts file from before registering its columns.
func (a *app) prepare() {
	a.db.Exec(`ALTER TABLE accounts ADD COLUMN about TEXT NOT NULL DEFAULT ''`)
	a.db.Exec(`ALTER TABLE accounts ADD COLUMN invited INTEGER NOT NULL DEFAULT 0`)
}

func (a *app) getAccount(name string) (accountRec, bool) {
	x := accountRec{Name: name}
	err := a.accountsDB().QueryRow(`SELECT hash, role, about, invited FROM `+a.accounts()+` WHERE name = ?`, name).
		Scan(&x.Hash, &x.Role, &x.About, &x.Invited)
	return x, err == nil
}

// putAccount saves an account whole, in the log if there is one.
func (a *app) putAccount(x accountRec) error {
	if a.data != nil && a.data.sync != nil {
		return a.data.sync.write(entryAccount, "", x.Name, map[string]any{
			"hash": x.Hash, "role": x.Role, "about": x.About, "invited": x.Invited})
	}
	_, err := a.db.Exec(`INSERT INTO accounts (name, hash, role, about, invited) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET hash = excluded.hash, role = excluded.role, about = excluded.about, invited = excluded.invited`,
		x.Name, x.Hash, x.Role, x.About, x.Invited)
	return err
}

// --- The link --------------------------------------------------------------

// linkToken is the token in an account's link; "" if it has none.
func (a *app) linkToken(x accountRec) string {
	if x.Invited == 0 || len(a.secret) == 0 {
		return ""
	}
	m := hmac.New(sha256.New, a.secret)
	fmt.Fprintf(m, "yggstore app link\x00%s\x00%d\x00%s", x.Name, x.Invited, x.Hash)
	return hex.EncodeToString(m.Sum(nil)[:20])
}

// linkWorks says whether token is the account's link, still in time.
func (a *app) linkWorks(x accountRec, token string) bool {
	want := a.linkToken(x)
	return want != "" && x.Role != roleApplied && time.Since(time.Unix(x.Invited, 0)) < inviteFor &&
		hmac.Equal([]byte(want), []byte(token))
}

// address is where this site is, for links in emails.
func (a *app) address(r *http.Request) string {
	host := a.site
	if host == "" {
		host = r.Header.Get("X-Forwarded-Host")
	}
	if host == "" {
		host = r.Host
	}
	scheme := "https"
	if h, _, err := net.SplitHostPort(host); (err == nil && isLocal(h)) || isLocal(host) {
		scheme = "http"
	}
	return scheme + "://" + host
}

func (a *app) link(r *http.Request, x accountRec) string {
	return a.address(r) + base + "welcome?" + url.Values{"e": {x.Name}, "t": {a.linkToken(x)}}.Encode()
}

// sendLink gives the account a link, and emails it. It returns the link,
// and whether it went by email. A link made less than resendAfter ago is
// left as it is and not sent again; one past its time gets a new one.
func (a *app) sendLink(r *http.Request, x accountRec, invite bool) (string, bool, error) {
	fresh := x.Invited == 0 || time.Since(time.Unix(x.Invited, 0)) >= inviteFor
	if !fresh && time.Since(time.Unix(x.Invited, 0)) < resendAfter && a.mail != nil {
		return a.link(r, x), true, nil // sent just now
	}
	if fresh {
		x.Invited = time.Now().Unix()
		if err := a.putAccount(x); err != nil {
			return "", false, err
		}
	}
	link := a.link(r, x)
	if a.mail == nil {
		return link, false, nil
	}
	site := strings.TrimPrefix(strings.TrimPrefix(a.address(r), "https://"), "http://")
	subject, body := "Your account on "+site, fmt.Sprintf(
		"You can now sign in to %s.\n\nChoose your password here:\n\n%s\n\nThe link works once, for 7 days. Then sign in at %s%slogin with %s and your password.\n",
		site, link, a.address(r), base, x.Name)
	if !invite {
		subject, body = "Your password on "+site, fmt.Sprintf(
			"Someone asked for a new password for %s on %s. If it was you, choose one here:\n\n%s\n\nThe link works once, for 7 days. If it wasn't you, do nothing: your password stays as it is.\n",
			x.Name, site, link)
	}
	if err := a.mail([]string{x.Name}, subject, body); err != nil {
		log.Printf("emailing %s their link: %v", x.Name, err)
		return link, false, nil
	}
	return link, true, nil
}

// --- Email -----------------------------------------------------------------

// yggstoreMail sends through the yggstore node on this machine, from its
// owner's address, with name shown beside it.
// from, if not "", is another address the node may send as: an app's own
// (yggstore app address), or the internal twin of one.
func yggstoreMail(name string) func(from string, to []string, subject, body string) error {
	return func(from string, to []string, subject, body string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		// -auto: an app's emails are a program's, so nobody's auto-reply answers
		// them and no shared mailbox passes them on (a loop otherwise).
		args := []string{"mail", "send", "-auto", "-name", name, "-to", strings.Join(to, ","), "-subject", subject}
		if from != "" {
			args = append(args, "-from", from)
		}
		cmd := exec.CommandContext(ctx, "yggstore", args...)
		cmd.Stdin = strings.NewReader(body)
		if os.Getenv("HOME") == "" { // a system service: the node's files are the user's
			if u, err := user.Current(); err == nil {
				cmd.Env = append(os.Environ(), "HOME="+u.HomeDir)
			}
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
}

// tellAdmins emails the admins, in the background.
func (a *app) tellAdmins(subject, body string) {
	if a.mail == nil {
		return
	}
	rows, err := a.accountsDB().Query(`SELECT name FROM `+a.accounts()+` WHERE role = ? AND hash != ''`, roleAdmin)
	if err != nil {
		return
	}
	var to []string
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			to = append(to, n)
		}
	}
	rows.Close()
	if len(to) == 0 {
		return
	}
	go func() {
		if err := a.mail(to, subject, body); err != nil {
			log.Printf("telling the admins %q: %v", subject, err)
		}
	}()
}

// --- Registering -------------------------------------------------------------

type registerPage struct {
	Error, Name, Email, About string
	Done                      bool
}

func (a *app) registerPage(w http.ResponseWriter, r *http.Request) {
	page(w, r, http.StatusOK, "register", registerPage{})
}

func (a *app) register(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	p := registerPage{Name: strings.TrimSpace(r.PostFormValue("name")), Email: cleanEmail(r.PostFormValue("email")),
		About: strings.TrimSpace(r.PostFormValue("about"))}
	fail := func(code int, msg string) {
		p.Error = msg
		page(w, r, code, "register", p)
	}
	done := func() { page(w, r, http.StatusOK, "register", registerPage{Done: true}) }
	if r.PostFormValue("website") != "" { // the field people don't see: a robot
		done()
		return
	}
	ip := "reg:" + r.Header.Get("X-Forwarded-For")
	if a.tooMany(ip) {
		fail(http.StatusTooManyRequests, "Too many tries. Try again later.")
		return
	}
	switch {
	case p.Name == "" || len(p.Name) > 100 || strings.ContainsAny(p.Name, "<>\r\n"):
		fail(http.StatusBadRequest, "Give your name, up to 100 characters.")
		return
	case !strings.Contains(p.Email, "@") || strings.ContainsAny(p.Email, " <>\",") || len(p.Email) > 200 || strings.HasPrefix(p.Email, groupKey):
		fail(http.StatusBadRequest, "That isn't an email address.")
		return
	case len(p.About) > 2000:
		fail(http.StatusBadRequest, "Say it in at most 2,000 characters.")
		return
	}
	a.wrong(ip) // each registration counts, so one address can't send many
	// Someone with an account, or waiting, sees the same as anyone: the
	// form doesn't say who has an account.
	if _, ok := a.getAccount(p.Email); ok {
		done()
		return
	}
	var waiting int
	a.accountsDB().QueryRow(`SELECT COUNT(*) FROM `+a.accounts()+` WHERE role = ?`, roleApplied).Scan(&waiting)
	if waiting >= maxWaiting {
		fail(http.StatusServiceUnavailable, "We can't take registrations just now. Try again in a few days.")
		return
	}
	about, _ := json.Marshal(registration{Name: p.Name, About: p.About, At: time.Now().Unix()})
	if err := a.putAccount(accountRec{Name: p.Email, Role: roleApplied, About: string(about)}); err != nil {
		fail(http.StatusInternalServerError, "That couldn't be saved. Try again later.")
		return
	}
	a.tellAdmins("New registration: "+p.Name, fmt.Sprintf("%s <%s> registered on %s:\n\n%s\n\nLet them in, or turn them down, on People: %s%speople\n",
		p.Name, p.Email, a.address(r), orNothing(p.About), a.address(r), base))
	done()
}

func orNothing(s string) string {
	if s == "" {
		return "(nothing more)"
	}
	return s
}

// waiting is who registered and hasn't been let in, oldest first.
type waiter struct {
	Email, Name, About, When string
	at                       int64
}

func (a *app) listWaiting() ([]waiter, error) {
	rows, err := a.accountsDB().Query(`SELECT name, about FROM `+a.accounts()+` WHERE role = ?`, roleApplied)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []waiter
	for rows.Next() {
		var x waiter
		var about string
		if err := rows.Scan(&x.Email, &about); err != nil {
			return nil, err
		}
		var reg registration
		json.Unmarshal([]byte(about), &reg)
		x.Name, x.About, x.at = reg.Name, reg.About, reg.At
		x.When = time.Unix(reg.At, 0).Format("2 Jan 2006 15:04")
		out = append(out, x)
	}
	for i := 1; i < len(out); i++ { // oldest first
		for j := i; j > 0 && out[j].at < out[j-1].at; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, rows.Err()
}

// --- Choosing a password from a link -------------------------------------------

type welcomePage struct {
	Email, Token, Error string
	Bad, Forgot         bool
}

func (a *app) welcomePage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	name, token := cleanEmail(r.URL.Query().Get("e")), r.URL.Query().Get("t")
	x, ok := a.getAccount(name)
	if !ok || !a.linkWorks(x, token) {
		page(w, r, http.StatusNotFound, "welcome", welcomePage{Bad: true, Forgot: a.mail != nil})
		return
	}
	page(w, r, http.StatusOK, "welcome", welcomePage{Email: name, Token: token})
}

func (a *app) welcome(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	name, token := cleanEmail(r.PostFormValue("e")), r.PostFormValue("t")
	ip := r.Header.Get("X-Forwarded-For")
	if a.tooMany(ip) {
		page(w, r, http.StatusTooManyRequests, "welcome", welcomePage{Bad: true, Error: "Too many tries. Try again later."})
		return
	}
	x, ok := a.getAccount(name)
	if !ok || !a.linkWorks(x, token) {
		a.wrong(ip)
		page(w, r, http.StatusNotFound, "welcome", welcomePage{Bad: true, Forgot: a.mail != nil})
		return
	}
	next := r.PostFormValue("new")
	p := welcomePage{Email: name, Token: token}
	switch {
	case len(next) < 10:
		p.Error = "The password needs at least 10 characters."
	case next != r.PostFormValue("again"):
		p.Error = "The two passwords aren't the same."
	}
	if p.Error != "" {
		page(w, r, http.StatusBadRequest, "welcome", p)
		return
	}
	x.Hash, x.Invited = hashPassword(next), 0
	if err := a.putAccount(x); err != nil {
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}
	a.db.Exec(`DELETE FROM sessions WHERE name = ?`, name) // a new password signs out everywhere
	if err := a.startSession(w, r, name); err != nil {
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, base, http.StatusSeeOther)
}

// --- A forgotten password ---------------------------------------------------

func (a *app) forgotPage(w http.ResponseWriter, r *http.Request) {
	page(w, r, http.StatusOK, "forgot", "")
}

func (a *app) forgot(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	name := cleanEmail(r.PostFormValue("email"))
	ip := "forgot:" + r.Header.Get("X-Forwarded-For")
	if a.tooMany(ip) || a.tooMany("forgot:"+name) {
		page(w, r, http.StatusTooManyRequests, "forgot", "Too many tries. Try again later.")
		return
	}
	a.wrong(ip)
	a.wrong("forgot:" + name)
	// The same answer whether there's an account or not.
	if x, ok := a.getAccount(name); ok && x.Role != roleApplied {
		if _, _, err := a.sendLink(r, x, x.Hash == ""); err != nil {
			log.Printf("a link for %s: %v", name, err)
		}
	}
	page(w, r, http.StatusOK, "forgot", "sent")
}

// --- The key links are made with ------------------------------------------------

// linkSecret is the key the app's machines share, or else one of this
// machine's own, kept beside the accounts file.
func linkSecret(syncKey []byte, path string) ([]byte, error) {
	if len(syncKey) > 0 {
		m := hmac.New(sha256.New, syncKey)
		m.Write([]byte("yggstore app links"))
		return m.Sum(nil), nil
	}
	if b, err := os.ReadFile(path); err == nil {
		k, err := hex.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(k) != 32 {
			return nil, fmt.Errorf("%s isn't a key", path)
		}
		return k, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	k := make([]byte, 32)
	rand.Read(k)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	_, err = f.WriteString(hex.EncodeToString(k) + "\n")
	return k, err
}

func init() {
	template.Must(dataPages.Parse(`
{{define "register"}}<title>Register</title>
{{if .Done}}<div class="edit signin"><h1>Thank you</h1><p>We'll look at your registration, and email you when you can sign in.</p></div>
{{else}}<form class="edit signin" method="post" action="` + base + `register"><h1>Register</h1>
{{if .Error}}<p class="err">{{.Error}}</p>{{end}}
<p class="muted">We look at each registration, and email you when you can sign in.</p>
<label>Your name <input name="name" value="{{.Name}}" maxlength="100" autocomplete="name" required autofocus></label>
<label>Your email <input name="email" type="email" value="{{.Email}}" autocomplete="email" required></label>
<label>Anything we should know (optional) <textarea name="about" maxlength="2000" rows="4">{{.About}}</textarea></label>
<label class="trap" aria-hidden="true">Leave this empty <input name="website" tabindex="-1" autocomplete="off"></label>
<button>Register</button>
<p><a href="` + base + `login">I have an account: sign in</a></p></form>{{end}}{{end}}

{{define "welcome"}}<title>Choose your password</title>
<form class="edit signin" method="post" action="` + base + `welcome"><h1>Choose your password</h1>
{{if .Error}}<p class="err">{{.Error}}</p>{{end}}
{{if .Bad}}<p>This link has been used, or has run out.{{if .Forgot}} <a href="` + base + `forgot">Ask for a new one</a>.{{end}}</p>
{{else}}<input type="hidden" name="e" value="{{.Email}}"><input type="hidden" name="t" value="{{.Token}}">
<input type="email" name="email" value="{{.Email}}" autocomplete="username" hidden>
<p>For <b>{{.Email}}</b>.</p>
<label>Password, at least 10 characters <input name="new" type="password" autocomplete="new-password" minlength="10" required autofocus></label>
<label>Password again <input name="again" type="password" autocomplete="new-password" minlength="10" required></label>
<button>Save it and sign in</button>{{end}}</form>{{end}}

{{define "forgot"}}<title>Forgot your password?</title>
<form class="edit signin" method="post" action="` + base + `forgot"><h1>Forgot your password?</h1>
{{if eq . "sent"}}<p>If there's an account for that address, an email with a link is on its way. The link works for 7 days.</p>
{{else}}{{if .}}<p class="err">{{.}}</p>{{end}}
<p class="muted">We'll email you a link to choose a new one.</p>
<label>Your email <input name="email" type="email" autocomplete="username" required autofocus></label>
<button>Send the link</button>{{end}}
<p><a href="` + base + `login">Back to sign in</a></p></form>{{end}}
`))
}
