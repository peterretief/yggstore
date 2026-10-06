package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/peterretief/yggstore/internal/mail"
)

// The mail section reads the mailbox this machine's node collects into
// (see package mail).

func (d *Dashboard) mailBox() (*mail.Box, error) {
	if d.cfg.MailDir == "" || d.cfg.Identity == nil {
		return nil, errors.New("mail needs a mailbox folder and a sharing key")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.box == nil {
		d.box = &mail.Box{Dir: d.cfg.MailDir, ID: d.cfg.Identity}
	}
	return d.box, nil
}

func (d *Dashboard) handleMailList(w http.ResponseWriter, r *http.Request) {
	out := struct {
		Available bool           `json:"available"`
		Error     string         `json:"error,omitempty"`
		Messages  []mail.Summary `json:"messages"`
	}{Messages: []mail.Summary{}}
	box, err := d.mailBox()
	if err == nil {
		out.Messages, err = box.List()
	}
	if err != nil {
		out.Error = err.Error()
	} else {
		out.Available = true
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSONResp(w, out)
}

func (d *Dashboard) handleMailRead(w http.ResponseWriter, r *http.Request) {
	box, err := d.mailBox()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	m, err := box.Read(r.URL.Query().Get("id"))
	if errors.Is(err, mail.ErrNoMessage) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	} else if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSONResp(w, m)
}

// download sends data as a file to save, never to show: a message's parts
// are written by strangers, and shown on this page they could act as it.
func download(w http.ResponseWriter, name string, data []byte) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Write(data)
}

func (d *Dashboard) handleMailRaw(w http.ResponseWriter, r *http.Request) {
	box, err := d.mailBox()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	id := r.URL.Query().Get("id")
	data, err := box.Raw(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	download(w, id+".eml", data)
}

func (d *Dashboard) handleMailAttachment(w http.ResponseWriter, r *http.Request) {
	box, err := d.mailBox()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	a, data, err := box.Attachment(r.URL.Query().Get("id"), n)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	download(w, a.Name, data)
}

func (d *Dashboard) handleMailDelete(w http.ResponseWriter, r *http.Request) {
	box, err := d.mailBox()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	var req struct{ ID string }
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	if err := box.Delete(ctx, d.cfg.Client, req.ID); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	d.event("info", "deleted a message")
	w.WriteHeader(http.StatusNoContent)
}
