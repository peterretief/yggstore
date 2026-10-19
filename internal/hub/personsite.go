package hub

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"

	"math/big"
	netmail "net/mail"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/peterretief/yggstore/internal/atomicfile"
	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/mail"
	"github.com/peterretief/yggstore/internal/msg"
	"github.com/peterretief/yggstore/internal/outbox"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/share"
	"github.com/peterretief/yggstore/internal/site"
	"github.com/peterretief/yggstore/internal/sitemove"
	"github.com/peterretief/yggstore/internal/websites"
)

// People's own websites and mail. The group's admin can give someone on the
// box a name (anna.example.org, a peers.PersonName on the box's entry),
// which the box's admin asks for from the box's dashboard. The box's node
// then holds the name for them:
//
//   - the hub publishes the site in their folder, websites/NAME, which
//     their files page edits (it starts as a page of their own);
//   - mail to the name and the site's contact form are sealed for the
//     person's own sharing key, not the box's. The node collects them like
//     any mail; the hub moves those that open with a person's key into
//     that person's mailbox, which their files page shows;
//   - if they ask, and the box's admin allows it, each message is also
//     forwarded to an ordinary email address of theirs, which they confirm
//     with a code sent to it. Forwarding goes through the group's mail
//     relay, from their name.
//
// Their keys stay on the box, so the box's owner could read their mail; the
// files page says so.

// keysDir is where people's sharing keys are, beside the hub's state file.
const keysDir = "keys"

// KeyPath is where person's sharing key is kept.
func KeyPath(statePath string, p Person) string {
	return filepath.Join(filepath.Dir(statePath), keysDir, p.Folder+".key")
}

// PersonKey is person's sharing key, made the first time it is wanted.
func PersonKey(statePath string, p Person) (*share.Identity, error) {
	return share.LoadOrCreate(KeyPath(statePath, p))
}

// Forward is where a person's mail is forwarded to.
type Forward struct {
	To        string `json:"to"`
	CodeHash  string `json:"code_hash,omitempty"` // of the code sent to To, until it is confirmed
	SentAt    int64  `json:"sent_at,omitempty"`   // when the code was sent (unix)
	Tries     int    `json:"tries,omitempty"`     // wrong codes typed
	Confirmed bool   `json:"confirmed,omitempty"` // the code came back: the address is theirs
	Allowed   bool   `json:"allowed,omitempty"`   // the box's admin allowed it
	Since     int64  `json:"since,omitempty"`     // unix ms: messages from then on go
}

// On is whether messages are forwarded now.
func (f *Forward) On() bool { return f != nil && f.Confirmed && f.Allowed }

const (
	codeFor    = time.Hour   // how long a code works
	codeEvery  = time.Minute // the least time between two codes
	codeTries  = 5
	forwardMax = 20 // messages forwarded in one pass
)

// Messenger sends direct messages through the box's node.
type Messenger interface {
	Send(ctx context.Context, to, typ, body string) (msg.Message, error)
}

type PeopleSitesConfig struct {
	StatePath string
	PeersPath string
	Self      string // the box's node's ID
	// NodeMail is the mailbox the box's node collects into; people's mail
	// is moved out of it. "": none.
	NodeMail string
	// Dir keeps, per person (by folder ID), their mailbox (DIR/ID/mail)
	// and their site's versions (DIR/ID/sites).
	Dir       string
	Client    client.Client
	Online    func(ctx context.Context) ([]peers.Peer, error)
	Announcer func() (site.Announcer, error)
	Messages  func() (Messenger, error)
	MaxBytes  int64 // the most a site may hold
	Logf      func(format string, args ...any)
}

// PeopleSites runs people's sites, sorts their mail and forwards it.
type PeopleSites struct {
	cfg PeopleSitesConfig

	mu       sync.Mutex
	running  map[string]*personSite // by folder ID
	notMine  map[string]bool        // node mail no person's key opens
	returned map[string]time.Time   // names given back, by name, when last asked
	refilled time.Time
	logged   map[string]string // the last problem logged, per person
}

// personSite is a person's websites: one watcher for all their names.
type personSite struct {
	name   string   // the first: their address's site, if they have one
	names  []string // every name they hold
	pub    *site.Publisher
	w      *websites.Watcher
	cancel context.CancelFunc
}

func NewPeopleSites(cfg PeopleSitesConfig) *PeopleSites {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &PeopleSites{cfg: cfg, running: map[string]*personSite{}, notMine: map[string]bool{},
		returned: map[string]time.Time{}, logged: map[string]string{}}
}

func (ps *PeopleSites) Run(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		ps.Step(ctx)
		select {
		case <-ctx.Done():
			ps.mu.Lock()
			for _, r := range ps.running {
				r.cancel()
			}
			ps.mu.Unlock()
			return
		case <-t.C:
		}
	}
}

// logOnce logs a person's problem when it changes.
func (ps *PeopleSites) logOnce(person, format string, args ...any) {
	m := fmt.Sprintf(format, args...)
	ps.mu.Lock()
	same := ps.logged[person] == m
	ps.logged[person] = m
	ps.mu.Unlock()
	if !same && m != "" {
		ps.cfg.Logf("%s: %s", person, m)
	}
}

// Step does one pass: keys, sites, mail, forwarding.
func (ps *PeopleSites) Step(ctx context.Context) {
	st, err := Load(ps.cfg.StatePath)
	if err != nil {
		return
	}
	list, err := peers.Load(ps.cfg.PeersPath)
	if err != nil {
		return
	}
	keys := map[string]*share.Identity{}
	for _, p := range st.People {
		id, err := PersonKey(ps.cfg.StatePath, p)
		if err != nil {
			ps.logOnce(p.Name, "their sharing key: %v", err)
			continue
		}
		if p.Code != id.Code() {
			Update(ctx, ps.cfg.StatePath, func(s *HubState) error {
				if i := s.person(p.Name); i >= 0 {
					s.People[i].Code = id.Code()
				}
				return nil
			})
			p.Code = id.Code()
		}
		var names []string
		for _, pn := range peers.PersonNamesOf(list, ps.cfg.Self, p.Name) {
			names = append(names, pn.Name)
		}
		switch {
		case len(names) > 0 && p.Active():
			ps.ensure(ctx, p, names)
		case len(names) > 0:
			ps.stop(p)
			for _, name := range names {
				ps.takeDown(ctx, p, name)
				if p.Removed {
					ps.giveBack(ctx, list, p, name)
				}
			}
		default:
			ps.stop(p) // a site moved away, say: the move lets it go
		}
		if !p.Removed {
			keys[p.Folder] = id
		}
	}
	ps.sortMail(keys, st)
	if time.Since(ps.refilled) > 10*time.Minute {
		ps.refilled = time.Now()
		for _, p := range st.People {
			if !p.Removed {
				(&mail.Box{Dir: ps.mailDir(p)}).Refill(ctx, ps.cfg.Client)
			}
		}
	}
	for _, p := range st.People {
		if from, ok := ForwardFrom(list, ps.cfg.Self, p.Name); ok && p.Active() && p.Forward.On() {
			if id := keys[p.Folder]; id != nil {
				ps.forward(ctx, p, from, id)
			}
		}
	}
}

func (ps *PeopleSites) mailDir(p Person) string { return filepath.Join(ps.cfg.Dir, p.Folder, "mail") }

// Mailbox is person's own mailbox.
func (ps *PeopleSites) Mailbox(p Person) (*mail.Box, error) {
	id, err := PersonKey(ps.cfg.StatePath, p)
	if err != nil {
		return nil, err
	}
	return &mail.Box{Dir: ps.mailDir(p), ID: id}, nil
}

// Stubs are the stubs the hub keeps for people's sites and mail, for repair.
func (ps *PeopleSites) Stubs() []string {
	var out []string
	filepath.WalkDir(ps.cfg.Dir, func(path string, e os.DirEntry, err error) error {
		if err == nil && !e.IsDir() && strings.HasSuffix(path, ".ystub") {
			out = append(out, path)
		}
		return nil
	})
	return out
}

// starterPage is a new site's first page, until its person changes it.
const starterPage = `<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width">
<title>%[1]s</title>
<style>body{font-family:system-ui,sans-serif;max-width:40em;margin:3em auto;padding:0 1em;line-height:1.5}</style>
</head>
<body>
<h1>%[1]s</h1>
<p>This page is new. Change it on your files page, under Your websites.</p>
</body>
</html>
`

// ForwardFrom is the name a person's messages are forwarded from: their
// address, or failing that their first site.
func ForwardFrom(list []peers.Peer, box, person string) (string, bool) {
	names := peers.PersonNamesOf(list, box, person)
	if len(names) == 0 {
		return "", false
	}
	return names[0].Name, true
}

// ensure runs person's sites: a folder for each (a page to start from, the
// first time) and the watcher that publishes them.
func (ps *PeopleSites) ensure(ctx context.Context, p Person, names []string) {
	ps.mu.Lock()
	r := ps.running[p.Folder]
	ps.mu.Unlock()
	if r != nil && slices.Equal(r.names, names) {
		return
	}
	ps.stop(p)
	ann, err := ps.cfg.Announcer()
	if err != nil {
		ps.logOnce(p.Name, "can't publish %s yet: %v", strings.Join(names, ", "), err)
		return
	}
	pub := &site.Publisher{Dir: filepath.Join(ps.cfg.Dir, p.Folder, "sites"), Client: ps.cfg.Client, Announce: ann,
		Log: ps.cfg.Logf, Skip: websites.Skip}
	dir := filepath.Join(p.Path, outbox.WebsitesDir)
	published := map[string]bool{}
	for _, s := range pub.Sites() {
		published[s] = true
	}
	mine := map[string]bool{}
	var fresh []string
	for _, name := range names {
		mine[name] = true
		if published[name] {
			continue // moved here, say: the watcher fills its folder
		}
		fresh = append(fresh, name)
		folder := filepath.Join(dir, name)
		if _, err := os.Stat(folder); os.IsNotExist(err) {
			if err := os.MkdirAll(folder, 0o755); err != nil {
				ps.logOnce(p.Name, "making the folder for %s: %v", name, err)
				return
			}
			atomicfile.Replace(filepath.Join(folder, "index.html"), []byte(fmt.Sprintf(starterPage, p.Name)), 0o644)
			ps.cfg.Logf("%s: %s is theirs now: started their site", p.Name, name)
		}
	}
	w := websites.New(websites.Config{Dir: dir, StatePath: filepath.Join(ps.cfg.Dir, p.Folder, "websites.json"),
		Pub: pub, Client: ps.cfg.Client, Online: ps.cfg.Online, MaxBytes: ps.cfg.MaxBytes, Logf: ps.cfg.Logf,
		Mine:  func() map[string]bool { return mine },
		Moved: func(name string) (string, time.Time, bool) { return sitemove.Moved(pub.Dir, name) },
		// A person publishes only the names they were given.
		Cloudflare: func(context.Context, string) (bool, string, error) { return false, "", nil }})
	for _, name := range fresh {
		w.Force(name) // a new site, or one taken down while they were shut out
	}
	cctx, cancel := context.WithCancel(ctx)
	ps.mu.Lock()
	ps.running[p.Folder] = &personSite{name: names[0], names: names, pub: pub, w: w, cancel: cancel}
	ps.mu.Unlock()
	go func() {
		if err := w.Run(cctx); err != nil {
			ps.cfg.Logf("%s: their sites %s: %v", p.Name, strings.Join(names, ", "), err)
		}
	}()
}

func (ps *PeopleSites) stop(p Person) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if r := ps.running[p.Folder]; r != nil {
		r.cancel()
		delete(ps.running, p.Folder)
	}
}

// takeDown stops the group serving person's site, while they are shut out
// or gone. Their folder stays.
func (ps *PeopleSites) takeDown(ctx context.Context, p Person, name string) {
	ann, err := ps.cfg.Announcer()
	if err != nil {
		return
	}
	pub := &site.Publisher{Dir: filepath.Join(ps.cfg.Dir, p.Folder, "sites"), Client: ps.cfg.Client, Announce: ann, Log: ps.cfg.Logf}
	for _, s := range pub.Sites() {
		if s == name {
			if err := pub.Remove(ctx, name); err != nil {
				ps.logOnce(p.Name, "taking %s down: %v", name, err)
				return
			}
			ps.cfg.Logf("%s: %s taken down while they can't use the box", p.Name, name)
		}
	}
}

// giveBack tells the group's admins that a removed person's name is free,
// every ten minutes until it is.
func (ps *PeopleSites) giveBack(ctx context.Context, list []peers.Peer, p Person, name string) {
	ps.mu.Lock()
	last := ps.returned[name]
	ps.mu.Unlock()
	if time.Since(last) < 10*time.Minute || ps.cfg.Messages == nil {
		return
	}
	m, err := ps.cfg.Messages()
	if err != nil {
		return
	}
	body, _ := json.Marshal(map[string]string{"name": name})
	sent := false
	for _, a := range list {
		if a.Admin {
			if _, err := m.Send(ctx, a.IP(), "name-return", string(body)); err == nil {
				sent = true
			}
		}
	}
	ps.mu.Lock()
	ps.returned[name] = time.Now()
	ps.mu.Unlock()
	if sent {
		ps.cfg.Logf("%s is removed: gave %s back to the group's admin", p.Name, name)
	}
}

// sortMail moves the messages in the node's mailbox that a person's key
// opens into that person's mailbox. Only collected messages move (their
// stub is written last), so the node doesn't fetch them again.
func (ps *PeopleSites) sortMail(keys map[string]*share.Identity, st HubState) {
	if ps.cfg.NodeMail == "" || len(keys) == 0 {
		return
	}
	ents, err := os.ReadDir(ps.cfg.NodeMail)
	if err != nil {
		return
	}
	for _, e := range ents {
		id, ok := strings.CutSuffix(e.Name(), ".sealed")
		ps.mu.Lock()
		skip := ps.notMine[id]
		ps.mu.Unlock()
		if !ok || skip {
			continue
		}
		from := filepath.Join(ps.cfg.NodeMail, id)
		if _, err := os.Stat(from + ".ystub"); err != nil {
			continue // still being collected
		}
		data, err := os.ReadFile(from + ".sealed")
		if err != nil {
			continue
		}
		whose := ""
		for folder, key := range keys {
			if _, err := mail.Open(key, data); err == nil {
				whose = folder
				break
			}
		}
		if whose == "" {
			ps.mu.Lock()
			ps.notMine[id] = true
			ps.mu.Unlock()
			continue
		}
		var p Person
		for _, q := range st.People {
			if q.Folder == whose {
				p = q
			}
		}
		to := filepath.Join(ps.mailDir(p), id)
		if err := os.MkdirAll(ps.mailDir(p), 0o700); err != nil {
			continue
		}
		// The sealed message goes last, so a half move leaves it where
		// it was, to be moved again.
		moved := true
		for _, ext := range []string{".json", ".ystub", ".sealed"} {
			if err := moveFile(from+ext, to+ext); err != nil && !errors.Is(err, os.ErrNotExist) {
				ps.cfg.Logf("%s: moving message %s into their mailbox: %v", p.Name, id, err)
				moved = false
				break
			}
		}
		if moved {
			ps.cfg.Logf("%s: a message (%s) is in their mailbox", p.Name, id)
		}
	}
}

// moveFile renames from to to, or copies it across file systems.
func moveFile(from, to string) error {
	if err := os.Rename(from, to); err == nil {
		return nil
	}
	data, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	if err := atomicfile.Replace(to, data, 0o600); err != nil {
		return err
	}
	return os.Remove(from)
}

// forwarded is which messages went on to the person's address already.
type forwarded struct {
	IDs []string `json:"ids"`
}

func (ps *PeopleSites) forward(ctx context.Context, p Person, name string, key *share.Identity) {
	box := &mail.Box{Dir: ps.mailDir(p), ID: key}
	path := filepath.Join(ps.cfg.Dir, p.Folder, "forwarded.json")
	var done forwarded
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &done)
	}
	sent := map[string]bool{}
	for _, id := range done.IDs {
		sent[id] = true
	}
	msgs, err := box.List()
	if err != nil {
		return
	}
	var online []peers.Peer
	n := 0
	for _, s := range msgs {
		if sent[s.ID] || s.Sent || s.Received < p.Forward.Since || s.Error != "" || n >= forwardMax {
			continue
		}
		m, err := box.Read(s.ID)
		if err != nil {
			continue
		}
		if online == nil {
			if online, err = ps.cfg.Online(ctx); err != nil {
				return
			}
		}
		replyTo := m.ReplyTo
		if replyTo == "" {
			replyTo = m.From
		}
		if _, err := netmail.ParseAddress(replyTo); err != nil {
			replyTo = ""
		}
		text := fmt.Sprintf("Forwarded from %s.\n\nFrom: %s\nTo: %s\nDate: %s\nSubject: %s\n\n%s",
			peers.MailAddress(name), m.From, m.To, time.UnixMilli(m.Date).Format(time.RFC1123), m.Subject, m.Text)
		if len(m.Attachments) > 0 {
			text += fmt.Sprintf("\n\n(%d attachment(s) are on your files page, under Messages.)", len(m.Attachments))
		}
		raw, rcpts, err := mail.Draft{From: peers.MailAddress(name), Name: p.Name, To: p.Forward.To,
			Subject: "Fwd: " + m.Subject, Text: text, ReplyTo: replyTo}.Compose()
		if err == nil {
			err = mail.Send(ctx, ps.cfg.Client, online, raw, rcpts)
		}
		if err != nil {
			ps.logOnce(p.Name, "forwarding their mail to %s: %v (will try again)", p.Forward.To, err)
			return
		}
		n++
		done.IDs = append(done.IDs, s.ID)
		b, _ := json.Marshal(done)
		atomicfile.Replace(path, b, 0o600)
		ps.logOnce(p.Name, "")
	}
	if n > 0 {
		ps.cfg.Logf("%s: forwarded %d message(s) to %s", p.Name, n, p.Forward.To)
	}
}

// SendCode starts forwarding person's mail to an address: a code goes to
// it, which they type on their files page (Confirm).
func (ps *PeopleSites) SendCode(ctx context.Context, p Person, name, to string) error {
	a, err := netmail.ParseAddress(strings.TrimSpace(to))
	if err != nil || !strings.Contains(a.Address, "@") {
		return errors.New("that isn't an email address")
	}
	if strings.EqualFold(strings.SplitN(a.Address, "@", 2)[1], strings.SplitN(peers.MailAddress(name), "@", 2)[1]) {
		return errors.New("forward to an address outside the group: mail to the group's own addresses comes here anyway")
	}
	if f := p.Forward; f != nil && !f.Confirmed && time.Since(time.Unix(f.SentAt, 0)) < codeEvery {
		return errors.New("a code was sent a moment ago: wait a minute before asking for another")
	}
	n, _ := rand.Int(rand.Reader, big.NewInt(1000000))
	code := fmt.Sprintf("%06d", n.Int64())
	online, err := ps.cfg.Online(ctx)
	if err != nil {
		return err
	}
	text := fmt.Sprintf("Your code is %s\n\nType it on your files page to have the mail to %s forwarded here. It works for an hour.\n\n"+
		"If you didn't ask for this, ignore this message: nothing is forwarded without the code.", code, peers.MailAddress(name))
	raw, rcpts, err := mail.Draft{From: peers.MailAddress(name), Name: p.Name, To: a.Address,
		Subject: "Your code for forwarding " + peers.MailAddress(name), Text: text}.Compose()
	if err != nil {
		return err
	}
	if err := mail.Send(ctx, ps.cfg.Client, online, raw, rcpts); err != nil {
		return fmt.Errorf("the code could not be sent: %w", err)
	}
	_, err = Update(ctx, ps.cfg.StatePath, func(s *HubState) error {
		i := s.person(p.Name)
		if i < 0 {
			return errors.New("you're no longer on this box")
		}
		s.People[i].Forward = &Forward{To: a.Address, CodeHash: tokenHash(code), SentAt: time.Now().Unix()}
		return nil
	})
	return err
}

// Confirm checks the code sent to the forwarding address.
func Confirm(f *Forward, code string, now time.Time) error {
	switch {
	case f == nil || f.CodeHash == "":
		return errors.New("ask for a code first")
	case f.Tries >= codeTries:
		return errors.New("too many wrong codes: ask for a new one")
	case now.Sub(time.Unix(f.SentAt, 0)) > codeFor:
		return errors.New("that code has run out: ask for a new one")
	case tokenHash(strings.TrimSpace(code)) != f.CodeHash:
		f.Tries++
		return errors.New("that isn't the code that was sent")
	}
	f.Confirmed, f.CodeHash, f.Tries, f.Since = true, "", 0, now.UnixMilli()
	return nil
}

// site is person's running site, if they have one.
func (ps *PeopleSites) site(p Person) *personSite {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.running[p.Folder]
}

// SetContact turns person's contact form on or off: the web nodes take it,
// sealed for their key, and their home page links to it.
func (ps *PeopleSites) SetContact(ctx context.Context, p Person, on bool) error {
	r := ps.site(p)
	if r == nil {
		return errors.New("you have no website yet")
	}
	code := ""
	if on {
		code = p.Code
	}
	if err := r.pub.SetContact(ctx, r.name, code); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errors.New("your site isn't published yet: try again in a minute")
		}
		return err
	}
	home := filepath.Join(p.Path, outbox.WebsitesDir, r.name, "index.html")
	b, err := os.ReadFile(home)
	if err != nil {
		return nil // no home page to link from
	}
	page := site.WithoutContactLink(string(b))
	if on {
		page = site.WithContactLink(page)
	}
	if page != string(b) {
		return atomicfile.Replace(home, []byte(page), 0o644) // the watcher publishes it
	}
	return nil
}

// SiteStatus is a person's site for their files page.
type PersonSiteStatus struct {
	Name      string `json:"name"`
	Published int64  `json:"published,omitempty"`
	Problem   string `json:"problem,omitempty"`
	Contact   bool   `json:"contact"`
}

func (ps *PeopleSites) SiteStatus(p Person) (PersonSiteStatus, bool) {
	r := ps.site(p)
	if r == nil {
		return PersonSiteStatus{}, false
	}
	out := PersonSiteStatus{Name: r.name, Contact: r.pub.Contact(r.name) != ""}
	for _, s := range r.w.Status() {
		if s.Site == r.name {
			out.Published, out.Problem = s.Published, s.Problem
		}
	}
	if out.Published == 0 {
		if m, err := r.pub.Current(r.name); err == nil {
			out.Published = m.StoredAt
		}
	}
	return out, true
}

// AllowForward lets person's mail be forwarded to the address they
// confirmed (on), or stops it. Only the box's admin does this.
func (s *HubState) AllowForward(person string, on bool) error {
	i := s.person(person)
	if i < 0 {
		return fmt.Errorf("nobody called %q on this box", person)
	}
	f := s.People[i].Forward
	if !on {
		s.People[i].Forward = nil
		return nil
	}
	if f == nil || !f.Confirmed {
		return fmt.Errorf("%s hasn't confirmed an address to forward to", s.People[i].Name)
	}
	f.Allowed = true
	return nil
}
