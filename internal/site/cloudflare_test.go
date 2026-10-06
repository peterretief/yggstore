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

	// A name that points elsewhere is left alone.
	fake.records = append(fake.records, map[string]any{"type": "A", "name": "example.org", "content": "192.0.2.1"})
	if _, err := cf.Route(ctx, "example.org", "http://localhost:8480"); err == nil || !strings.Contains(err.Error(), "somewhere else") {
		t.Fatalf("route over another record: %v", err)
	}
	if len(fake.config["ingress"].([]any)) != 3 {
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
	if _, _, err := TunnelFromToken("6f1d2c3b-0a4e-4b5f-9c7d-8e9f0a1b2c3d"); err == nil {
		t.Fatal("tunnel ID taken for a token")
	}
}
