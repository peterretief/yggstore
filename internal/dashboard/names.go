package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/atomicfile"
	"github.com/peterretief/yggstore/internal/peers"
)

// Names under the group's domain (see peers/names.go). A member asks for one
// on their dashboard; the request goes as a message to the admin nodes, whose
// dashboards list it. The admin approves it by adding the name to the
// member's entry in the group's list, which then reaches every node: the web
// nodes deliver the address's mail to that node and let it send as it.

const (
	typeNameClaim  = "name-claim"  // member → admins: {"name", "code"}
	typeNameAnswer = "name-answer" // admin → member: {"name", "approved"}
)

// reserved are labels members can't ask for; an admin can still give them.
var reserved = map[string]bool{
	"www": true, "mail": true, "mail-in": true, "smtp": true, "imap": true, "admin": true, "administrator": true,
	"root": true, "postmaster": true, "abuse": true, "hostmaster": true, "webmaster": true, "noreply": true,
	"no-reply": true, "security": true,
}

type nameClaim struct {
	Name     string `json:"name"` // anna.example.org
	Node     string `json:"node"` // the asking node's ID
	NodeName string `json:"node_name,omitempty"`
	Code     string `json:"code,omitempty"` // the asker's sharing code
	Time     int64  `json:"time"`           // unix ms
	Status   string `json:"status"`         // asked, approved, declined
	Mine     bool   `json:"mine,omitempty"` // asked from this dashboard
}

type namesFile struct {
	After  int64       `json:"after"` // last message read
	Claims []nameClaim `json:"claims"`
}

func (d *Dashboard) loadNames() namesFile {
	var f namesFile
	if b, err := os.ReadFile(d.cfg.NamesPath); err == nil {
		json.Unmarshal(b, &f)
	}
	return f
}

func (d *Dashboard) saveNames(f namesFile) error {
	b, _ := json.MarshalIndent(f, "", "  ")
	return atomicfile.Replace(d.cfg.NamesPath, append(b, '\n'), 0o600)
}

func selfEntry(list []peers.Peer, self string) (peers.Peer, bool) {
	for _, p := range list {
		if p.IP() == self {
			return p, true
		}
	}
	return peers.Peer{}, false
}

func isAdmin(list []peers.Peer, ip string) bool {
	p, ok := selfEntry(list, ip)
	return ok && p.Admin
}

// syncNames reads name requests and answers that came in, notes requests
// the group's list now grants, and gives the mailbox its address once this
// node has a name. poll calls it.
func (d *Dashboard) syncNames(ctx context.Context, list []peers.Peer) {
	if d.cfg.NamesPath == "" {
		return
	}
	d.namesMu.Lock()
	defer d.namesMu.Unlock()
	f := d.loadNames()
	changed := false
	if l, err := d.msgClient(); err == nil {
		mctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		msgs, err := l.Messages(mctx, f.After, "direct", 1000)
		cancel()
		if err == nil {
			for _, m := range msgs {
				f.After, changed = max(f.After, m.N), true
				switch m.Type {
				case typeNameClaim:
					d.takeClaim(&f, list, m.From, m.FromName, m.Body, m.Time)
				case typeNameAnswer:
					takeAnswer(&f, list, m.From, m.Body)
				}
			}
		}
	}
	own, _ := selfEntry(list, d.cfg.SelfID)
	for i := range f.Claims {
		c := &f.Claims[i]
		if c.Status == "asked" && c.Mine && contains(own.Names, c.Name) {
			c.Status, changed = "approved", true
			d.event("ok", fmt.Sprintf("you now have %s and https://%s", peers.MailAddress(c.Name), c.Name))
		}
	}
	if changed {
		if err := d.saveNames(f); err != nil {
			d.event("warn", "could not save name requests: "+err.Error())
		}
	}
	if len(own.Names) > 0 {
		if box, err := d.mailBox(); err == nil && box.Address() == "" {
			if err := box.SetAddress(peers.MailAddress(own.Names[0])); err == nil {
				d.event("info", "your mail address is now "+peers.MailAddress(own.Names[0]))
			}
		}
	}
}

func (d *Dashboard) takeClaim(f *namesFile, list []peers.Peer, from, fromName, body string, at int64) {
	if !isAdmin(list, d.cfg.SelfID) {
		return
	}
	var req struct{ Name, Code string }
	if json.Unmarshal([]byte(body), &req) != nil || !strings.HasPrefix(req.Code, "ys1") {
		return
	}
	domain := peers.GroupDomain(list)
	l, ok := strings.CutSuffix(req.Name, "."+domain)
	if domain == "" || !ok || peers.ValidLabel(l) != nil {
		return
	}
	for i, c := range f.Claims {
		if c.Name == req.Name && c.Node == from {
			if c.Status != "asked" {
				return // already answered
			}
			f.Claims = append(f.Claims[:i], f.Claims[i+1:]...)
			break
		}
	}
	f.Claims = append(f.Claims, nameClaim{Name: req.Name, Node: from, NodeName: fromName, Code: req.Code, Time: at, Status: "asked"})
	d.event("info", fmt.Sprintf("%s asks for %s: approve or decline it under Names", fromName, peers.MailAddress(req.Name)))
}

func takeAnswer(f *namesFile, list []peers.Peer, from, body string) {
	if !isAdmin(list, from) {
		return
	}
	var ans struct {
		Name     string `json:"name"`
		Approved bool   `json:"approved"`
	}
	if json.Unmarshal([]byte(body), &ans) != nil {
		return
	}
	for i := range f.Claims {
		if c := &f.Claims[i]; c.Mine && c.Name == ans.Name && c.Status == "asked" && !ans.Approved {
			c.Status = "declined"
		}
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

type namesState struct {
	Domain   string      `json:"domain"`
	Admin    bool        `json:"admin"`
	Me       string      `json:"me"`       // the person's name, as people see it
	Mine     []string    `json:"mine"`     // names this node has
	Claims   []nameClaim `json:"claims"`   // asked from here
	Requests []nameClaim `json:"requests"` // from others, on an admin dashboard
	Given    []givenName `json:"given"`    // every name in the group, on an admin dashboard
	Error    string      `json:"error,omitempty"`
}

type givenName struct {
	Name string `json:"name"`
	Node string `json:"node"`
}

func (d *Dashboard) handleNames(w http.ResponseWriter, r *http.Request) {
	out := namesState{Me: d.me(), Mine: []string{}, Claims: []nameClaim{}, Requests: []nameClaim{}, Given: []givenName{}}
	list, err := peers.Load(d.cfg.PeersPath)
	if err != nil {
		out.Error = err.Error()
		writeJSONResp(w, out)
		return
	}
	out.Domain, out.Admin = peers.GroupDomain(list), isAdmin(list, d.cfg.SelfID)
	if own, ok := selfEntry(list, d.cfg.SelfID); ok {
		out.Mine = append(out.Mine, own.Names...)
	}
	d.namesMu.Lock()
	f := d.loadNames()
	d.namesMu.Unlock()
	for _, c := range f.Claims {
		switch {
		case c.Mine && c.Status != "approved":
			out.Claims = append(out.Claims, c)
		case !c.Mine && out.Admin && (c.Status == "asked" || time.Since(time.UnixMilli(c.Time)) < 30*24*time.Hour):
			out.Requests = append(out.Requests, c)
		}
	}
	sort.Slice(out.Requests, func(i, j int) bool { return out.Requests[i].Time > out.Requests[j].Time })
	if out.Admin {
		for _, p := range list {
			for _, n := range p.Names {
				out.Given = append(out.Given, givenName{Name: n, Node: p.Name})
			}
		}
	}
	writeJSONResp(w, out)
}

// handleClaim asks for LABEL.domain: an admin's dashboard gives it at once,
// a member's sends the request to the admins.
func (d *Dashboard) handleClaim(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Label string `json:"label"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req)
	l := strings.ToLower(strings.TrimSpace(req.Label))
	list, err := peers.Load(d.cfg.PeersPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	domain := peers.GroupDomain(list)
	admin := isAdmin(list, d.cfg.SelfID)
	switch {
	case domain == "":
		err = errors.New(`the group has no domain yet: the admin sets "domain" on their entry in peers.json`)
	case d.cfg.Identity == nil:
		err = errors.New("this dashboard has no sharing key, which your mail is sealed for")
	case peers.ValidLabel(l) != nil:
		err = peers.ValidLabel(l)
	case reserved[l] && !admin:
		err = fmt.Errorf("%s is kept for the group; choose another", l)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	name := l + "." + domain
	if p, ok := peers.NameOwner(list, name); ok {
		msg := name + " is taken"
		if p.IP() == d.cfg.SelfID {
			msg = name + " is already yours"
		}
		http.Error(w, msg, http.StatusConflict)
		return
	}
	if admin {
		if err := d.giveName(d.cfg.SelfID, name, d.cfg.Identity.Code()); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		d.event("ok", fmt.Sprintf("you now have %s and https://%s", peers.MailAddress(name), name))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	lc, err := d.msgClient()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	body, _ := json.Marshal(map[string]string{"name": name, "code": d.cfg.Identity.Code()})
	sent := 0
	for _, p := range list {
		if p.Admin {
			if _, err = lc.Send(r.Context(), p.IP(), typeNameClaim, string(body)); err == nil {
				sent++
			}
		}
	}
	if sent == 0 {
		http.Error(w, "could not send the request to the group's admin: "+shortErr(err), http.StatusBadGateway)
		return
	}
	d.namesMu.Lock()
	f := d.loadNames()
	keep := f.Claims[:0]
	for _, c := range f.Claims {
		if !(c.Mine && c.Name == name) {
			keep = append(keep, c)
		}
	}
	f.Claims = append(keep, nameClaim{Name: name, Node: d.cfg.SelfID, Time: time.Now().UnixMilli(), Status: "asked", Mine: true})
	err = d.saveNames(f)
	d.namesMu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	d.event("info", fmt.Sprintf("asked the group's admin for %s", peers.MailAddress(name)))
	w.WriteHeader(http.StatusNoContent)
}

// giveName adds name to node's entry in the group's list, with the sharing
// code its mail is sealed for; poll then sends every node the new list.
func (d *Dashboard) giveName(node, name, code string) error {
	d.namesMu.Lock()
	defer d.namesMu.Unlock()
	list, err := peers.Load(d.cfg.PeersPath)
	if err != nil {
		return err
	}
	if p, ok := peers.NameOwner(list, name); ok && p.IP() != node {
		return fmt.Errorf("%s is already %s's", name, p.Name)
	}
	found := false
	for i := range list {
		if list[i].IP() == node {
			if !contains(list[i].Names, name) {
				list[i].Names = append(list[i].Names, name)
			}
			list[i].Code, found = code, true
		}
	}
	if !found {
		return errors.New("that node is no longer in the group")
	}
	if err := peers.Validate(list); err != nil {
		return err
	}
	if err := peers.Write(d.cfg.PeersPath, list); err != nil {
		return err
	}
	d.poke()
	return nil
}

// handleDecide approves or declines a member's request (admin dashboards).
func (d *Dashboard) handleDecide(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name    string `json:"name"`
		Node    string `json:"node"`
		Approve bool   `json:"approve"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req)
	list, err := peers.Load(d.cfg.PeersPath)
	if err != nil || !isAdmin(list, d.cfg.SelfID) {
		http.Error(w, "only an admin dashboard decides on names", http.StatusForbidden)
		return
	}
	d.namesMu.Lock()
	f := d.loadNames()
	var c *nameClaim
	for i := range f.Claims {
		if f.Claims[i].Name == req.Name && f.Claims[i].Node == req.Node && !f.Claims[i].Mine {
			c = &f.Claims[i]
		}
	}
	d.namesMu.Unlock()
	if c == nil {
		http.Error(w, "no such request", http.StatusNotFound)
		return
	}
	if req.Approve {
		if err := d.giveName(c.Node, c.Name, c.Code); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
	}
	status := map[bool]string{true: "approved", false: "declined"}[req.Approve]
	d.namesMu.Lock()
	f = d.loadNames()
	for i := range f.Claims {
		if f.Claims[i].Name == req.Name && f.Claims[i].Node == req.Node && !f.Claims[i].Mine {
			f.Claims[i].Status = status
		}
	}
	err = d.saveNames(f)
	d.namesMu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if lc, err := d.msgClient(); err == nil {
		body, _ := json.Marshal(map[string]any{"name": req.Name, "approved": req.Approve})
		lc.Send(r.Context(), req.Node, typeNameAnswer, string(body))
	}
	d.event("ok", fmt.Sprintf("%s %s for %s", status, peers.MailAddress(req.Name), c.NodeName))
	w.WriteHeader(http.StatusNoContent)
}

// handleRemoveName takes a name back (admin dashboards).
func (d *Dashboard) handleRemoveName(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req)
	d.namesMu.Lock()
	defer d.namesMu.Unlock()
	list, err := peers.Load(d.cfg.PeersPath)
	if err != nil || !isAdmin(list, d.cfg.SelfID) {
		http.Error(w, "only an admin dashboard takes names back", http.StatusForbidden)
		return
	}
	found := ""
	for i := range list {
		keep := list[i].Names[:0]
		for _, n := range list[i].Names {
			if n == req.Name {
				found = list[i].Name
			} else {
				keep = append(keep, n)
			}
		}
		if list[i].Names = keep; len(keep) == 0 {
			list[i].Names = nil
		}
	}
	if found == "" {
		http.Error(w, "no one has "+req.Name, http.StatusNotFound)
		return
	}
	if err := peers.Write(d.cfg.PeersPath, list); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	d.poke()
	d.event("info", fmt.Sprintf("took %s back from %s", req.Name, found))
	w.WriteHeader(http.StatusNoContent)
}

func (d *Dashboard) poke() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}
