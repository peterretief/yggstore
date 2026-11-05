package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/hub"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/site"
)

// A client's own domain (docs/developer-areas.md): a box's owner asks for
// acme.com for someone on the box, as for a name. Unlike a name, it always
// waits for the group's admin: approving it adds the domain to the group's
// Cloudflare account, routes it to the web nodes, and gives it to the
// person as a site. The box then shows the two nameservers to set at the
// registrar. Its mail is set up apart, by hand.
const (
	typeDomainClaim = "domain-claim" // box → admins: {"name", "code", "person"}
	maxOwnDomains   = 5              // per person
)

type boxDomain struct {
	Name   string   `json:"name"`
	Status string   `json:"status"` // asked, approved, declined
	NS     []string `json:"ns,omitempty"`
}

// cleanDomain is the domain someone typed, or why it can't be one.
func cleanDomain(s, groupDomain string) (string, error) {
	d := strings.ToLower(strings.TrimSpace(s))
	d = strings.TrimPrefix(strings.TrimPrefix(d, "https://"), "http://")
	d = strings.TrimPrefix(strings.TrimSuffix(d, "/"), "www.")
	if err := peers.ValidDomain(d); err != nil {
		return "", err
	}
	if groupDomain != "" && (d == groupDomain || strings.HasSuffix(d, "."+groupDomain)) {
		return "", fmt.Errorf("%s is under the group's own domain: give it as a name instead", d)
	}
	return d, nil
}

// claimDomain asks the group's admin for domain, for person on this box.
func (d *Dashboard) claimDomain(ctx context.Context, person, domain string) (string, error) {
	st, err := hub.Load(d.cfg.HubState)
	if err != nil {
		return "", err
	}
	p, ok := st.PersonNamed(person)
	if !ok || !p.Active() {
		return "", fmt.Errorf("nobody called %q uses this box now", person)
	}
	list, err := peers.Load(d.cfg.PeersPath)
	if err != nil {
		return "", err
	}
	name, err := cleanDomain(domain, peers.GroupDomain(list))
	if err != nil {
		return "", err
	}
	if _, ok := peers.NameOwner(list, name); ok {
		return "", fmt.Errorf("%s is in the group already", name)
	}
	if len(peers.PersonNamesOf(list, d.cfg.SelfID, p.Name)) > maxOwnDomains {
		return "", fmt.Errorf("%s has %d sites, the most for now", p.Name, maxOwnDomains)
	}
	id, err := hub.PersonKey(d.cfg.HubState, p)
	if err != nil {
		return "", fmt.Errorf("%s's sharing key: %w", p.Name, err)
	}
	mine := nameClaim{Name: name, Node: d.cfg.SelfID, Code: id.Code(), Person: p.Name, Time: time.Now().UnixMilli(), Status: "asked", Mine: true, Domain: true}
	admin := isAdmin(list, d.cfg.SelfID)
	if !admin {
		lc, err := d.msgClient()
		if err != nil {
			return "", err
		}
		body, _ := json.Marshal(map[string]string{"name": name, "code": id.Code(), "person": p.Name})
		sent := 0
		for _, a := range list {
			if a.Admin {
				if _, err = lc.Send(ctx, a.IP(), typeDomainClaim, string(body)); err == nil {
					sent++
				}
			}
		}
		if sent == 0 {
			return "", errors.New("could not send the request to the group's admin: " + shortErr(err))
		}
	}
	d.namesMu.Lock()
	defer d.namesMu.Unlock()
	f := d.loadNames()
	keep := f.Claims[:0]
	for _, c := range f.Claims {
		if !(c.Mine && c.Domain && c.Name == name) {
			keep = append(keep, c)
		}
	}
	f.Claims = append(keep, mine)
	if admin { // it waits on this dashboard's own Names
		other := mine
		other.Mine, other.NodeName = false, d.me()
		f.Claims = append(f.Claims, other)
	}
	if err := d.saveNames(f); err != nil {
		return "", err
	}
	return fmt.Sprintf("asked the group's admin for %s, for %s", name, p.Name), nil
}

// takeDomainClaim keeps a box's request (admin dashboards). It waits under
// Names for the admin: a domain is never given automatically. Called with
// namesMu held.
func (d *Dashboard) takeDomainClaim(ctx context.Context, f *namesFile, list []peers.Peer, from, fromName, body string, at int64) {
	if !isAdmin(list, d.cfg.SelfID) {
		return
	}
	var req struct{ Name, Code, Person string }
	if json.Unmarshal([]byte(body), &req) != nil || !strings.HasPrefix(req.Code, "ys1") || req.Person == "" || len(req.Person) > 60 {
		return
	}
	name, err := cleanDomain(req.Name, peers.GroupDomain(list))
	if err != nil || name != req.Name {
		return
	}
	for i, c := range f.Claims {
		if c.Name == name && c.Node == from {
			if c.Status != "asked" {
				return // already answered
			}
			f.Claims = append(f.Claims[:i], f.Claims[i+1:]...)
			break
		}
	}
	f.Claims = append(f.Claims, nameClaim{Name: name, Node: from, NodeName: fromName, Code: req.Code, Person: req.Person, Time: at, Status: "asked", Domain: true})
	d.event("info", fmt.Sprintf("%s (on %s's box) asks for the domain %s: approve or decline it under Names", req.Person, fromName, name))
	d.mailAdmin(ctx, "Domain request: "+name, fmt.Sprintf("%s asks for %s for %s, on their box.\r\n\r\nApprove or decline it under Names on your dashboard. "+
		"Approving adds the domain to the group's Cloudflare account and routes its website to the web nodes.\r\n", fromName, name, req.Person))
}

// addDomain does the Cloudflare part of approving a domain.
func (d *Dashboard) addDomain(ctx context.Context, domain string) ([]string, error) {
	if d.addDomainFn != nil {
		return d.addDomainFn(ctx, domain)
	}
	api, err := os.ReadFile(d.cfg.CloudflareToken)
	if err != nil {
		return nil, fmt.Errorf("the Cloudflare API token: %w", err)
	}
	tun, err := os.ReadFile(d.cfg.TunnelToken)
	if err != nil {
		return nil, fmt.Errorf("the tunnel's token: %w", err)
	}
	return site.AddDomain(ctx, string(api), strings.TrimSpace(string(tun)), domain, d.siteService())
}

// giveDomain adds domain to the group's list as one of person's sites.
func (d *Dashboard) giveDomain(node, domain, code, person string) error {
	d.namesMu.Lock()
	defer d.namesMu.Unlock()
	list, err := peers.Load(d.cfg.PeersPath)
	if err != nil {
		return err
	}
	if p, ok := peers.NameOwner(list, domain); ok {
		if p.IP() == node {
			return nil
		}
		return fmt.Errorf("%s is already %s's", domain, p.Name)
	}
	for i, p := range list {
		if p.IP() == node {
			list[i].PeopleNames = append(list[i].PeopleNames, peers.PersonName{Name: domain, Person: person, Code: code, Site: true})
			if err := peers.Validate(list); err != nil {
				return err
			}
			if err := peers.Write(d.cfg.PeersPath, list); err != nil {
				return err
			}
			d.poke()
			return nil
		}
	}
	return errors.New("that node is no longer in the group")
}

// handleDomainLive says whether a domain asked for here answers yet.
func (d *Dashboard) handleDomainLive(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	d.namesMu.Lock()
	known := false
	for _, c := range d.loadNames().Claims {
		known = known || (c.Mine && c.Domain && c.Name == name)
	}
	d.namesMu.Unlock()
	if !known {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	writeJSONResp(w, map[string]bool{"live": site.Answers(ctx, name)})
}
