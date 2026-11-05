package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/hub"
	"github.com/peterretief/yggstore/internal/peers"
)

func TestCleanDomain(t *testing.T) {
	for in, want := range map[string]string{"Acme.COM": "acme.com", "https://www.acme.co.za/": "acme.co.za", " acme.com ": "acme.com"} {
		if got, err := cleanDomain(in, "example.org"); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"acme", "a b.com", "shop.example.org", "example.org", ""} {
		if got, err := cleanDomain(bad, "example.org"); err == nil {
			t.Errorf("%q: took it as %q", bad, got)
		}
	}
}

func TestOwnDomainWaitsForTheAdmin(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "hub.json")
	ctx, admin, member, do, _, listOf := namePair(t, func(c *Config) { c.HubState, c.HubPeople = state, filepath.Join(dir, "users") })
	admin.cfg.Client = client.Client{HTTP: &http.Client{Timeout: time.Second}} // no nodes answer here
	member.cfg.Client = admin.cfg.Client
	if _, err := hub.Update(context.Background(), state, func(s *hub.HubState) error {
		_, err := s.AddPerson("acme", filepath.Join(dir, "users"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	cf := 0
	fail := errors.New("the API token needs Zone: Edit")
	var cfErr error = fail
	admin.addDomainFn = func(_ context.Context, domain string) ([]string, error) {
		cf++
		return []string{"ana.ns.cloudflare.com", "bob.ns.cloudflare.com"}, cfErr
	}
	if w := do(member, post("/api/box/people/domain", strings.NewReader(`{"Name":"acme","Label":"www.Acme.com"}`))); w.Code != 200 {
		t.Fatalf("ask: %d %s", w.Code, w.Body)
	}
	var req nameClaim
	for deadline := time.Now().Add(5 * time.Second); req.Name == ""; {
		admin.syncNames(ctx, listOf(admin))
		admin.approveNames(ctx, listOf(admin))
		admin.namesMu.Lock()
		for _, c := range admin.loadNames().Claims {
			req = c
		}
		admin.namesMu.Unlock()
		if req.Name == "" {
			if time.Now().After(deadline) {
				t.Fatal("the request never reached the admin")
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	admin.cfg.ApproveNames = true
	admin.approveNames(ctx, listOf(admin))
	if req.Name != "acme.com" || !req.Domain || req.Person != "acme" || cf != 0 {
		t.Fatalf("given without the admin, or a bad request: %+v, cloudflare called %d times", req, cf)
	}
	decide := `{"name":"acme.com","node":"200::2","approve":true}`
	// A failure at Cloudflare gives nothing, and can be tried again.
	if w := do(admin, post("/api/names/decide", strings.NewReader(decide))); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "Zone: Edit") {
		t.Fatalf("cloudflare failed: %d %s", w.Code, w.Body)
	}
	if _, ok := peers.NameOwner(listOf(admin), "acme.com"); ok {
		t.Fatal("given though Cloudflare failed")
	}
	cfErr = nil
	if w := do(admin, post("/api/names/decide", strings.NewReader(decide))); w.Code != http.StatusNoContent {
		t.Fatalf("approve: %d %s", w.Code, w.Body)
	}
	list := listOf(admin)
	p, pn, ok := peers.NameHolder(list, "acme.com")
	if !ok || p.Name != "pi" || !pn.Site || pn.Person != "acme" {
		t.Fatalf("not given as a site: %+v %+v", p, pn)
	}
	if keeper, given, under := peers.SiteKeeper(list, "www.acme.com"); !given || under || keeper.Name != "pi" {
		t.Fatalf("keeper %s given %v under %v", keeper.Name, given, under)
	}
	// The box hears the nameservers.
	var dom boxDomain
	for deadline := time.Now().Add(5 * time.Second); dom.Status != "approved"; {
		member.syncNames(ctx, listOf(member))
		r := httptest.NewRequest(http.MethodGet, "/api/box/people", nil)
		r.Host = "127.0.0.1:7480"
		var out struct{ People []boxPerson }
		json.Unmarshal(do(member, r).Body.Bytes(), &out)
		if len(out.People) == 1 && len(out.People[0].Domains) == 1 {
			dom = out.People[0].Domains[0]
		}
		if dom.Status != "approved" {
			if time.Now().After(deadline) {
				t.Fatalf("the box wasn't told: %+v", dom)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if dom.Name != "acme.com" || len(dom.NS) != 2 {
		t.Fatalf("the box sees %+v", dom)
	}
}

func TestSiteServiceHasADefault(t *testing.T) {
	d := New(Config{})
	if got := d.siteService(); !strings.HasPrefix(got, "http://") {
		t.Fatalf("the tunnel would be given %q", got)
	}
	d = New(Config{SiteService: "http://localhost:9000"})
	if got := d.siteService(); got != "http://localhost:9000" {
		t.Fatal(got)
	}
}
