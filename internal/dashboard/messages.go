package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/msg"
	"github.com/peterretief/yggstore/internal/peers"
)

// The messages section talks to this machine's node through its local
// messaging API (see package msg).

type msgState struct {
	Available bool         `json:"available"`
	Error     string       `json:"error,omitempty"`
	Status    *msg.Status  `json:"status,omitempty"`
	Messages  []msg.Stored `json:"messages"`
	Members   []string     `json:"members"` // nodes you can message
}

func (d *Dashboard) msgClient() (msg.Local, error) {
	if d.cfg.MsgAPI == "" {
		return msg.Local{}, errors.New("messaging is not set up for this dashboard")
	}
	token, err := msg.ReadToken(d.cfg.MsgTokenPath)
	if err != nil {
		return msg.Local{}, err
	}
	return msg.Local{Addr: d.cfg.MsgAPI, Token: token, HTTP: &http.Client{Timeout: 5 * time.Second}}, nil
}

func (d *Dashboard) handleMsgState(w http.ResponseWriter, r *http.Request) {
	out := msgState{Messages: []msg.Stored{}, Members: []string{}}
	if list, err := peers.Load(d.cfg.PeersPath); err == nil {
		for _, p := range list {
			if p.IP() != d.cfg.SelfID {
				out.Members = append(out.Members, p.Name)
			}
		}
	}
	l, err := d.msgClient()
	if err == nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		var st msg.Status
		if st, err = l.Status(ctx); err == nil {
			out.Status = &st
			out.Messages, err = l.Latest(ctx, "", 50)
		}
	}
	if err != nil {
		out.Error = err.Error()
		if strings.Contains(out.Error, "404") || strings.Contains(out.Error, "page not found") {
			out.Error = "this machine's node runs an older yggstore without messaging; update and restart it"
		}
	} else {
		out.Available = true
	}
	writeJSONResp(w, out)
}

func readMsgReq(w http.ResponseWriter, r *http.Request) (struct{ To, Topic, Body string }, bool) {
	var req struct{ To, Topic, Body string }
	if err := json.NewDecoder(io.LimitReader(r.Body, msg.MaxBody*2)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return req, false
	}
	return req, true
}

func (d *Dashboard) handleMsgSend(w http.ResponseWriter, r *http.Request) {
	req, ok := readMsgReq(w, r)
	if !ok {
		return
	}
	if strings.TrimSpace(req.Body) == "" {
		http.Error(w, "write a message first", http.StatusBadRequest)
		return
	}
	l, err := d.msgClient()
	if err == nil {
		if req.Topic != "" {
			_, err = l.Publish(r.Context(), strings.TrimSpace(req.Topic), "text/plain", req.Body)
		} else {
			_, err = l.Send(r.Context(), req.To, "text/plain", req.Body)
		}
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (d *Dashboard) handleMsgSub(w http.ResponseWriter, r *http.Request) {
	req, ok := readMsgReq(w, r)
	if !ok {
		return
	}
	l, err := d.msgClient()
	if err == nil {
		topic := strings.TrimSpace(req.Topic)
		if strings.HasSuffix(r.URL.Path, "/unsubscribe") {
			err = l.Unsubscribe(r.Context(), topic)
		} else {
			err = l.Subscribe(r.Context(), topic)
		}
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
