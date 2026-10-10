package msg

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/peers"
)

// fakeNet connects engines in memory. Node IDs are "200::N", addresses
// "[200::N]:7400"; a node can be switched off, or made to drop deliveries.
type fakeNet struct {
	mu      sync.Mutex
	engines map[string]*Engine // by address
	ids     map[string]string  // engine -> its ID, to know the caller
	down    map[string]bool
	drop    map[string]bool // deliveries to this address are lost, history still works
}

type caller struct {
	n    *fakeNet
	self string
}

func (n *fakeNet) target(addr string) (*Engine, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.down[addr] || n.engines[addr] == nil {
		return nil, errors.New("connection refused")
	}
	return n.engines[addr], nil
}

func (c caller) Deliver(ctx context.Context, addr string, m Message) error {
	e, err := c.n.target(addr)
	if err != nil {
		return err
	}
	c.n.mu.Lock()
	lost := c.n.drop[addr]
	c.n.mu.Unlock()
	if lost {
		return nil // lost on the way, but the sender thinks it arrived
	}
	return e.Receive(c.self, m)
}

func (c caller) Announce(ctx context.Context, addr string, topics []string) error {
	e, err := c.n.target(addr)
	if err != nil {
		return err
	}
	return e.Announced(c.self, topics)
}

func (c caller) History(ctx context.Context, addr, topic string, after uint64) ([]Message, error) {
	e, err := c.n.target(addr)
	if err != nil {
		return nil, err
	}
	return e.History(topic, after)
}

type group struct {
	t     *testing.T
	net   *fakeNet
	list  []peers.Peer
	nodes []*Engine
	dirs  []string
	stop  []context.CancelFunc
}

func newGroup(t *testing.T, n int) *group {
	g := &group{t: t, net: &fakeNet{engines: map[string]*Engine{}, ids: map[string]string{}, down: map[string]bool{}, drop: map[string]bool{}}}
	for i := 1; i <= n; i++ {
		g.list = append(g.list, peers.Peer{Name: fmt.Sprintf("node%d", i), Addr: fmt.Sprintf("[200::%d]:7400", i)})
	}
	// All the folders first: cleanups run last-first, so every node is
	// stopped before any folder is removed (a node writes into another's
	// folder when it delivers to it).
	for range n {
		g.dirs = append(g.dirs, t.TempDir())
		g.nodes = append(g.nodes, nil)
		g.stop = append(g.stop, nil)
	}
	for i := range n {
		g.start(i)
	}
	return g
}

func (g *group) start(i int) {
	id := fmt.Sprintf("200::%d", i+1)
	e, err := Open(g.dirs[i], id, func() []peers.Peer { return g.list }, caller{g.net, id}, g.t.Logf)
	if err != nil {
		g.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	stop := func() { cancel(); <-done }
	// Runs before the TempDirs are removed: once Run is back, this node
	// writes nothing more.
	g.t.Cleanup(stop)
	g.nodes[i], g.stop[i] = e, stop
	g.net.mu.Lock()
	g.net.engines[g.list[i].Addr] = e
	g.net.mu.Unlock()
	go func() {
		defer close(done)
		e.Run(ctx)
	}()
}

func (g *group) setDown(i int, down bool) {
	g.net.mu.Lock()
	g.net.down[g.list[i].Addr] = down
	g.net.mu.Unlock()
}

// eventually waits for cond, nudging the engines along.
func (g *group) eventually(what string, cond func() bool) {
	g.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			g.t.Fatalf("timed out waiting for %s", what)
		}
		for _, e := range g.nodes {
			e.poke()
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func bodies(list []Stored) []string {
	var out []string
	for _, s := range list {
		out = append(out, s.Body)
	}
	return out
}

func TestDirectMessage(t *testing.T) {
	g := newGroup(t, 3)
	if _, err := g.nodes[0].Send("node2", "text/plain", "hello node2"); err != nil {
		t.Fatal(err)
	}
	g.eventually("delivery", func() bool { return len(g.nodes[1].Messages(0, "direct", 0)) == 1 })
	got := g.nodes[1].Messages(0, "", 0)[0]
	if got.From != "200::1" || got.FromName != "node1" || got.Body != "hello node2" || got.To != "200::2" {
		t.Fatalf("got %+v", got)
	}
	if len(g.nodes[2].Messages(0, "", 0)) != 0 {
		t.Fatal("a third node got the direct message")
	}
	if _, err := g.nodes[0].Send("stranger", "", "x"); err == nil {
		t.Fatal("sent to a non-member")
	}
}

func TestOfflineNodeGetsMessagesLater(t *testing.T) {
	g := newGroup(t, 2)
	g.setDown(1, true)
	g.nodes[0].Send("node2", "", "while you were out")
	g.eventually("a failed try", func() bool {
		st := g.nodes[0].Status()
		return len(st.Pending) == 1 && st.Pending[0].Tries > 0
	})
	// Push the retry far off: the node announcing itself when it's back
	// must bring delivery forward.
	g.nodes[0].mu.Lock()
	for _, p := range g.nodes[0].outbox {
		for _, a := range p.Targets {
			a.Next = time.Now().Add(time.Hour).UnixMilli()
		}
	}
	g.nodes[0].mu.Unlock()
	g.setDown(1, false)
	g.nodes[1].requestCatchUp("") // as on start-up
	g.eventually("delivery when it's back", func() bool { return len(g.nodes[1].Messages(0, "", 0)) == 1 })
	g.eventually("outbox empty", func() bool { return len(g.nodes[0].Status().Pending) == 0 })
}

func TestTopics(t *testing.T) {
	g := newGroup(t, 4)
	for _, i := range []int{1, 2} {
		if err := g.nodes[i].Subscribe("alerts"); err != nil {
			t.Fatal(err)
		}
	}
	g.eventually("subscriptions announced", func() bool { return len(g.nodes[0].Status().Subscribers["alerts"]) == 2 })
	g.nodes[0].Publish("alerts", "", "disk nearly full")
	g.nodes[3].Publish("alerts", "", "pi2 is down")
	for _, i := range []int{1, 2} {
		g.eventually("both alerts", func() bool { return len(g.nodes[i].Messages(0, "alerts", 0)) == 2 })
	}
	if len(g.nodes[3].Messages(0, "", 0)) != 0 || len(g.nodes[0].Messages(0, "", 0)) != 0 {
		t.Fatal("a node that didn't subscribe got topic messages")
	}
	// Publishing to a topic you follow puts it in your own inbox too.
	g.nodes[1].Publish("alerts", "", "seen it")
	g.eventually("own message", func() bool { return len(g.nodes[1].Messages(0, "alerts", 0)) == 3 })
	g.eventually("others get it", func() bool { return len(g.nodes[2].Messages(0, "alerts", 0)) == 3 })
}

func TestLateSubscriberCatchesUp(t *testing.T) {
	g := newGroup(t, 2)
	for i := 1; i <= 3; i++ {
		g.nodes[0].Publish("news", "", fmt.Sprintf("item %d", i))
	}
	g.nodes[1].Subscribe("news")
	g.eventually("history", func() bool { return len(g.nodes[1].Messages(0, "news", 0)) == 3 })
	if b := bodies(g.nodes[1].Messages(0, "news", 0)); b[0] != "item 1" || b[2] != "item 3" {
		t.Fatalf("history out of order: %v", b)
	}
}

func TestGapIsRepaired(t *testing.T) {
	g := newGroup(t, 2)
	g.nodes[1].Subscribe("feed")
	g.eventually("announced", func() bool { return len(g.nodes[0].Status().Subscribers["feed"]) == 1 })
	g.nodes[0].Publish("feed", "", "1")
	g.eventually("first", func() bool { return len(g.nodes[1].Messages(0, "feed", 0)) == 1 })
	g.net.mu.Lock()
	g.net.drop[g.list[1].Addr] = true
	g.net.mu.Unlock()
	g.nodes[0].Publish("feed", "", "2 (lost)")
	g.eventually("lost one sent", func() bool { return len(g.nodes[0].Status().Pending) == 0 })
	g.net.mu.Lock()
	g.net.drop[g.list[1].Addr] = false
	g.net.mu.Unlock()
	g.nodes[0].Publish("feed", "", "3")
	// Message 3 shows message 2 is missing; it is fetched from history.
	g.eventually("gap filled", func() bool { return len(g.nodes[1].Messages(0, "feed", 0)) == 3 })
}

func TestSurvivesRestart(t *testing.T) {
	g := newGroup(t, 2)
	g.nodes[1].Subscribe("t")
	// node1 knows node2 follows t, so both messages wait for node2.
	g.eventually("subscription known", func() bool { return len(g.nodes[0].Status().Subscribers["t"]) == 1 })
	g.setDown(1, true)
	g.nodes[0].Send("node2", "", "queued")
	g.nodes[0].Publish("t", "", "kept")
	g.stop[0]()
	g.start(0) // the sender restarts with its queue
	if st := g.nodes[0].Status(); len(st.Pending) != 2 {
		t.Fatalf("outbox not kept across a restart: %+v", st.Pending)
	}
	g.setDown(1, false)
	g.nodes[0].mu.Lock()
	for _, p := range g.nodes[0].outbox {
		for _, a := range p.Targets {
			a.Next = 0
		}
	}
	g.nodes[0].mu.Unlock()
	g.nodes[1].requestCatchUp("")
	g.eventually("both arrive", func() bool { return len(g.nodes[1].Messages(0, "", 0)) == 2 })
	g.stop[1]()
	g.start(1)
	if n := len(g.nodes[1].Messages(0, "", 0)); n != 2 {
		t.Fatalf("inbox after restart has %d messages", n)
	}
	// Nothing arrives twice after the restart.
	g.nodes[1].requestCatchUp("")
	time.Sleep(200 * time.Millisecond)
	if n := len(g.nodes[1].Messages(0, "", 0)); n != 2 {
		t.Fatalf("duplicates after restart: %d", n)
	}
}

func TestWaitWakesOnNewMessage(t *testing.T) {
	g := newGroup(t, 2)
	done := make(chan []Stored)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done <- g.nodes[1].Wait(ctx, 0, "")
	}()
	time.Sleep(50 * time.Millisecond)
	g.nodes[0].Send("node2", "", "wake up")
	if got := <-done; len(got) != 1 || got[0].Body != "wake up" {
		t.Fatalf("waited and got %v", bodies(got))
	}
}

func TestRejectsBadMessages(t *testing.T) {
	g := newGroup(t, 1)
	e := g.nodes[0]
	if err := e.Receive("200::9", Message{ID: newID(), To: "200::5", Body: "x"}); err == nil {
		t.Fatal("accepted a direct message for another node")
	}
	if err := e.Receive("200::9", Message{ID: "short", To: "200::1"}); err == nil {
		t.Fatal("accepted a bad id")
	}
	big := make([]byte, MaxBody+1)
	if _, err := e.Publish("x", "", string(big)); err == nil {
		t.Fatal("accepted an oversized message")
	}
	if err := e.Subscribe("Bad Topic!"); err == nil {
		t.Fatal("accepted a bad topic name")
	}
}

// A node that subscribes while it can't reach a publisher (say, its network
// is still coming up) gets the messages soon after, without waiting for the
// next regular announce.
func TestSubscriberCutOffAtStartCatchesUp(t *testing.T) {
	retryFirst = 50 * time.Millisecond
	defer func() { retryFirst = 30 * time.Second }()
	g := newGroup(t, 2)
	g.setDown(0, true)
	g.nodes[1].Subscribe("sites")
	time.Sleep(100 * time.Millisecond) // its announce and catch-up fail
	g.setDown(0, false)
	g.nodes[0].Publish("sites", "", "new version")
	deadline := time.Now().Add(5 * time.Second)
	for len(g.nodes[1].Messages(0, "sites", 0)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the subscriber never got the message")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// One member can't fill another node with messages: past MaxHeld of theirs,
// more are refused (and the sender keeps them to try later), while other
// members' messages still get through.
func TestOneSenderCantFillANode(t *testing.T) {
	g := newGroup(t, 1)
	e := g.nodes[0]
	body := strings.Repeat("x", MaxBody)
	n := 0
	for ; n < MaxHeld/MaxBody+10; n++ {
		if err := e.Receive("200::9", Message{ID: newID(), To: "200::1", Body: body}); err != nil {
			if !errors.Is(err, ErrFull) {
				t.Fatal(err)
			}
			break
		}
	}
	if n != MaxHeld/MaxBody {
		t.Fatalf("took %d messages of %d KB, want %d", n, MaxBody>>10, MaxHeld/MaxBody)
	}
	if err := e.Receive("200::8", Message{ID: newID(), To: "200::1", Body: "hello"}); err != nil {
		t.Fatalf("another member's message refused: %v", err)
	}
}

// A restart keeps who follows what, even when the topics never changed.
func TestFollowersSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	list := func() []peers.Peer {
		return []peers.Peer{{Name: "a", Addr: "[200::1]:7400"}, {Name: "b", Addr: "[200::2]:7400"}}
	}
	clock := time.Unix(1_000_000, 0)
	open := func() *Engine {
		e, err := Open(dir, "200::1", list, nil, t.Logf)
		if err != nil {
			t.Fatal(err)
		}
		e.now = func() time.Time { return clock }
		return e
	}
	e := open()
	e.Announced("200::2", []string{"sites"})
	clock = clock.Add(50 * time.Minute) // announced again, topics unchanged
	e.Announced("200::2", []string{"sites"})
	clock = clock.Add(20 * time.Minute) // restart 70 minutes after the first
	e = open()
	e.now = func() time.Time { return clock }
	if at := e.st.Subscribers["200::2"].At; clock.Unix()-at > int64(announceTTL/time.Second) {
		t.Fatalf("follower looks lapsed after a restart: announced %ds ago", clock.Unix()-at)
	}
}
