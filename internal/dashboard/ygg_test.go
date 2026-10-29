package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/site"
)

func TestYggButtonPage(t *testing.T) {
	_, admin, member, do, _, _ := namePair(t, nil)
	list := []peers.Peer{
		{Name: "desktop", Addr: "[200::1]:7400", Admin: true, Domain: "example.org", Names: []string{"peter.example.org"}, Code: "ys1peter"},
		{Name: "pi", Addr: "[200::2]:7400", Names: []string{"pi.example.org"}, Code: "ys1pi",
			PeopleNames: []peers.PersonName{{Name: "anna.example.org", Person: "anna", Code: "ys1anna"}}},
	}
	for _, d := range []*Dashboard{admin, member} {
		if err := peers.Write(d.cfg.PeersPath, list); err != nil {
			t.Fatal(err)
		}
	}
	to := func(d *Dashboard, site string) yggToOut {
		r := httptest.NewRequest(http.MethodGet, "/api/ygg?to="+site, nil)
		r.Host = "127.0.0.1:7480"
		var out yggToOut
		json.Unmarshal(do(d, r).Body.Bytes(), &out)
		return out
	}

	if out := to(member, "Peter.Example.org"); out.Error != "" || out.To != "desktop" || out.Account != "pi" || out.Site != "peter.example.org" {
		t.Fatalf("a member's site: %+v", out)
	}
	for site, want := range map[string]string{
		"anna.example.org":   "someone on a box",
		"pi.example.org":     "your own website",
		"nobody.example.org": "doesn't say whose",
		"":                   "no website given",
	} {
		if out := to(member, site); !strings.Contains(out.Error, want) {
			t.Errorf("%q: %+v, want an error with %q", site, out, want)
		}
	}

	// The asking page is never shown inside another page.
	r := httptest.NewRequest(http.MethodGet, "/ygg?to=peter.example.org", nil)
	r.Host = "127.0.0.1:7480"
	if w := do(member, r); w.Code != 200 || w.Header().Get("X-Frame-Options") != "DENY" ||
		!strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatalf("/ygg: %d %v", w.Code, w.Header())
	}
	// Another page can open it, but can't give: the payment needs the header.
	r = httptest.NewRequest(http.MethodPost, "/api/wallet/pay", strings.NewReader(`{"to":"desktop","amount":1}`))
	r.Host = "127.0.0.1:7480"
	if w := do(member, r); w.Code != http.StatusForbidden {
		t.Fatalf("a payment without the header: %d", w.Code)
	}
}

func TestKeepYggButton(t *testing.T) {
	d := New(Config{SitesDir: t.TempDir()})
	dir := t.TempDir()
	page := "<html><body>Menu</body></html>"
	os.WriteFile(filepath.Join(dir, "index.html"), []byte(page), 0o644)
	read := func() string { b, _ := os.ReadFile(filepath.Join(dir, "index.html")); return string(b) }

	if err := d.keepYggButton(dir, "peter.example.org"); err != nil || read() != page {
		t.Fatalf("button added while off: %v %s", err, read())
	}
	os.MkdirAll(filepath.Dir(d.yggFlag("peter.example.org")), 0o700)
	os.WriteFile(d.yggFlag("peter.example.org"), nil, 0o600)
	if !d.yggButtonOn("peter.example.org") {
		t.Fatal("flag not seen")
	}
	if err := d.keepYggButton(dir, "peter.example.org"); err != nil || read() != site.WithYggButton(page, "peter.example.org") {
		t.Fatalf("button not kept on a new version: %v %s", err, read())
	}
}
