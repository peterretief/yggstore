package outbox

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/files"
	"github.com/peterretief/yggstore/internal/peers"
)

// A node that is down doesn't hold a delete up: it is sent its deletes
// when it answers again, and its debt goes if it leaves the group.
func TestDeleteWithANodeDown(t *testing.T) {
	var down atomic.Bool
	var deletes atomic.Int32
	w, dir, events := setupWith(t, 4, func(i int, h http.Handler) http.Handler {
		if i != 3 {
			return h
		}
		return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			if down.Load() {
				http.Error(rw, "down", http.StatusBadGateway)
				return
			}
			if r.Method == http.MethodDelete {
				deletes.Add(1)
			}
			h.ServeHTTP(rw, r)
		})
	})
	probeTimeout = 2 * time.Second
	os.WriteFile(filepath.Join(dir, FilesDir, "film.mkv"), randBytes(300_000), 0o644)
	settle(t, w, filepath.Join(dir, FilesDir))
	stub := filepath.Join(dir, FilesDir, "film.mkv"+files.StubExt)
	b, err := os.ReadFile(stub)
	if err != nil {
		t.Fatalf("not stored: %v (events: %v)", err, *events)
	}

	down.Store(true)
	os.WriteFile(filepath.Join(dir, DeleteDir, "film.mkv"+files.StubExt), b, 0o644)
	settle(t, w, filepath.Join(dir, DeleteDir))
	if left, _ := os.ReadDir(filepath.Join(dir, DeleteDir)); len(left) != 0 {
		t.Fatalf("delete held up by a node that is down (events: %v)", *events)
	}
	if _, err := os.Stat(stub); !os.IsNotExist(err) {
		t.Fatal("stub still in files/")
	}
	owed := w.loadOwed()
	if len(owed) != 1 || len(owed[w.addrOf(t, "d")]) == 0 {
		t.Fatalf("owed %v", owed)
	}
	if last := (*events)[len(*events)-1]; !strings.Contains(last, "d (") {
		t.Fatalf("event %q doesn't name the node that is down", last)
	}

	// Still down: nothing changes.
	w.owedNext = time.Time{}
	w.poll(context.Background())
	if len(w.loadOwed()) != 1 {
		t.Fatal("debt dropped while the node is down")
	}

	// Back: it is sent its deletes.
	down.Store(false)
	want := len(owed[w.addrOf(t, "d")])
	w.owedNext = time.Time{}
	w.poll(context.Background())
	if len(w.loadOwed()) != 0 || int(deletes.Load()) != want {
		t.Fatalf("owed %v after the node came back; %d deletes, want %d (events: %v)", w.loadOwed(), deletes.Load(), want, *events)
	}
}

func TestOwedDroppedWhenNodeLeaves(t *testing.T) {
	w, _, events := setup(t, 2)
	w.owe(map[string][]string{"[200::9]:7400": {"aa", "bb"}})
	w.poll(context.Background())
	if len(w.loadOwed()) != 0 {
		t.Fatalf("debt of a node not in the group kept (events: %v)", *events)
	}
}

func (w *Watcher) addrOf(t *testing.T, name string) string {
	t.Helper()
	var list []peers.Peer
	b, _ := os.ReadFile(w.cfg.PeersPath)
	json.Unmarshal(b, &list)
	for _, p := range list {
		if p.Name == name {
			return p.Addr
		}
	}
	t.Fatalf("no node %s", name)
	return ""
}
