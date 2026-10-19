package dashboard

import (
	"fmt"
	"html"
	"net/http"
	"slices"
	"strings"

	"github.com/peterretief/yggstore/internal/devices"
)

// The dashboard's pages: each of your devices may open some of them, ticked
// under Your devices (on the admin page). Every page is the same index.html,
// showing its own part; what guards them is that each action and each part
// of the state belongs to one page, and a device gets only its own.

// PagePaths are where the pages open; "/" is the overview every device
// gets, with links to the pages it may open.
var PagePaths = map[string]string{
	"/storage":  "files",
	"/web":      "web",
	"/messages": "messages",
	"/mail":     "mail",
	"/wallet":   "wallet",
	"/admin":    "admin",
}

// pageNames are the pages as people read them.
var pageNames = map[string]string{"files": "Files", "web": "Website", "messages": "Messages", "mail": "Mail", "wallet": "Wallet", "admin": "Admin"}

// pageRoutes give each address the page it belongs to; an address is
// matched exactly or as a folder.
var pageRoutes = []struct{ path, page string }{
	{"/admin", "admin"}, {"/network", "admin"}, {"/api/network", "admin"}, {"/api/nodes", "admin"},
	{"/sites", "admin"}, {"/api/moves", "admin"},
	{"/api/invites", "admin"}, {"/api/box", "admin"}, {"/api/devices", "admin"},

	{"/storage", "files"}, {FilesPage, "files"}, {"/api/verify", "files"}, {"/api/restore", "files"},
	{"/api/restore-to", "files"}, {"/api/restore-folder", "files"}, {"/api/recovery", "files"},
	{"/api/versions", "files"}, {"/api/open-folder", "files"}, {"/api/delete", "files"},
	{"/api/share", "files"}, {"/api/contacts", "files"},

	{"/web", "web"}, {"/api/names", "web"}, {"/api/sites", "web"}, {"/api/me", "web"},
	{"/messages", "messages"}, {"/api/msg", "messages"},
	{"/mail", "mail"}, {"/api/mail", "mail"},
	{"/wallet", "wallet"}, {"/api/wallet", "wallet"},
}

// pageOf is the page path belongs to: "" for the overview and the state
// every page reads, and admin for any other action, so a new one is closed
// until it is given a page.
func pageOf(path string) string {
	for _, r := range pageRoutes {
		if path == r.path || strings.HasPrefix(path, r.path+"/") {
			return r.page
		}
	}
	if path == "/api/state" || !strings.HasPrefix(path, "/api/") {
		return ""
	}
	return "admin"
}

// viewerPages are the pages the request's device may open: all of them on
// this machine, or from this machine's own Yggdrasil address.
func (d *Dashboard) viewerPages(r *http.Request) []string {
	local := localIP(r)
	if local == nil || local.IsLoopback() || remoteIP(r) == d.cfg.YggAddr {
		return devices.Pages
	}
	if d.cfg.Devices == nil {
		return nil
	}
	dev, ok := d.cfg.Devices.Device(remoteIP(r))
	if !ok {
		return nil
	}
	var out []string
	for _, p := range devices.Pages {
		if dev.Can(p) {
			out = append(out, p)
		}
	}
	return out
}

func (d *Dashboard) canOpen(r *http.Request, page string) bool {
	return page == "" || slices.Contains(d.viewerPages(r), page)
}

// pageRefused tells a device it isn't let in to this page.
func pageRefused(w http.ResponseWriter, r *http.Request, page string) {
	msg := fmt.Sprintf("This device isn't let in to the %s page. Tick it for this device under Your devices, on the Admin page, from a device that has Admin.", pageNames[page])
	if strings.HasPrefix(r.URL.Path, "/api/") || r.Method != http.MethodGet {
		http.Error(w, msg, http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width">
<title>Not on this device</title><body style="font-family:system-ui,sans-serif;max-width:40em;margin:3em auto;padding:0 1em;line-height:1.5">
<h1>%s isn't open to this device</h1><p>%s</p><p><a href="/">Back to the pages it can open</a></p></body>`,
		html.EscapeString(pageNames[page]), html.EscapeString(msg))
}
