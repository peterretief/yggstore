// Package workcopy keeps track of where a person's working copy is: the
// place (a computer, or a box's folder for a device) where their files are
// stored, restored and deleted. A person can work from several places, but
// from one at a time, so two places never change the same files.
//
// Each person has a topic in the group's messaging, named from their
// sharing code, which every place holding their sharing key follows. Notes
// on it are sealed with that key, so only the person's own places can
// write them:
//
//   - holding: this place has the working copy;
//   - copy: a new recovery copy was saved (its stub is in the note), so any
//     place can take over from the latest one;
//   - request: another place wants the working copy;
//   - released: the holder saved a final copy and handed over to a place.
//
// The newest holding or released note says who holds it. The other places
// stop changing files until they are handed the working copy.
package workcopy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/peterretief/yggstore/internal/manifest"
	"github.com/peterretief/yggstore/internal/msg"
	"github.com/peterretief/yggstore/internal/share"
)

// Kinds of note.
const (
	Holding  = "holding"
	CopyNote = "copy"
	Request  = "request"
	Released = "released"
)

// Note is one note on a person's topic.
type Note struct {
	Kind  string `json:"kind"`
	Place string `json:"place"`        // the place that wrote it
	To    string `json:"to,omitempty"` // released: the place handed to
	At    int64  `json:"at"`           // unix milliseconds
	// Stub is the recovery copy's stub, for copy and released.
	Stub json.RawMessage `json:"stub,omitempty"`
}

// Bus is the group's messaging, as this machine's node offers it.
type Bus interface {
	Subscribe(ctx context.Context, topic string) error
	Messages(ctx context.Context, after int64, topic string, limit int) ([]msg.Stored, error)
	Publish(ctx context.Context, topic, typ, body string) (msg.Message, error)
}

type Config struct {
	Bus   func() (Bus, error)
	ID    func() *share.Identity // the person's sharing key
	Place func() string          // this place's name
	// SaveNow saves a final recovery copy, for handing over.
	SaveNow  func(ctx context.Context) (manifest.Manifest, error)
	Event    func(kind, msg string)
	Interval time.Duration // how often to read new notes (5 s)
	Now      func() time.Time
}

// State is where the working copy is, as this place last heard.
type State struct {
	Holder string `json:"holder,omitempty"` // the place holding it; "" until known
	Since  int64  `json:"since,omitempty"`  // unix milliseconds
	Mine   bool   `json:"mine"`             // this place holds it
	// Wanted is a place that asked for it and hasn't been handed it yet.
	Wanted string `json:"wanted,omitempty"`
	// LastCopy is when the newest recovery copy was saved, and where.
	LastCopy      int64  `json:"last_copy,omitempty"`
	LastCopyPlace string `json:"last_copy_place,omitempty"`
	Error         string `json:"error,omitempty"`
}

type Tracker struct {
	cfg Config

	mu      sync.Mutex
	topic   string
	after   int64
	notes   []Note
	state   State
	ready   bool // the topic's history has been read
	latest  *Note
	handled map[int64]bool // requests answered, by note time
	wake    chan struct{}
}

func New(cfg Config) *Tracker {
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Event == nil {
		cfg.Event = func(string, string) {}
	}
	return &Tracker{cfg: cfg, handled: map[int64]bool{}, wake: make(chan struct{}, 1)}
}

// Topic is the person's topic: "workcopy/" and a hash of their sharing code.
func Topic(code string) string {
	h := sha256.Sum256([]byte(code))
	return "workcopy/" + hex.EncodeToString(h[:8])
}

// State says where the working copy is.
func (t *Tracker) State() State {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state
}

// Holding is whether this place may change files. Until the topic has been
// read it says yes, so a place that can't reach its node works as before.
func (t *Tracker) Holding() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return !t.ready || t.state.Holder == "" || t.state.Mine
}

// Latest is the newest recovery copy's stub any place announced.
func (t *Tracker) Latest() (manifest.Manifest, bool) {
	t.mu.Lock()
	n := t.latest
	t.mu.Unlock()
	if n == nil {
		return manifest.Manifest{}, false
	}
	m, err := manifest.Unmarshal(n.Stub)
	return m, err == nil
}

// Run reads the topic until ctx ends.
func (t *Tracker) Run(ctx context.Context) {
	tick := time.NewTicker(t.cfg.Interval)
	defer tick.Stop()
	for {
		if err := t.Step(ctx); err != nil {
			t.setError(err.Error())
		} else {
			t.setError("")
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-t.wake:
		}
	}
}

func (t *Tracker) setError(e string) {
	t.mu.Lock()
	t.state.Error = e
	t.mu.Unlock()
}

// Step reads new notes and acts on them: answering a request when this
// place holds the working copy, and claiming it when nobody does yet.
func (t *Tracker) Step(ctx context.Context) error {
	id := t.cfg.ID()
	if id == nil {
		return errors.New("no sharing key")
	}
	bus, err := t.cfg.Bus()
	if err != nil {
		return err
	}
	topic := Topic(id.Code())
	t.mu.Lock()
	if t.topic != topic { // a new sharing key: start again
		t.topic, t.after, t.notes, t.latest, t.ready = topic, 0, nil, nil, false
		t.state = State{}
	}
	after := t.after
	t.mu.Unlock()
	if after == 0 {
		if err := bus.Subscribe(ctx, topic); err != nil {
			return err
		}
	}
	for {
		got, err := bus.Messages(ctx, after, topic, 500)
		if err != nil {
			return err
		}
		for _, s := range got {
			after = max(after, s.N)
			n, err := open(id, s.Body)
			if err != nil {
				continue // not written with the person's key
			}
			t.mu.Lock()
			t.notes = append(t.notes, n)
			t.mu.Unlock()
		}
		if len(got) < 500 {
			break
		}
	}
	t.mu.Lock()
	t.after, t.ready = after, true
	t.mu.Unlock()
	t.fold()

	place := t.cfg.Place()
	st := t.State()
	switch {
	case st.Holder == "":
		// Nobody holds it: the first place to look does.
		return t.publish(ctx, bus, id, Note{Kind: Holding, Place: place})
	case st.Mine && st.Wanted != "":
		return t.handOver(ctx, bus, id, place, st.Wanted)
	case st.Holder == place && !st.Mine:
		// Handed to us (released), not yet claimed: Take does that.
	}
	return nil
}

// fold works out the state from the notes, in the order they were written.
func (t *Tracker) fold() {
	place := t.cfg.Place()
	t.mu.Lock()
	defer t.mu.Unlock()
	notes := append([]Note(nil), t.notes...)
	sort.SliceStable(notes, func(i, j int) bool { return notes[i].At < notes[j].At })
	var st State
	var latest *Note
	var lastRequest *Note
	for i := range notes {
		n := notes[i]
		switch n.Kind {
		case Holding:
			st.Holder, st.Since = n.Place, n.At
			lastRequest = nil
		case Released:
			st.Holder, st.Since = n.To, n.At
			lastRequest = nil
		case Request:
			if n.Place != st.Holder {
				lastRequest = &notes[i]
			}
		}
		if (n.Kind == CopyNote || n.Kind == Released) && len(n.Stub) > 0 {
			latest = &notes[i]
		}
	}
	st.Mine = st.Holder != "" && st.Holder == place
	if released := lastReleased(notes); st.Mine && released != nil && released.To == place && st.Since == released.At {
		st.Mine = false // handed to us, but not unpacked and claimed yet
	}
	if lastRequest != nil && !t.handled[lastRequest.At] {
		st.Wanted = lastRequest.Place
	}
	if latest != nil {
		st.LastCopy, st.LastCopyPlace = latest.At, latest.Place
	}
	st.Error = t.state.Error
	t.state, t.latest = st, latest
}

func lastReleased(notes []Note) *Note {
	for i := len(notes) - 1; i >= 0; i-- {
		switch notes[i].Kind {
		case Holding:
			return nil
		case Released:
			return &notes[i]
		}
	}
	return nil
}

// handOver saves a final copy and hands the working copy to place to.
func (t *Tracker) handOver(ctx context.Context, bus Bus, id *share.Identity, place, to string) error {
	t.mu.Lock()
	var at int64
	for _, n := range t.notes {
		if n.Kind == Request && n.Place == to && n.At > at {
			at = n.At
		}
	}
	t.handled[at] = true
	t.mu.Unlock()
	m, err := t.cfg.SaveNow(ctx)
	if err != nil {
		t.mu.Lock()
		delete(t.handled, at)
		t.mu.Unlock()
		return fmt.Errorf("saving a final copy for %s: %w", to, err)
	}
	stub, err := manifest.Marshal(m)
	if err != nil {
		return err
	}
	if err := t.publish(ctx, bus, id, Note{Kind: Released, Place: place, To: to, Stub: stub}); err != nil {
		return err
	}
	t.cfg.Event("info", fmt.Sprintf("your files are now worked on at %s: this place has stopped changing them", to))
	return nil
}

// Copied announces a new recovery copy, so any place can take over from it.
func (t *Tracker) Copied(ctx context.Context, m manifest.Manifest) error {
	if !t.State().Mine {
		return nil
	}
	stub, err := manifest.Marshal(m)
	if err != nil {
		return err
	}
	bus, err := t.cfg.Bus()
	if err != nil {
		return err
	}
	return t.publish(ctx, bus, t.cfg.ID(), Note{Kind: CopyNote, Place: t.cfg.Place(), Stub: stub})
}

// Ask asks the holder to hand the working copy to this place.
func (t *Tracker) Ask(ctx context.Context) error {
	bus, err := t.cfg.Bus()
	if err != nil {
		return err
	}
	return t.publish(ctx, bus, t.cfg.ID(), Note{Kind: Request, Place: t.cfg.Place()})
}

// Claim says this place now holds the working copy (after unpacking it).
func (t *Tracker) Claim(ctx context.Context) error {
	bus, err := t.cfg.Bus()
	if err != nil {
		return err
	}
	return t.publish(ctx, bus, t.cfg.ID(), Note{Kind: Holding, Place: t.cfg.Place()})
}

// HandedHere is the stub of the copy the holder handed to this place, if
// it did and this place hasn't claimed it yet.
func (t *Tracker) HandedHere() (manifest.Manifest, bool) {
	place := t.cfg.Place()
	t.mu.Lock()
	notes := append([]Note(nil), t.notes...)
	t.mu.Unlock()
	sort.SliceStable(notes, func(i, j int) bool { return notes[i].At < notes[j].At })
	r := lastReleased(notes)
	if r == nil || r.To != place {
		return manifest.Manifest{}, false
	}
	m, err := manifest.Unmarshal(r.Stub)
	return m, err == nil
}

func (t *Tracker) publish(ctx context.Context, bus Bus, id *share.Identity, n Note) error {
	if id == nil {
		return errors.New("no sharing key")
	}
	n.At = t.cfg.Now().UnixMilli()
	body, err := seal(id, n)
	if err != nil {
		return err
	}
	t.mu.Lock()
	topic := t.topic
	t.mu.Unlock()
	if topic == "" {
		topic = Topic(id.Code())
	}
	if _, err := bus.Publish(ctx, topic, "application/json", body); err != nil {
		return err
	}
	// Our own notes count at once, without waiting to read them back.
	t.mu.Lock()
	t.notes = append(t.notes, n)
	t.mu.Unlock()
	t.fold()
	select {
	case t.wake <- struct{}{}:
	default:
	}
	return nil
}

// seal writes a note sealed for the person themselves: only someone with
// their sharing key can write or read one.
func seal(id *share.Identity, n Note) (string, error) {
	b, err := json.Marshal(n)
	if err != nil {
		return "", err
	}
	s, err := id.Seal(id.Code(), b, "", "")
	return string(s), err
}

func open(id *share.Identity, body string) (Note, error) {
	r, err := id.Open([]byte(body))
	if err != nil {
		return Note{}, err
	}
	if r.From != id.Code() {
		return Note{}, errors.New("not from this person")
	}
	var n Note
	if err := json.Unmarshal(r.Stub, &n); err != nil {
		return Note{}, err
	}
	return n, nil
}
