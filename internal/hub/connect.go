package hub

import (
	"context"
	_ "embed"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
)

// A computer on the LAN connects to the box with one command from the page,
// instead of setting Syncthing up by hand: connect.sh (Linux, macOS) or
// connect.ps1 (Windows) installs Syncthing, adds the box and then asks
// /api/connect/folders which folders to accept and where to put them.

//go:embed connect.sh
var connectSh string

//go:embed connect.ps1
var connectPs1 string

// SyncthingPort is the port the box's Syncthing listens on, as devices are
// told to reach it.
const SyncthingPort = "22000"

var (
	deviceIDRE = regexp.MustCompile(`^[A-Z0-9]{7}(-[A-Z0-9]{7}){7}$`)
	boxNameRE  = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
)

// ConnectScript is the connect script (connect.sh or connect.ps1) for a
// box: the hub at hubURL, called name, with Syncthing ID id, at addrs.
func ConnectScript(script, hubURL, name, id string, addrs []string) string {
	return strings.NewReplacer("@HUB@", hubURL, "@BOX@", name, "@BOX_ID@", id,
		"@ADDRS@", strings.Join(addrs, " ")).Replace(script)
}

// boxAddrs are where devices reach this box's Syncthing: its LAN
// addresses, then wherever discovery finds it.
func boxAddrs() []string {
	var out []string
	for _, ip := range LANAddrs() {
		if strings.Contains(ip, ":") {
			ip = "[" + ip + "]"
		}
		out = append(out, "tcp://"+ip+":"+SyncthingPort)
	}
	return append(out, "dynamic")
}

func (h *Hub) handleConnect(script, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !hostRE.MatchString(r.Host) {
			http.Error(w, "bad host", http.StatusBadRequest)
			return
		}
		var id string
		if h.cfg.Users != nil {
			id = h.cfg.Users.Status().DeviceID
		}
		if !deviceIDRE.MatchString(id) {
			http.Error(w, "this box isn't taking devices through Syncthing", http.StatusServiceUnavailable)
			return
		}
		host, _ := os.Hostname()
		name := strings.Trim(boxNameRE.ReplaceAllString(host, "-"), "-.")
		if name == "" {
			name = "box"
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprint(w, ConnectScript(script, "http://"+r.Host, name, id, boxAddrs()))
		h.cfg.Users.Fetched(r.RemoteAddr, strings.TrimPrefix(r.URL.Path, "/"))
	}
}

// handleConnectFolders lists the folders the box shares with a device, a
// line each: ID, label and where it goes under the device's home folder,
// separated by tabs. Nothing until the box has taken the device.
func (h *Hub) handleConnectFolders(w http.ResponseWriter, r *http.Request) {
	dev := r.URL.Query().Get("device")
	if !deviceIDRE.MatchString(dev) || h.cfg.Users == nil {
		http.Error(w, "no such device", http.StatusBadRequest)
		return
	}
	list, err := h.cfg.Users.FoldersFor(r.Context(), dev)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	for _, f := range list {
		fmt.Fprintf(w, "%s\t%s\t%s\n", f.ID, f.Label, f.Dir)
	}
}

// DeviceFolder is a folder the box shares with a device, and the folder
// under the device's home folder the connect script puts it in.
type DeviceFolder struct {
	ID, Label, Dir string
}

// FoldersFor lists the folders the box's Syncthing shares with a device it
// has taken: the person's own folder first, then the websites and teams.
func (u *Users) FoldersFor(ctx context.Context, device string) ([]DeviceFolder, error) {
	st, err := Load(u.cfg.StatePath)
	if err != nil {
		return nil, err
	}
	if !known(st.Users, device) {
		return nil, nil
	}
	folders, err := u.cfg.ST.Folders(ctx)
	if err != nil {
		return nil, err
	}
	var own, rest []DeviceFolder
	for _, f := range folders {
		if !f.Shared(device) {
			continue
		}
		label := strings.NewReplacer("\t", " ", "\n", " ", "\r", " ").Replace(f.Label)
		switch {
		case f.ID == WebsitesFolderID:
			rest = append(rest, DeviceFolder{f.ID, label, "yggstore-websites"})
		case strings.HasPrefix(f.ID, "yggstore-team-"):
			name := strings.TrimPrefix(f.ID, "yggstore-team-")
			for _, t := range st.Teams {
				if n := strings.Trim(unsafeName.ReplaceAllString(t.Name, "-"), "-."); t.Folder == f.ID && n != "" {
					name = n
				}
			}
			rest = append(rest, DeviceFolder{f.ID, label, "yggstore-teams/" + name})
		default:
			own = append(own, DeviceFolder{f.ID, label, "yggstore"})
		}
	}
	return append(own, rest...), nil
}
