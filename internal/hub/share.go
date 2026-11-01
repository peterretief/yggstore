package hub

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/light"
	"github.com/peterretief/yggstore/internal/peers"
)

// Files shared by link (docs/trial.md): someone on the box shares a file
// from their files page, and anyone with the link downloads it, without
// signing in, through the group's web nodes (my.DOMAIN/s/BOX/TOKEN), until
// ShareFor has passed or they stop sharing it.
const (
	ShareFor  = 30 * 24 * time.Hour
	MaxShares = 20 // per person
	// SharedPath is where the box answers a web node for a link.
	SharedPath = FilesPath + "/shared/"
)

// Share is one file shared by link.
type Share struct {
	Token  string `json:"token"`
	Person string `json:"person"`
	Path   string `json:"path"` // as their files page shows it
	Made   int64  `json:"made"`
}

func (sh Share) live(now time.Time) bool { return now.Sub(time.Unix(sh.Made, 0)) < ShareFor }

// ShareFile gives the link's share for person's file at path: the one
// already made, or a new one.
func (s *HubState) ShareFile(person, path string, now time.Time) (Share, error) {
	s.Shares = slices.DeleteFunc(s.Shares, func(sh Share) bool { return !sh.live(now) })
	n := 0
	for _, sh := range s.Shares {
		if strings.EqualFold(sh.Person, person) {
			if sh.Path == path {
				return sh, nil
			}
			n++
		}
	}
	if n >= MaxShares {
		return Share{}, errors.New("You share 20 files already: stop sharing one first.")
	}
	b := make([]byte, 18)
	rand.Read(b)
	sh := Share{Token: hex.EncodeToString(b), Person: person, Path: path, Made: now.Unix()}
	s.Shares = append(s.Shares, sh)
	return sh, nil
}

// StopSharing ends person's links to path.
func (s *HubState) StopSharing(person, path string) {
	s.Shares = slices.DeleteFunc(s.Shares, func(sh Share) bool { return strings.EqualFold(sh.Person, person) && sh.Path == path })
}

// shareURL is a share's link, or "" when the group has no web address.
func (f *filesPage) shareURL(sh Share) string {
	list, _ := peers.Load(f.u.cfg.PeersPath)
	at := SignInAt(list)
	if at == "" {
		return ""
	}
	return at + light.SharePath + f.boxName() + "/" + sh.Token
}

type shareView struct {
	Path  string `json:"path"`
	URL   string `json:"url"`
	Until int64  `json:"until"`
}

// share answers the files page: GET lists p's links; POST {path} shares
// a file, {path, stop} stops sharing it.
func (f *filesPage) share(w http.ResponseWriter, r *http.Request, p Person) {
	now := time.Now()
	view := func(sh Share) shareView {
		return shareView{Path: sh.Path, URL: f.shareURL(sh), Until: time.Unix(sh.Made, 0).Add(ShareFor).Unix()}
	}
	if r.Method == http.MethodGet {
		st, err := Load(f.u.cfg.StatePath)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		out := []shareView{}
		for _, sh := range st.Shares {
			if strings.EqualFold(sh.Person, p.Name) && sh.live(now) {
				out = append(out, view(sh))
			}
		}
		jsonOK(w, out)
		return
	}
	var req struct {
		Path string `json:"path"`
		Stop bool   `json:"stop"`
	}
	if r.Method != http.MethodPost || json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !req.Stop && !f.live(p).IsFile(req.Path) {
		http.Error(w, "Only a file can be shared, and that isn't one of yours.", http.StatusNotFound)
		return
	}
	var sh Share
	_, err := Update(r.Context(), f.u.cfg.StatePath, func(s *HubState) error {
		if req.Stop {
			s.StopSharing(p.Name, req.Path)
			return nil
		}
		var err error
		sh, err = s.ShareFile(p.Name, req.Path, now)
		return err
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if req.Stop {
		jsonOK(w, map[string]string{})
		return
	}
	v := view(sh)
	if v.URL == "" {
		http.Error(w, "The group has no web address, so its files can't be shared by link.", http.StatusConflict)
		return
	}
	jsonOK(w, v)
}

// shared sends a shared file to whoever has its link, as a download.
func (f *filesPage) shared(w http.ResponseWriter, r *http.Request, token string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "use GET", http.StatusMethodNotAllowed)
		return
	}
	st, err := Load(f.u.cfg.StatePath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	now := time.Now()
	i := slices.IndexFunc(st.Shares, func(sh Share) bool { return token != "" && sh.Token == token && sh.live(now) })
	var p Person
	ok := i >= 0
	if ok {
		p, ok = st.PersonNamed(st.Shares[i].Person)
	}
	if !ok || !p.WebAccess() || p.leaving() {
		http.Error(w, "This link has stopped working: it was more than 30 days old, or the file isn't shared any more.", http.StatusNotFound)
		return
	}
	// Never a page on the group's address: always a download.
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	f.live(p).Download(w, r, st.Shares[i].Path)
}
