package sitemove

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/peterretief/yggstore/internal/atomicfile"
	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/msg"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/share"
	"github.com/peterretief/yggstore/internal/site"
)

// Bus is the node's messaging.
type Bus interface {
	Send(ctx context.Context, to, typ, body string) (msg.Message, error)
	Messages(ctx context.Context, after int64, topic string, limit int) ([]msg.Stored, error)
}

// Holder is what a machine keeps sites for: the node itself (person ""),
// or the people on its box.
type Holder interface {
	// Handles is whether this holder keeps sites for person here: a
	// node's dashboard for "", a box's hub for its people.
	Handles(person string) bool
	// Publisher keeps person's sites.
	Publisher(person string) (*site.Publisher, error)
	// Identity is the sharing key person's sites go with: the contact
	// form's messages are sealed for it.
	Identity(person string) (*share.Identity, error)
	// Name is person as this machine calls them ("" for the node).
	Name(person string) string
	// CanTake says why person can't take a site here, if they can't:
	// they're shut out, say, or the machine collects no mail for the
	// site's contact form.
	CanTake(person string) error
}

// Config is one machine's part in moves.
type Config struct {
	Self      string // this node's ID
	Peers     func() []peers.Peer
	Bus       func() (Bus, error)
	Client    client.Client
	Online    func(ctx context.Context) ([]peers.Peer, error)
	StatePath string
	Holder    Holder // nil: holds no sites (an admin's only part)
	// Give, on an admin's machine, gives a moved site's name to its new
	// holder in the group's list and sends the list out.
	Give func(m Move, r Ready) error
	Logf func(format string, args ...any)
	Now  func() time.Time
}

// Role is a machine's part in a move.
const (
	RoleAdmin = "admin"
	RoleOld   = "old"
	RoleNew   = "new"
)

// Pending is a move as one machine has it.
type Pending struct {
	Move
	Role    string `json:"role"`
	Admin   string `json:"admin,omitempty"` // who started it
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Updated int64  `json:"updated"` // unix
}

// Statuses, by role:
//
//	admin: asked → named → done, or refused
//	old:   waiting → handed → done, or refused
//	new:   wanted → ready → done, or refused
const (
	StatusAsked   = "asked"
	StatusNamed   = "named"
	StatusWaiting = "waiting"
	StatusHanded  = "handed"
	StatusWanted  = "wanted"
	StatusReady   = "ready"
	StatusDone    = "done"
	StatusRefused = "refused"
)

type agentState struct {
	After int64     `json:"after"` // last direct message read
	Moves []Pending `json:"moves"`
}

// Agent plays this machine's part in moves.
type Agent struct {
	cfg Config
	mu  sync.Mutex
}

func NewAgent(cfg Config) *Agent {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Agent{cfg: cfg}
}

func (a *Agent) load() agentState {
	var st agentState
	if b, err := os.ReadFile(a.cfg.StatePath); err == nil {
		json.Unmarshal(b, &st)
	}
	return st
}

func (a *Agent) save(st agentState) {
	// Finished moves are kept a week, for the admin's page.
	cut := a.cfg.Now().Add(-7 * 24 * time.Hour).Unix()
	st.Moves = slices.DeleteFunc(st.Moves, func(p Pending) bool {
		return (p.Status == StatusDone || p.Status == StatusRefused) && p.Updated < cut
	})
	b, _ := json.MarshalIndent(st, "", "  ")
	os.MkdirAll(filepath.Dir(a.cfg.StatePath), 0o700)
	if err := atomicfile.Replace(a.cfg.StatePath, append(b, '\n'), 0o600); err != nil {
		a.cfg.Logf("moves: saving %s: %v", a.cfg.StatePath, err)
	}
}

// Moves lists the moves this machine has a part in, newest first.
func (a *Agent) Moves() []Pending {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := a.load().Moves
	slices.SortFunc(out, func(x, y Pending) int { return int(y.Time - x.Time) })
	return out
}

// inbound is a message to handle: from the bus, or this machine's own.
type inbound struct {
	from, typ, body string
}

// Run plays the part every few seconds until ctx ends.
func (a *Agent) Run(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		a.Step(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Step reads new messages and moves every move on as far as it goes.
func (a *Agent) Step(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.load()
	bus, err := a.cfg.Bus()
	var queue []inbound
	if err == nil {
		mctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		msgs, err := bus.Messages(mctx, st.After, "direct", 500)
		cancel()
		if err == nil {
			for _, m := range msgs {
				st.After = max(st.After, m.N)
				if strings.HasPrefix(m.Type, "site-") {
					queue = append(queue, inbound{m.From, m.Type, m.Body})
				}
			}
		}
	}
	for len(queue) > 0 {
		in := queue[0]
		queue = queue[1:]
		queue = append(queue, a.handle(ctx, &st, bus, in)...)
	}
	queue = a.finish(ctx, &st, bus)
	for len(queue) > 0 {
		in := queue[0]
		queue = queue[1:]
		queue = append(queue, a.handle(ctx, &st, bus, in)...)
	}
	a.save(st)
}

// Start begins a move, on the admin's machine.
func (a *Agent) Start(ctx context.Context, m Move) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.isAdmin(a.cfg.Self) {
		return errors.New("only the group's admin moves sites")
	}
	st := a.load()
	for _, p := range st.Moves {
		if p.Site == m.Site && p.Status != StatusDone && p.Status != StatusRefused {
			return fmt.Errorf("%s is moving already", m.Site)
		}
	}
	if m.From == m.To && strings.EqualFold(m.FromPerson, m.ToPerson) {
		return fmt.Errorf("%s is there already", m.Site)
	}
	if _, ok := a.node(m.To); !ok {
		return errors.New("that machine isn't in the group")
	}
	m.Time = a.cfg.Now().UnixMilli()
	st.Moves = append(st.Moves, Pending{Move: m, Role: RoleAdmin, Admin: a.cfg.Self, Status: StatusAsked, Updated: a.cfg.Now().Unix()})
	bus, _ := a.cfg.Bus()
	var queue []inbound
	for _, to := range []string{m.From, m.To} {
		if q := a.send(ctx, bus, to, TypeMove, Body(m)); q != nil {
			queue = append(queue, *q)
		}
		if m.From == m.To {
			break // one machine has both parts
		}
	}
	for len(queue) > 0 {
		in := queue[0]
		queue = queue[1:]
		queue = append(queue, a.handle(ctx, &st, bus, in)...)
	}
	a.save(st)
	a.cfg.Logf("moves: moving %s to %s", m.Site, a.Describe(m.To, m.ToPerson))
	return nil
}

// send sends a message, or hands it back to be handled here when it is
// for this machine.
func (a *Agent) send(ctx context.Context, bus Bus, to, typ, body string) *inbound {
	if to == a.cfg.Self {
		return &inbound{a.cfg.Self, typ, body}
	}
	if bus == nil {
		a.cfg.Logf("moves: can't send %s: no messaging", typ)
		return nil
	}
	if _, err := bus.Send(ctx, to, typ, body); err != nil {
		a.cfg.Logf("moves: sending %s to %s: %v", typ, a.Describe(to, ""), err)
	}
	return nil
}

func (a *Agent) node(id string) (peers.Peer, bool) {
	for _, p := range a.cfg.Peers() {
		if p.IP() == id {
			return p, true
		}
	}
	return peers.Peer{}, false
}

func (a *Agent) isAdmin(id string) bool {
	for _, p := range a.cfg.Peers() {
		if p.IP() == id && p.Admin {
			return true
		}
	}
	return false
}

// Describe is a holder as people know it: "Tim on localmail".
func (a *Agent) Describe(node, person string) string {
	name := node
	if p, ok := a.node(node); ok {
		name = p.Name
	}
	if person != "" {
		return person + " on " + name
	}
	return name
}

func find(st *agentState, site, role string) *Pending {
	for i := len(st.Moves) - 1; i >= 0; i-- {
		p := &st.Moves[i]
		if p.Site == site && p.Role == role && p.Status != StatusDone && p.Status != StatusRefused {
			return p
		}
	}
	return nil
}

func (a *Agent) set(p *Pending, status, reason string) {
	p.Status, p.Reason, p.Updated = status, reason, a.cfg.Now().Unix()
}

// refuse ends this machine's part in a move and tells the admin why.
func (a *Agent) refuse(ctx context.Context, bus Bus, p *Pending, reason string) []inbound {
	a.set(p, StatusRefused, reason)
	a.cfg.Logf("moves: %s can't move: %s", p.Site, reason)
	if q := a.send(ctx, bus, p.Admin, TypeRefused, Body(Refused{Site: p.Site, Reason: reason})); q != nil {
		return []inbound{*q}
	}
	return nil
}

func (a *Agent) handle(ctx context.Context, st *agentState, bus Bus, in inbound) []inbound {
	var out []inbound
	add := func(q *inbound) {
		if q != nil {
			out = append(out, *q)
		}
	}
	switch in.typ {
	case TypeMove:
		var m Move
		if json.Unmarshal([]byte(in.body), &m) != nil || site.ValidName(m.Site) != nil {
			return nil
		}
		if !a.isAdmin(in.from) {
			a.cfg.Logf("moves: ignored a move of %s from %s, who isn't an admin", m.Site, a.Describe(in.from, ""))
			return nil
		}
		h := a.cfg.Holder
		if m.From == a.cfg.Self && h != nil && h.Handles(m.FromPerson) && find(st, m.Site, RoleOld) == nil {
			p := Pending{Move: m, Role: RoleOld, Admin: in.from}
			st.Moves = append(st.Moves, p)
			pp := &st.Moves[len(st.Moves)-1]
			pub, err := h.Publisher(m.FromPerson)
			if err == nil {
				_, err = Export(pub.Dir, m.Site)
			}
			if err != nil {
				out = append(out, a.refuse(ctx, bus, pp, fmt.Sprintf("%s doesn't publish it: %v", a.Describe(m.From, m.FromPerson), err))...)
			} else {
				a.set(pp, StatusWaiting, "")
			}
		}
		if m.To == a.cfg.Self && h != nil && h.Handles(m.ToPerson) && find(st, m.Site, RoleNew) == nil {
			st.Moves = append(st.Moves, Pending{Move: m, Role: RoleNew, Admin: in.from})
			pp := &st.Moves[len(st.Moves)-1]
			id, err := h.Identity(m.ToPerson)
			if err == nil {
				err = h.CanTake(m.ToPerson)
			}
			if err != nil {
				out = append(out, a.refuse(ctx, bus, pp, fmt.Sprintf("%s can't take it: %v", a.Describe(m.To, m.ToPerson), err))...)
				break
			}
			a.set(pp, StatusWanted, "")
			add(a.send(ctx, bus, m.From, TypeWant, Body(Want{Site: m.Site, Code: id.Code()})))
		}

	case TypeWant:
		var w Want
		if json.Unmarshal([]byte(in.body), &w) != nil {
			return nil
		}
		p := find(st, w.Site, RoleOld)
		if p == nil || p.To != in.from || p.Status != StatusWaiting {
			return nil
		}
		h := a.cfg.Holder
		pub, err := h.Publisher(p.FromPerson)
		var b Bundle
		if err == nil {
			b, err = Export(pub.Dir, p.Site)
		}
		var id *share.Identity
		if err == nil {
			id, err = h.Identity(p.FromPerson)
		}
		var online []peers.Peer
		if err == nil {
			online, err = a.cfg.Online(ctx)
		}
		var sealed string
		if err == nil {
			sealed, err = Pack(ctx, a.cfg.Client, online, id, a.Describe(p.From, p.FromPerson), w.Code, b)
		}
		if err != nil {
			a.cfg.Logf("moves: handing %s over: %v (will try again when asked)", p.Site, err)
			return nil
		}
		a.set(p, StatusHanded, "")
		a.cfg.Logf("moves: handed %s's %d versions to %s", p.Site, len(b.Files), a.Describe(p.To, p.ToPerson))
		add(a.send(ctx, bus, p.To, TypeHandover, Body(Handover{Site: p.Site, Sealed: sealed})))

	case TypeHandover:
		var hv Handover
		if json.Unmarshal([]byte(in.body), &hv) != nil {
			return nil
		}
		p := find(st, hv.Site, RoleNew)
		if p == nil || p.From != in.from || p.Status != StatusWanted {
			return nil
		}
		h := a.cfg.Holder
		id, err := h.Identity(p.ToPerson)
		var pub *site.Publisher
		if err == nil {
			pub, err = h.Publisher(p.ToPerson)
		}
		if err != nil {
			out = append(out, a.refuse(ctx, bus, p, err.Error())...)
			break
		}
		b, m, err := Unpack(ctx, a.cfg.Client, id, hv.Sealed)
		if err != nil || b.Site != p.Site {
			a.cfg.Logf("moves: taking %s: %v", p.Site, err)
			out = append(out, a.refuse(ctx, bus, p, fmt.Sprintf("the versions didn't arrive: %v", err))...)
			break
		}
		if err := Import(pub.Dir, b, id.Code()); err != nil {
			out = append(out, a.refuse(ctx, bus, p, err.Error())...)
			break
		}
		Unmoved(pub.Dir, p.Site)
		Forget(ctx, a.cfg.Client, m)
		a.set(p, StatusReady, "")
		a.cfg.Logf("moves: have %s's %d versions; waiting for its name", p.Site, len(b.Files))
		add(a.send(ctx, bus, p.Admin, TypeReady, Body(Ready{Site: p.Site, Person: h.Name(p.ToPerson), Code: id.Code()})))

	case TypeReady:
		var r Ready
		if json.Unmarshal([]byte(in.body), &r) != nil {
			return nil
		}
		p := find(st, r.Site, RoleAdmin)
		if p == nil || p.To != in.from || p.Status != StatusAsked || !strings.EqualFold(r.Person, p.ToPerson) {
			return nil
		}
		if a.cfg.Give == nil {
			a.set(p, StatusRefused, "this machine can't give names")
			return nil
		}
		if r.Person != "" {
			p.ToPerson = r.Person // as the box calls them
		}
		if err := a.cfg.Give(p.Move, r); err != nil {
			a.set(p, StatusRefused, "giving the name: "+err.Error())
			a.cfg.Logf("moves: %s: giving the name to %s: %v", p.Site, a.Describe(p.To, p.ToPerson), err)
			return nil
		}
		a.set(p, StatusNamed, "")
		a.cfg.Logf("moves: %s is %s's now; waiting for it to be served from there", p.Site, a.Describe(p.To, p.ToPerson))

	case TypeMoved:
		var d Done
		if json.Unmarshal([]byte(in.body), &d) != nil {
			return nil
		}
		if p := find(st, d.Site, RoleAdmin); p != nil && p.To == in.from {
			a.set(p, StatusDone, "")
			a.cfg.Logf("moves: %s moved to %s", p.Site, a.Describe(p.To, p.ToPerson))
		}
		if p := find(st, d.Site, RoleOld); p != nil && p.To == in.from {
			if _, pn, ok := peers.NameHolder(a.cfg.Peers(), p.Site); ok && pn.Person != "" {
				p.ToPerson = pn.Person // as the box calls them
			}
			if pub, err := a.cfg.Holder.Publisher(p.FromPerson); err == nil {
				if err := LetGo(pub.Dir, p.Site, a.Describe(p.To, p.ToPerson), a.cfg.Now()); err != nil && !os.IsNotExist(err) {
					a.cfg.Logf("moves: letting %s go: %v", p.Site, err)
					return nil
				}
			}
			a.set(p, StatusDone, "")
			a.cfg.Logf("moves: %s is published from %s now; let it go here", p.Site, a.Describe(p.To, p.ToPerson))
		}

	case TypeRefused:
		var r Refused
		if json.Unmarshal([]byte(in.body), &r) != nil {
			return nil
		}
		if p := find(st, r.Site, RoleAdmin); p != nil && (in.from == p.From || in.from == p.To) {
			a.set(p, StatusRefused, r.Reason)
			a.cfg.Logf("moves: %s won't move: %s", p.Site, r.Reason)
			// Tell the other side, so it stops waiting.
			other := p.To
			if in.from == p.To {
				other = p.From
			}
			if other != in.from {
				add(a.send(ctx, bus, other, TypeRefused, Body(r)))
			}
		}
		for _, role := range []string{RoleOld, RoleNew} {
			if p := find(st, r.Site, role); p != nil && in.from == p.Admin {
				a.set(p, StatusRefused, r.Reason)
			}
		}
	}
	return out
}

// finish announces a site whose versions arrived here once the list gives
// this machine its name, and tells the old holder and the admin.
func (a *Agent) finish(ctx context.Context, st *agentState, bus Bus) []inbound {
	var out []inbound
	list := a.cfg.Peers()
	for i := range st.Moves {
		p := &st.Moves[i]
		if p.Role != RoleNew || p.Status != StatusReady || !a.holds(list, p.Site, p.ToPerson) {
			continue
		}
		pub, err := a.cfg.Holder.Publisher(p.ToPerson)
		if err != nil {
			continue
		}
		if _, err := pub.AnnounceAll(ctx); err != nil {
			a.cfg.Logf("moves: announcing %s: %v (will try again)", p.Site, err)
			continue
		}
		a.set(p, StatusDone, "")
		a.cfg.Logf("moves: %s is published from here now", p.Site)
		for _, to := range []string{p.From, p.Admin} {
			if q := a.send(ctx, bus, to, TypeMoved, Body(Done{Site: p.Site})); q != nil {
				out = append(out, *q)
			}
			if p.From == p.Admin {
				break
			}
		}
	}
	return out
}

// holds is whether the list gives name to person on this machine.
func (a *Agent) holds(list []peers.Peer, name, person string) bool {
	p, pn, ok := peers.NameHolder(list, name)
	return ok && p.IP() == a.cfg.Self && strings.EqualFold(pn.Person, person)
}

// Give gives a site's name to its new holder in list: off whoever held
// it, onto the node's sites or as a site of the person on its box.
func Give(list []peers.Peer, m Move, r Ready) ([]peers.Peer, error) {
	out := slices.Clone(list)
	to := -1
	for i := range out {
		out[i].Names = slices.DeleteFunc(slices.Clone(out[i].Names), func(n string) bool { return n == m.Site })
		out[i].Sites = slices.DeleteFunc(slices.Clone(out[i].Sites), func(n string) bool { return n == m.Site })
		out[i].PeopleNames = slices.DeleteFunc(slices.Clone(out[i].PeopleNames), func(pn peers.PersonName) bool { return pn.Name == m.Site })
		if to < 0 && out[i].IP() == m.To {
			to = i
		}
	}
	if to < 0 {
		return nil, errors.New("the new holder is no longer in the group")
	}
	if m.ToPerson != "" {
		person := r.Person // as the box calls them
		if person == "" {
			person = m.ToPerson
		}
		out[to].PeopleNames = append(out[to].PeopleNames, peers.PersonName{Name: m.Site, Person: person, Code: r.Code, Site: true})
	} else {
		out[to].Sites = append(out[to].Sites, m.Site)
		if out[to].Code == "" {
			out[to].Code = r.Code
		}
	}
	for i := range out {
		if len(out[i].Names) == 0 {
			out[i].Names = nil
		}
		if len(out[i].Sites) == 0 {
			out[i].Sites = nil
		}
		if len(out[i].PeopleNames) == 0 {
			out[i].PeopleNames = nil
		}
	}
	return out, peers.Validate(out)
}
