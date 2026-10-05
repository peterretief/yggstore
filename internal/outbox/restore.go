package outbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/peterretief/yggstore/internal/atomicfile"
)

// Restored is one finished (or failed) restore, so the dashboard can say
// where it went.
type Restored struct {
	Name  string `json:"name"`
	Path  string `json:"path,omitempty"` // full path of the rebuilt file or folder
	Dir   string `json:"dir"`            // the folder it was restored into
	Size  int64  `json:"size"`
	At    int64  `json:"at"`
	Error string `json:"error,omitempty"`
}

const keepRestored = 10

type settings struct {
	RestoreTo string `json:"restore_to,omitempty"`
}

func (w *Watcher) settingsPath() string {
	return filepath.Join(w.cfg.Dir, privateDir, "settings.json")
}

func (w *Watcher) readSettings() settings {
	var s settings
	if b, err := os.ReadFile(w.settingsPath()); err == nil {
		json.Unmarshal(b, &s)
	}
	return s
}

// DefaultRestoreDir is restored/ inside the watched folder.
func (w *Watcher) DefaultRestoreDir() string { return filepath.Join(w.cfg.Dir, RestoredDir) }

// RestoreDir is where restores go: the folder chosen with SetRestoreDir, or
// restored/ inside the watched folder.
func (w *Watcher) RestoreDir() string {
	if d := w.readSettings().RestoreTo; d != "" {
		return d
	}
	return w.DefaultRestoreDir()
}

// SetRestoreDir chooses where restores go; "" goes back to the default. The
// folder must be a full path, and can't be inside the watched folder (other
// than restored/), or the restored files would be stored again.
func (w *Watcher) SetRestoreDir(dir string) error {
	if dir != "" {
		if !filepath.IsAbs(dir) {
			return fmt.Errorf("give the full path of the folder, such as %s", filepath.Join(filepath.Dir(w.cfg.Dir), "Restored"))
		}
		dir = filepath.Clean(dir)
		if dir == w.DefaultRestoreDir() {
			dir = ""
		} else if w.IsUnder(dir) {
			return errors.New("that folder is inside the outbox, so restored files would be stored again; choose one outside it")
		}
	}
	if dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("can't use that folder: %w", err)
		}
		probe, err := os.CreateTemp(dir, ".yggstore-write-test-*")
		if err != nil {
			return fmt.Errorf("can't write to that folder: %w", err)
		}
		probe.Close()
		os.Remove(probe.Name())
	}
	s := w.readSettings()
	s.RestoreTo = dir
	b, _ := json.MarshalIndent(s, "", "  ")
	if err := os.MkdirAll(filepath.Join(w.cfg.Dir, privateDir), 0o700); err != nil {
		return err
	}
	if err := atomicfile.Replace(w.settingsPath(), append(b, '\n'), 0o600); err != nil {
		return err
	}
	w.cfg.Event("info", "restores now go to "+w.RestoreDir())
	return nil
}

// Restores lists the latest restores, newest first.
func (w *Watcher) Restores() []Restored {
	w.actMu.Lock()
	defer w.actMu.Unlock()
	out := make([]Restored, len(w.restored))
	for i, r := range w.restored {
		out[len(out)-1-i] = r
	}
	return out
}

func (w *Watcher) noteRestore(r Restored) {
	r.At = time.Now().Unix()
	w.actMu.Lock()
	w.restored = append(w.restored, r)
	if len(w.restored) > keepRestored {
		w.restored = w.restored[len(w.restored)-keepRestored:]
	}
	w.actMu.Unlock()
}

// LogPath is where the dashboard keeps its activity log for this outbox.
func (w *Watcher) LogPath() string { return filepath.Join(w.cfg.Dir, privateDir, "activity.log") }
