package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/localstore"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/server"
	"github.com/peterretief/yggstore/internal/transport"
)

func get(h http.Handler, path, from, host string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	req.RemoteAddr, req.Host = from, host
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestState: the page sees this box's node up, and counts the group.
func TestState(t *testing.T) {
	dir := t.TempDir()
	// The loopback transport sees every caller, the hub included, as 127.0.0.1.
	nodeList := filepath.Join(dir, "node-peers.json")
	peers.Write(nodeList, []peers.Peer{{Name: "self", Addr: "127.0.0.1:1"}})
	live, err := peers.NewLive(nodeList, filepath.Join(dir, "node-pushed.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	node := httptest.NewServer(server.Handler(localstore.New(t.TempDir()), server.Options{Transport: transport.Loopback{}, Peers: live}))
	defer node.Close()
	list := filepath.Join(dir, "peers.json")
	peers.Write(list, []peers.Peer{{Name: "pi", Addr: strings.TrimPrefix(node.URL, "http://")}, {Name: "gone", Addr: "127.0.0.1:1"}})

	h := New(Config{PeersPath: list, SelfName: "pi", Client: client.New(), Version: "v1"}).Handler()
	rec := get(h, "/api/state", "192.168.1.20:5000", "pi.local:7490")
	var st State
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(rec.Code, rec.Body.String())
	}
	if st.Node == nil || st.Node.Name != "pi" || !st.Node.Up || st.Up != 1 || st.Nodes != 2 {
		t.Fatalf("got %+v (node %+v); want pi up, 1 of 2", st, st.Node)
	}
}

func TestOnlyLAN(t *testing.T) {
	h := New(Config{PeersPath: "missing.json", Client: client.New()}).Handler()
	for from, want := range map[string]int{"127.0.0.1:1": 200, "192.168.1.5:1": 200, "10.1.2.3:1": 200, "[fd00::5]:1": 200,
		"8.8.8.8:1": 403, "[2001:db8::1]:1": 403} {
		if rec := get(h, "/", from, "pi.local:7490"); rec.Code != want {
			t.Errorf("from %s: %d, want %d", from, rec.Code, want)
		}
	}
}

func TestBins(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"yggstore-linux-arm64", "yggstore-windows-amd64.exe", "yggstore-linux-evil", "notes.txt"} {
		os.WriteFile(filepath.Join(dir, n), []byte("build "+n), 0o755)
	}
	bins := map[string]string{}
	if err := FromDir(bins, dir); err != nil {
		t.Fatal(err)
	}
	if len(bins) != 1 || bins["linux-arm64"] == "" {
		t.Fatalf("got %v; want linux-arm64 only", bins)
	}
	h := New(Config{Bins: bins, Client: client.New()}).Handler()
	if rec := get(h, "/bin/linux-arm64", "192.168.1.5:1", "pi.local"); rec.Code != 200 || rec.Body.String() != "build yggstore-linux-arm64" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
	if rec := get(h, "/bin/linux-amd64", "192.168.1.5:1", "pi.local"); rec.Code != 404 {
		t.Fatalf("missing build: %d, want 404", rec.Code)
	}
}

// TestInstallScript: the script names the hub it came from, is valid shell,
// and a Host that could break out of it is refused.
func TestInstallScript(t *testing.T) {
	h := New(Config{Client: client.New()}).Handler()
	for _, host := range []string{"pi.local:7490", "192.168.1.9", "[fd00::9]:7490"} {
		rec := get(h, "/install.sh", "192.168.1.5:1", host)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "HUB='http://"+host+"'") {
			t.Fatalf("%s: %d %q", host, rec.Code, rec.Body.String())
		}
	}
	for _, host := range []string{"pi.local'; rm -rf ~; '", "a b", "$(id)"} {
		if rec := get(h, "/install.sh", "192.168.1.5:1", host); rec.Code != 400 {
			t.Fatalf("host %q: %d, want 400", host, rec.Code)
		}
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	script := filepath.Join(t.TempDir(), "install.sh")
	os.WriteFile(script, get(h, "/install.sh", "192.168.1.5:1", "pi.local").Body.Bytes(), 0o644)
	if out, err := exec.Command(sh, "-n", script).CombinedOutput(); err != nil {
		t.Fatalf("not valid shell: %v %s", err, out)
	}
	// With no invite it stops before doing anything.
	if out, err := exec.Command(sh, script).CombinedOutput(); err == nil || !strings.Contains(string(out), "usage") {
		t.Fatalf("no invite: %v %s", err, out)
	}
}
