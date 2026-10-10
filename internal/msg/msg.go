// Package msg lets the group's nodes message each other: direct messages to
// one node, and topics that any node can publish to and subscribe to.
//
// Every node runs an Engine inside its node process. Messages travel over
// Yggdrasil between members only, so the sender of each is proven by its
// address. Delivery is store-and-forward: the sender keeps a message and
// retries until the recipient takes it, for up to a week, so nodes that are
// off for a while still get their messages.
//
// Topics work by soft state. A node tells every member which topics it
// subscribes to, and repeats this every few minutes; each publisher sends a
// topic's messages to the nodes that asked. Each publisher numbers its
// messages per topic, so a subscriber that missed some (it was away, or
// subscribed late) asks the publisher for the history it lacks.
//
// Messages are small (up to 64 KB). Anything larger is stored in the group
// and its stub sent in a message.
package msg

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/peterretief/yggstore/internal/atomicfile"
	"github.com/peterretief/yggstore/internal/peers"
)

const (
	MaxBody      = 64 << 10
	Retention    = 30 * 24 * time.Hour // how long inboxes and topic histories are kept
	GiveUpAfter  = 7 * 24 * time.Hour  // undelivered messages are dropped after this
	announceTTL  = time.Hour           // a subscription not repeated within this lapses
	announceEach = 10 * time.Minute
	historyPage  = 500
)

// Message is one message as it travels.
type Message struct {
	ID    string `json:"id"`
	From  string `json:"from,omitempty"` // sender's node ID, set by the receiver from the connection
	To    string `json:"to,omitempty"`   // recipient's node ID, for a direct message
	Topic string `json:"topic,omitempty"`
	Seq   uint64 `json:"seq,omitempty"` // the publisher's number for it in the topic, from 1
	Time  int64  `json:"time"`          // unix milliseconds, by the sender's clock
	Type  string `json:"type,omitempty"`
	Body  string `json:"body"`
}

// Stored is a received message, numbered in the order it arrived here.
type Stored struct {
	N        int64  `json:"n"`
	Received int64  `json:"received"` // unix milliseconds
	FromName string `json:"from_name,omitempty"`
	Message
}

var topicName = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]{0,63}$`)

// ValidTopic reports whether t is a usable topic name: lower-case letters,
// digits and . _ / -, up to 64 characters.
func ValidTopic(t string) bool { return topicName.MatchString(t) }

func validate(m Message) error {
	switch {
	case len(m.ID) != 32:
		return errors.New("bad message id")
	case len(m.Body) > MaxBody:
		return fmt.Errorf("message body over %d KB", MaxBody>>10)
	case !utf8.ValidString(m.Body) || !utf8.ValidString(m.Type) || len(m.Type) > 100:
		return errors.New("message must be UTF-8 text")
	case m.Topic != "" && !ValidTopic(m.Topic):
		return errors.New("bad topic name")
	case m.Topic != "" && m.Seq == 0:
		return errors.New("topic message without a sequence number")
	}
	return nil
}

// Network carries messages between nodes; HTTP implements it (see Client).
type Network interface {
	Deliver(ctx context.Context, addr string, m Message) error
	Announce(ctx context.Context, addr string, topics []string) error
	History(ctx context.Context, addr, topic string, after uint64) ([]Message, error)
}

// ErrNoMessaging means the node runs a yggstore without messaging.
var ErrNoMessaging = errors.New("the node's yggstore has no messaging; it needs updating")

type attempt struct {
	Addr  string `json:"addr"`
	Tries int    `json:"tries"`
	Next  int64  `json:"next"` // unix milliseconds
	Err   string `json:"err,omitempty"`
	busy  bool
}

type pending struct {
	Msg     Message             `json:"msg"`
	Targets map[string]*attempt `json:"targets"` // by node ID
}

type subscriber struct {
	Topics []string `json:"topics"`
	At     int64    `json:"at"` // unix seconds of the last announcement
}

type state struct {
	Subs        []string              `json:"subscriptions"`
	Subscribers map[string]subscriber `json:"subscribers"` // by node ID
	Seq         map[string]uint64     `json:"seq"`         // my last number, per topic
	Cursors     map[string]uint64     `json:"cursors"`     // "publisher topic": highest seq with none missing before it
}

// Failed is a message that could not be delivered.
type Failed struct {
	Msg  Message `json:"msg"`
	To   string  `json:"to"`
	Err  string  `json:"err"`
	When int64   `json:"when"`
}

// Engine is one node's messaging.
type Engine struct {
	work  sync.WaitGroup // goroutines Run started; Run waits for them
	dir   string
	self  string // this node's ID
	peers func() []peers.Peer
	net   Network
	logf  func(string, ...any)
	now   func() time.Time

	mu      sync.Mutex
	st      state
	inbox   []Stored
	seen    map[string]bool
	held    map[string]int64           // bytes of messages kept, per sender
	ahead   map[string]map[uint64]bool // received out of order, per cursor key
	outbox  map[string]*pending        // by message ID
	failed  []Failed
	nextN   int64
	waiters []chan struct{}
	kick    chan struct{}
	catchUp chan string // topics to catch up on, "" for all
	noMsg   map[string]time.Time
	sending chan struct{} // limits deliveries in flight
	// retryIn is how soon to announce again after some member couldn't be
	// reached; it doubles up to announceEach while they stay unreachable.
	retryIn    time.Duration
	retryFirst time.Duration
}

// Open loads the engine kept in dir. self is this node's ID (its overlay IP).
func Open(dir, self string, list func() []peers.Peer, net Network, logf func(string, ...any)) (*Engine, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if err := os.MkdirAll(filepath.Join(dir, "published"), 0o700); err != nil {
		return nil, err
	}
	e := &Engine{dir: dir, self: self, peers: list, net: net, logf: logf, now: time.Now,
		seen: map[string]bool{}, held: map[string]int64{}, ahead: map[string]map[uint64]bool{}, outbox: map[string]*pending{},
		kick: make(chan struct{}, 1), catchUp: make(chan string, 16), noMsg: map[string]time.Time{}, retryFirst: retryFirst,
		sending: make(chan struct{}, 8)}
	if err := readJSON(filepath.Join(dir, "state.json"), &e.st); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if e.st.Subscribers == nil {
		e.st.Subscribers = map[string]subscriber{}
	}
	if e.st.Seq == nil {
		e.st.Seq = map[string]uint64{}
	}
	if e.st.Cursors == nil {
		e.st.Cursors = map[string]uint64{}
	}
	var out []*pending
	if err := readJSON(filepath.Join(dir, "outbox.json"), &out); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, p := range out {
		e.outbox[p.Msg.ID] = p
	}
	readJSON(filepath.Join(dir, "failed.json"), &e.failed)
	if err := e.loadInbox(); err != nil {
		return nil, err
	}
	return e, nil
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	return atomicfile.Replace(path, append(b, '\n'), 0o600)
}

// loadInbox reads the inbox, dropping what is past retention.
func (e *Engine) loadInbox() error {
	path := filepath.Join(e.dir, "inbox.jsonl")
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	cutoff := e.now().Add(-Retention).UnixMilli()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 4<<20)
	dropped := false
	for sc.Scan() {
		var s Stored
		if json.Unmarshal(sc.Bytes(), &s) != nil {
			dropped = true // a torn last line after a crash
			continue
		}
		e.nextN = max(e.nextN, s.N)
		if s.Received < cutoff {
			dropped = true
			continue
		}
		e.inbox = append(e.inbox, s)
		e.seen[s.ID] = true
		e.held[s.From] += int64(len(s.Body))
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if dropped {
		return e.rewriteInbox()
	}
	return nil
}

func (e *Engine) rewriteInbox() error {
	var b strings.Builder
	for _, s := range e.inbox {
		line, _ := json.Marshal(s)
		b.Write(line)
		b.WriteByte('\n')
	}
	return atomicfile.Replace(filepath.Join(e.dir, "inbox.jsonl"), []byte(b.String()), 0o600)
}

func appendLine(path string, v any) error {
	line, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (e *Engine) saveState() {
	if err := writeJSON(filepath.Join(e.dir, "state.json"), e.st); err != nil {
		e.logf("messaging: saving state: %v", err)
	}
}

func (e *Engine) saveOutbox() {
	list := make([]*pending, 0, len(e.outbox))
	for _, p := range e.outbox {
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Msg.Time < list[j].Msg.Time })
	if err := writeJSON(filepath.Join(e.dir, "outbox.json"), list); err != nil {
		e.logf("messaging: saving outbox: %v", err)
	}
}

func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func publishedPath(dir, topic string) string {
	sum := sha256.Sum256([]byte(topic))
	return filepath.Join(dir, "published", hex.EncodeToString(sum[:8])+".jsonl")
}

// member finds a peer by node ID, name or address.
func (e *Engine) member(who string) (peers.Peer, bool) {
	for _, p := range e.peers() {
		if p.IP() == who || p.Name == who || p.Addr == who {
			return p, true
		}
	}
	return peers.Peer{}, false
}

func (e *Engine) nameOf(id string) string {
	if p, ok := e.member(id); ok {
		return p.Name
	}
	return ""
}

func (e *Engine) poke() {
	select {
	case e.kick <- struct{}{}:
	default:
	}
}

// Send queues a direct message to one member (by name, node ID or address).
func (e *Engine) Send(to, typ, body string) (Message, error) {
	p, ok := e.member(to)
	if !ok {
		return Message{}, fmt.Errorf("%q is not a member of the group", to)
	}
	if p.IP() == e.self {
		return Message{}, errors.New("that's this node")
	}
	m := Message{ID: newID(), From: e.self, To: p.IP(), Time: e.now().UnixMilli(), Type: typ, Body: body}
	if err := validate(m); err != nil {
		return Message{}, err
	}
	e.mu.Lock()
	e.outbox[m.ID] = &pending{Msg: m, Targets: map[string]*attempt{p.IP(): {Addr: p.Addr}}}
	e.saveOutbox()
	e.mu.Unlock()
	e.poke()
	return m, nil
}

// Publish sends a message to a topic's subscribers. It is kept in the
// topic's history too, for subscribers that miss it. If this node is
// subscribed, it gets the message in its own inbox as well.
func (e *Engine) Publish(topic, typ, body string) (Message, error) {
	if !ValidTopic(topic) {
		return Message{}, fmt.Errorf("%q is not a topic name: use lower-case letters, digits and . _ / - (up to 64)", topic)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	m := Message{ID: newID(), From: e.self, Topic: topic, Seq: e.st.Seq[topic] + 1, Time: e.now().UnixMilli(), Type: typ, Body: body}
	if err := validate(m); err != nil {
		return Message{}, err
	}
	if err := appendLine(publishedPath(e.dir, topic), m); err != nil {
		return Message{}, err
	}
	e.st.Seq[topic] = m.Seq
	e.saveState()
	targets := map[string]*attempt{}
	cutoff := e.now().Add(-announceTTL).Unix()
	for id, s := range e.st.Subscribers {
		if s.At < cutoff || id == e.self || !contains(s.Topics, topic) {
			continue
		}
		if p, ok := e.member(id); ok {
			targets[id] = &attempt{Addr: p.Addr}
		}
	}
	if len(targets) > 0 {
		e.outbox[m.ID] = &pending{Msg: m, Targets: targets}
		e.saveOutbox()
	}
	if contains(e.st.Subs, topic) {
		e.store(m, e.self)
	}
	e.poke()
	return m, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Subscribe starts receiving a topic, including its recent history.
func (e *Engine) Subscribe(topic string) error {
	if !ValidTopic(topic) {
		return fmt.Errorf("%q is not a topic name: use lower-case letters, digits and . _ / - (up to 64)", topic)
	}
	e.mu.Lock()
	if !contains(e.st.Subs, topic) {
		e.st.Subs = append(e.st.Subs, topic)
		sort.Strings(e.st.Subs)
		e.saveState()
	}
	e.mu.Unlock()
	e.requestCatchUp(topic)
	return nil
}

// Unsubscribe stops receiving a topic. Messages already received are kept.
func (e *Engine) Unsubscribe(topic string) {
	e.mu.Lock()
	keep := e.st.Subs[:0]
	for _, t := range e.st.Subs {
		if t != topic {
			keep = append(keep, t)
		}
	}
	e.st.Subs = keep
	e.saveState()
	e.mu.Unlock()
	e.requestCatchUp("") // re-announces too
}

func (e *Engine) requestCatchUp(topic string) {
	select {
	case e.catchUp <- topic:
	default:
	}
}

// store adds a message to the inbox. The caller holds e.mu.
func (e *Engine) store(m Message, from string) bool {
	if e.seen[m.ID] {
		return false
	}
	m.From = from
	e.nextN++
	s := Stored{N: e.nextN, Received: e.now().UnixMilli(), FromName: e.nameOf(from), Message: m}
	if err := appendLine(filepath.Join(e.dir, "inbox.jsonl"), s); err != nil {
		e.logf("messaging: saving a message: %v", err)
		e.nextN--
		return false
	}
	e.seen[m.ID] = true
	e.held[from] += int64(len(m.Body))
	e.inbox = append(e.inbox, s)
	if m.Topic != "" {
		e.advance(from, m.Topic, m.Seq)
	}
	for _, w := range e.waiters {
		close(w)
	}
	e.waiters = nil
	return true
}

func cursorKey(from, topic string) string { return from + " " + topic }

// advance records that seq arrived from a publisher, and reports whether
// some before it are missing. The caller holds e.mu.
func (e *Engine) advance(from, topic string, seq uint64) (gap bool) {
	k := cursorKey(from, topic)
	cur := e.st.Cursors[k]
	if seq <= cur {
		return false
	}
	if seq > cur+1 {
		if e.ahead[k] == nil {
			e.ahead[k] = map[uint64]bool{}
		}
		e.ahead[k][seq] = true
		return true
	}
	cur = seq
	for e.ahead[k][cur+1] {
		delete(e.ahead[k], cur+1)
		cur++
	}
	e.st.Cursors[k] = cur
	e.saveState()
	return len(e.ahead[k]) > 0
}

// Receive takes a message delivered by the member caller.
func (e *Engine) Receive(caller string, m Message) error {
	if err := validate(m); err != nil {
		return err
	}
	if m.Topic == "" && m.To != e.self {
		return errors.New("this message is for another node")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if m.Topic != "" && !contains(e.st.Subs, m.Topic) {
		return nil // no longer subscribed; taking it stops the retries
	}
	if !e.seen[m.ID] && e.held[caller]+int64(len(m.Body)) > MaxHeld {
		return ErrFull // the sender keeps it and tries again later
	}
	if e.store(m, caller) && m.Topic != "" {
		if len(e.ahead[cursorKey(caller, m.Topic)]) > 0 {
			e.requestCatchUp(m.Topic)
		}
	}
	return nil
}

// MaxHeld is how much of one member's messages a node keeps at a time, so
// one member can't fill another's memory and disk.
const MaxHeld = 32 << 20

// ErrFull means a node holds as many of the sender's messages as it will.
var ErrFull = errors.New("this node holds too many of your messages; try again later")

// Announced records the topics a member subscribes to.
func (e *Engine) Announced(caller string, topics []string) error {
	if len(topics) > 1000 {
		return errors.New("too many topics")
	}
	for _, t := range topics {
		if !ValidTopic(t) {
			return fmt.Errorf("bad topic %q", t)
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	old := e.st.Subscribers[caller]
	now := e.now().Unix()
	e.st.Subscribers[caller] = subscriber{Topics: topics, At: now}
	// Save a change at once, and the time now and then, so a restart
	// doesn't take followers for lapsed until they announce again.
	if strings.Join(old.Topics, ",") != strings.Join(topics, ",") || now-old.At > int64(announceTTL/time.Second)/2 {
		e.saveState()
	}
	// A node announcing itself is reachable: deliver what waits for it now,
	// rather than when its backoff ends.
	waiting := false
	for _, p := range e.outbox {
		if a := p.Targets[caller]; a != nil && !a.busy {
			a.Next, waiting = 0, true
		}
	}
	if waiting {
		e.poke()
	}
	return nil
}

// History returns this node's messages in a topic after seq, oldest first,
// up to a page.
func (e *Engine) History(topic string, after uint64) ([]Message, error) {
	if !ValidTopic(topic) {
		return nil, errors.New("bad topic")
	}
	f, err := os.Open(publishedPath(e.dir, topic))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	cutoff := e.now().Add(-Retention).UnixMilli()
	var out []Message
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 4<<20)
	for sc.Scan() && len(out) < historyPage {
		var m Message
		if json.Unmarshal(sc.Bytes(), &m) != nil || m.Topic != topic || m.Seq <= after || m.Time < cutoff {
			continue
		}
		out = append(out, m)
	}
	return out, sc.Err()
}

// Messages returns received messages numbered after n, optionally in one
// topic ("direct" for direct messages), oldest first, at most limit.
func (e *Engine) Messages(after int64, topic string, limit int) []Stored {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.messages(after, topic, limit)
}

func (e *Engine) messages(after int64, topic string, limit int) []Stored {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	i := sort.Search(len(e.inbox), func(i int) bool { return e.inbox[i].N > after })
	var out []Stored
	for ; i < len(e.inbox) && len(out) < limit; i++ {
		s := e.inbox[i]
		if topic == "" || s.Topic == topic || (topic == "direct" && s.Topic == "") {
			out = append(out, s)
		}
	}
	return out
}

// Latest returns the last limit messages, oldest first.
func (e *Engine) Latest(topic string, limit int) []Stored {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []Stored
	for i := len(e.inbox) - 1; i >= 0 && len(out) < limit; i-- {
		s := e.inbox[i]
		if topic == "" || s.Topic == topic || (topic == "direct" && s.Topic == "") {
			out = append(out, s)
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// Wait returns messages after n, waiting until there are some or ctx ends.
func (e *Engine) Wait(ctx context.Context, after int64, topic string) []Stored {
	for {
		e.mu.Lock()
		got := e.messages(after, topic, 1000)
		if len(got) > 0 {
			e.mu.Unlock()
			return got
		}
		if len(e.inbox) > 0 {
			after = max(after, e.inbox[len(e.inbox)-1].N) // skip others' topics
		}
		w := make(chan struct{})
		e.waiters = append(e.waiters, w)
		e.mu.Unlock()
		select {
		case <-w:
		case <-ctx.Done():
			return nil
		}
	}
}

// Pending is an undelivered message, for status.
type Pending struct {
	Msg   Message `json:"msg"`
	To    string  `json:"to"`
	Name  string  `json:"name,omitempty"`
	Tries int     `json:"tries"`
	Next  int64   `json:"next"`
	Err   string  `json:"err,omitempty"`
}

// Status describes the engine, for people.
type Status struct {
	Self          string              `json:"self"`
	Subscriptions []string            `json:"subscriptions"`
	Subscribers   map[string][]string `json:"subscribers"` // topic -> member names who follow it
	Pending       []Pending           `json:"pending"`
	Failed        []Failed            `json:"failed"`
	Last          int64               `json:"last"` // number of the newest message
}

func (e *Engine) Status() Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := Status{Self: e.self, Subscriptions: append([]string{}, e.st.Subs...), Subscribers: map[string][]string{},
		Pending: []Pending{}, Failed: append([]Failed{}, e.failed...), Last: e.nextN}
	cutoff := e.now().Add(-announceTTL).Unix()
	for id, s := range e.st.Subscribers {
		if s.At < cutoff {
			continue
		}
		name := e.nameOf(id)
		if name == "" {
			name = id
		}
		for _, t := range s.Topics {
			st.Subscribers[t] = append(st.Subscribers[t], name)
		}
	}
	for _, names := range st.Subscribers {
		sort.Strings(names)
	}
	for _, p := range e.outbox {
		for id, a := range p.Targets {
			st.Pending = append(st.Pending, Pending{Msg: p.Msg, To: id, Name: e.nameOf(id), Tries: a.Tries, Next: a.Next, Err: a.Err})
		}
	}
	sort.Slice(st.Pending, func(i, j int) bool { return st.Pending[i].Msg.Time < st.Pending[j].Msg.Time })
	return st
}

// Run delivers, announces and catches up until ctx ends.
func (e *Engine) Run(ctx context.Context) {
	tick := time.NewTicker(2 * time.Second)
	announce := time.NewTicker(announceEach)
	prune := time.NewTicker(6 * time.Hour)
	defer tick.Stop()
	defer announce.Stop()
	defer prune.Stop()
	defer e.work.Wait() // nothing writes to dir once Run has returned
	e.work.Add(1)
	go func() {
		defer e.work.Done()
		e.catchUpLoop(ctx)
	}()
	e.requestCatchUp("")
	for {
		e.deliver(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-e.kick:
		case <-announce.C:
			e.requestCatchUp("")
		case <-prune.C:
			e.prune()
		}
	}
}

func (e *Engine) prune() {
	e.mu.Lock()
	defer e.mu.Unlock()
	cutoff := e.now().Add(-Retention).UnixMilli()
	i := 0
	for i < len(e.inbox) && e.inbox[i].Received < cutoff {
		delete(e.seen, e.inbox[i].ID)
		if e.held[e.inbox[i].From] -= int64(len(e.inbox[i].Body)); e.held[e.inbox[i].From] <= 0 {
			delete(e.held, e.inbox[i].From)
		}
		i++
	}
	if i > 0 {
		e.inbox = append([]Stored(nil), e.inbox[i:]...)
		if err := e.rewriteInbox(); err != nil {
			e.logf("messaging: pruning inbox: %v", err)
		}
	}
	for t := range e.st.Seq {
		e.prunePublished(t, cutoff)
	}
	gone := e.now().Add(-announceTTL).Unix()
	for id, s := range e.st.Subscribers {
		if s.At < gone {
			delete(e.st.Subscribers, id)
		}
	}
	e.saveState()
}

func (e *Engine) prunePublished(topic string, cutoff int64) {
	path := publishedPath(e.dir, topic)
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var keep []byte
	changed := false
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var m Message
		if json.Unmarshal([]byte(line), &m) != nil || m.Time < cutoff {
			changed = true
			continue
		}
		keep = append(keep, line+"\n"...)
	}
	if changed {
		atomicfile.Replace(path, keep, 0o600)
	}
}

// backoff is the wait after the nth failed try: 5s doubling to 10 minutes.
func backoff(n int) time.Duration {
	d := 5 * time.Second
	for i := 1; i < n && d < 10*time.Minute; i++ {
		d *= 2
	}
	return min(d, 10*time.Minute)
}

// deliver starts every due delivery, up to 8 running at once.
func (e *Engine) deliver(ctx context.Context) {
	type job struct {
		id, to string
		a      *attempt
		m      Message
	}
	var jobs []job
	e.mu.Lock()
	now := e.now()
	changed := false
	for id, p := range e.outbox {
		for to, a := range p.Targets {
			if a.busy || a.Next > now.UnixMilli() {
				continue
			}
			if now.Sub(time.UnixMilli(p.Msg.Time)) > GiveUpAfter {
				e.failed = append(e.failed, Failed{Msg: p.Msg, To: to, Err: a.Err, When: now.Unix()})
				if len(e.failed) > 100 {
					e.failed = e.failed[len(e.failed)-100:]
				}
				writeJSON(filepath.Join(e.dir, "failed.json"), e.failed)
				delete(p.Targets, to)
				changed = true
				continue
			}
			a.busy = true
			jobs = append(jobs, job{id, to, a, p.Msg})
		}
		if len(p.Targets) == 0 {
			delete(e.outbox, id)
			changed = true
		}
	}
	if changed {
		e.saveOutbox()
	}
	e.mu.Unlock()
	// Each delivery runs on its own, so a slow node doesn't hold up the rest.
	for _, j := range jobs {
		e.work.Add(1)
		go func() {
			defer e.work.Done()
			select {
			case e.sending <- struct{}{}:
			case <-ctx.Done():
				e.mu.Lock()
				j.a.busy = false
				e.mu.Unlock()
				return
			}
			defer func() { <-e.sending }()
			dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			err := e.net.Deliver(dctx, j.a.Addr, j.m)
			cancel()
			e.mu.Lock()
			defer e.mu.Unlock()
			j.a.busy = false
			p := e.outbox[j.id]
			if p == nil {
				return
			}
			if err == nil {
				delete(p.Targets, j.to)
				if len(p.Targets) == 0 {
					delete(e.outbox, j.id)
				}
			} else {
				j.a.Tries++
				j.a.Err = err.Error()
				j.a.Next = e.now().Add(backoff(j.a.Tries)).UnixMilli()
			}
			e.saveOutbox()
			if err == nil {
				e.poke()
			}
		}()
	}
}

// catchUpLoop announces this node's subscriptions to every member and asks
// publishers for messages it is missing.
func (e *Engine) catchUpLoop(ctx context.Context) {
	for {
		var topic string
		select {
		case <-ctx.Done():
			return
		case topic = <-e.catchUp:
		}
		// Coalesce a burst of requests.
		all := topic == ""
	drain:
		for {
			select {
			case t := <-e.catchUp:
				all = all || t == "" || t != topic
			default:
				break drain
			}
		}
		if missed := e.syncWithMembers(ctx, all, topic); missed {
			// Some member didn't hear this node's subscriptions (often
			// because the network was still coming up): try again soon,
			// not at the next announce.
			e.retryIn = min(max(2*e.retryIn, e.retryFirst), announceEach)
			time.AfterFunc(e.retryIn, func() { e.requestCatchUp("") })
		} else {
			e.retryIn = 0
		}
	}
}

// retryFirst is the first wait before announcing again to unreachable
// members. Engines take it when opened.
var retryFirst = 30 * time.Second

// syncWithMembers reports whether some member couldn't be reached.
func (e *Engine) syncWithMembers(ctx context.Context, all bool, topic string) (missed bool) {
	e.mu.Lock()
	subs := append([]string{}, e.st.Subs...)
	e.mu.Unlock()
	topics := subs
	if !all {
		topics = []string{topic}
	}
	var wg sync.WaitGroup
	var missedMu sync.Mutex
	sem := make(chan struct{}, 8)
	for _, p := range e.peers() {
		if p.IP() == e.self {
			continue
		}
		e.mu.Lock()
		skip := e.now().Before(e.noMsg[p.IP()])
		e.mu.Unlock()
		if skip {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			if err := e.net.Announce(cctx, p.Addr, subs); err != nil {
				if errors.Is(err, ErrNoMessaging) {
					e.mu.Lock()
					e.noMsg[p.IP()] = e.now().Add(time.Hour)
					e.mu.Unlock()
				} else {
					missedMu.Lock()
					missed = true
					missedMu.Unlock()
				}
				return
			}
			for _, t := range topics {
				e.fetchHistory(cctx, p, t)
			}
		}()
	}
	wg.Wait()
	return missed
}

func (e *Engine) fetchHistory(ctx context.Context, p peers.Peer, topic string) {
	for {
		e.mu.Lock()
		after := e.st.Cursors[cursorKey(p.IP(), topic)]
		e.mu.Unlock()
		msgs, err := e.net.History(ctx, p.Addr, topic, after)
		if err != nil {
			return
		}
		e.mu.Lock()
		if len(msgs) > 0 && msgs[0].Seq > after+1 {
			// The publisher no longer has the ones in between (past
			// retention): stop waiting for them.
			e.st.Cursors[cursorKey(p.IP(), topic)] = msgs[0].Seq - 1
		}
		for _, m := range msgs {
			if m.Topic == topic && validate(m) == nil {
				if e.seen[m.ID] || e.held[p.IP()]+int64(len(m.Body)) <= MaxHeld {
					e.store(m, p.IP())
				}
				e.advance(p.IP(), topic, m.Seq) // already-seen ones still move the cursor
			}
		}
		e.mu.Unlock()
		if len(msgs) < historyPage {
			return
		}
	}
}
