package dashboard

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/hub"
	"github.com/peterretief/yggstore/internal/mail"
	"github.com/peterretief/yggstore/internal/peers"
)

// The group's email page (admin dashboards only): every mailbox on every
// box, and the machines' own, with how much mail comes and goes. A box
// counts its mailboxes from the messages' plain notes (when each came,
// whether it's read, which folder), so nothing is opened: no senders,
// subjects or words leave the box. The counts ride on the hourly usage
// summary (groupusage.go).

//go:embed groupmail.html
var groupMailHTML []byte

const mailWeek = 7 * 24 * time.Hour

type mailboxUsage struct {
	Person  string `json:"person,omitempty"` // "" is the machine's own mailbox
	Shared  bool   `json:"shared,omitempty"` // support@ and the like, for everyone on the box's network
	Inbox   int    `json:"inbox"`
	Unread  int    `json:"unread"`
	Total   int    `json:"total"`
	InWeek  int    `json:"in_week"`  // came in the last seven days
	OutWeek int    `json:"out_week"` // sent in the last seven days
	LastIn  int64  `json:"last_in,omitempty"`
	LastOut int64  `json:"last_out,omitempty"`
	Bytes   int64  `json:"bytes"`
}

// countMail reads a mailbox's notes; ok is false if there's no mailbox.
func countMail(dir string, now time.Time) (mailboxUsage, bool) {
	var m mailboxUsage
	ents, err := os.ReadDir(dir)
	if err != nil {
		return m, false
	}
	weekAgo := now.Add(-mailWeek).UnixMilli()
	for _, e := range ents {
		id, ok := strings.CutSuffix(e.Name(), ".sealed")
		if !ok {
			continue
		}
		if fi, err := e.Info(); err == nil {
			m.Bytes += fi.Size()
		}
		var meta mail.Meta
		if b, err := os.ReadFile(filepath.Join(dir, id+".json")); err == nil {
			json.Unmarshal(b, &meta)
		}
		m.Total++
		if meta.Sent {
			if meta.Received >= weekAgo {
				m.OutWeek++
			}
			m.LastOut = max(m.LastOut, meta.Received)
			continue
		}
		if meta.Received >= weekAgo {
			m.InWeek++
		}
		m.LastIn = max(m.LastIn, meta.Received)
		if meta.Folder == "" || meta.Folder == mail.Inbox {
			m.Inbox++
			seen := false
			for _, f := range meta.Flags {
				seen = seen || f == `\Seen`
			}
			if !seen {
				m.Unread++
			}
		}
	}
	return m, true
}

// mailSummary counts this machine's own mailbox and, on a box, its people's.
func (d *Dashboard) mailSummary(now time.Time) []mailboxUsage {
	out := []mailboxUsage{}
	if d.cfg.MailDir != "" {
		if m, ok := countMail(d.cfg.MailDir, now); ok {
			out = append(out, m)
		}
	}
	if !d.onBox() || d.cfg.HubPeople == "" {
		return out
	}
	st, err := hub.Load(d.cfg.HubState)
	if err != nil {
		return out
	}
	// The hub keeps people's mail beside their files: HUB/people/FOLDER/mail.
	people := filepath.Join(filepath.Dir(d.cfg.HubPeople), "people")
	for _, p := range st.People {
		if p.Removed || p.Folder == "" {
			continue
		}
		m, ok := countMail(filepath.Join(people, p.Folder, "mail"), now)
		if !ok {
			m = mailboxUsage{}
		}
		m.Person, m.Shared = p.Name, p.Shared
		out = append(out, m)
	}
	return out
}

type groupMailState struct {
	Domain    string         `json:"domain"`
	Machines  []mailMachine  `json:"machines"`
	Mailboxes []mailboxShown `json:"mailboxes"`
}

type mailMachine struct {
	Machine   string `json:"machine"`
	At        int64  `json:"at"`        // when it last reported (0: never)
	Reports   bool   `json:"reports"`   // its yggstore counts mail
	Mailboxes int    `json:"mailboxes"` // people's, and its own
	InWeek    int    `json:"in_week"`
	OutWeek   int    `json:"out_week"`
	Unread    int    `json:"unread"`
	LastIn    int64  `json:"last_in,omitempty"`
}

type mailboxShown struct {
	Machine   string   `json:"machine"`
	Addresses []string `json:"addresses"`
	At        int64    `json:"at"`
	mailboxUsage
}

func (d *Dashboard) handleGroupMailPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(groupMailHTML)
}

func (d *Dashboard) handleGroupMail(w http.ResponseWriter, r *http.Request) {
	list, err := peers.Load(d.cfg.PeersPath)
	if err != nil || !isAdmin(list, d.cfg.SelfID) {
		http.Error(w, "only an admin dashboard shows the group's email", http.StatusForbidden)
		return
	}
	domain := peers.GroupDomain(list)
	out := groupMailState{Domain: domain, Machines: []mailMachine{}, Mailboxes: []mailboxShown{}}

	// Each machine's addresses: its own names, and its people's.
	own := map[string][]string{}
	person := map[string][]string{} // "machine\x00person"
	addName := func(to *[]string, name string) {
		if domain != "" && strings.HasSuffix(name, "."+domain) {
			*to = append(*to, peers.MailAddress(name))
		}
	}
	for _, p := range list {
		m := p.Machine()
		for _, n := range p.Names {
			s := own[m]
			addName(&s, n)
			own[m] = s
		}
		for _, pn := range p.PeopleNames {
			k := m + "\x00" + pn.Person
			s := person[k]
			addName(&s, pn.Name)
			person[k] = s
		}
	}

	u := d.usageReports()
	u.mu.Lock()
	reports := make([]boxUsage, 0, len(u.by))
	for _, b := range u.by {
		reports = append(reports, b)
	}
	u.mu.Unlock()
	// A machine with a node per disk sends one report from each.
	byMachine := map[string]*mailMachine{}
	for _, b := range reports {
		mm := byMachine[b.Box]
		if mm == nil {
			mm = &mailMachine{Machine: b.Box}
			byMachine[b.Box] = mm
		}
		mm.At = max(mm.At, b.At)
		mm.Reports = mm.Reports || b.Mail != nil
		for _, mb := range b.Mail {
			mm.Mailboxes++
			mm.InWeek += mb.InWeek
			mm.OutWeek += mb.OutWeek
			mm.Unread += mb.Unread
			mm.LastIn = max(mm.LastIn, mb.LastIn)
			shown := mailboxShown{Machine: b.Box, At: b.At, mailboxUsage: mb, Addresses: []string{}}
			if mb.Person == "" {
				shown.Addresses = append(shown.Addresses, own[b.Box]...)
			} else {
				shown.Addresses = append(shown.Addresses, person[b.Box+"\x00"+mb.Person]...)
				if a := peers.LocalAddress(list, b.Box, mb.Person); a != "" {
					shown.Addresses = append(shown.Addresses, a)
				}
			}
			out.Mailboxes = append(out.Mailboxes, shown)
		}
	}
	// Machines that haven't reported at all.
	for _, p := range list {
		if m := p.Machine(); byMachine[m] == nil && !p.Leaving {
			byMachine[m] = &mailMachine{Machine: m}
		}
	}
	for _, mm := range byMachine {
		out.Machines = append(out.Machines, *mm)
	}
	sort.Slice(out.Machines, func(i, j int) bool { return out.Machines[i].Machine < out.Machines[j].Machine })
	sort.Slice(out.Mailboxes, func(i, j int) bool {
		a, b := out.Mailboxes[i], out.Mailboxes[j]
		if a.Machine != b.Machine {
			return a.Machine < b.Machine
		}
		return a.Person < b.Person
	})
	writeJSONResp(w, out)
}
