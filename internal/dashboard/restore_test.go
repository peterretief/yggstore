package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterretief/yggstore/internal/outbox"
)

func TestRestoreFolderSetting(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "outfiles")
	d := New(Config{PeersPath: filepath.Join(dir, "peers.json"), StubDir: out, SelfID: "200::1",
		Listen: "127.0.0.1:7480", Outbox: outbox.New(outbox.Config{Dir: out})})
	h := d.Handler()
	do := func(path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "http://127.0.0.1:7480"+path, strings.NewReader(body))
		r.Header.Set("X-Yggstore", "1")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	elsewhere := filepath.Join(dir, "Restores")
	w := do("/api/restore-to", `{"dir": "`+filepath.ToSlash(elsewhere)+`"}`)
	var st RestoreState
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &st) != nil || st.Dir != elsewhere || st.Default != filepath.Join(out, "restored") {
		t.Fatalf("restore-to: %d %s", w.Code, w.Body)
	}
	if w := do("/api/restore-to", `{"dir": "`+filepath.ToSlash(filepath.Join(out, "x"))+`"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("folder inside the outbox accepted: %d", w.Code)
	}
	// Only restored items and the restore folder can be opened.
	if w := do("/api/open-folder?path="+url.QueryEscape("/etc"), ""); w.Code != http.StatusNotFound {
		t.Fatalf("open-folder /etc: %d", w.Code)
	}
	if w := do("/api/restore-to", `{"dir": ""}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"dir":"`+filepath.ToSlash(filepath.Join(out, "restored"))) {
		t.Fatalf("reset: %d %s", w.Code, w.Body)
	}
}
