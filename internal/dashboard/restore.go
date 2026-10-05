package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/outbox"
)

// RestoreState is where restores go and what was restored lately.
type RestoreState struct {
	Dir     string            `json:"dir"`
	Default string            `json:"default"`
	Recent  []outbox.Restored `json:"recent"`
}

func (d *Dashboard) restoreState() *RestoreState {
	o := d.cfg.Outbox
	recent := o.Restores()
	if recent == nil {
		recent = []outbox.Restored{}
	}
	return &RestoreState{Dir: o.RestoreDir(), Default: o.DefaultRestoreDir(), Recent: recent}
}

// handleRestoreTo sets the restore folder: {"dir": "/full/path"}, or "" for
// the default.
func (d *Dashboard) handleRestoreTo(w http.ResponseWriter, r *http.Request) {
	if d.cfg.Outbox == nil {
		http.Error(w, "dashboard was started without -outfiles", http.StatusBadRequest)
		return
	}
	var req struct {
		Dir string `json:"dir"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := d.cfg.Outbox.SetRestoreDir(expandHome(strings.TrimSpace(req.Dir))); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(d.restoreState())
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

// handleOpenFolder shows a restored item in the file manager. Only recent
// restores and the restore folder can be opened, so a page can't make the
// dashboard open arbitrary paths.
func (d *Dashboard) handleOpenFolder(w http.ResponseWriter, r *http.Request) {
	if d.cfg.Outbox == nil {
		http.Error(w, "dashboard was started without -outfiles", http.StatusBadRequest)
		return
	}
	path := r.URL.Query().Get("path")
	st := d.restoreState()
	allowed := path == st.Dir
	for _, x := range st.Recent {
		if x.Path != "" && x.Path == path {
			allowed = true
		}
	}
	if !allowed {
		http.Error(w, "not a restored item", http.StatusNotFound)
		return
	}
	if _, err := os.Stat(path); err != nil {
		http.Error(w, "it is no longer there: "+err.Error(), http.StatusNotFound)
		return
	}
	if err := openInFileManager(path, path != st.Dir); err != nil {
		http.Error(w, fmt.Sprintf("could not open the file manager (%v); the path is %s", err, path), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// openInFileManager opens a folder, or the folder holding an item with the
// item selected where the system supports it.
func openInFileManager(path string, selectItem bool) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		if selectItem {
			cmd = exec.Command("explorer", "/select,"+path)
		} else {
			cmd = exec.Command("explorer", path)
		}
	case "darwin":
		if selectItem {
			cmd = exec.Command("open", "-R", path)
		} else {
			cmd = exec.Command("open", path)
		}
	default:
		dir := path
		if fi, err := os.Stat(path); err == nil && !fi.IsDir() {
			dir = filepath.Dir(path)
		}
		cmd = exec.Command("xdg-open", dir)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait() // explorer exits non-zero even when it worked; don't judge by it
	return nil
}

// handleVersions lists every version of a listed item.
func (d *Dashboard) handleVersions(w http.ResponseWriter, r *http.Request) {
	stub := r.URL.Query().Get("stub")
	if d.cfg.Outbox == nil || !d.knownStub(stub) {
		http.Error(w, "unknown stub", http.StatusNotFound)
		return
	}
	vs, err := d.cfg.Outbox.Versions(stub)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for i := range vs {
		vs[i].Stub = "" // the browser only needs the IDs
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(vs)
}

// handleRestoreFolder rebuilds a folder of the outbox as it was at a time:
// {"dir": "Photos/2026", "at": unix seconds}. "" is the whole outbox.
func (d *Dashboard) handleRestoreFolder(w http.ResponseWriter, r *http.Request) {
	if d.cfg.Outbox == nil {
		http.Error(w, "dashboard was started without -outfiles", http.StatusBadRequest)
		return
	}
	var req struct {
		Dir string `json:"dir"`
		At  int64  `json:"at"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil || req.At <= 0 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	at := time.Unix(req.At, 0)
	d.Event("info", fmt.Sprintf("restoring /%s as it was on %s…", strings.Trim(req.Dir, "/"), at.Format("2 Jan 2006 15:04")))
	go d.cfg.Outbox.RestoreFolderAt(context.Background(), req.Dir, at)
	w.WriteHeader(http.StatusAccepted)
}
