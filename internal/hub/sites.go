package hub

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/websites"
)

// Site is a website in the hub's websites/ folder, as the running hub last
// had it, for yggstore hub sites and the hub's page.
type Site struct {
	Name      string `json:"name"` // its domain
	Published int64  `json:"published,omitempty"`
	Version   string `json:"version,omitempty"`
	Problem   string `json:"problem,omitempty"` // why it isn't published (or online)
	// Force: yggstore hub site publish asked for it to be published as it
	// is, even if it looks broken.
	Force bool `json:"force,omitempty"`

	// From before websites/, when each site was a Syncthing folder of its
	// own: kept until the hub has moved it into websites/ and stopped
	// syncing the old folder.
	Folder string `json:"folder,omitempty"`
	Path   string `json:"path,omitempty"`
}

// WebsitesFolderID is the Syncthing folder the hub's websites/ syncs in,
// with every paired device.
const WebsitesFolderID = "yggstore-websites"

// SyncthingFile is whether a path inside a synced folder is Syncthing's own
// (its marker, ignore file, versions and files being written).
func SyncthingFile(rel string, dir bool) bool {
	b := path.Base(rel)
	return b == ".stfolder" || b == ".stversions" || b == ".stignore" ||
		strings.HasPrefix(b, ".syncthing.") || strings.HasPrefix(b, "~syncthing~")
}

// Websites runs the websites package's watcher on the hub's websites/
// folder, and keeps the hub's state in step with it: yggstore hub site
// publish asks for a forced publish there, and the watcher's view of each
// site is written back for yggstore hub sites and the hub's page.
type Websites struct {
	W         *websites.Watcher
	StatePath string
	Logf      func(format string, args ...any)
}

func (h *Websites) Run(ctx context.Context) {
	if h.Logf == nil {
		h.Logf = func(string, ...any) {}
	}
	h.moveOldSites()
	go func() {
		if err := h.W.Run(ctx); err != nil {
			h.Logf("hub: websites: %v", err)
		}
	}()
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		h.sync(ctx)
	}
}

// moveOldSites moves the folders of sites kept the old way, one Syncthing
// folder each, into websites/, before the watcher starts (which would
// otherwise fill a new folder for them). Users stops syncing the old ones.
func (h *Websites) moveOldSites() {
	st, err := Load(h.StatePath)
	if err != nil {
		return
	}
	os.MkdirAll(h.W.Dir(), 0o755)
	for _, si := range st.Sites {
		if si.Path == "" {
			continue
		}
		to := filepath.Join(h.W.Dir(), si.Name)
		if _, err := os.Stat(to); err == nil {
			continue
		}
		if err := os.Rename(si.Path, to); err != nil {
			if !os.IsNotExist(err) {
				h.Logf("hub: site %s: moving %s into %s: %v", si.Name, si.Path, h.W.Dir(), err)
			}
			continue
		}
		for _, f := range []string{".stfolder", ".stversions", ".stignore"} {
			os.RemoveAll(filepath.Join(to, f))
		}
		h.Logf("hub: site %s moved into %s", si.Name, to)
	}
}

// sync passes forced publishes to the watcher, and writes its view of each
// site into the hub's state.
func (h *Websites) sync(ctx context.Context) {
	status := h.W.Status()
	Update(ctx, h.StatePath, func(s *HubState) error {
		old := map[string]Site{}
		for _, si := range s.Sites {
			old[si.Name] = si
			if si.Force {
				h.W.Force(si.Name)
			}
		}
		sites := []Site{}
		for _, ws := range status {
			si := old[ws.Site]
			si.Name, si.Problem, si.Force = ws.Site, ws.Problem, false
			if ws.Published > 0 {
				si.Published, si.Version = ws.Published, ws.Version
			}
			delete(old, ws.Site)
			sites = append(sites, si)
		}
		for _, si := range old {
			if si.Folder != "" { // an old folder Users hasn't dropped yet
				si.Force = false // passed on above
				sites = append(sites, si)
			}
		}
		s.Sites = sites
		return nil
	})
}
