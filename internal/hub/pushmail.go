package hub

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	netmail "net/mail"
	"path/filepath"
	"slices"
	"time"

	"github.com/peterretief/yggstore/internal/mail"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/webpush"
)

// New mail notices: someone on the box presses "Tell me about new mail on
// this device" on their mail page, and the browser there (a phone, or a
// computer) gets the browser's own notice when a message reaches their
// mailbox. The box sends it through the browser's push service, encrypted
// for that browser; it says who the mail is from, not what it says.

const (
	PushWorkerPath = FilesPath + "/mail-sw.js"
	ManifestPath   = FilesPath + "/mail.webmanifest"
	maxDevicesPush = 10 // devices a person gets notices on
)

//go:embed mail-sw.js
var pushWorker []byte

var mailManifest = []byte(`{"name":"Your mail","short_name":"Mail","start_url":"/files/mail","scope":"/files/","display":"standalone",` +
	`"background_color":"#ffffff","theme_color":"#0b5cad"}`)

// pushSender is the box's key for notices, made the first time it's wanted.
func (ps *PeopleSites) pushSender() (*webpush.Sender, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.push != nil {
		return ps.push, nil
	}
	contact := "https://yggstore.org"
	if list, err := peers.Load(ps.cfg.PeersPath); err == nil && peers.GroupDomain(list) != "" {
		contact = "https://" + peers.GroupDomain(list)
	}
	s, err := webpush.LoadOrCreate(filepath.Join(filepath.Dir(ps.cfg.StatePath), "push.key"), contact)
	if err != nil {
		return nil, err
	}
	ps.push = s
	return s, nil
}

// servePushFiles answers the notices' worker and the page's manifest
// (on an iPhone, notices need the page on the Home Screen).
func servePushFiles(w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Path {
	case PushWorkerPath:
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(pushWorker)
	case ManifestPath:
		w.Header().Set("Content-Type", "application/manifest+json")
		w.Write(mailManifest)
	default:
		return false
	}
	return true
}

// pushOp is the mail page's notices: the key to subscribe with, turning
// them on for a device, and off.
func (f *filesPage) pushOp(w http.ResponseWriter, r *http.Request, p Person, op string) bool {
	ps := f.u.cfg.People
	switch op {
	case "pushkey":
		s, err := ps.pushSender()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return true
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"key": s.PublicKey()})
		return true
	case "push", "pushoff":
	default:
		return false
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST", http.StatusMethodNotAllowed)
		return true
	}
	var req struct {
		Subscription webpush.Subscription `json:"subscription"`
		Endpoint     string               `json:"endpoint"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return true
	}
	if op == "push" {
		if err := req.Subscription.Check(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return true
		}
		req.Endpoint = req.Subscription.Endpoint
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	_, err := Update(ctx, f.u.cfg.StatePath, func(s *HubState) error {
		i := s.person(p.Name)
		if i < 0 {
			return errors.New("you're no longer on this box")
		}
		subs := slices.DeleteFunc(s.People[i].Push, func(x webpush.Subscription) bool { return x.Endpoint == req.Endpoint })
		if op == "push" {
			subs = append(subs, req.Subscription)
			if len(subs) > maxDevicesPush {
				subs = subs[len(subs)-maxDevicesPush:]
			}
		}
		s.People[i].Push = subs
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return true
	}
	w.WriteHeader(http.StatusNoContent)
	return true
}

// notifyMail tells p's devices that message id is in their mailbox.
func (ps *PeopleSites) notifyMail(p Person, box *mail.Box, id string) {
	if len(p.Push) == 0 {
		return
	}
	s, err := ps.pushSender()
	if err != nil {
		ps.logOnce(p.Name, "new mail notices: %v", err)
		return
	}
	who := "someone"
	if m, err := box.Read(id); err == nil {
		if a, err := netmail.ParseAddress(m.From); err == nil {
			who = a.Address
			if a.Name != "" {
				who = a.Name
			}
		} else if m.From != "" {
			who = m.From
		}
	}
	payload, _ := json.Marshal(map[string]string{"title": "New mail", "body": "From " + who, "url": MailPath})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var gone []string
	for _, sub := range p.Push {
		switch err := s.Send(ctx, sub, payload, 24*time.Hour); {
		case errors.Is(err, webpush.ErrGone):
			gone = append(gone, sub.Endpoint)
		case err != nil:
			ps.cfg.Logf("%s: a new mail notice: %v", p.Name, err)
		}
	}
	if len(gone) > 0 {
		Update(ctx, ps.cfg.StatePath, func(st *HubState) error {
			if i := st.person(p.Name); i >= 0 {
				st.People[i].Push = slices.DeleteFunc(st.People[i].Push, func(x webpush.Subscription) bool { return slices.Contains(gone, x.Endpoint) })
			}
			return nil
		})
	}
}
