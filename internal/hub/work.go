package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/files"
	"github.com/peterretief/yggstore/internal/manifest"
)

// WorkRequest asks the box to be where a person works: to unpack their
// latest recovery copy into a folder of theirs and pair their device with
// it, from wherever the device is. A web node sends it when the person
// opens the link in their recovery email.
type WorkRequest struct {
	ID     string          `json:"id"`   // the person's recovery ID
	Name   string          `json:"name"` // what to call them
	Stub   json.RawMessage `json:"stub"` // their latest recovery copy
	Device string          `json:"device"`
	// Move takes the folder from the devices it is synced with now.
	Move bool `json:"move,omitempty"`
}

// WorkResponse says how to connect, or that the folder is in use.
type WorkResponse struct {
	BoxID   string `json:"box_id,omitempty"` // the box's Syncthing ID
	BoxName string `json:"box_name,omitempty"`
	Folder  string `json:"folder,omitempty"` // as Syncthing will offer it
	// InUse names the devices syncing the folder now, when Move wasn't set.
	InUse []string `json:"in_use,omitempty"`
	Error string   `json:"error,omitempty"`
}

var deviceID = regexp.MustCompile(`^[A-Z2-7]{7}(-[A-Z2-7]{7}){7}$`)

// ErrInUse means the person's folder is synced with other devices.
var ErrInUse = errors.New("in use on other devices")

// WorkHere does what a WorkRequest asks.
func (u *Users) WorkHere(ctx context.Context, req WorkRequest) (WorkResponse, error) {
	req.Device = strings.ToUpper(strings.TrimSpace(req.Device))
	if !deviceID.MatchString(req.Device) {
		return WorkResponse{}, errors.New("that isn't a Syncthing device ID: in Syncthing, Actions > Show ID")
	}
	if u.cfg.Client.HTTP == nil {
		return WorkResponse{}, errors.New("this box can't fetch from the group")
	}
	m, err := manifest.Unmarshal(req.Stub)
	if err != nil {
		return WorkResponse{}, fmt.Errorf("the recovery copy's stub: %w", err)
	}
	st, err := Load(u.cfg.StatePath)
	if err != nil {
		return WorkResponse{}, err
	}
	var person Person
	found := false
	for _, p := range st.People {
		if p.RecoveryID != "" && p.RecoveryID == req.ID {
			person, found = p, true
		}
	}
	if found {
		var others []string
		for _, d := range st.DevicesOf(person.Name) {
			if d.Device != req.Device {
				others = append(others, d.Name)
			}
		}
		if len(others) > 0 && !req.Move {
			return WorkResponse{InUse: others}, ErrInUse
		}
	}

	// Unpack the copy before anything changes, so a copy that can't be
	// read changes nothing.
	tmp, err := os.MkdirTemp(u.cfg.Dir, ".work-")
	if err != nil {
		if err := os.MkdirAll(u.cfg.Dir, 0o700); err != nil {
			return WorkResponse{}, err
		}
		if tmp, err = os.MkdirTemp(u.cfg.Dir, ".work-"); err != nil {
			return WorkResponse{}, err
		}
	}
	defer os.RemoveAll(tmp)
	got, err := files.Restore(ctx, u.cfg.Client, m, tmp, nil)
	if err != nil {
		return WorkResponse{}, fmt.Errorf("fetching the recovery copy: %w", err)
	}

	short := shortHash(req.ID)
	if !found {
		if _, err := Update(ctx, u.cfg.StatePath, func(s *HubState) error {
			name := uniqueName(s.Users, s.People, u.cfg.Dir, req.Name, short)
			person = Person{Name: name, Folder: "yggstore-p-" + short, Path: filepath.Join(u.cfg.Dir, name), Added: time.Now().Unix(), RecoveryID: req.ID}
			s.People = append(s.People, person)
			return nil
		}); err != nil {
			return WorkResponse{}, err
		}
	}
	u.stopWatch(person.Folder) // nothing stored from the folder while it is replaced
	if err := unpack(filepath.Join(got, "outbox"), filepath.Join(got, "home"), person.Path); err != nil {
		return WorkResponse{}, fmt.Errorf("unpacking the copy: %w", err)
	}

	if _, err := Update(ctx, u.cfg.StatePath, func(s *HubState) error {
		for i := range s.Users {
			if s.Users[i].Person == person.Name && s.Users[i].Device != req.Device {
				s.Users[i].Remove = true // Move: the old devices stop syncing first
			}
		}
		for i := range s.Users {
			if s.Users[i].Device == req.Device {
				s.Users[i].Person, s.Users[i].Remove = person.Name, false
				return nil
			}
		}
		s.Users = append(s.Users, User{Name: person.Name + "-" + strings.ToLower(req.Device[:7]), Device: req.Device,
			Folder: person.Folder, Path: person.Path, Added: time.Now().Unix(), Address: "the recovery link", Person: person.Name})
		return nil
	}); err != nil {
		return WorkResponse{}, err
	}
	u.cfg.Logf("hub: %s works here now, from device %s; their latest copy is in %s", person.Name, short7(req.Device), person.Path)
	host, _ := os.Hostname()
	u.mu.Lock()
	id := u.myID
	u.mu.Unlock()
	return WorkResponse{BoxID: id, BoxName: host, Folder: "yggstore (" + host + ")"}, nil
}

func shortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:4])
}

func short7(id string) string {
	if len(id) >= 7 {
		return id[:7]
	}
	return id
}

// unpack puts a recovery copy's outbox into a person's folder, replacing
// the stubs and records there, and their own things (sharing key, mail…)
// into its .yggstore/home, which Syncthing doesn't sync.
func unpack(outbox, home, dest string) error {
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return err
	}
	for _, sub := range []string{"files", "whole", "received", ".yggstore"} {
		from, to := filepath.Join(outbox, sub), filepath.Join(dest, sub)
		if _, err := os.Stat(from); os.IsNotExist(err) {
			continue
		}
		if err := os.RemoveAll(to); err != nil {
			return err
		}
		if err := os.Rename(from, to); err != nil {
			return err
		}
	}
	if _, err := os.Stat(home); err == nil {
		to := filepath.Join(dest, ".yggstore", "home")
		os.RemoveAll(to)
		if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
			return err
		}
		if err := os.Rename(home, to); err != nil {
			return err
		}
	}
	return nil
}

// WorkHandler answers WorkRequests from the group's members (the web
// nodes), over Yggdrasil: POST /v1/work.
func (u *Users) WorkHandler(member func(ip string) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if r.Method != http.MethodPost || member == nil || !member(host) {
			http.Error(w, "only the group's nodes ask this", http.StatusForbidden)
			return
		}
		var req WorkRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()
		resp, err := u.WorkHere(ctx, req)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case errors.Is(err, ErrInUse):
			w.WriteHeader(http.StatusConflict)
		case err != nil:
			resp.Error = err.Error()
			w.WriteHeader(http.StatusBadRequest)
		}
		json.NewEncoder(w).Encode(resp)
	})
}
