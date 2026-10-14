package workhere

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/files"
	"github.com/peterretief/yggstore/internal/localstore"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/server"
	"github.com/peterretief/yggstore/internal/transport"
)

// The live page works on the folder itself, at once.
func TestLive(t *testing.T) {
	ctx := context.Background()
	var nodes []peers.Peer
	for i := range 4 {
		srv := httptest.NewServer(server.Handler(localstore.New(t.TempDir()), server.Options{
			Transport: transport.Loopback{}, Allowed: map[string]bool{"127.0.0.1": true}}))
		t.Cleanup(srv.Close)
		nodes = append(nodes, peers.Peer{Name: string(rune('a' + i)), Addr: strings.TrimPrefix(srv.URL, "http://")})
	}
	out := t.TempDir()
	m, _, _, err := files.PutReader(ctx, client.New(), strings.NewReader("the quarterly report"), "report.txt", nodes, files.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(out, "files", "Docs"), 0o755)
	files.WriteJSON(filepath.Join(out, "files", "Docs", "report.txt.ystub"), m)
	os.WriteFile(filepath.Join(out, "files", "Docs", ".report.txt.ystub.challenges.json"), []byte("{}"), 0o644)
	holding := true
	l := &Live{Outbox: out, Client: client.New(), Holding: func() bool { return holding }}

	do := func(method, target, body string, hdr map[string]string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, target, strings.NewReader(body))
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		l.ServeHTTP(w, r)
		return w
	}
	names := func(dir string) string {
		w := do("POST", "/files?op=list", `{"path":"`+dir+`"}`, nil)
		if w.Code != 200 {
			return "error " + w.Body.String()
		}
		var lst Listing
		json.Unmarshal(w.Body.Bytes(), &lst)
		var got []string
		for _, e := range lst.Entries {
			n := e.Name
			if e.Folder {
				n += "/"
			}
			if e.Note != "" {
				n += " (" + e.Note + ")"
			}
			got = append(got, n)
		}
		return strings.Join(got, ", ")
	}
	change := func(op, p, to string) int {
		b, _ := json.Marshal(Change{Op: op, Path: p, To: to})
		return do("POST", "/files?op=change", string(b), nil).Code
	}

	if w := do("GET", "/files", "", nil); !strings.Contains(w.Body.String(), `data-live="1"`) {
		t.Fatal("page not marked live")
	}
	if got := names("Docs"); got != "report.txt" {
		t.Fatalf("Docs: %s", got)
	}
	if w := do("GET", "/files?op=get&path="+url.QueryEscape("Docs/report.txt"), "", nil); w.Body.String() != "the quarterly report" {
		t.Fatalf("download: %d %q", w.Code, w.Body)
	}
	big := strings.Repeat("x", 3<<20)
	if w := do("POST", "/files?op=put", big, map[string]string{"X-Dir": "Docs", "X-Name": url.PathEscape("big file.bin")}); w.Code != 200 {
		t.Fatalf("upload: %d %s", w.Code, w.Body)
	}
	if got := names("Docs"); got != "big file.bin (storing…), report.txt" {
		t.Fatalf("after upload: %s", got)
	}
	if w := do("GET", "/files?op=get&path="+url.QueryEscape("Docs/big file.bin"), "", nil); w.Body.Len() != len(big) {
		t.Fatalf("download of a file not stored yet: %d bytes", w.Body.Len())
	}
	for _, c := range []struct {
		op, p, to string
		code      int
	}{
		{OpFolder, "Projects", "", 200},
		{OpMove, "Docs/report.txt", "Projects/report.txt", 200},
		{OpMove, "Docs/big file.bin", "Docs/bigger.bin", 200},
		{OpMove, "Projects", "Projects/in", 400},
		{OpFolder, ".hidden", "", 400},
		{OpMove, "Docs/nothing", "Docs/x", 400},
		{OpDelete, "Docs/bigger.bin", "", 200},
	} {
		if code := change(c.op, c.p, c.to); code != c.code {
			t.Errorf("%s %s %s: %d, want %d", c.op, c.p, c.to, code, c.code)
		}
	}
	if got := names(""); got != "Docs/, Projects/" {
		t.Fatalf("top: %s", got)
	}
	if got := names("Projects"); got != "report.txt" {
		t.Fatalf("Projects: %s", got)
	}
	if got := names("Docs"); got != "" {
		t.Fatalf("Docs after: %s", got)
	}
	if !exists(filepath.Join(out, "files", "Projects", ".report.txt.ystub.challenges.json")) {
		t.Fatal("the challenges file didn't move with its stub")
	}
	holding = false
	if code := change(OpDelete, "Projects", ""); code != http.StatusConflict {
		t.Fatalf("a change while another place has the files: %d", code)
	}
}
