package dashboard

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/devices"
	"github.com/peterretief/yggstore/internal/workhere"
)

// Your devices: the laptops and phones that may open the dashboard, and
// your files at /files, over Yggdrasil. A device that isn't on the list
// can ask to be; the request shows here, to be allowed on this machine
// or on a device already allowed.

// deviceAsk is a device asking to be let in.
type deviceAsk struct {
	Addr string    `json:"addr"`
	Name string    `json:"name"`
	At   time.Time `json:"at"`
}

// maxAsks keeps strangers on Yggdrasil from filling the list.
const maxAsks = 10

// newDevicePages are what a device let in from the dashboard may open
// until you tick more.
var newDevicePages = []string{"files", "web", "messages", "mail"}

// hadDevice reports whether addr was on the list before (so allowing it
// again, to rename it, keeps its pages).
func (d *Dashboard) hadDevice(addr string) bool {
	_, ok := d.cfg.Devices.Device(addr)
	return ok
}

// FilesPage is where your files open.
const FilesPage = "/files"

func (d *Dashboard) filesPage() http.Handler {
	l := &workhere.Live{Holding: d.Holding}
	if d.cfg.Outbox != nil {
		l.Outbox = d.cfg.Outbox.Dir()
	}
	c := client.New()
	c.HTTP.Timeout = 0 // files of any size
	l.Client = c
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if d.cfg.Outbox == nil {
			http.Error(w, "this dashboard watches no folder (-outfiles)", http.StatusNotFound)
			return
		}
		l.ServeHTTP(w, r)
	})
}

// ask takes a request from a device not on the list (it can reach only
// this).
func (d *Dashboard) ask(w http.ResponseWriter, r *http.Request, remote string) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	r.ParseForm()
	name := strings.TrimSpace(r.PostForm.Get("name"))
	if len(name) > 60 {
		name = name[:60]
	}
	if name == "" {
		name = "a device"
	}
	d.mu.Lock()
	kept := d.asks[:0]
	for _, a := range d.asks {
		if a.Addr != remote && time.Since(a.At) < 24*time.Hour {
			kept = append(kept, a)
		}
	}
	d.asks = kept
	full := len(d.asks) >= maxAsks
	if !full {
		d.asks = append(d.asks, deviceAsk{Addr: remote, Name: name, At: time.Now()})
	}
	d.mu.Unlock()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	msg := `Asked. On your computer, open the dashboard and press <b>Allow</b> under <b>Your devices</b>; then <a href="/files">open your files</a>.`
	if full {
		w.WriteHeader(http.StatusTooManyRequests)
		msg = `Too many devices are asking right now. Add this one on your computer instead (below).`
	}
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width">
<title>Asked</title><body style="font-family:system-ui,sans-serif;max-width:40em;margin:3em auto;padding:0 1em;line-height:1.5">
<h1>%s</h1><p>This device's Yggdrasil address is <code>%s</code>.</p></body>`, msg, html.EscapeString(remote))
}

func (d *Dashboard) handleDevices(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"on": d.cfg.Devices != nil, "peers": d.cfg.YggPeers, "you": remoteIP(r)}
	if d.cfg.Devices == nil {
		writeJSONResp(w, out)
		return
	}
	list, err := devices.Load(d.cfg.Devices.Path())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if list == nil {
		list = []devices.Device{}
	}
	d.mu.Lock()
	asks := []deviceAsk{}
	for _, a := range d.asks {
		if time.Since(a.At) < 24*time.Hour && !d.cfg.Devices.Allowed(a.Addr) {
			asks = append(asks, a)
		}
	}
	d.mu.Unlock()
	out["devices"], out["asks"], out["url"] = list, asks, d.yggURL()
	writeJSONResp(w, out)
}

func (d *Dashboard) handleDeviceAction(w http.ResponseWriter, r *http.Request) {
	if d.cfg.Devices == nil {
		http.Error(w, "the dashboard isn't open to your devices: start it with -remote", http.StatusConflict)
		return
	}
	var req struct {
		Addr, Name string
		Pages      []string
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var err error
	switch r.PathValue("action") {
	case "add":
		name := strings.TrimSpace(req.Name)
		if name == "" {
			name = "my device"
		}
		// A new device gets every page but Admin, which is ticked for it
		// on purpose.
		had := d.hadDevice(req.Addr)
		var dev devices.Device
		if dev, err = devices.Add(d.cfg.Devices.Path(), req.Addr, name); err == nil && !had {
			_, err = devices.SetPages(d.cfg.Devices.Path(), dev.Addr, newDevicePages)
		}
		if err == nil {
			d.event("ok", fmt.Sprintf("%s (%s) may open your dashboard now: files, website, messages and mail", name, req.Addr))
		}
	case "pages":
		if remoteIP(r) == strings.Trim(req.Addr, "[]") && !slices.Contains(req.Pages, "admin") {
			http.Error(w, "this is the device you're using: take Admin away from it on another one", http.StatusConflict)
			return
		}
		var dev devices.Device
		if dev, err = devices.SetPages(d.cfg.Devices.Path(), req.Addr, req.Pages); err == nil {
			names := []string{}
			for _, p := range dev.Pages {
				names = append(names, pageNames[p])
			}
			d.event("ok", fmt.Sprintf("%s may open: %s", dev.Name, strings.Join(names, ", ")))
		}
	case "remove":
		if remoteIP(r) == strings.Trim(req.Addr, "[]") {
			http.Error(w, "this is the device you're using: remove it from another one", http.StatusConflict)
			return
		}
		var dev devices.Device
		if dev, err = devices.Remove(d.cfg.Devices.Path(), req.Addr); err == nil {
			d.event("ok", fmt.Sprintf("%s can't open your dashboard and files any more", dev.Name))
		}
	case "dismiss":
		ip, _ := devices.Parse(req.Addr)
		d.mu.Lock()
		kept := d.asks[:0]
		for _, a := range d.asks {
			if a.Addr != ip {
				kept = append(kept, a)
			}
		}
		d.asks = kept
		d.mu.Unlock()
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
