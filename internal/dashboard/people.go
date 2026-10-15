package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/peterretief/yggstore/internal/devices"
	"github.com/peterretief/yggstore/internal/hub"
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
	return "http://" + net.JoinHostPort(d.cfg.SelfID, HubPort)
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
		people = append(people, bp)
	}
	out["people"], out["files_at"] = people, d.boxURL()+hub.FilesPath
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
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	action := r.PathValue("action")
	var token, done string
	_, err := hub.Update(r.Context(), d.cfg.HubState, func(s *hub.HubState) error {
		switch action {
		case "add":
			p, err := s.AddPerson(req.Name, d.cfg.HubPeople)
			if err != nil {
				return err
			}
			req.Name = p.Name
			token, err = s.Invite(p.Name, time.Now())
			done = p.Name + " is on the box now: send them their invite link"
			return err
		case "invite":
			var err error
			token, err = s.Invite(req.Name, time.Now())
			done = "a new invite link for " + req.Name
			return err
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
		out["link"] = d.boxURL() + hub.JoinPath + "?t=" + token
	}
	writeJSONResp(w, out)
}
