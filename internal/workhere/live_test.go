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

	// Kept whole and the websites open, a stray .ysend isn't shown, and a
	// website's files can be changed, but not the site itself.
	os.MkdirAll(filepath.Join(out, "whole"), 0o755)
	files.WriteJSON(filepath.Join(out, "whole", "Photos.ystub"), m)
	os.WriteFile(filepath.Join(out, "whole", "old.ysend"), []byte("x"), 0o644)
	os.MkdirAll(filepath.Join(out, "websites", "example.org"), 0o755)
	os.WriteFile(filepath.Join(out, "websites", "example.org", "index.html"), []byte("<h1>hi</h1>"), 0o644)
	if got := names(""); got != "Docs/, Kept whole/, Projects/, Your websites/" {
		t.Fatalf("top with the others: %s", got)
	}
	if got := names("Kept whole"); got != "Photos" {
		t.Fatalf("Kept whole: %s", got)
	}
	if got := names("Your websites/example.org"); got != "index.html" {
		t.Fatalf("a website: %s", got)
	}
	if w := do("GET", "/files?op=get&path="+url.QueryEscape("Your websites/example.org/index.html"), "", nil); w.Body.String() != "<h1>hi</h1>" {
		t.Fatalf("a website's file: %q", w.Body)
	}
	if w := do("POST", "/files?op=put", "body{}", map[string]string{"X-Dir": url.PathEscape("Your websites/example.org"), "X-Name": "style.css", "X-Replace": "1"}); w.Code != 200 {
		t.Fatalf("upload to a website: %d %s", w.Code, w.Body)
	}
	if b, _ := os.ReadFile(filepath.Join(out, "websites", "example.org", "style.css")); string(b) != "body{}" {
		t.Fatalf("the website's new file: %q", b)
	}
	for _, c := range []struct {
		op, p, to string
		code      int
	}{
		{OpFolder, "Your websites/example.org/about", "", 200},
		{OpMove, "Your websites/example.org/style.css", "Your websites/example.org/about/style.css", 200},
		{OpMove, "Your websites/example.org/index.html", "Projects/index.html", 400},
		{OpMove, "Projects/report.txt", "Your websites/example.org/report.txt", 400},
		{OpDelete, "Your websites/example.org", "", 400},
		{OpDelete, "Your websites", "", 400},
		{OpDelete, "Kept whole", "", 400},
		{OpFolder, "Your websites/new.org", "", 400},
		{OpDelete, "Your websites/example.org/about/style.css", "", 200},
	} {
		if code := change(c.op, c.p, c.to); code != c.code {
			t.Errorf("%s %s %s: %d, want %d", c.op, c.p, c.to, code, c.code)
		}
	}
	// A folder's files make its folders as they come.
	for _, c := range []struct {
		dir  string
		code int
	}{
		{"Trip/day 1", 200},
		{"Trip/day 2/evening", 200},
		{"Projects/report.txt/inside", 400}, // a file is there
		{"Trip/.git", 400},
		{"Your websites/new.org", 400},
		{"Your websites/example.org/img/small", 200},
	} {
		if w := do("POST", "/files?op=put", "photo", map[string]string{"X-Dir": url.PathEscape(c.dir), "X-Name": "a.jpg"}); w.Code != c.code {
			t.Errorf("upload into %s: %d, want %d", c.dir, w.Code, c.code)
		}
	}
	if got := names("Trip"); got != "day 1/, day 2/" {
		t.Fatalf("Trip: %s", got)
	}
	if b, _ := os.ReadFile(filepath.Join(out, "files", "Trip", "day 2", "evening", "a.jpg")); string(b) != "photo" {
		t.Fatalf("a folder's file: %q", b)
	}
	if exists(filepath.Join(out, "websites", "new.org")) {
		t.Fatal("a website was made by an upload")
	}
	if w := do("POST", "/files?op=put", "x", map[string]string{"X-Dir": url.PathEscape("Kept whole"), "X-Name": "a.txt"}); w.Code != 400 {
		t.Fatalf("upload into Kept whole: %d", w.Code)
	}

	holding = false
	if code := change(OpDelete, "Projects", ""); code != http.StatusConflict {
		t.Fatalf("a change while another place has the files: %d", code)
	}
}
