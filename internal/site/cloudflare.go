package site

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Cloudflare adds a site to the web nodes' tunnel: a route in the tunnel's
// settings and a DNS record pointing the name at the tunnel. It needs an
// API token allowed to edit the account's tunnels and the zones' DNS.
type Cloudflare struct {
	APIToken string
	Account  string
	Tunnel   string
	Base     string // the API; empty for Cloudflare's
	HTTP     *http.Client
}

// TunnelFromToken reads the account and tunnel a tunnel token belongs to.
func TunnelFromToken(token string) (account, tunnel string, err error) {
	token = strings.TrimSpace(token)
	b, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		b, err = base64.RawStdEncoding.DecodeString(token)
	}
	var t struct{ A, T string }
	if err != nil || json.Unmarshal(b, &t) != nil || t.A == "" || t.T == "" {
		return "", "", errors.New("not a tunnel token")
	}
	return t.A, t.T, nil
}

// Answers reports whether https://domain/ answers 200 now.
func Answers(ctx context.Context, domain string) bool {
	c := http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+domain+"/", nil)
	if err != nil {
		return false
	}
	resp, err := c.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// AddRoutes makes a site reach the web nodes through their tunnel, with
// the API token and the tunnel's token: its route and DNS record, and
// those of www. too for a domain that is a whole zone. A www. that can't be
// added is reported but doesn't fail it. It reports what it added.
func AddRoutes(ctx context.Context, apiToken, tunnelToken, domain, service string) (added []string, wwwErr, err error) {
	account, tunnel, err := TunnelFromToken(tunnelToken)
	if err != nil {
		return nil, nil, err
	}
	cf := &Cloudflare{APIToken: strings.TrimSpace(apiToken), Account: account, Tunnel: tunnel}
	added, err = cf.Route(ctx, domain, service)
	if err != nil {
		return added, nil, err
	}
	if _, zone, zerr := cf.Zone(ctx, domain); zerr == nil && zone == domain {
		more, werr := cf.Route(ctx, "www."+domain, service)
		added = append(added, more...)
		wwwErr = werr
	}
	return added, wwwErr, nil
}

// AddDomain puts a domain on the group's Cloudflare account, if it isn't
// there, and routes it and its www. to the web nodes. It gives the two
// nameservers the domain's registrar must point at. It changes nothing but
// the new zone's website records: mail records are for the admin to bring.
func AddDomain(ctx context.Context, apiToken, tunnelToken, domain, service string) ([]string, error) {
	if service == "" {
		return nil, errors.New("no address for the web nodes to route the domain to")
	}
	account, tunnel, err := TunnelFromToken(tunnelToken)
	if err != nil {
		return nil, err
	}
	cf := &Cloudflare{APIToken: strings.TrimSpace(apiToken), Account: account, Tunnel: tunnel}
	ns, err := cf.AddZone(ctx, domain)
	if err != nil {
		return nil, err
	}
	if _, err := cf.Route(ctx, domain, service); err != nil {
		return ns, err
	}
	if _, err := cf.Route(ctx, "www."+domain, service); err != nil {
		return ns, err
	}
	return ns, nil
}

// AddZone adds domain to the account, or finds it there, and gives its
// nameservers. The API token needs Zone: Edit.
func (c *Cloudflare) AddZone(ctx context.Context, domain string) ([]string, error) {
	type zone struct {
		Name        string   `json:"name"`
		NameServers []string `json:"name_servers"`
	}
	var zones []zone
	if err := c.call(ctx, "GET", "/zones?name="+url.QueryEscape(domain), nil, &zones); err != nil {
		return nil, err
	}
	if len(zones) > 0 {
		return zones[0].NameServers, nil
	}
	var made zone
	body := map[string]any{"name": domain, "type": "full", "account": map[string]string{"id": c.Account}}
	if err := c.call(ctx, "POST", "/zones", body, &made); err != nil {
		return nil, fmt.Errorf("adding %s to Cloudflare (the API token needs Zone: Edit): %w", domain, err)
	}
	return made.NameServers, nil
}

// TakeMail sends every address at domain to the group's mail Worker: it
// turns Email Routing on and sets the catch-all. It changes nothing if the
// domain's mail already goes somewhere else, so a domain in use is never
// taken over by accident.
func TakeMail(ctx context.Context, apiToken, tunnelToken, domain, worker string) error {
	account, tunnel, err := TunnelFromToken(tunnelToken)
	if err != nil {
		return err
	}
	return (&Cloudflare{APIToken: strings.TrimSpace(apiToken), Account: account, Tunnel: tunnel}).TakeMail(ctx, domain, worker)
}

// TakeMail is the package's TakeMail, on c.
func (c *Cloudflare) TakeMail(ctx context.Context, domain, worker string) error {
	zone, name, err := c.Zone(ctx, domain)
	if err != nil {
		return err
	}
	if name != domain {
		return fmt.Errorf("%s is part of %s, not a domain of its own", domain, name)
	}
	type action struct {
		Type  string   `json:"type"`
		Value []string `json:"value,omitempty"`
	}
	type rule struct {
		Enabled  bool     `json:"enabled"`
		Actions  []action `json:"actions"`
		Matchers []struct {
			Type string `json:"type"`
		} `json:"matchers"`
	}
	ours := func(r rule) bool {
		for _, a := range r.Actions {
			if a.Type != "worker" || len(a.Value) != 1 || a.Value[0] != worker {
				return false
			}
		}
		return len(r.Actions) > 0
	}
	var rules []rule
	if err := c.call(ctx, "GET", "/zones/"+zone+"/email/routing/rules", nil, &rules); err != nil {
		return err
	}
	for _, r := range rules {
		if r.Enabled && !ours(r) {
			return fmt.Errorf("%s's mail already goes somewhere else: remove its Email Routing rules in Cloudflare first, if it should move", domain)
		}
	}
	var settings struct {
		Enabled bool `json:"enabled"`
	}
	if err := c.call(ctx, "GET", "/zones/"+zone+"/email/routing", nil, &settings); err != nil {
		return err
	}
	if !settings.Enabled {
		if err := c.call(ctx, "POST", "/zones/"+zone+"/email/routing/enable", map[string]any{}, nil); err != nil {
			return fmt.Errorf("turning on Email Routing for %s: %w", domain, err)
		}
	}
	body := map[string]any{"enabled": true, "name": "yggstore", "matchers": []map[string]string{{"type": "all"}},
		"actions": []map[string]any{{"type": "worker", "value": []string{worker}}}}
	return c.call(ctx, "PUT", "/zones/"+zone+"/email/routing/rules/catch_all", body, nil)
}

// EnableSending sets domain up for Cloudflare Email Sending, if it isn't:
// Cloudflare adds its bounce, SPF, DKIM and DMARC records. apiToken finds
// the zone; sendToken needs Email Sending: Edit.
func EnableSending(ctx context.Context, apiToken, sendToken, domain string) error {
	return enableSending(ctx, &Cloudflare{APIToken: strings.TrimSpace(apiToken)}, &Cloudflare{APIToken: strings.TrimSpace(sendToken)}, domain)
}

func enableSending(ctx context.Context, finder, sender *Cloudflare, domain string) error {
	zone, _, err := finder.Zone(ctx, domain)
	if err != nil {
		return err
	}
	var subs []struct {
		Name string `json:"name"`
	}
	if err := sender.call(ctx, "GET", "/zones/"+zone+"/email/sending/subdomains", nil, &subs); err != nil {
		return err
	}
	for _, s := range subs {
		if s.Name == domain {
			return nil
		}
	}
	return sender.call(ctx, "POST", "/zones/"+zone+"/email/sending/subdomains", map[string]string{"name": domain}, nil)
}

// DMARCReports sends domain's DMARC reports to addr, at the group's domain:
// it adds rua to the domain's DMARC record, and the record in the group's
// zone that lets another domain's reports come there.
func DMARCReports(ctx context.Context, apiToken, domain, group, addr string) error {
	return (&Cloudflare{APIToken: strings.TrimSpace(apiToken)}).DMARCReports(ctx, domain, group, addr)
}

// DMARCReports is the package's DMARCReports, on c.
func (c *Cloudflare) DMARCReports(ctx context.Context, domain, group, addr string) error {
	type record struct {
		ID      string `json:"id"`
		Content string `json:"content"`
	}
	find := func(zone, name string) ([]record, error) {
		var out []record
		err := c.call(ctx, "GET", "/zones/"+zone+"/dns_records?type=TXT&name="+url.QueryEscape(name), nil, &out)
		return out, err
	}
	groupZone, _, err := c.Zone(ctx, group)
	if err != nil {
		return err
	}
	allow := domain + "._report._dmarc." + group
	have, err := find(groupZone, allow)
	if err != nil {
		return err
	}
	if len(have) == 0 {
		body := map[string]any{"type": "TXT", "name": allow, "content": `"v=DMARC1"`, "ttl": 1}
		if err := c.call(ctx, "POST", "/zones/"+groupZone+"/dns_records", body, nil); err != nil {
			return err
		}
	}
	zone, _, err := c.Zone(ctx, domain)
	if err != nil {
		return err
	}
	have, err = find(zone, "_dmarc."+domain)
	if err != nil {
		return err
	}
	if len(have) == 0 {
		body := map[string]any{"type": "TXT", "name": "_dmarc." + domain, "content": `"v=DMARC1; p=reject; rua=mailto:` + addr + `"`, "ttl": 1}
		return c.call(ctx, "POST", "/zones/"+zone+"/dns_records", body, nil)
	}
	text := strings.TrimRight(strings.Trim(have[0].Content, `"`), "; ")
	if strings.Contains(text, "rua=") {
		return nil // reports go somewhere already
	}
	return c.call(ctx, "PATCH", "/zones/"+zone+"/dns_records/"+have[0].ID, map[string]string{"content": `"` + text + "; rua=mailto:" + addr + `"`}, nil)
}

// Route makes host reach the web nodes through the tunnel. It changes
// nothing if host already has a DNS record that points elsewhere, so a
// site still served from somewhere else isn't taken over by accident. It
// reports what it added.
func (c *Cloudflare) Route(ctx context.Context, host, service string) ([]string, error) {
	zone, _, err := c.Zone(ctx, host)
	if err != nil {
		return nil, err
	}
	target := c.Tunnel + ".cfargotunnel.com"
	var records []struct{ Type, Content string }
	if err := c.call(ctx, "GET", "/zones/"+zone+"/dns_records?name="+url.QueryEscape(host), nil, &records); err != nil {
		return nil, err
	}
	needDNS := true
	for _, r := range records {
		if r.Type == "CNAME" && r.Content == target {
			needDNS = false
			continue
		}
		if r.Type != "A" && r.Type != "AAAA" && r.Type != "CNAME" {
			continue // a domain's mail (MX, TXT) doesn't decide where its website is
		}
		return nil, fmt.Errorf("%s already has a DNS record (%s %s) pointing somewhere else; if it should move to the group, delete that record (or the route in the other tunnel) in the Cloudflare dashboard, then publish again", host, r.Type, r.Content)
	}

	var added []string
	path := "/accounts/" + c.Account + "/cfd_tunnel/" + c.Tunnel + "/configurations"
	var got struct {
		Config map[string]any `json:"config"`
	}
	if err := c.call(ctx, "GET", path, nil, &got); err != nil {
		return nil, err
	}
	conf := got.Config
	if conf == nil {
		conf = map[string]any{}
	}
	ingress, _ := conf["ingress"].([]any)
	found := false
	for _, r := range ingress {
		if m, _ := r.(map[string]any); m != nil && m["hostname"] == host {
			found = true
		}
	}
	if !found {
		rule := map[string]any{"hostname": host, "service": service, "originRequest": map[string]any{}}
		// before the catch-all rule, which has no hostname and must be last
		if n := len(ingress); n > 0 {
			if m, _ := ingress[n-1].(map[string]any); m != nil && m["hostname"] == nil {
				ingress = append(ingress[:n-1:n-1], rule, ingress[n-1])
			} else {
				ingress = append(ingress, rule, map[string]any{"service": "http_status:404"})
			}
		} else {
			ingress = []any{rule, map[string]any{"service": "http_status:404"}}
		}
		conf["ingress"] = ingress
		if err := c.call(ctx, "PUT", path, map[string]any{"config": conf}, nil); err != nil {
			return nil, err
		}
		added = append(added, "tunnel route "+host+" → "+service)
	}

	if needDNS {
		rec := map[string]any{"type": "CNAME", "name": host, "content": target, "proxied": true, "comment": "yggstore site"}
		if err := c.call(ctx, "POST", "/zones/"+zone+"/dns_records", rec, nil); err != nil {
			return added, err
		}
		added = append(added, "DNS record "+host)
	}
	return added, nil
}

// AAAA gives host an AAAA record for ip, DNS only (a Yggdrasil address
// can't go through Cloudflare's proxy), unless it has that one already. It
// leaves alone, and reports, a record of host that points elsewhere. It
// reports whether it added the record.
func (c *Cloudflare) AAAA(ctx context.Context, host, ip string) (bool, error) {
	zone, _, err := c.Zone(ctx, host)
	if err != nil {
		return false, err
	}
	var records []struct{ Type, Content string }
	if err := c.call(ctx, "GET", "/zones/"+zone+"/dns_records?name="+url.QueryEscape(host), nil, &records); err != nil {
		return false, err
	}
	for _, r := range records {
		if r.Type == "AAAA" && net.ParseIP(r.Content).Equal(net.ParseIP(ip)) {
			return false, nil
		}
		return false, fmt.Errorf("%s already has a DNS record (%s %s); delete it in the Cloudflare dashboard if %s should point at %s", host, r.Type, r.Content, host, ip)
	}
	rec := map[string]any{"type": "AAAA", "name": host, "content": ip, "proxied": false, "ttl": 1, "comment": "yggstore machine's Yggdrasil address"}
	if err := c.call(ctx, "POST", "/zones/"+zone+"/dns_records", rec, nil); err != nil {
		return false, err
	}
	return true, nil
}

// Zone finds the Cloudflare zone host is in.
func (c *Cloudflare) Zone(ctx context.Context, host string) (id, name string, err error) {
	labels := strings.Split(strings.TrimPrefix(host, "*."), ".")
	for i := 0; i+2 <= len(labels); i++ {
		try := strings.Join(labels[i:], ".")
		var zones []struct{ ID, Name string }
		if err := c.call(ctx, "GET", "/zones?name="+url.QueryEscape(try), nil, &zones); err != nil {
			return "", "", err
		}
		if len(zones) > 0 {
			return zones[0].ID, zones[0].Name, nil
		}
	}
	return "", "", fmt.Errorf("%s is not in any zone this Cloudflare token can see; add the domain to Cloudflare first", host)
}

func (c *Cloudflare) call(ctx context.Context, method, path string, body, result any) error {
	base := c.Base
	if base == "" {
		base = "https://api.cloudflare.com/client/v4"
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIToken)
	req.Header.Set("Content-Type", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var out struct {
		Success bool            `json:"success"`
		Result  json.RawMessage `json:"result"`
		Errors  []struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&out); err != nil {
		return fmt.Errorf("cloudflare: %s %s: %s", method, path, resp.Status)
	}
	if !out.Success {
		var msgs []string
		for _, e := range out.Errors {
			msgs = append(msgs, e.Message)
		}
		hint := ""
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
			hint = " (the API token needs Account › Cloudflare Tunnel › Edit, Zone › Zone › Read and Zone › DNS › Edit)"
		}
		return fmt.Errorf("cloudflare: %s%s", strings.Join(msgs, "; "), hint)
	}
	if result != nil && len(out.Result) > 0 {
		return json.Unmarshal(out.Result, result)
	}
	return nil
}
