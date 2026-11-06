package site

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeCloudflare keeps one zone's DNS records and one tunnel's settings.
type fakeCloudflare struct {
	mu      sync.Mutex
	records []map[string]any
	config  map[string]any
}

func (f *fakeCloudflare) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	reply := func(result any) {
		json.NewEncoder(w).Encode(map[string]any{"success": true, "result": result})
	}
	if r.Header.Get("Authorization") != "Bearer api-token" {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []any{map[string]any{"message": "Authentication error"}}})
		return
	}
	switch {
	case r.URL.Path == "/zones":
		if r.URL.Query().Get("name") == "example.org" {
			reply([]any{map[string]any{"id": "z1", "name": "example.org"}})
		} else {
			reply([]any{})
		}
	case r.URL.Path == "/zones/z1/dns_records" && r.Method == "GET":
		var out []any
		for _, rec := range f.records {
			if rec["name"] == r.URL.Query().Get("name") {
				out = append(out, rec)
			}
		}
		reply(out)
	case r.URL.Path == "/zones/z1/dns_records" && r.Method == "POST":
		var rec map[string]any
		json.NewDecoder(r.Body).Decode(&rec)
		f.records = append(f.records, rec)
		reply(rec)
	case r.URL.Path == "/accounts/acc/cfd_tunnel/tun/configurations" && r.Method == "GET":
		reply(map[string]any{"config": f.config})
	case r.URL.Path == "/accounts/acc/cfd_tunnel/tun/configurations" && r.Method == "PUT":
		var body struct{ Config map[string]any }
		json.NewDecoder(r.Body).Decode(&body)
		f.config = body.Config
		reply(nil)
	default:
		http.NotFound(w, r)
	}
}

func TestCloudflareRoute(t *testing.T) {
	fake := &fakeCloudflare{config: map[string]any{
		"warp-routing": map[string]any{"enabled": false},
		"ingress": []any{
			map[string]any{"hostname": "old.example.org", "service": "http://localhost:8480"},
			map[string]any{"service": "http_status:404"},
		},
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	cf := &Cloudflare{APIToken: "api-token", Account: "acc", Tunnel: "tun", Base: srv.URL}
	ctx := context.Background()

	added, err := cf.Route(ctx, "www.example.org", "http://localhost:8480")
	if err != nil || len(added) != 2 {
		t.Fatalf("first route: %v %v", added, err)
	}
	ingress := fake.config["ingress"].([]any)
	if len(ingress) != 3 || ingress[1].(map[string]any)["hostname"] != "www.example.org" || ingress[2].(map[string]any)["hostname"] != nil {
		t.Fatalf("ingress: %v (want the new rule before the catch-all)", ingress)
	}
	if fake.config["warp-routing"] == nil {
		t.Fatal("other tunnel settings were lost")
	}
	if r := fake.records[0]; r["content"] != "tun.cfargotunnel.com" || r["proxied"] != true {
		t.Fatalf("DNS record: %v", r)
	}

	// Again: nothing to add.
	if added, err := cf.Route(ctx, "www.example.org", "http://localhost:8480"); err != nil || len(added) != 0 {
		t.Fatalf("second route: %v %v", added, err)
	}

	// A domain's mail records don't stand in the way of its website.
	fake.records = append(fake.records,
		map[string]any{"type": "MX", "name": "mail.example.org", "content": "route1.mx.cloudflare.net"},
		map[string]any{"type": "TXT", "name": "mail.example.org", "content": "v=spf1 ~all"})
	if added, err := cf.Route(ctx, "mail.example.org", "http://localhost:8480"); err != nil || len(added) != 2 {
		t.Fatalf("route beside mail records: %v %v", added, err)
	}

	// A name that points elsewhere is left alone.
	fake.records = append(fake.records, map[string]any{"type": "A", "name": "example.org", "content": "192.0.2.1"})
	if _, err := cf.Route(ctx, "example.org", "http://localhost:8480"); err == nil || !strings.Contains(err.Error(), "somewhere else") {
		t.Fatalf("route over another record: %v", err)
	}
	if len(fake.config["ingress"].([]any)) != 4 {
		t.Fatal("tunnel changed although the DNS record points elsewhere")
	}

	if _, err := cf.Route(ctx, "example.net", "http://localhost:8480"); err == nil {
		t.Fatal("route in an unknown zone worked")
	}
	cf.APIToken = "wrong"
	if _, err := cf.Route(ctx, "www.example.org", "http://localhost:8480"); err == nil || !strings.Contains(err.Error(), "DNS › Edit") {
		t.Fatalf("bad token: %v", err)
	}
}

func TestTunnelFromToken(t *testing.T) {
	tok := base64.StdEncoding.EncodeToString([]byte(`{"a":"acc","t":"tun","s":"secret"}`))
	if a, tn, err := TunnelFromToken(tok + "\n"); err != nil || a != "acc" || tn != "tun" {
		t.Fatalf("%s %s %v", a, tn, err)
	}
	if _, _, err := TunnelFromToken("00000000-0000-0000-0000-000000000000"); err == nil {
		t.Fatal("tunnel ID taken for a token")
	}
}

func TestCloudflareAAAA(t *testing.T) {
	fake := &fakeCloudflare{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	cf := &Cloudflare{APIToken: "api-token", Base: srv.URL}
	ctx := context.Background()
	if added, err := cf.AAAA(ctx, "box.ygg.example.org", "201:1193::a74"); err != nil || !added {
		t.Fatalf("add: %v %v", added, err)
	}
	if r := fake.records[0]; r["type"] != "AAAA" || r["content"] != "201:1193::a74" || r["proxied"] != false {
		t.Fatalf("record %v", r)
	}
	if added, err := cf.AAAA(ctx, "box.ygg.example.org", "201:1193:0::a74"); err != nil || added {
		t.Fatalf("again: %v %v", added, err)
	}
	if _, err := cf.AAAA(ctx, "box.ygg.example.org", "201::5"); err == nil || !strings.Contains(err.Error(), "already has") {
		t.Fatalf("over another address: %v", err)
	}
	if len(fake.records) != 1 {
		t.Fatalf("records %v", fake.records)
	}
}

func TestAddZone(t *testing.T) {
	var posted map[string]any
	zones := []map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			json.NewDecoder(r.Body).Decode(&posted)
			z := map[string]any{"id": "z1", "name": posted["name"], "name_servers": []string{"ana.ns.example", "bob.ns.example"}}
			zones = append(zones, z)
			json.NewEncoder(w).Encode(map[string]any{"success": true, "result": z})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"success": true, "result": zones})
	}))
	t.Cleanup(srv.Close)
	cf := &Cloudflare{APIToken: "t", Account: "acct", Base: srv.URL}
	ns, err := cf.AddZone(context.Background(), "acme.com")
	if err != nil || len(ns) != 2 || posted["name"] != "acme.com" || posted["account"].(map[string]any)["id"] != "acct" {
		t.Fatalf("%v %v %v", ns, err, posted)
	}
	// Asked again, it's found, not added twice.
	posted = nil
	if ns, err := cf.AddZone(context.Background(), "acme.com"); err != nil || len(ns) != 2 || posted != nil {
		t.Fatalf("again: %v %v %v", ns, err, posted)
	}
}

func TestTakeMail(t *testing.T) {
	var rules []map[string]any
	enabled, caught := false, ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		ok := func(v any) { json.NewEncoder(w).Encode(map[string]any{"success": true, "result": v}) }
		switch {
		case strings.HasSuffix(r.URL.Path, "/zones"):
			ok([]map[string]string{{"id": "z1", "name": "acme.com"}})
		case strings.HasSuffix(r.URL.Path, "/email/routing/rules/catch_all"):
			var b struct {
				Actions []struct {
					Value []string `json:"value"`
				} `json:"actions"`
			}
			json.NewDecoder(r.Body).Decode(&b)
			caught = b.Actions[0].Value[0]
			ok(map[string]any{})
		case strings.HasSuffix(r.URL.Path, "/email/routing/rules"):
			ok(rules)
		case strings.HasSuffix(r.URL.Path, "/email/routing/enable"):
			enabled = true
			ok(map[string]any{})
		case strings.HasSuffix(r.URL.Path, "/email/routing"):
			ok(map[string]bool{"enabled": enabled})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	cf := &Cloudflare{APIToken: "t", Account: "a", Base: srv.URL}
	ctx := context.Background()
	// Mail already going elsewhere is left alone.
	rules = []map[string]any{{"enabled": true, "actions": []map[string]any{{"type": "worker", "value": []string{"mail-bridge-test"}}}}}
	if err := cf.TakeMail(ctx, "acme.com", "yggstore-mail"); err == nil || !strings.Contains(err.Error(), "somewhere else") || caught != "" || enabled {
		t.Fatalf("took mail in use: %v, caught %q", err, caught)
	}
	rules = []map[string]any{{"enabled": true, "actions": []map[string]any{{"type": "forward", "value": []string{"x@example.net"}}}}}
	if err := cf.TakeMail(ctx, "acme.com", "yggstore-mail"); err == nil || caught != "" {
		t.Fatalf("took forwarded mail: %v", err)
	}
	// A disabled rule, such as the default catch-all, isn't in use.
	rules = []map[string]any{{"enabled": false, "actions": []map[string]any{{"type": "drop"}}}}
	if err := cf.TakeMail(ctx, "acme.com", "yggstore-mail"); err != nil || caught != "yggstore-mail" || !enabled {
		t.Fatalf("%v caught %q enabled %v", err, caught, enabled)
	}
	if err := cf.TakeMail(ctx, "shop.acme.com", "yggstore-mail"); err == nil {
		t.Fatal("took mail for part of a zone")
	}
}

func TestEnableSending(t *testing.T) {
	var posted []string
	subs := []map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		ok := func(v any) { json.NewEncoder(w).Encode(map[string]any{"success": true, "result": v}) }
		switch {
		case strings.HasSuffix(r.URL.Path, "/zones"):
			ok([]map[string]string{{"id": "z1", "name": "acme.com"}})
		case r.Method == http.MethodPost:
			if r.Header.Get("Authorization") != "Bearer send" {
				t.Errorf("made with %q", r.Header.Get("Authorization"))
			}
			posted = append(posted, r.URL.Path)
			subs = append(subs, map[string]string{"name": "acme.com"})
			ok(map[string]any{})
		default:
			ok(subs)
		}
	}))
	t.Cleanup(srv.Close)
	api, send := &Cloudflare{APIToken: "api", Base: srv.URL}, &Cloudflare{APIToken: "send", Base: srv.URL}
	for range 2 { // the second time it's there already
		if err := enableSending(context.Background(), api, send, "acme.com"); err != nil {
			t.Fatal(err)
		}
	}
	if len(posted) != 1 || posted[0] != "/zones/z1/email/sending/subdomains" {
		t.Fatalf("posted %v", posted)
	}
}
