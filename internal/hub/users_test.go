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

	"github.com/peterretief/yggstore/internal/peers"
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
	if fake.options["globalAnnounceEnabled"] != true || fake.options["relaysEnabled"] != true {
		t.Fatalf("paired devices can't reach the hub from away: %v", fake.options)
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
	if len(st.Users) != 1 || st.Users[0].Name != "anna-s-laptop" || st.Users[0].Folder != "yggstore-HUBHUBH-ANNAAAA" {
		t.Fatalf("users %+v; want anna-s-laptop only", st.Users)
	}
	if _, ok := fake.devices["EVILLLL-2"]; ok {
		t.Fatal("a device from the internet was paired")
	}
	if _, ok := fake.pending["EVILLLL-2"]; ok {
		t.Fatal("the internet device was left pending")
	}
	fo := fake.folders["yggstore-HUBHUBH-ANNAAAA"]
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
	if _, ok := fake.devices["ONEEEEE-1"]; ok || fake.folders["yggstore-HUBHUBH-ONEEEEE"] != nil {
		t.Fatal("removed device still in Syncthing")
	}
	// With "one" gone, "two" may pair now, and joins paused like the rest.
	if len(st.Users) != 1 || st.Users[0].Name != "two" || fake.folders["yggstore-HUBHUBH-TWOOOOO"]["paused"] != true {
		t.Fatalf("users after remove: %+v", st.Users)
	}
}

// TestWebsitesSharedWithEveryDevice: the websites folder goes to every
// paired device once ready, and follows new devices; a site's old folder of
// its own is no longer synced.
func TestWebsitesSharedWithEveryDevice(t *testing.T) {
	u, fake, dir := setupUsers(t, 0)
	statePath := filepath.Join(dir, "hub.json")
	ctx := context.Background()
	ready := false
	u.cfg.Websites, u.cfg.WebsitesReady = filepath.Join(dir, "websites"), func() bool { return ready }
	fake.folders["yggstore-site-shop.example.org"] = map[string]any{"id": "yggstore-site-shop.example.org"}
	Update(ctx, statePath, func(s *HubState) error {
		s.Sites = append(s.Sites, Site{Name: "shop.example.org", Folder: "yggstore-site-shop.example.org", Path: filepath.Join(dir, "sites", "shop.example.org")})
		return nil
	})
	fake.pending["ANNAAAA-1"] = syncthing.PendingDevice{Name: "anna", Address: "192.168.1.20:22000"}
	u.step(ctx)
	if fake.folders[WebsitesFolderID] != nil {
		t.Fatal("websites shared before the folder was ready")
	}
	if st, _ := Load(statePath); fake.folders["yggstore-site-shop.example.org"] != nil || st.Sites[0].Folder != "" {
		t.Fatalf("the site's old folder is still synced: %+v", st.Sites)
	}

	ready = true
	u.step(ctx)
	devices := func() string {
		b, _ := json.Marshal(fake.folders[WebsitesFolderID]["devices"])
		return string(b)
	}
	if d := devices(); !strings.Contains(d, "HUBHUBH") || !strings.Contains(d, "ANNAAAA-1") {
		t.Fatalf("websites shared with %s; want the hub and anna", d)
	}
	fake.pending["BOBBBBB-2"] = syncthing.PendingDevice{Name: "bob", Address: "192.168.1.21:22000"}
	u.step(ctx)
	if d := devices(); !strings.Contains(d, "BOBBBBB-2") {
		t.Fatalf("new device not given the websites: %s", d)
	}
}

// folderDevices lists the devices a folder is shared with in the fake.
func folderDevices(fake *fakeST, id string) []string {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fo := fake.folders[id]
	if fo == nil {
		return nil
	}
	var out []string
	b, _ := json.Marshal(fo["devices"])
	var devs []struct {
		DeviceID string `json:"deviceID"`
	}
	json.Unmarshal(b, &devs)
	for _, d := range devs {
		out = append(out, d.DeviceID)
	}
	return out
}

// A person's devices share one folder; removing a device leaves it with the
// rest, and removing the last keeps the files on the box, unshared.
func TestPeopleShareOneFolder(t *testing.T) {
	u, fake, dir := setupUsers(t, 0)
	statePath := filepath.Join(dir, "hub.json")
	ctx := context.Background()
	fake.pending["LAPTOPP-1"] = syncthing.PendingDevice{Name: "anna-laptop", Address: "192.168.1.5:22000"}
	fake.pending["PHONEEE-2"] = syncthing.PendingDevice{Name: "anna-phone", Address: "192.168.1.6:22000"}
	if err := u.step(ctx); err != nil {
		t.Fatal(err)
	}
	st, _ := Load(statePath)
	if len(st.People) != 2 {
		t.Fatalf("people %+v", st.People)
	}
	laptop, _ := st.PersonNamed("anna-laptop")

	// The phone is Anna's too.
	Update(ctx, statePath, func(s *HubState) error {
		for i := range s.Users {
			if s.Users[i].Name == "anna-phone" {
				s.Users[i].Person = "anna-laptop"
			}
		}
		return nil
	})
	if err := u.step(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(folderDevices(fake, laptop.Folder), " "); got != "HUBHUBH-AAAAAAA LAPTOPP-1 PHONEEE-2" {
		t.Fatalf("Anna's folder shared with %s", got)
	}
	if fake.folders["yggstore-PHONEEE"] != nil {
		t.Fatal("the phone's own folder still shared, with no device")
	}

	// The laptop goes; the phone keeps the folder.
	Update(ctx, statePath, func(s *HubState) error {
		for i := range s.Users {
			if s.Users[i].Name == "anna-laptop" {
				s.Users[i].Remove = true
			}
		}
		return nil
	})
	u.step(ctx)
	if got := strings.Join(folderDevices(fake, laptop.Folder), " "); got != "HUBHUBH-AAAAAAA PHONEEE-2" {
		t.Fatalf("after removing the laptop, shared with %s", got)
	}
	// The phone goes too: the folder is unshared, its files kept.
	os.WriteFile(filepath.Join(laptop.Path, "note.txt.ystub"), []byte("{}"), 0o644)
	Update(ctx, statePath, func(s *HubState) error { s.Users[0].Remove = true; return nil })
	u.step(ctx)
	if fake.folders[laptop.Folder] != nil {
		t.Fatal("folder still shared with no devices")
	}
	if _, err := os.Stat(filepath.Join(laptop.Path, "note.txt.ystub")); err != nil {
		t.Fatal("files went with the last device")
	}
	if st, _ := Load(statePath); len(st.People) != 2 {
		t.Fatalf("people after removing devices: %+v", st.People)
	}
}

// Devices paired before people existed become people, keeping their folders.
func TestOldDevicesBecomePeople(t *testing.T) {
	u, fake, dir := setupUsers(t, 0)
	statePath := filepath.Join(dir, "hub.json")
	ctx := context.Background()
	Update(ctx, statePath, func(s *HubState) error {
		s.Users = []User{{Name: "bob-pc", Device: "BOBBBBB-1", Folder: "yggstore-BOBBBBB", Path: filepath.Join(dir, "users", "bob-pc"), Added: 1}}
		return nil
	})
	if err := u.step(ctx); err != nil {
		t.Fatal(err)
	}
	st, _ := Load(statePath)
	p, ok := st.PersonNamed("bob-pc")
	if !ok || p.Folder != "yggstore-BOBBBBB" || st.Users[0].Person != "bob-pc" {
		t.Fatalf("after migrating: %+v %+v", st.People, st.Users)
	}
	if got := strings.Join(folderDevices(fake, "yggstore-BOBBBBB"), " "); got != "HUBHUBH-AAAAAAA BOBBBBB-1" {
		t.Fatalf("shared with %s", got)
	}
}

// A team folder is shared with every device of every member, follows the
// members as they change, and stays on the box when the team is removed.
func TestTeamFolders(t *testing.T) {
	u, fake, dir := setupUsers(t, 0)
	statePath := filepath.Join(dir, "hub.json")
	ctx := context.Background()
	fake.pending["ANNALAP-1"] = syncthing.PendingDevice{Name: "anna", Address: "192.168.1.5:22000"}
	fake.pending["BOBPCPC-2"] = syncthing.PendingDevice{Name: "bob", Address: "192.168.1.6:22000"}
	fake.pending["CAROLPC-3"] = syncthing.PendingDevice{Name: "carol", Address: "192.168.1.7:22000"}
	u.step(ctx)
	teamPath := filepath.Join(dir, "teams", "Projects")
	Update(ctx, statePath, func(s *HubState) error {
		s.Teams = append(s.Teams, Team{Name: "Projects", Folder: TeamFolderID("Projects"), Path: teamPath, Members: []string{"anna", "bob"}})
		return nil
	})
	if err := u.step(ctx); err != nil {
		t.Fatal(err)
	}
	id := TeamFolderID("Projects")
	if got := strings.Join(folderDevices(fake, id), " "); got != "HUBHUBH-AAAAAAA ANNALAP-1 BOBPCPC-2" {
		t.Fatalf("team shared with %s", got)
	}

	// Carol joins, Bob leaves.
	Update(ctx, statePath, func(s *HubState) error { s.Teams[0].Members = []string{"anna", "carol"}; return nil })
	u.step(ctx)
	if got := strings.Join(folderDevices(fake, id), " "); got != "HUBHUBH-AAAAAAA ANNALAP-1 CAROLPC-3" {
		t.Fatalf("after changing members, shared with %s", got)
	}

	// A conflict copy is listed.
	os.MkdirAll(filepath.Join(teamPath, "files"), 0o755)
	os.WriteFile(filepath.Join(teamPath, "files", "plan.sync-conflict-20261014-101010-ANNALAP.docx.ystub"), []byte("{}"), 0o644)
	st, _ := Load(statePath)
	if cs := st.Conflicts(); len(cs) != 1 || cs[0].Folder != "team Projects" {
		t.Fatalf("conflicts %+v", cs)
	}

	// Removed: unshared, files kept, gone from the list.
	Update(ctx, statePath, func(s *HubState) error { s.Teams[0].Remove = true; return nil })
	u.step(ctx)
	if fake.folders[id] != nil {
		t.Fatal("removed team still shared")
	}
	if _, err := os.Stat(filepath.Join(teamPath, "files")); err != nil {
		t.Fatal("removing the team lost its files")
	}
	if st, _ := Load(statePath); len(st.Teams) != 0 {
		t.Fatalf("teams %+v", st.Teams)
	}
}

// TestOwnersFirstDevice: the box's owner, already on the box, gets their
// first device on their own folder; anyone else's is a new person.
func TestOwnersFirstDevice(t *testing.T) {
	u, fake, dir := setupUsers(t, 0)
	ctx := context.Background()
	statePath := filepath.Join(dir, "hub.json")
	u.cfg.PeersPath = filepath.Join(dir, "peers.json")
	u.cfg.SelfAddr = "[200::3]:7400"
	peers.Write(u.cfg.PeersPath, []peers.Peer{{Name: "desktop", Addr: "[200::1]:7400", Admin: true, Owner: "peter"},
		{Name: "localmail", Addr: "[200::3]:7400", Owner: "Tim"}})
	var tim Person
	Update(ctx, statePath, func(s *HubState) error {
		var err error
		tim, err = s.AddPerson("Tim", filepath.Join(dir, "users"))
		return err
	})
	fake.pending["TIMLAPT-1"] = syncthing.PendingDevice{Name: "TIM-LAPTOP", Address: "10.0.0.20:22000"}
	if err := u.step(ctx); err != nil {
		t.Fatal(err)
	}
	fake.pending["TIMPHON-2"] = syncthing.PendingDevice{Name: "visitor", Address: "10.0.0.21:22000"}
	if err := u.step(ctx); err != nil {
		t.Fatal(err)
	}
	st, _ := Load(statePath)
	if len(st.Users) != 2 || st.Users[0].Person != "Tim" || st.Users[0].Folder != tim.Folder {
		t.Fatalf("the owner's first device: %+v", st.Users)
	}
	if st.Users[1].Person == "Tim" || len(st.People) != 2 {
		t.Fatalf("a second device went to the owner too: %+v", st.Users)
	}
	if fo := fake.folders[tim.Folder]; fo == nil {
		t.Fatalf("Tim's folder isn't shared: %v", fake.folders)
	}
}
