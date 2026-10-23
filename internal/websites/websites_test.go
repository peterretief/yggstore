package websites

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/localstore"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/server"
	"github.com/peterretief/yggstore/internal/site"
	"github.com/peterretief/yggstore/internal/transport"
)

type announcer struct{ n int }

func (a *announcer) Publish(context.Context, string, string, string) error { a.n++; return nil }

type world struct {
	w        *Watcher
	dir      string
	now      time.Time
	onCF     map[string]bool
	served   map[string]bool
	answers  map[string]bool
	routed   []string
	logs     []string
	ann      *announcer
	nodes    []peers.Peer
	withAPI  bool
	attempts int
}

func setup(t *testing.T) *world {
	t.Helper()
	x := &world{dir: filepath.Join(t.TempDir(), "websites"), now: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC),
		onCF: map[string]bool{}, served: map[string]bool{}, answers: map[string]bool{}, ann: &announcer{}}
	for i := range 4 {
		h := server.Handler(localstore.New(t.TempDir()), server.Options{
			Transport: transport.Loopback{}, Allowed: map[string]bool{"127.0.0.1": true}})
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		x.nodes = append(x.nodes, peers.Peer{Name: string(rune('a' + i)), Addr: strings.TrimPrefix(srv.URL, "http://")})
	}
	os.MkdirAll(x.dir, 0o755)
	pub := &site.Publisher{Dir: filepath.Join(t.TempDir(), "pub"), Client: client.New(), Announce: x.ann, Skip: Skip}
	x.w = New(Config{Dir: x.dir, StatePath: filepath.Join(t.TempDir(), "websites.json"), Pub: pub, Client: client.New(), TokenPath: "~/.yggstore/cloudflare-api.token",
		Online:  func(context.Context) ([]peers.Peer, error) { x.attempts++; return x.nodes, nil },
		Mine:    func() map[string]bool { return map[string]bool{"me.group.example": true} },
		Served:  func() map[string]bool { return x.served },
		Answers: func(_ context.Context, d string) bool { return x.answers[d] },
		Cloudflare: func(_ context.Context, d string) (bool, string, error) {
			return x.onCF[d], d, nil
		},
		Now:  func() time.Time { return x.now },
		Logf: func(f string, a ...any) { x.logs = append(x.logs, f) },
	})
	return x
}

// settle looks at the folders, lets the quiet time pass, and looks again.
func (x *world) settle(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	x.w.Step(ctx)
	x.now = x.now.Add(2 * time.Minute)
	x.w.Step(ctx)
}

func (x *world) site(name string, pages map[string]string) {
	os.MkdirAll(filepath.Join(x.dir, name), 0o755)
	for p, body := range pages {
		os.MkdirAll(filepath.Dir(filepath.Join(x.dir, name, p)), 0o755)
		os.WriteFile(filepath.Join(x.dir, name, p), []byte(body), 0o644)
	}
}

func (x *world) note(name string) string {
	b, _ := os.ReadFile(filepath.Join(x.dir, name+NoteSuffix))
	return string(b)
}

func published(x *world, name string) bool {
	for _, s := range x.w.cfg.Pub.Sites() {
		if s == name {
			return true
		}
	}
	return false
}

func TestNewDomainWaitsForCloudflare(t *testing.T) {
	x := setup(t)
	x.site("new.example.org", map[string]string{"index.html": "<h1>hi</h1>"})
	x.settle(t)
	if published(x, "new.example.org") || x.attempts != 0 {
		t.Fatal("published before the domain was on Cloudflare")
	}
	if n := x.note("new.example.org"); !strings.Contains(n, "isn't set up on Cloudflare") || !strings.Contains(n, "nameservers") {
		t.Fatalf("note = %q", n)
	}

	// On Cloudflare now; the next check (a few minutes on) finds it.
	x.onCF["new.example.org"] = true
	x.settle(t)
	if published(x, "new.example.org") {
		t.Fatal("Cloudflare checked again before the recheck time")
	}
	x.now = x.now.Add(5 * time.Minute)
	x.settle(t)
	if !published(x, "new.example.org") {
		t.Fatalf("not published once on Cloudflare (logs: %v)", x.logs)
	}
	// No API token: published, but the route is added by hand.
	if n := x.note("new.example.org"); !strings.Contains(n, "isn't online at https://new.example.org/") || !strings.Contains(n, "cloudflare-api.token") {
		t.Fatalf("note = %q", n)
	}
	x.answers["new.example.org"] = true
	x.now = x.now.Add(5 * time.Minute)
	x.settle(t)
	if _, err := os.Stat(filepath.Join(x.dir, "new.example.org"+NoteSuffix)); !os.IsNotExist(err) {
		t.Fatalf("note still there once online: %q", x.note("new.example.org"))
	}
	if st := x.w.Status(); len(st) != 1 || st[0].Problem != "" || st[0].Version == "" {
		t.Fatalf("status %+v", st)
	}
}

func TestRouteAddedWithAPIToken(t *testing.T) {
	x := setup(t)
	x.w.cfg.Route = func(_ context.Context, d string) ([]string, error) {
		x.routed = append(x.routed, d)
		x.answers[d] = true
		return []string{"tunnel route " + d}, nil
	}
	x.onCF["shop.example.org"] = true
	x.site("shop.example.org", map[string]string{"index.html": "shop"})
	x.settle(t)
	if !published(x, "shop.example.org") || len(x.routed) != 1 || x.note("shop.example.org") != "" {
		t.Fatalf("published %v, routed %v, note %q", published(x, "shop.example.org"), x.routed, x.note("shop.example.org"))
	}
}

func TestOthersSitesAndBadNamesLeftAlone(t *testing.T) {
	x := setup(t)
	x.served["taken.example.org"] = true
	x.onCF["taken.example.org"] = true
	x.site("taken.example.org", map[string]string{"index.html": "mine?"})
	x.site("My Site", map[string]string{"index.html": "x"})
	x.settle(t)
	if published(x, "taken.example.org") || x.attempts != 0 {
		t.Fatal("published a site another node publishes")
	}
	if !strings.Contains(x.note("taken.example.org"), "another node") {
		t.Fatalf("note = %q", x.note("taken.example.org"))
	}
	if !strings.Contains(x.note("My Site"), "isn't a domain name") {
		t.Fatalf("note = %q", x.note("My Site"))
	}
}

// A name the group gave this node is published without asking Cloudflare.
func TestGroupNamePublishedStraightAway(t *testing.T) {
	x := setup(t)
	x.site("me.group.example", map[string]string{"index.html": "me"})
	x.settle(t)
	if !published(x, "me.group.example") || x.note("me.group.example") != "" {
		t.Fatalf("published %v, note %q", published(x, "me.group.example"), x.note("me.group.example"))
	}
}

func TestEditsPublishedMistakesHeldBackFolderRefilled(t *testing.T) {
	x := setup(t)
	x.site("me.group.example", map[string]string{"index.html": strings.Repeat("v1 ", 100), "css/site.css": "body{}"})
	x.settle(t)
	vs, _ := x.w.cfg.Pub.Versions("me.group.example")
	if len(vs) != 1 {
		t.Fatalf("%d versions after the first publish", len(vs))
	}

	// An edit is published as a new version, once the gap has passed.
	os.WriteFile(filepath.Join(x.dir, "me.group.example", "index.html"), []byte(strings.Repeat("v2 ", 100)), 0o644)
	x.settle(t)
	if vs, _ := x.w.cfg.Pub.Versions("me.group.example"); len(vs) != 2 {
		t.Fatalf("%d versions after an edit", len(vs))
	}

	// No index.html: held back, with a note.
	os.Remove(filepath.Join(x.dir, "me.group.example", "index.html"))
	x.settle(t)
	if vs, _ := x.w.cfg.Pub.Versions("me.group.example"); len(vs) != 2 || !strings.Contains(x.note("me.group.example"), "no index.html") {
		t.Fatalf("%d versions, note %q", len(vs), x.note("me.group.example"))
	}

	// Forced, it is published as it is.
	x.w.Force("me.group.example")
	x.settle(t)
	if vs, _ := x.w.cfg.Pub.Versions("me.group.example"); len(vs) != 3 {
		t.Fatalf("%d versions after forcing", len(vs))
	}
	os.WriteFile(filepath.Join(x.dir, "me.group.example", "index.html"), []byte(strings.Repeat("v2 ", 100)), 0o644)
	x.settle(t)
	if vs, _ := x.w.cfg.Pub.Versions("me.group.example"); len(vs) != 4 {
		t.Fatalf("%d versions after putting index.html back", len(vs))
	}

	// The folder deleted: it comes back with the version being served.
	os.RemoveAll(filepath.Join(x.dir, "me.group.example"))
	x.settle(t)
	got, err := os.ReadFile(filepath.Join(x.dir, "me.group.example", "index.html"))
	if err != nil || string(got) != strings.Repeat("v2 ", 100) {
		t.Fatalf("folder not refilled with the live version: %q, %v (logs: %v)", got, err, x.logs)
	}
	if _, err := os.Stat(filepath.Join(x.dir, "me.group.example"+NoteSuffix)); !os.IsNotExist(err) {
		t.Fatal("the note stayed after the folder was refilled")
	}
	if vs, _ := x.w.cfg.Pub.Versions("me.group.example"); len(vs) != 4 {
		t.Fatalf("refilling made a new version: %d", len(vs))
	}
}

// What happened on 2026-10-22: a push replaced most of a site, and the
// change was held back. "Keep the site as it is" puts the live version
// back in the folder, keeping what was there; going back to the version
// before puts it online and in the folder.
func TestKeepAndGoBack(t *testing.T) {
	x := setup(t)
	x.w.cfg.Replaced, x.w.cfg.Buttons = filepath.Join(t.TempDir(), "replaced"), true
	const name = "me.group.example"
	x.site(name, map[string]string{"index.html": strings.Repeat("her page ", 200), "photo.jpg": strings.Repeat("x", 4000)})
	x.settle(t)
	os.RemoveAll(filepath.Join(x.dir, name))
	x.site(name, map[string]string{"index.html": "starter"})
	x.settle(t)
	if vs, _ := x.w.cfg.Pub.Versions(name); len(vs) != 1 || !strings.Contains(x.note(name), "went from 2 files") {
		t.Fatalf("%d versions, note %q", len(vs), x.note(name))
	}

	x.w.Refill(name)
	x.settle(t)
	if got, _ := os.ReadFile(filepath.Join(x.dir, name, "index.html")); string(got) != strings.Repeat("her page ", 200) || x.note(name) != "" {
		t.Fatalf("after keeping: %q, note %q", got, x.note(name))
	}
	kept, _ := filepath.Glob(filepath.Join(x.w.cfg.Replaced, name+"-*", "index.html"))
	if len(kept) != 1 {
		t.Fatalf("the change wasn't kept: %v", kept)
	}
	if b, _ := os.ReadFile(kept[0]); string(b) != "starter" {
		t.Fatalf("kept %q", b)
	}

	// An ordinary edit, then back to the version before.
	os.WriteFile(filepath.Join(x.dir, name, "index.html"), []byte(strings.Repeat("her new page ", 200)), 0o644)
	x.now = x.now.Add(time.Hour)
	x.settle(t)
	if vs, _ := x.w.cfg.Pub.Versions(name); len(vs) != 2 {
		t.Fatalf("%d versions after an edit", len(vs))
	}
	if _, err := x.w.cfg.Pub.Rollback(context.Background(), name, ""); err != nil {
		t.Fatal(err)
	}
	x.w.Refill(name)
	x.settle(t)
	if got, _ := os.ReadFile(filepath.Join(x.dir, name, "index.html")); string(got) != strings.Repeat("her page ", 200) {
		t.Fatalf("after going back the folder holds %q", got)
	}
	x.settle(t)
	if vs, _ := x.w.cfg.Pub.Versions(name); len(vs) != 2 || !vs[1].Current {
		t.Fatalf("going back published again, or the wrong one is online: %+v", vs)
	}
}

// A version published some other way (the dashboard's Edit) comes into
// the folder, so the next edit there doesn't undo it.
func TestFolderCatchesUpWithOtherPublishes(t *testing.T) {
	x := setup(t)
	x.site("me.group.example", map[string]string{"index.html": strings.Repeat("v1 ", 100)})
	x.settle(t)

	elsewhere := t.TempDir()
	os.WriteFile(filepath.Join(elsewhere, "index.html"), []byte(strings.Repeat("dashboard ", 40)), 0o644)
	if _, _, err := x.w.cfg.Pub.Publish(context.Background(), "me.group.example", elsewhere, x.nodes); err != nil {
		t.Fatal(err)
	}
	x.settle(t)
	got, _ := os.ReadFile(filepath.Join(x.dir, "me.group.example", "index.html"))
	if string(got) != strings.Repeat("dashboard ", 40) {
		t.Fatalf("folder holds %q, not the version published from the dashboard (logs: %v)", got, x.logs)
	}
	if vs, _ := x.w.cfg.Pub.Versions("me.group.example"); len(vs) != 2 {
		t.Fatalf("catching up made a new version: %d", len(vs))
	}

	// After a restart too: published elsewhere while the watcher was down.
	os.WriteFile(filepath.Join(elsewhere, "index.html"), []byte(strings.Repeat("while down ", 40)), 0o644)
	x.w.cfg.Pub.Publish(context.Background(), "me.group.example", elsewhere, x.nodes)
	x.w = New(x.w.cfg)
	x.w.load()
	x.settle(t)
	got, _ = os.ReadFile(filepath.Join(x.dir, "me.group.example", "index.html"))
	if string(got) != strings.Repeat("while down ", 40) {
		t.Fatalf("after a restart the folder holds %q (logs: %v)", got, x.logs)
	}
}

func TestSkip(t *testing.T) {
	for rel, want := range map[string]bool{"index.html": false, "img/a.png": false, ".stfolder": true,
		".stversions/old.html": true, "~syncthing~x.tmp": true, "img/Thumbs.db": true, ".well-known/security.txt": false, ".git/HEAD": true} {
		if Skip(rel, false) != want {
			t.Errorf("Skip(%q) = %v", rel, !want)
		}
	}
}
