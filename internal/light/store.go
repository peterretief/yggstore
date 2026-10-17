package light

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/peterretief/yggstore/internal/atomicfile"
	"github.com/peterretief/yggstore/internal/passkey"
	"github.com/peterretief/yggstore/internal/peers"
)

// Signing in happens on the web nodes, so it works while any one of them
// is up, whichever box is off (docs/web-access.md, "Always on"). Each box
// sends every web node its people's keys (public halves and PIN hashes
// only) when they change; the web node checks sign-ins against them, keeps
// the sessions, and passes each new one to the other web nodes. Relaying a
// signed-in request to the box, it names the person in PersonHeader, which
// the box takes only from the group's web nodes.

// PersonHeader names whom a web node signed in, on a request it relays.
const PersonHeader = "X-Yggstore-Person"

// KindBrowser marks a key a browser keeps (see passkey.VerifyBrowser), for
// a device that can't make a passkey, such as a phone without a screen
// lock. It needs a PIN as well.
const KindBrowser = "browser"

// SessionFor is how long a session lasts unused, and ShortSessionFor one
// started with a browser key, after which the PIN is asked again.
const (
	SessionFor      = 30 * 24 * time.Hour
	ShortSessionFor = time.Hour
)

// Key is one of a person's passkeys or browser keys, as a box tells the
// web nodes.
type Key struct {
	passkey.Key
	RP       string `json:"rp,omitempty"`   // the address it signs in at
	Kind     string `json:"kind,omitempty"` // "" or KindBrowser
	Name     string `json:"name"`           // the device, as they named it
	PIN      string `json:"pin,omitempty"`  // a browser key's PIN hash (passkey.PINHash)
	PINFails int    `json:"pin_fails,omitempty"`
	Added    int64  `json:"added,omitempty"`
	Used     int64  `json:"used,omitempty"`
}

// Person is someone on a box who may sign in on the web.
type Person struct {
	Name string `json:"name"`
	Web  bool   `json:"web"` // may sign in on the web now
	Keys []Key  `json:"keys"`
	// Recovery is their recovery ID: the web nodes' record of their
	// latest recovery copy, for their files while the box is off.
	Recovery string `json:"recovery,omitempty"`
}

// People is what a box sends the web nodes: everyone on it with a key.
type People struct {
	People []Person `json:"people"`
}

// Session is a browser signed in.
type Session struct {
	Hash    string `json:"hash"` // of the cookie's token
	Box     string `json:"box"`
	Person  string `json:"person"`
	Key     string `json:"key"`    // the key's ID
	Device  string `json:"device"` // the key's name
	Host    string `json:"host"`   // the address it was started at
	Started int64  `json:"started"`
	Used    int64  `json:"used"`
	Short   bool   `json:"short,omitempty"` // started with a browser key
}

// For is how long the session lasts unused.
func (ss Session) For() time.Duration {
	if ss.Short {
		return ShortSessionFor
	}
	return SessionFor
}

func (ss Session) live(now time.Time) bool { return now.Sub(time.Unix(ss.Used, 0)) < ss.For() }

// Store keeps the web node's side of signing in.
type Store struct {
	Dir   string
	Peers func() []peers.Peer
	// Others are the group's other web nodes (node API addresses), which
	// share sessions and wrong PINs.
	Others func(ctx context.Context) []string
	// Client reaches them over Yggdrasil.
	Client *http.Client
	Log    func(format string, args ...any)

	challenges passkey.Challenges

	mu     sync.Mutex
	loaded bool
	st     storeState

	webMu sync.Mutex
	web   map[string]bool // the other web nodes' addresses
	webAt time.Time
}

type storeState struct {
	Boxes    map[string][]Person `json:"boxes"` // by box name
	Sessions []Session           `json:"sessions"`
	// Fails are wrong PINs in a row, per key ID, as the web nodes saw
	// them since the box last said.
	Fails map[string]int `json:"fails,omitempty"`
}

func (s *Store) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log(format, args...)
	}
}

func (s *Store) path() string { return filepath.Join(s.Dir, "light.json") }

// lock locks the store, reading it the first time.
func (s *Store) lock() {
	s.mu.Lock()
	if s.loaded {
		return
	}
	s.loaded = true
	if b, err := os.ReadFile(s.path()); err == nil {
		if err := json.Unmarshal(b, &s.st); err != nil {
			s.logf("light: %s: %v", s.path(), err)
		}
	}
	if s.st.Boxes == nil {
		s.st.Boxes = map[string][]Person{}
	}
	if s.st.Fails == nil {
		s.st.Fails = map[string]int{}
	}
}

// saveLocked writes the store; s.mu is held.
func (s *Store) saveLocked() {
	b, _ := json.Marshal(s.st)
	err := os.MkdirAll(s.Dir, 0o700)
	if err == nil {
		err = atomicfile.Replace(s.path(), b, 0o600)
	}
	if err != nil {
		s.logf("light: keeping sign-ins: %v", err)
	}
}

// found is a key and whose it is.
type found struct {
	Box    string
	Person Person
	Key    Key
	Fails  int // wrong PINs in a row, counting what the web nodes saw
}

// key finds a key by its ID.
func (s *Store) key(id string) (found, bool) {
	s.lock()
	defer s.mu.Unlock()
	for box, ps := range s.st.Boxes {
		for _, p := range ps {
			for _, k := range p.Keys {
				if k.ID == id {
					return found{box, p, k, max(k.PINFails, s.st.Fails[id])}, true
				}
			}
		}
	}
	return found{}, false
}

// SetPeople takes a box's people, ending the sessions of anyone it no
// longer lets in, or whose key is gone.
func (s *Store) SetPeople(box string, ps []Person) {
	s.lock()
	defer s.mu.Unlock()
	s.st.Boxes[box] = ps
	for _, p := range ps {
		for _, k := range p.Keys {
			if f, ok := s.st.Fails[k.ID]; ok && k.PINFails >= f {
				delete(s.st.Fails, k.ID) // the box has them now
			}
		}
	}
	s.pruneLocked(time.Now())
	s.saveLocked()
}

// allowedLocked is whether ss's person and key still let it in.
func (s *Store) allowedLocked(ss Session) bool {
	for _, p := range s.st.Boxes[ss.Box] {
		if !strings.EqualFold(p.Name, ss.Person) {
			continue
		}
		for _, k := range p.Keys {
			if k.ID == ss.Key {
				return p.Web
			}
		}
	}
	return false
}

func (s *Store) pruneLocked(now time.Time) {
	kept := s.st.Sessions[:0]
	for _, ss := range s.st.Sessions {
		if ss.live(now) && s.allowedLocked(ss) {
			kept = append(kept, ss)
		}
	}
	s.st.Sessions = kept
}

func tokenHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// newSession signs f's person in with f's key, at host. It gives the
// cookie's token, which only the browser keeps.
func (s *Store) newSession(f found, host string, now time.Time) (string, Session) {
	b := make([]byte, 32)
	rand.Read(b)
	token := passkey.B64.EncodeToString(b)
	ss := Session{Hash: tokenHash(token), Box: f.Box, Person: f.Person.Name, Key: f.Key.ID, Device: f.Key.Name, Host: host,
		Started: now.Unix(), Used: now.Unix(), Short: f.Key.Kind == KindBrowser}
	s.lock()
	s.pruneLocked(now)
	s.st.Sessions = append(s.st.Sessions, ss)
	s.saveLocked()
	s.mu.Unlock()
	go s.tellOthers(http.MethodPut, "/v1/light/session", ss)
	return token, ss
}

// addLocked keeps a session another web node started, or the later use of
// one it has.
func (s *Store) addLocked(ss Session) {
	for i := range s.st.Sessions {
		if s.st.Sessions[i].Hash == ss.Hash {
			s.st.Sessions[i].Used = max(s.st.Sessions[i].Used, ss.Used)
			return
		}
	}
	s.st.Sessions = append(s.st.Sessions, ss)
}

// session is who is signed in with token at host. A session this web node
// doesn't know (or thinks unused for too long) may be another's: it asks
// them.
func (s *Store) session(ctx context.Context, token, host string, now time.Time) (Session, bool) {
	if token == "" {
		return Session{}, false
	}
	h := tokenHash(token)
	get := func() (Session, bool) {
		s.lock()
		defer s.mu.Unlock()
		for _, ss := range s.st.Sessions {
			if ss.Hash == h && ss.live(now) && strings.EqualFold(ss.Host, host) && s.allowedLocked(ss) {
				return ss, true
			}
		}
		return Session{}, false
	}
	if ss, ok := get(); ok {
		return ss, true
	}
	for _, ss := range s.askOthers(ctx, h) {
		if ss.Hash == h {
			s.lock()
			s.addLocked(ss)
			s.saveLocked()
			s.mu.Unlock()
		}
	}
	return get()
}

// touch notes that a session is in use. The other web nodes hear of it
// now and then, so a session in use doesn't end on them.
func (s *Store) touch(ss Session, now time.Time) bool {
	if now.Sub(time.Unix(ss.Used, 0)) < ss.For()/24 {
		return false
	}
	s.lock()
	for i := range s.st.Sessions {
		if s.st.Sessions[i].Hash == ss.Hash {
			s.st.Sessions[i].Used = now.Unix()
			ss = s.st.Sessions[i]
		}
	}
	s.saveLocked()
	s.mu.Unlock()
	go s.tellOthers(http.MethodPut, "/v1/light/session", ss)
	return true
}

// end signs a session out, here and on the other web nodes.
func (s *Store) end(hash string, tell bool) {
	s.lock()
	kept := s.st.Sessions[:0]
	for _, ss := range s.st.Sessions {
		if ss.Hash != hash {
			kept = append(kept, ss)
		}
	}
	s.st.Sessions = kept
	s.saveLocked()
	s.mu.Unlock()
	if tell {
		go s.tellOthers(http.MethodDelete, "/v1/light/session/"+hash, nil)
	}
}

// sessionsOf are a person's sessions.
func (s *Store) sessionsOf(box, person string) []Session {
	s.lock()
	defer s.mu.Unlock()
	out := []Session{}
	for _, ss := range s.st.Sessions {
		if ss.Box == box && strings.EqualFold(ss.Person, person) && ss.live(time.Now()) {
			out = append(out, ss)
		}
	}
	return out
}

// setFails notes a key's wrong PINs in a row (0 after a right one).
func (s *Store) setFails(id string, n int, tell bool) {
	s.lock()
	if n == 0 {
		delete(s.st.Fails, id)
	} else {
		s.st.Fails[id] = max(n, s.st.Fails[id])
	}
	s.saveLocked()
	s.mu.Unlock()
	if tell {
		go s.tellOthers(http.MethodPost, "/v1/light/fails", map[string]any{"key": id, "fails": n})
	}
}

// setPIN keeps a PIN chosen for a browser key the box has said yes to.
func (s *Store) setPIN(box, id, hash string) {
	s.lock()
	defer s.mu.Unlock()
	for i := range s.st.Boxes[box] {
		for j := range s.st.Boxes[box][i].Keys {
			if k := &s.st.Boxes[box][i].Keys[j]; k.ID == id {
				k.PIN = hash
			}
		}
	}
	s.saveLocked()
}

// addKey keeps a key a box has just made, before the box's own word of it
// arrives.
func (s *Store) addKey(f found) {
	s.lock()
	defer s.mu.Unlock()
	ps := s.st.Boxes[f.Box]
	for i := range ps {
		if strings.EqualFold(ps[i].Name, f.Person.Name) {
			for _, k := range ps[i].Keys {
				if k.ID == f.Key.ID {
					return
				}
			}
			ps[i].Keys = append(ps[i].Keys, f.Key)
			s.saveLocked()
			return
		}
	}
	f.Person.Keys = []Key{f.Key}
	s.st.Boxes[f.Box] = append(ps, f.Person)
	s.saveLocked()
}

// isWebNode is whether ip is one of the group's other web nodes.
func (s *Store) isWebNode(ctx context.Context, ip string) bool {
	if s.Others == nil {
		return false
	}
	s.webMu.Lock()
	defer s.webMu.Unlock()
	if s.web[ip] {
		return true
	}
	if time.Since(s.webAt) > 5*time.Minute || s.web == nil {
		s.web, s.webAt = map[string]bool{}, time.Now()
		for _, a := range s.Others(ctx) {
			if h, _, err := net.SplitHostPort(a); err == nil {
				if p := net.ParseIP(h); p != nil {
					s.web[p.String()] = true
				}
			}
		}
	}
	return s.web[ip]
}

// boxAt is the name of the box at ip: the first node at that address in
// the group's list.
func (s *Store) boxAt(ip string) string {
	for _, p := range s.Peers() {
		if p.IP() == ip {
			return p.Name
		}
	}
	return ""
}

func (s *Store) others(ctx context.Context) []string {
	if s.Others == nil || s.Client == nil {
		return nil
	}
	return s.Others(ctx)
}

// tellOthers sends the other web nodes a change.
func (s *Store) tellOthers(method, path string, v any) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var body []byte
	if v != nil {
		body, _ = json.Marshal(v)
	}
	for _, n := range s.others(ctx) {
		req, err := http.NewRequestWithContext(ctx, method, "http://"+n+path, bytes.NewReader(body))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := s.Client.Do(req)
		if err != nil {
			s.logf("light: telling %s: %v", n, err)
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

// askOthers asks the other web nodes for a session they may have.
func (s *Store) askOthers(ctx context.Context, hash string) []Session {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var out []Session
	for _, n := range s.others(ctx) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+n+"/v1/light/session/"+hash, nil)
		if err != nil {
			continue
		}
		resp, err := s.Client.Do(req)
		if err != nil {
			continue
		}
		var ss Session
		if resp.StatusCode == http.StatusOK && json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(&ss) == nil {
			out = append(out, ss)
		}
		resp.Body.Close()
	}
	return out
}

// Serve answers members on the node's API (/v1/light/…): boxes sending
// their people, and the other web nodes sharing sessions and wrong PINs.
func (s *Store) Serve(w http.ResponseWriter, r *http.Request, caller string) bool {
	path, ok := strings.CutPrefix(r.URL.Path, "/v1/light/")
	if !ok {
		return false
	}
	if path == "people" {
		box := s.boxAt(caller)
		if box == "" || r.Method != http.MethodPut {
			http.Error(w, "only a box sends its people", http.StatusForbidden)
			return true
		}
		var ps People
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&ps); err != nil {
			http.Error(w, "bad people", http.StatusBadRequest)
			return true
		}
		s.SetPeople(box, ps.People)
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	if !s.isWebNode(r.Context(), caller) {
		http.Error(w, "only the group's web nodes", http.StatusForbidden)
		return true
	}
	switch hash, isSession := strings.CutPrefix(path, "session/"); {
	case path == "session" && r.Method == http.MethodPut:
		var ss Session
		if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&ss); err != nil || len(ss.Hash) != 64 {
			http.Error(w, "bad session", http.StatusBadRequest)
			return true
		}
		s.lock()
		s.addLocked(ss)
		s.saveLocked()
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case isSession && r.Method == http.MethodGet:
		s.lock()
		var got *Session
		for _, ss := range s.st.Sessions {
			if ss.Hash == hash {
				got = &ss
			}
		}
		s.mu.Unlock()
		if got == nil {
			http.NotFound(w, r)
			return true
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(got)
	case isSession && r.Method == http.MethodDelete:
		s.end(hash, false)
		w.WriteHeader(http.StatusNoContent)
	case path == "fails" && r.Method == http.MethodPost:
		var req struct {
			Key   string `json:"key"`
			Fails int    `json:"fails"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil || req.Key == "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return true
		}
		s.setFails(req.Key, req.Fails, false)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
	return true
}

// SendPeople sends a web node a box's people (from the box, over
// Yggdrasil).
func SendPeople(ctx context.Context, c *http.Client, node string, ps People) error {
	b, _ := json.Marshal(ps)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://"+node+"/v1/light/people", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

var errNoKey = errors.New("This key isn't one the group knows. Use the one you made with your invite, or ask the box's owner for a new invite.")
