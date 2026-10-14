package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/syncthing"
	"github.com/peterretief/yggstore/internal/workcopy"
	"github.com/peterretief/yggstore/internal/workhere"
)

// A person who paired on the LAN asks the box's page for their link; a web
// node emails it; opened elsewhere, it finds their folder.
func TestAnywhereFromTheBoxPage(t *testing.T) {
	u, fake, _ := setupUsers(t, 0)
	ctx := context.Background()
	d := newDesktop(t) // only for its test nodes
	b := &bus{}
	u.cfg.Client = client.New()
	u.cfg.Bus = func() (workcopy.Bus, error) { return b, nil }
	u.cfg.Online = func(context.Context) ([]peers.Peer, error) { return d.nodes, nil }

	// A web node, keeping records and sending email.
	var sent struct{ to, text string }
	store := &workhere.Store{Dir: t.TempDir(), Email: func(_ context.Context, to, subject, text string) error {
		sent.to, sent.text = to, text
		return nil
	}}
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !store.Serve(w, r, "200::1") {
			http.NotFound(w, r)
		}
	}))
	defer web.Close()
	webAddr := strings.TrimPrefix(web.URL, "http://")
	u.cfg.WebNodes = func(context.Context) []string { return []string{webAddr} }
	u.cfg.MailNodes = u.cfg.WebNodes
	u.cfg.LinkHost = func(context.Context) string { return "example.org" }
	u.cfg.SelfAddr = "[200::3]:7400"
	box := httptest.NewServer(u.WorkHandler(func(string) bool { return true }))
	defer box.Close()
	store.BoxURL = func(string) string { return box.URL }

	fake.pending[laptopID] = syncthing.PendingDevice{Name: "anna-pc", Address: "192.168.1.5:22000"}
	u.step(ctx)
	os.MkdirAll(filepath.Join(u.cfg.Dir, "anna-pc", "files"), 0o755)
	os.WriteFile(filepath.Join(u.cfg.Dir, "anna-pc", "files", "notes.txt.ystub"), []byte(`{"n":1}`), 0o644)

	if _, err := u.Anywhere(ctx, "NOTPAIR-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA", "anna@example.com"); err == nil {
		t.Fatal("a device that isn't paired got a link")
	}
	msg, err := u.Anywhere(ctx, strings.ToLower(laptopID), "anna@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if sent.to != "anna@example.com" || !strings.Contains(sent.text, "https://example.org"+workhere.PagePath+"#") {
		t.Fatalf("sent %+v (%s)", sent, msg)
	}
	link := sent.text[strings.Index(sent.text, "https://"):]
	link = link[:strings.IndexAny(link, " \n")]
	_, frag, _ := strings.Cut(link, "#")
	id, half, _ := strings.Cut(frag, ".")

	// Opened on the client's PC: the laptop has the folder, so it asks.
	open := func(move bool) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"id": id, "link": half, "device": clientPC, "move": move})
		w := httptest.NewRecorder()
		store.ServeHTTP(w, httptest.NewRequest(http.MethodPost, workhere.PagePath, strings.NewReader(string(body))))
		return w
	}
	if w := open(false); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "anna-pc") {
		t.Fatalf("open: %d %s", w.Code, w.Body)
	}
	// Stored on the box after the last copy: moving devices mustn't lose it.
	os.WriteFile(filepath.Join(u.cfg.Dir, "anna-pc", "files", "late.txt.ystub"), []byte(`{"late":1}`), 0o644)
	if w := open(true); w.Code != http.StatusOK {
		t.Fatalf("move: %d %s", w.Code, w.Body)
	}
	u.step(ctx)
	st, _ := Load(u.cfg.StatePath)
	p, _ := st.PersonNamedOK("anna-pc")
	if got := strings.Join(folderDevices(fake, p.Folder), " "); got != "HUBHUBH-AAAAAAA "+clientPC {
		t.Fatalf("after moving, shared with %s", got)
	}
	if b, _ := os.ReadFile(filepath.Join(p.Path, "files", "notes.txt.ystub")); string(b) != `{"n":1}` {
		t.Fatalf("their files after moving: %q", b)
	}
	if _, err := os.Stat(filepath.Join(p.Path, "files", "late.txt.ystub")); err != nil {
		t.Fatal("moving to another device lost a stub stored after the last copy")
	}
}
