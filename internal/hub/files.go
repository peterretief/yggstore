package hub

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/devices"
	"github.com/peterretief/yggstore/internal/workhere"
)

// Each person on the box opens their own files in the browser, from their
// laptop or phone, over Yggdrasil: the box's /files is the dashboard's files
// page, on the folder of the person whose device asks. A Yggdrasil address
// can only be used by the device holding its private key, so the address
// says whose device it is. A device that isn't let in can ask; the box's
// admin lets it in (yggstore hub devices), or the person does, from a
// device of theirs already let in.

//go:embed devices.html
var devicesHTML []byte

// FilesPath is where the box serves people's files.
const FilesPath = "/files"

// DeviceAsk is a device asking to open someone's files.
type DeviceAsk struct {
	Addr string `json:"addr"`
	Name string `json:"name"`          // the device, as it named itself
	Who  string `json:"who,omitempty"` // whose files, as they put it
	At   int64  `json:"at"`
}

// maxAsks keeps strangers on Yggdrasil from filling the list; asks are kept
// for a day.
const (
	maxAsks = 10
	askFor  = 24 * time.Hour
)

// PersonAt is whose files the device at addr opens.
func (s HubState) PersonAt(addr string) (Person, bool) {
	ip, err := devices.Parse(addr)
	if err != nil {
		return Person{}, false
	}
	for _, p := range s.People {
		for _, d := range p.FilesDevices {
			if d.Addr == ip {
				return p, true
			}
		}
	}
	return Person{}, false
}

// LetIn lets the device at addr open person's files, under name (or the
// name it asked with). Letting it in again renames it.
func (s *HubState) LetIn(person, addr, name string) (devices.Device, error) {
	ip, err := devices.Parse(addr)
	if err != nil {
		return devices.Device{}, err
	}
	i := -1
	for j, p := range s.People {
		if strings.EqualFold(p.Name, person) {
			i = j
		}
	}
	if i < 0 {
		return devices.Device{}, fmt.Errorf("nobody called %q on this box (see yggstore hub people)", person)
	}
	if other, ok := s.PersonAt(ip); ok && other.Name != s.People[i].Name {
		return devices.Device{}, fmt.Errorf("%s opens %s's files already: remove it there first", ip, other.Name)
	}
	name = strings.TrimSpace(name)
	for _, a := range s.Asks {
		if a.Addr == ip && name == "" {
			name = a.Name
		}
	}
	if name == "" {
		name = "a device"
	}
	s.Asks = dropAsk(s.Asks, ip)
	p := &s.People[i]
	if !p.Active() {
		return devices.Device{}, fmt.Errorf("%s can't let devices in now: the box's owner has shut them out", p.Name)
	}
	for j := range p.FilesDevices {
		if p.FilesDevices[j].Addr == ip {
			p.FilesDevices[j].Name = name
			return p.FilesDevices[j], nil
		}
	}
	if p.MaxDevices > 0 && len(p.FilesDevices) >= p.MaxDevices {
		return devices.Device{}, fmt.Errorf("%s may have %d devices let in, and has: remove one first", p.Name, p.MaxDevices)
	}
	d := devices.Device{Addr: ip, Name: name, Added: time.Now().UTC().Truncate(time.Second)}
	p.FilesDevices = append(p.FilesDevices, d)
	return d, nil
}

// LetOut stops a device, by its address or name, opening anyone's files,
// or only person's if person is set. It returns whose it was.
func (s *HubState) LetOut(person, key string) (string, devices.Device, error) {
	ip, _ := devices.Parse(key)
	for i := range s.People {
		p := &s.People[i]
		if person != "" && !strings.EqualFold(p.Name, person) {
			continue
		}
		for j, d := range p.FilesDevices {
			if d.Addr == ip || (d.Name != "" && d.Name == key) {
				p.FilesDevices = append(p.FilesDevices[:j], p.FilesDevices[j+1:]...)
				return p.Name, d, nil
			}
		}
	}
	return "", devices.Device{}, fmt.Errorf("no device %q opens files on this box", key)
}

// Ask keeps a device's ask, replacing an earlier one from it. It is false
// if too many are asking.
func (s *HubState) Ask(addr, name, who string, now time.Time) bool {
	s.Asks = dropAsk(s.Asks, addr)
	s.PruneAsks(now)
	if len(s.Asks) >= maxAsks {
		return false
	}
	s.Asks = append(s.Asks, DeviceAsk{Addr: addr, Name: name, Who: who, At: now.Unix()})
	return true
}

// PruneAsks drops the asks older than a day.
func (s *HubState) PruneAsks(now time.Time) {
	kept := s.Asks[:0]
	for _, a := range s.Asks {
		if now.Sub(time.Unix(a.At, 0)) < askFor {
			kept = append(kept, a)
		}
	}
	s.Asks = kept
}

func dropAsk(asks []DeviceAsk, addr string) []DeviceAsk {
	kept := asks[:0]
	for _, a := range asks {
		if a.Addr != addr {
			kept = append(kept, a)
		}
	}
	return kept
}

// FilesHandler serves people's files pages over Yggdrasil; self is the
// box's Yggdrasil address. Asked over the LAN, it sends the browser on to
// self, where the device is checked: nothing is served over the LAN itself.
func (u *Users) FilesHandler(self string) http.Handler {
	c := client.New()
	c.HTTP.Timeout = 0 // files of any size
	return &filesPage{u: u, self: self, client: c}
}

type filesPage struct {
	u      *Users
	self   string
	client client.Client

	mu     sync.Mutex
	seenAt map[string]time.Time // per device, when last written down
	used   map[string]*usage    // per person
}

// usage is what a person stores, counted now and then, plus what they put
// since.
type usage struct {
	at    time.Time
	bytes int64
}

// seenEvery is how often a device's last visit is written down.
const seenEvery = 5 * time.Minute

// seen writes down when a device last opened the page, now and then.
func (f *filesPage) seen(r *http.Request, person, addr string) {
	now := time.Now()
	f.mu.Lock()
	if f.seenAt == nil {
		f.seenAt = map[string]time.Time{}
	}
	if now.Sub(f.seenAt[addr]) < seenEvery {
		f.mu.Unlock()
		return
	}
	f.seenAt[addr] = now
	f.mu.Unlock()
	Update(r.Context(), f.u.cfg.StatePath, func(s *HubState) error {
		if i := s.person(person); i >= 0 {
			for j := range s.People[i].FilesDevices {
				if s.People[i].FilesDevices[j].Addr == addr {
					s.People[i].FilesDevices[j].LastSeen = now.UTC().Truncate(time.Second)
				}
			}
		}
		return nil
	})
}

// room is whether a file of size fits in the person's quota. What they
// store is counted at most every half minute; uploads in between add to it.
func (f *filesPage) room(p Person, size int64) error {
	if p.Quota <= 0 {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.used == nil {
		f.used = map[string]*usage{}
	}
	us := f.used[p.Name]
	if us == nil || time.Since(us.at) > 30*time.Second {
		b, _ := Usage(p)
		us = &usage{at: time.Now(), bytes: b}
		f.used[p.Name] = us
	}
	if size < 0 {
		size = 0
	}
	if us.bytes+size > p.Quota {
		return fmt.Errorf("That doesn't fit: you may store %s on this box and store %s. Delete something first, or ask the box's owner for more room.",
			Size(p.Quota), Size(us.bytes))
	}
	us.bytes += size
	return nil
}

// Size is n bytes for people to read.
func Size(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}

// join is the invite link's page: opened, it offers to let this device in;
// sent, it does, once.
func (f *filesPage) join(w http.ResponseWriter, r *http.Request, remote string, p Person, in bool) {
	if r.Method != http.MethodPost {
		if in && p.Active() {
			http.Redirect(w, r, FilesPath, http.StatusFound)
			return
		}
		// Sending it is a separate step, so a link preview doesn't use it up.
		page(w, "Open your files here", `<h1>Open your files on this device</h1>
<p>This lets this device open your files on the box from anywhere, over Yggdrasil. The link works once.</p>
<form method="post" action="`+JoinPath+`"><input type="hidden" name="t" value="`+html.EscapeString(r.URL.Query().Get("t"))+`">
<p><input name="name" placeholder="Name this device, e.g. my laptop" maxlength="60" style="padding:.5em;width:18em"></p>
<button style="padding:.5em 1em">Let this device in</button></form>`)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	r.ParseForm()
	name := strings.TrimSpace(r.PostForm.Get("name"))
	if len(name) > 60 {
		name = name[:60]
	}
	if name == "" {
		name = "a device"
	}
	var who Person
	_, err := Update(r.Context(), f.u.cfg.StatePath, func(s *HubState) error {
		var err error
		who, err = s.UseInvite(r.PostForm.Get("t"), remote, name, time.Now())
		return err
	})
	if err != nil {
		w.WriteHeader(http.StatusForbidden)
		page(w, "Invite", `<h1>`+html.EscapeString(err.Error())+`</h1>`)
		return
	}
	f.u.cfg.Logf("hub: %s (%s) opens %s's files now, with their invite", name, remote, who.Name)
	http.Redirect(w, r, FilesPath, http.StatusSeeOther)
}

func (f *filesPage) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	remote, err := devices.Parse(host)
	if err != nil {
		if f.self == "" || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
			http.Error(w, "your files open over Yggdrasil, at the box's Yggdrasil address", http.StatusForbidden)
			return
		}
		port := "7490"
		if a, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
			_, port, _ = net.SplitHostPort(a.String())
		}
		http.Redirect(w, r, "http://"+net.JoinHostPort(f.self, port)+r.URL.RequestURI(), http.StatusFound)
		return
	}
	// The Host must name the box, which stops DNS rebinding.
	h := r.Host
	if hh, _, err := net.SplitHostPort(h); err == nil {
		h = hh
	}
	if ip := net.ParseIP(strings.Trim(h, "[]")); ip == nil || !ip.Equal(net.ParseIP(f.self)) {
		http.Error(w, "the box only answers to its own address", http.StatusForbidden)
		return
	}
	st, err := Load(f.u.cfg.StatePath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	p, ok := st.PersonAt(remote)
	if r.URL.Path == JoinPath {
		f.join(w, r, remote, p, ok)
		return
	}
	if ok && !p.Active() {
		w.WriteHeader(http.StatusForbidden)
		page(w, "Shut out", `<h1>Your files on this box are closed for now</h1><p>The box's owner has shut `+html.EscapeString(p.Name)+
			` out. Your files are still on the box: ask them.</p>`)
		return
	}
	if r.URL.Path == FilesPath+"/ask" && r.Method == http.MethodPost {
		if ok { // let in already (the asked page reloaded): on to the files
			http.Redirect(w, r, FilesPath, http.StatusSeeOther)
			return
		}
		f.ask(w, r, remote)
		return
	}
	if !ok {
		notLetIn(w, remote)
		return
	}
	// Other web pages open in the browser can't add this header.
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-Yggstore") != "1" {
		http.Error(w, "missing X-Yggstore header", http.StatusForbidden)
		return
	}
	f.seen(r, p.Name, remote)
	switch r.URL.Path {
	case FilesPath:
		name := p.Name
		(&workhere.Live{Outbox: p.Path, Client: f.client, Holding: func() bool { return f.u.Holding(name) },
			Devices: FilesPath + "/devices", Room: func(size int64) error { return f.room(p, size) }}).ServeHTTP(w, r)
	case FilesPath + "/devices":
		f.devices(w, r, p, remote)
	default:
		http.NotFound(w, r)
	}
}

func (f *filesPage) ask(w http.ResponseWriter, r *http.Request, remote string) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	r.ParseForm()
	clip := func(s, or string) string {
		s = strings.TrimSpace(s)
		if len(s) > 60 {
			s = s[:60]
		}
		if s == "" {
			return or
		}
		return s
	}
	name, who := clip(r.PostForm.Get("name"), "a device"), clip(r.PostForm.Get("who"), "")
	full := false
	_, err := Update(r.Context(), f.u.cfg.StatePath, func(s *HubState) error {
		full = !s.Ask(remote, name, who, time.Now())
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	f.u.cfg.Logf("hub: %s (%s) asks to open %q's files", name, remote, who)
	msg := `Asked. On a device of yours that opens your files already, open <b>Your devices</b> and press <b>Allow</b>, or ask the box's owner; then <a href="/files">open your files</a>.`
	if full {
		w.WriteHeader(http.StatusTooManyRequests)
		msg = `Too many devices are asking right now. Ask the box's owner to let this one in (below).`
	}
	page(w, "Asked", `<h1>`+msg+`</h1><p>This device's Yggdrasil address is <code>`+html.EscapeString(remote)+`</code>.</p>`)
}

// notLetIn tells a device that isn't let in how to be.
func notLetIn(w http.ResponseWriter, remote string) {
	w.WriteHeader(http.StatusForbidden)
	a := html.EscapeString(remote)
	page(w, "Not let in yet", `<h1>This box opens your files only on your own devices</h1>
<p>This device's Yggdrasil address is <code>`+a+`</code>.</p>
<p>If it's yours, ask to let it in:</p>
<form method="post" action="/files/ask">
<p><input name="name" placeholder="Name this device, e.g. my phone" maxlength="60" style="padding:.5em;width:18em"></p>
<p><input name="who" placeholder="Your name, for the box's owner" maxlength="60" style="padding:.5em;width:18em"></p>
<button style="padding:.5em 1em">Ask to let it in</button></form>
<p>Then press <b>Allow</b> under <b>Your devices</b> on a device of yours that opens your files already.
For your first device, open the invite link the box's owner sent you, on this device.</p>`)
}

func page(w http.ResponseWriter, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintf(w, `<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width">
<title>%s</title><body style="font-family:system-ui,sans-serif;max-width:40em;margin:3em auto;padding:0 1em;line-height:1.5">%s</body></html>`,
		html.EscapeString(title), body)
}

// devices is the person's own list: the devices that open their files, and
// the devices asking.
func (f *filesPage) devices(w http.ResponseWriter, r *http.Request, p Person, remote string) {
	op := r.URL.Query().Get("op")
	if op == "" && r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(devicesHTML)
		return
	}
	if op == "list" && r.Method == http.MethodGet {
		st, err := Load(f.u.cfg.StatePath)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		st.PruneAsks(time.Now())
		p, _ = st.PersonNamed(p.Name)
		out := map[string]any{"person": p.Name, "you": remote, "devices": p.FilesDevices, "asks": st.Asks}
		if p.FilesDevices == nil {
			out["devices"] = []devices.Device{}
		}
		if st.Asks == nil {
			out["asks"] = []DeviceAsk{}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(out)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct{ Addr, Name string }
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ip, err := devices.Parse(req.Addr)
	if err != nil && op != "remove" {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var msg string
	_, err = Update(r.Context(), f.u.cfg.StatePath, func(s *HubState) error {
		switch op {
		case "allow":
			d, err := s.LetIn(p.Name, ip, req.Name)
			msg = fmt.Sprintf("%s (%s) opens %s's files now", d.Name, d.Addr, p.Name)
			return err
		case "remove":
			if ip == remote {
				return errors.New("this is the device you're using: remove it from another one")
			}
			_, d, err := s.LetOut(p.Name, req.Addr)
			msg = fmt.Sprintf("%s (%s) can't open %s's files any more", d.Name, d.Addr, p.Name)
			return err
		case "dismiss":
			s.Asks = dropAsk(s.Asks, ip)
			return nil
		}
		return errors.New("unknown request")
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if msg != "" {
		f.u.cfg.Logf("hub: %s, from %s", msg, remote)
	}
	w.WriteHeader(http.StatusNoContent)
}
