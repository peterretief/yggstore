package site

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// Zone finds the Cloudflare zone host is in.
func (c *Cloudflare) Zone(ctx context.Context, host string) (id, name string, err error) {
	labels := strings.Split(host, ".")
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
