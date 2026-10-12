package hub

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/files"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/site"
)

// Site is a website kept on the hub: a folder synced with the paired
// devices, published to the group whenever it changes. It is set up on the
// hub beforehand (yggstore hub site add), so the hub's machine is the one
// that publishes it.
type Site struct {
	Name   string `json:"name"` // its domain
	Folder string `json:"folder"`
	Path   string `json:"path"`
	Added  int64  `json:"added"`
	// Seeded: the folder was filled with the version being served (if
	// any), so it is ready to share.
	Seeded bool `json:"seeded,omitempty"`
	// Remove is set by "yggstore hub site remove": the folder is no longer
	// synced or published (the files and the published site stay).
	Remove bool `json:"remove,omitempty"`
	// Force publishes the folder as it is, even if it fails a check.
	Force bool `json:"force,omitempty"`

	Published int64  `json:"published,omitempty"` // when it was last published
	Version   string `json:"version,omitempty"`
	Problem   string `json:"problem,omitempty"` // why the latest change isn't published
}

// SiteFolderID is the Syncthing folder a site syncs in.
func SiteFolderID(name string) string { return "yggstore-site-" + name }

// SyncthingFile is whether a path inside a synced folder is Syncthing's own
// (its marker, ignore file, versions and files being written), which is not
// part of the website.
func SyncthingFile(rel string, dir bool) bool {
	b := path.Base(rel)
	return b == ".stfolder" || b == ".stversions" || b == ".stignore" || temp(b)
}

func temp(base string) bool {
	return strings.HasPrefix(base, ".syncthing.") || strings.HasPrefix(base, "~syncthing~")
}

type SitesConfig struct {
	StatePath string
	Pub       *site.Publisher
	Client    client.Client // fetches the version being served, to fill a new folder
	Online    func(ctx context.Context) ([]peers.Peer, error)
	// MaxBytes is the most a site may hold (0 = no limit).
	MaxBytes int64
	// Quiet is how long a folder must be unchanged before it is published,
	// so a device's sync has finished; Gap the least time between two
	// publishes of a site.
	Quiet, Gap time.Duration
	Logf       func(format string, args ...any)
}

// Sites publishes the hub's websites when their folders change.
type Sites struct {
	cfg   SitesConfig
	last  map[string]scan // as last published, or found when seeded
	seen  map[string]seenScan
	tried map[string]time.Time // last publish, or attempt
}

type seenScan struct {
	s  scan
	at time.Time
}

func NewSites(cfg SitesConfig) *Sites {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Sites{cfg: cfg, last: map[string]scan{}, seen: map[string]seenScan{}, tried: map[string]time.Time{}}
}

func (s *Sites) Run(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		s.step(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Sites) step(ctx context.Context) {
	st, err := Load(s.cfg.StatePath)
	if err != nil {
		s.cfg.Logf("hub: %v", err)
		return
	}
	for _, si := range st.Sites {
		if si.Remove || ctx.Err() != nil {
			continue
		}
		if !si.Seeded {
			s.seed(ctx, si)
			continue
		}
		s.maybePublish(ctx, si)
	}
}

// seed fills a new site's folder with the version being served, so whoever
// edits it starts from the live site. A folder that already has files is
// left as it is.
func (s *Sites) seed(ctx context.Context, si Site) {
	if err := os.MkdirAll(si.Path, 0o755); err != nil {
		s.cfg.Logf("hub: site %s: %v", si.Name, err)
		return
	}
	if sc := scanSite(si.Path); sc.files == 0 {
		if m, err := s.cfg.Pub.Current(si.Name); err != nil {
			s.cfg.Logf("hub: site %s isn't published from this machine yet; it will be once its folder has an index.html", si.Name)
		} else {
			tmp := filepath.Join(filepath.Dir(si.Path), ".seeding-"+si.Name)
			os.RemoveAll(tmp)
			root, err := files.Restore(ctx, s.cfg.Client, m, tmp, nil)
			if err == nil {
				err = moveContents(root, si.Path)
			}
			os.RemoveAll(tmp)
			if err != nil {
				s.cfg.Logf("hub: site %s: fetching the version being served: %v (will try again)", si.Name, err)
				return
			}
			s.cfg.Logf("hub: site %s: its folder holds the version being served (%s)", si.Name, m.FileID[:8])
		}
	}
	s.last[si.Name] = scanSite(si.Path)
	Update(ctx, s.cfg.StatePath, func(st *HubState) error {
		for i := range st.Sites {
			if st.Sites[i].Name == si.Name {
				st.Sites[i].Seeded = true
			}
		}
		return nil
	})
}

func moveContents(from, to string) error {
	ents, err := os.ReadDir(from)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if err := os.Rename(filepath.Join(from, e.Name()), filepath.Join(to, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

func (s *Sites) maybePublish(ctx context.Context, si Site) {
	sc := scanSite(si.Path)
	if last, ok := s.last[si.Name]; ok && last == sc && !si.Force {
		return
	}
	if seen := s.seen[si.Name]; seen.s != sc {
		s.seen[si.Name] = seenScan{sc, time.Now()} // changed: wait for it to settle
		return
	}
	if sc.temp || time.Since(s.seen[si.Name].at) < s.cfg.Quiet {
		return
	}
	problem := ""
	if !si.Force {
		problem = s.check(si.Name, sc)
	}
	if problem != "" {
		if problem != si.Problem {
			s.cfg.Logf("hub: site %s not published: %s", si.Name, problem)
			s.record(ctx, si.Name, func(x *Site) { x.Problem = problem })
		}
		return
	}
	if time.Since(s.tried[si.Name]) < s.cfg.Gap {
		return
	}
	s.tried[si.Name] = time.Now()
	online, err := s.cfg.Online(ctx)
	var v site.Version
	if err == nil {
		v, _, err = s.cfg.Pub.Publish(ctx, si.Name, si.Path, online)
	}
	if err != nil {
		problem = "publishing: " + err.Error()
		if problem != si.Problem {
			s.cfg.Logf("hub: site %s: %s (will try again)", si.Name, problem)
		}
		s.record(ctx, si.Name, func(x *Site) { x.Problem = problem })
		return
	}
	s.last[si.Name] = sc
	s.cfg.Logf("hub: site %s published: version %s, %d files, %d bytes", si.Name, v.ID[:8], v.Files, v.Bytes)
	s.record(ctx, si.Name, func(x *Site) {
		x.Published, x.Version, x.Problem, x.Force = time.Now().Unix(), v.ID[:8], "", false
	})
}

// check is why a folder shouldn't be published as it is: it would break or
// empty the site, most likely by mistake.
func (s *Sites) check(name string, sc scan) string {
	if !sc.index {
		return "it has no index.html (yggstore hub site publish publishes it anyway)"
	}
	if s.cfg.MaxBytes > 0 && sc.bytes > s.cfg.MaxBytes {
		return fmt.Sprintf("it holds %.1f MB, more than the %.1f MB a site may (-site-max)", float64(sc.bytes)/(1<<20), float64(s.cfg.MaxBytes)/(1<<20))
	}
	if vs, _ := s.cfg.Pub.Versions(name); len(vs) > 0 {
		for _, v := range vs {
			if v.Current && v.Bytes > 0 && sc.bytes < v.Bytes/2 {
				return fmt.Sprintf("it shrank from %d to %d bytes since the version being served (yggstore hub site publish publishes it anyway)", v.Bytes, sc.bytes)
			}
		}
	}
	return ""
}

func (s *Sites) record(ctx context.Context, name string, f func(*Site)) {
	Update(ctx, s.cfg.StatePath, func(st *HubState) error {
		for i := range st.Sites {
			if st.Sites[i].Name == name {
				f(&st.Sites[i])
			}
		}
		return nil
	})
}

// scan sums up a site's folder, leaving out Syncthing's files, to see when
// it changes.
type scan struct {
	files  int
	bytes  int64
	newest int64 // newest modification, unix nanoseconds
	index  bool  // has index.html
	temp   bool  // Syncthing is writing a file
}

func scanSite(dir string) scan {
	var sc scan
	filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil || p == dir {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if temp(e.Name()) {
			sc.temp = true
		}
		if SyncthingFile(rel, e.IsDir()) {
			if e.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := e.Info()
		if err != nil {
			return nil
		}
		if t := info.ModTime().UnixNano(); t > sc.newest {
			sc.newest = t
		}
		if info.Mode().IsRegular() {
			sc.files++
			sc.bytes += info.Size()
			if rel == "index.html" {
				sc.index = true
			}
		}
		return nil
	})
	return sc
}
