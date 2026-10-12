package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/syncthing"
)

// fakeST is enough of Syncthing's REST API for the hub.
type fakeST struct {
	mu      sync.Mutex
	pending map[string]syncthing.PendingDevice
	devices map[string]map[string]any
	folders map[string]map[string]any
	options map[string]any
}

func newFakeST() *fakeST {
	return &fakeST{pending: map[string]syncthing.PendingDevice{}, devices: map[string]map[string]any{},
		folders: map[string]map[string]any{}, options: map[string]any{}}
}

func (f *fakeST) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("X-API-Key") != "key" {
		http.Error(w, "forbidden", 403)
		return
	}
	body := map[string]any{}
	json.NewDecoder(r.Body).Decode(&body)
	p := r.URL.Path
	out := func(v any) { json.NewEncoder(w).Encode(v) }
	switch {
	case p == "/rest/system/status":
		out(map[string]string{"myID": "HUBHUBH-AAAAAAA"})
	case p == "/rest/config/options":
		for k, v := range body {
			f.options[k] = v
		}
	case p == "/rest/cluster/pending/devices" && r.Method == "GET":
		out(f.pending)
	case p == "/rest/cluster/pending/devices" && r.Method == "DELETE":
		delete(f.pending, r.URL.Query().Get("device"))
	case strings.HasPrefix(p, "/rest/config/defaults/"):
		out(map[string]any{"someDefault": 1})
	case p == "/rest/config/folders" && r.Method == "GET":
		list := []map[string]any{}
		for _, fo := range f.folders {
			list = append(list, fo)
		}
		out(list)
	case strings.HasPrefix(p, "/rest/config/devices/"):
		id := strings.TrimPrefix(p, "/rest/config/devices/")
		if r.Method == "DELETE" {
			delete(f.devices, id)
		} else {
			f.devices[id] = body
			delete(f.pending, id)
		}
	case strings.HasPrefix(p, "/rest/config/folders/"):
		id := strings.TrimPrefix(p, "/rest/config/folders/")
		switch r.Method {
		case "DELETE":
			delete(f.folders, id)
		case "PATCH":
			for k, v := range body {
				f.folders[id][k] = v
			}
		default:
			f.folders[id] = body
		}
	default:
		http.NotFound(w, r)
	}
}

func setupUsers(t *testing.T, limit int) (*Users, *fakeST, string) {
	t.Helper()
	fake := newFakeST()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	u := NewUsers(UsersConfig{ST: syncthing.New(srv.URL, "key"), Dir: filepath.Join(dir, "users"),
		StatePath: filepath.Join(dir, "hub.json"), Limit: limit, Logf: t.Logf})
	if err := u.setup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fake.options["globalAnnounceEnabled"] != false || fake.options["relaysEnabled"] != false {
		t.Fatalf("hub left Syncthing open beyond the LAN: %v", fake.options)
	}
	return u, fake, dir
}

func TestPairsOnlyTheLAN(t *testing.T) {
	u, fake, dir := setupUsers(t, 0)
	fake.pending["ANNAAAA-1"] = syncthing.PendingDevice{Name: "anna's laptop", Address: "192.168.1.20:22000"}
	fake.pending["EVILLLL-2"] = syncthing.PendingDevice{Name: "stranger", Address: "203.0.113.9:22000"}
	if err := u.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, _ := Load(filepath.Join(dir, "hub.json"))
	if len(st.Users) != 1 || st.Users[0].Name != "anna-s-laptop" || st.Users[0].Folder != "yggstore-ANNAAAA" {
		t.Fatalf("users %+v; want anna-s-laptop only", st.Users)
	}
	if _, ok := fake.devices["EVILLLL-2"]; ok {
		t.Fatal("a device from the internet was paired")
	}
	if _, ok := fake.pending["EVILLLL-2"]; ok {
		t.Fatal("the internet device was left pending")
	}
	fo := fake.folders["yggstore-ANNAAAA"]
	if fo == nil || fo["someDefault"] == nil || fo["path"] != st.Users[0].Path {
		t.Fatalf("folder %v; want Syncthing's defaults with the user's path", fo)
	}
	if v, _ := json.Marshal(fo["versioning"]); !strings.Contains(string(v), "trashcan") {
		t.Fatalf("folder keeps no deleted stubs: %s", v)
	}
	if b, _ := os.ReadFile(filepath.Join(st.Users[0].Path, ".stignore")); !strings.Contains(string(b), ".yggstore") {
		t.Fatalf(".stignore %q", b)
	}
}

func TestSwitchesAndLimit(t *testing.T) {
	u, fake, dir := setupUsers(t, 1)
	statePath := filepath.Join(dir, "hub.json")
	ctx := context.Background()

	Update(ctx, statePath, func(s *HubState) error { s.PairingOff = true; return nil })
	fake.pending["ONEEEEE-1"] = syncthing.PendingDevice{Name: "one", Address: "10.0.0.5:22000"}
	u.step(ctx)
	if st, _ := Load(statePath); len(st.Users) != 0 {
		t.Fatal("paired with pairing off")
	}

	Update(ctx, statePath, func(s *HubState) error { s.PairingOff = false; return nil })
	fake.pending["TWOOOOO-2"] = syncthing.PendingDevice{Name: "two", Address: "10.0.0.6:22000"}
	u.step(ctx)
	if st, _ := Load(statePath); len(st.Users) != 1 {
		t.Fatalf("%d paired; the limit is 1 a day", len(st.Users))
	}

	Update(ctx, statePath, func(s *HubState) error { s.Paused = true; return nil })
	u.step(ctx)
	for id, fo := range fake.folders {
		if fo["paused"] != true {
			t.Fatalf("%s not paused", id)
		}
	}

	Update(ctx, statePath, func(s *HubState) error {
		s.Users[0].Remove = true
		s.Users[0].Added = time.Now().Add(-48 * time.Hour).Unix()
		return nil
	})
	u.step(ctx)
	st, _ := Load(statePath)
	if _, ok := fake.devices["ONEEEEE-1"]; ok || fake.folders["yggstore-ONEEEEE"] != nil {
		t.Fatal("removed device still in Syncthing")
	}
	// With "one" gone, "two" may pair now, and joins paused like the rest.
	if len(st.Users) != 1 || st.Users[0].Name != "two" || fake.folders["yggstore-TWOOOOO"]["paused"] != true {
		t.Fatalf("users after remove: %+v", st.Users)
	}
}
