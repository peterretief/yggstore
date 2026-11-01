package light

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterretief/yggstore/internal/peers"
)

func TestSignupsPerAddress(t *testing.T) {
	list := []peers.Peer{{Name: "desktop", Addr: "[201::1]:7400", Admin: true, Domain: "example.org"}, {Name: "box", Addr: "[200::2]:7400", Trial: true}}
	ok := true
	box := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ok {
			http.Error(w, "anna.example.org is taken", http.StatusConflict)
			return
		}
		jsonOK(w, map[string]string{"label": "anna", "email": "anna@example.net", "token": "abc"})
	}))
	t.Cleanup(box.Close)
	sent := 0
	rl := &Relay{Peers: func() []peers.Peer { return list }, BoxURL: func(string) string { return box.URL }, Log: t.Logf,
		Email: func(context.Context, string, string, string) error { sent++; return nil }}
	post := func() int {
		r := httptest.NewRequest(http.MethodPost, Base+"/signup", strings.NewReader(`{"label":"anna","email":"anna@example.net","password":"long enough pw"}`))
		r.Host = "my.example.org"
		r.Header.Set("X-Yggstore", "1")
		r.Header.Set("CF-Connecting-IP", "192.0.2.7")
		w := httptest.NewRecorder()
		rl.ServeHTTP(w, r)
		return w.Code
	}
	// Refused sign-ups don't count.
	ok = false
	for range 5 {
		if c := post(); c != http.StatusConflict {
			t.Fatalf("refused: %d", c)
		}
	}
	ok = true
	for i := range SignupsPerIP {
		if c := post(); c != http.StatusOK {
			t.Fatalf("sign-up %d: %d", i+1, c)
		}
	}
	if c := post(); c != http.StatusTooManyRequests || sent != SignupsPerIP {
		t.Fatalf("one too many: %d, %d sent", c, sent)
	}
}

func TestTrialEmailOnlyFromTheTrialBox(t *testing.T) {
	list := []peers.Peer{{Name: "desktop", Addr: "[201::1]:7400", Admin: true, Domain: "example.org"}, {Name: "box", Addr: "[200::2]:7400", Trial: true},
		{Name: "other", Addr: "[200::3]:7400"}}
	var to, text string
	s := &Store{Dir: filepath.Join(t.TempDir(), "light"), Peers: func() []peers.Peer { return list }, Log: t.Logf,
		Email: func(_ context.Context, a, _, b string) error { to, text = a, b; return nil }}
	ask := func(caller string) int {
		r := httptest.NewRequest(http.MethodPost, "/v1/light/trial-email", strings.NewReader(`{"to":"cara@example.net","kind":"warn","login":"cara@box","until":1900000000}`))
		w := httptest.NewRecorder()
		s.Serve(w, r, caller)
		return w.Code
	}
	if c := ask("200::3"); c != http.StatusForbidden || to != "" {
		t.Fatalf("another member asked: %d", c)
	}
	if c := ask("200::2"); c != http.StatusNoContent || to != "cara@example.net" || !strings.Contains(text, "https://my.example.org") {
		t.Fatalf("the trial box asked: %d %q %q", c, to, text)
	}
}
