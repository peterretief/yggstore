package workhere

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/peterretief/yggstore/internal/recovery"
)

func TestLinkOpensThroughTheBox(t *testing.T) {
	var got map[string]any
	box := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		if got["device"] == "BUSY" && got["move"] != true {
			w.WriteHeader(http.StatusConflict)
			io.WriteString(w, `{"in_use":["anna-laptop"]}`)
			return
		}
		io.WriteString(w, `{"box_id":"BOXBOXB-1","folder":"yggstore (box)"}`)
	}))
	defer box.Close()
	s := &Store{Dir: t.TempDir(), BoxURL: func(string) string { return box.URL }}

	// The dashboard keeps its record on the web node.
	link, group := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	key, err := recovery.Key(link, group)
	if err != nil {
		t.Fatal(err)
	}
	sealed, _ := recovery.Seal(key, []byte(`{"stub":"the copy"}`))
	rec := Record{ID: "abcdefghijklmnop", GroupHalf: group, Sealed: sealed, Box: "[200::5]:7400", Name: "anna"}
	b, _ := json.Marshal(rec)
	w := httptest.NewRecorder()
	if !s.Serve(w, httptest.NewRequest(http.MethodPut, "/v1/work/abcdefghijklmnop", strings.NewReader(string(b))), "200::1") || w.Code != http.StatusNoContent {
		t.Fatalf("put: %d %s", w.Code, w.Body)
	}

	post := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, PagePath, strings.NewReader(body)))
		return w
	}
	if w := post(`{"id":"abcdefghijklmnop","link":"` + link + `","device":"DEV"}`); w.Code != 200 || !strings.Contains(w.Body.String(), "BOXBOXB-1") {
		t.Fatalf("open: %d %s", w.Code, w.Body)
	}
	if string(got["stub"].(map[string]any)["stub"].(string)) != "the copy" || got["name"] != "anna" {
		t.Fatalf("the box got %v", got)
	}
	if w := post(`{"id":"abcdefghijklmnop","link":"` + link + `","device":"BUSY"}`); w.Code != 409 || !strings.Contains(w.Body.String(), "anna-laptop") {
		t.Fatalf("in use: %d %s", w.Code, w.Body)
	}
	other := strings.Repeat("C", 43)
	if w := post(`{"id":"abcdefghijklmnop","link":"` + other + `","device":"DEV"}`); w.Code != http.StatusForbidden {
		t.Fatalf("wrong link: %d %s", w.Code, w.Body)
	}
	if w := post(`{"id":"unknownunknownxx","link":"` + link + `","device":"DEV"}`); w.Code != http.StatusNotFound {
		t.Fatalf("unknown: %d", w.Code)
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, PagePath, nil))
	if !strings.Contains(w.Body.String(), "<h1>Your files</h1>") {
		t.Fatal("no page")
	}
}
