package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/peterretief/yggstore/internal/devices"
	"github.com/peterretief/yggstore/internal/hub"
	"github.com/peterretief/yggstore/internal/peers"
)

// People on this box: on a box, its admin adds the people who use it,
// sends each an invite link, sees what they store and when their devices
// were last on, limits them, suspends them and removes them. The dashboard
// opens only on the admin's own devices, so this is the admin's alone. It
// changes the hub's state file, which the hub picks up within seconds.

// HubPort is the port the hub answers on, where people open their files.
const HubPort = "7490"

type boxPerson struct {
	Name        string           `json:"name"`
	Added       int64            `json:"added"`
	Bytes       int64            `json:"bytes"`
	Files       int              `json:"files"`
	Quota       int64            `json:"quota,omitempty"`
	MaxDevices  int              `json:"max_devices,omitempty"`
	Suspended   bool             `json:"suspended,omitempty"`
	Removed     bool             `json:"removed,omitempty"`
	InviteUntil int64            `json:"invite_until,omitempty"`
	Syncthing   int              `json:"syncthing"` // devices syncing their folder
	Devices     []devices.Device `json:"devices"`
	Path        string           `json:"path"`
	// Their own name in the group (see hub/personsite.go): given, or asked
	// for and waiting for the group's admin, or declined.
	Site     string  `json:"site,omitempty"`
	Asked    string  `json:"asked,omitempty"`
	Declined string  `json:"declined,omitempty"`
	Forward  *boxFwd `json:"forward,omitempty"`
	// Through the web (see hub/light.go): their passkeys, and whether the
	// admin turned web access off.
	Passkeys []boxKey `json:"passkeys"`
	WebOff   bool     `json:"web_off,omitempty"`
	// Their password: what they sign in with, and whether it is still the
	// temporary one (until when), or since when their own.
	Login    string `json:"login"`
	Password string `json:"password,omitempty"` // "temporary" or "own"
	PwUntil  int64  `json:"pw_until,omitempty"`
	PwSet    int64  `json:"pw_set,omitempty"`
}

type boxKey struct {
	Name string `json:"name"`
	Used int64  `json:"used,omitempty"`
}

type boxFwd struct {
	To        string `json:"to"`
	Confirmed bool   `json:"confirmed"`
	Allowed   bool   `json:"allowed"`
}

// usageCache keeps what each person stores for a minute: counting means
// reading every stub.
type usageCache struct {
	mu   sync.Mutex
	at   map[string]time.Time
	used map[string][2]int64
}

func (c *usageCache) get(p hub.Person) (int64, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.at == nil {
		c.at, c.used = map[string]time.Time{}, map[string][2]int64{}
	}
	if time.Since(c.at[p.Name]) > time.Minute {
		b, n := hub.Usage(p)
		c.at[p.Name], c.used[p.Name] = time.Now(), [2]int64{b, int64(n)}
	}
	u := c.used[p.Name]
	return u[0], int(u[1])
}

func (d *Dashboard) onBox() bool {
	if d.cfg.HubState == "" {
		return false
	}
	_, err := os.Stat(d.cfg.HubState)
	return err == nil
}

// boxURL is where the box's people open their files, over Yggdrasil.
func (d *Dashboard) boxURL() string {
	if ip := net.ParseIP(d.cfg.SelfID); ip == nil || ip.To4() != nil {
		return ""
	}
	return peers.HostURL(d.yggHost(d.cfg.SelfID), HubPort)
}

func (d *Dashboard) handlePeople(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"on": d.onBox()}
	if !d.onBox() {
		writeJSONResp(w, out)
		return
	}
	st, err := hub.Load(d.cfg.HubState)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	list, _ := peers.Load(d.cfg.PeersPath)
	d.namesMu.Lock()
	claims := d.loadNames().Claims
	d.namesMu.Unlock()
	people := []boxPerson{}
	for _, p := range st.People {
		bp := boxPerson{Name: p.Name, Added: p.Added, Quota: p.Quota, MaxDevices: p.MaxDevices, Suspended: p.Suspended,
			Removed: p.Removed, Syncthing: len(st.DevicesOf(p.Name)), Devices: p.FilesDevices, Path: p.Path}
		if bp.Devices == nil {
			bp.Devices = []devices.Device{}
		}
		bp.Bytes, bp.Files = d.usage.get(p)
		if t, ok := st.InviteOf(p.Name); ok {
			bp.InviteUntil = t.Unix()
		}
		if pn, ok := peers.PersonNameOf(list, d.cfg.SelfID, p.Name); ok {
			bp.Site = pn.Name
		}
		for _, c := range claims {
			if c.Mine && strings.EqualFold(c.Person, p.Name) {
				switch c.Status {
				case "asked":
					bp.Asked = c.Name
				case "declined":
					bp.Declined = c.Name
				}
			}
		}
		if f := p.Forward; f != nil {
			bp.Forward = &boxFwd{To: f.To, Confirmed: f.Confirmed, Allowed: f.Allowed}
		}
		bp.Passkeys, bp.WebOff, bp.Login = []boxKey{}, p.WebOff, hub.LoginOf(list, d.cfg.SelfID, p.Name)
		if pw := p.Password; pw != nil {
			bp.Password, bp.PwSet = map[bool]string{true: "temporary", false: "own"}[pw.Temp], pw.Set
			if pw.Temp {
				bp.PwUntil = pw.Expires
			}
		}
		for _, k := range p.Passkeys {
			bp.Passkeys = append(bp.Passkeys, boxKey{Name: k.Name, Used: k.Used})
		}
		people = append(people, bp)
	}
	out["people"], out["files_at"], out["domain"] = people, d.boxURL()+hub.FilesPath, peers.GroupDomain(list)
	writeJSONResp(w, out)
}

func (d *Dashboard) handlePeopleAction(w http.ResponseWriter, r *http.Request) {
	if !d.onBox() {
		http.Error(w, "this machine isn't a box: it has no hub", http.StatusConflict)
		return
	}
	var req struct {
		Name       string
		Addr       string
		Quota      *int64 // bytes, 0 for no limit
		MaxDevices *int   `json:"max_devices"`
		Label      string // for "name": the name to ask the group for
		Password   string // for "set-password"
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	action := r.PathValue("action")
	// A chosen password is typed on the box itself (yggstore admin), with
	// the person there; the admin page gives temporary ones instead.
	if action == "set-password" && !fromThisMachine(r) {
		http.Error(w, "a password can only be chosen on the box itself", http.StatusForbidden)
		return
	}
	if action == "name" {
		msg, err := d.claimForPerson(r.Context(), req.Name, req.Label)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		d.event("ok", "box: "+msg)
		writeJSONResp(w, map[string]string{})
		return
	}
	var token, done, pw string
	_, err := hub.Update(r.Context(), d.cfg.HubState, func(s *hub.HubState) error {
		switch action {
		case "add":
			p, err := s.AddPerson(req.Name, d.cfg.HubPeople)
			if err != nil {
				return err
			}
			req.Name = p.Name
			pw, err = s.SetTempPassword(p.Name, time.Now())
			done = p.Name + " is on the box now: give them their temporary password"
			return err
		case "set-password":
			done = req.Name + " has the password they chose: their old one, and the browsers it signed in, stop working"
			return s.SetChosenPassword(req.Name, req.Password, time.Now())
		case "password":
			var err error
			pw, err = s.SetTempPassword(req.Name, time.Now())
			done = "a new temporary password for " + req.Name + ": their old one, and the browsers it signed in, stop working"
			return err
		case "invite":
			_, had := s.InviteOf(req.Name)
			var err error
			token, err = s.Invite(req.Name, time.Now())
			done = "an invite link for " + req.Name
			if had {
				done = req.Name + "'s invite link, the same as before"
			}
			return err
		case "invite-cancel":
			if _, ok := s.InviteOf(req.Name); !ok {
				return fmt.Errorf("%s has no invite to cancel", req.Name)
			}
			s.DropInvite(req.Name)
			done = req.Name + "'s invite link is cancelled: it doesn't work any more"
			return nil
		case "limit":
			q, m := int64(-1), -1
			if req.Quota != nil {
				q = *req.Quota
			}
			if req.MaxDevices != nil {
				m = *req.MaxDevices
			}
			if q < -1 || m < -1 {
				return errors.New("a limit can't be below 0")
			}
			done = "changed what " + req.Name + " may use"
			return s.Limit(req.Name, q, m)
		case "suspend", "resume":
			done = req.Name + map[bool]string{true: " is shut out for now; their files stay", false: " may use the box again"}[action == "suspend"]
			return s.Suspend(req.Name, action == "suspend")
		case "remove":
			done = req.Name + " is removed: their devices are shut out, and their files stay on the box"
			return s.RemovePerson(req.Name)
		case "forward-allow", "forward-stop":
			on := action == "forward-allow"
			done = req.Name + map[bool]string{true: "'s mail is forwarded now", false: "'s mail isn't forwarded any more"}[on]
			return s.AllowForward(req.Name, on)
		case "web-on", "web-off":
			i := -1
			for j, p := range s.People {
				if strings.EqualFold(p.Name, req.Name) {
					i = j
				}
			}
			if i < 0 {
				return fmt.Errorf("nobody called %q on this box", req.Name)
			}
			s.People[i].WebOff = action == "web-off" // the box tells the web nodes, which end their sessions
			done = req.Name + map[bool]string{true: " can't open their files through the web now", false: " can open their files through the web again"}[s.People[i].WebOff]
			return nil
		case "device-remove":
			who, dev, err := s.LetOut(req.Name, req.Addr)
			done = fmt.Sprintf("%s can't open %s's files any more", dev.Name, who)
			return err
		}
		return errors.New("unknown request")
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	d.event("ok", "box: "+done)
	out := map[string]string{}
	if token != "" {
		out["ygg_link"] = d.boxURL() + hub.JoinPath + "?t=" + token
	}
	if pw != "" {
		list, _ := peers.Load(d.cfg.PeersPath)
		out["password"], out["login"], out["at"], out["name"] = pw, hub.LoginOf(list, d.cfg.SelfID, req.Name), hub.SignInAt(list), req.Name
	}
	writeJSONResp(w, out)
}

// claimForPerson asks the group's admin for LABEL.domain for someone on the
// box, with their own sharing code; an admin's box gives it at once. When
// the admin approves, the name goes on the box's entry for them.
func (d *Dashboard) claimForPerson(ctx context.Context, person, label string) (string, error) {
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
	domain, admin := peers.GroupDomain(list), isAdmin(list, d.cfg.SelfID)
	l := strings.ToLower(strings.TrimSpace(label))
	switch {
	case domain == "":
		return "", errors.New(`the group has no domain yet: the admin sets "domain" on their entry in peers.json`)
	case peers.ValidLabel(l) != nil:
		return "", peers.ValidLabel(l)
	case reserved[l] && !admin:
		return "", fmt.Errorf("%s is kept for the group; choose another", l)
	}
	name := l + "." + domain
	if _, ok := peers.NameOwner(list, name); ok {
		return "", fmt.Errorf("%s is taken", name)
	}
	if pn, ok := peers.PersonNameOf(list, d.cfg.SelfID, p.Name); ok {
		return "", fmt.Errorf("%s has %s already: one name each", p.Name, pn.Name)
	}
	id, err := hub.PersonKey(d.cfg.HubState, p)
	if err != nil {
		return "", fmt.Errorf("%s's sharing key: %w", p.Name, err)
	}
	if admin {
		if err := d.giveName(d.cfg.SelfID, name, id.Code(), p.Name); err != nil {
			return "", err
		}
		return fmt.Sprintf("%s has %s and https://%s now", p.Name, peers.MailAddress(name), name), nil
	}
	lc, err := d.msgClient()
	if err != nil {
		return "", err
	}
	body, _ := json.Marshal(map[string]string{"name": name, "code": id.Code(), "person": p.Name})
	sent := 0
	for _, a := range list {
		if a.Admin {
			if _, err = lc.Send(ctx, a.IP(), typeNameClaim, string(body)); err == nil {
				sent++
			}
		}
	}
	if sent == 0 {
		return "", errors.New("could not send the request to the group's admin: " + shortErr(err))
	}
	d.namesMu.Lock()
	defer d.namesMu.Unlock()
	f := d.loadNames()
	keep := f.Claims[:0]
	for _, c := range f.Claims {
		if !(c.Mine && strings.EqualFold(c.Person, p.Name)) {
			keep = append(keep, c)
		}
	}
	f.Claims = append(keep, nameClaim{Name: name, Node: d.cfg.SelfID, Person: p.Name, Time: time.Now().UnixMilli(), Status: "asked", Mine: true})
	if err := d.saveNames(f); err != nil {
		return "", err
	}
	return fmt.Sprintf("asked the group's admin for %s for %s", peers.MailAddress(name), p.Name), nil
}
