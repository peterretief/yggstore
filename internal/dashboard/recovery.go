package dashboard

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"

	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/recovery"
)

// RecoveryDir is where the recovery copies' stubs and the recovery key are
// kept, under the yggstore home. Repair looks after the copies there.
func RecoveryDir(home string) string { return filepath.Join(home, "recovery") }

// runRecovery keeps a recovery copy in the group: the outbox's stubs and
// history, the person's things in the yggstore home, and the member list.
func (d *Dashboard) runRecovery(ctx context.Context) {
	if d.cfg.Outbox == nil || d.cfg.Home == "" {
		return
	}
	k := recovery.New(recovery.Config{
		Dir: RecoveryDir(d.cfg.Home),
		Sources: []recovery.Source{
			{Name: "outbox", Path: d.cfg.Outbox.Dir(), Keep: recovery.OutboxKeep},
			{Name: "home", Path: d.cfg.Home, Keep: recovery.HomeKeep},
			{Name: "peers.json", Path: d.cfg.PeersPath},
		},
		Client: d.cfg.Client,
		Online: func(context.Context) ([]peers.Peer, error) { return d.onlinePeers() },
		Event:  d.event,
	})
	d.mu.Lock()
	d.recovery = k
	d.mu.Unlock()
	if err := k.Run(ctx); err != nil {
		d.event("bad", "recovery: "+err.Error())
	}
}

func (d *Dashboard) recoveryStatus() *recovery.Status {
	d.mu.Lock()
	k := d.recovery
	d.mu.Unlock()
	if k == nil {
		return nil
	}
	s := k.Status()
	return &s
}

// handleRecovery turns recovery on or off, and sets where the link goes.
func (d *Dashboard) handleRecovery(w http.ResponseWriter, r *http.Request) {
	var req struct {
		On    bool   `json:"on"`
		Email string `json:"email"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	d.mu.Lock()
	k := d.recovery
	d.mu.Unlock()
	if k == nil {
		http.Error(w, "recovery needs the outbox", http.StatusConflict)
		return
	}
	was := k.Status().On
	if err := k.Set(req.On, req.Email); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	switch {
	case was && !req.On:
		d.event("warn", "recovery turned off: if this machine is lost, your files can't be opened")
	case !was && req.On:
		d.event("info", "recovery turned on")
	}
	w.WriteHeader(http.StatusNoContent)
}
