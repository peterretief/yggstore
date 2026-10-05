package msg

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Client is the Network over HTTP between nodes, on their storage port.
type Client struct{ HTTP *http.Client }

func NewClient() Client { return Client{HTTP: &http.Client{Timeout: 30 * time.Second}} }

func (c Client) do(ctx context.Context, method, u string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ErrNoMessaging
	}
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(out)
	}
	return nil
}

func (c Client) Deliver(ctx context.Context, addr string, m Message) error {
	return c.do(ctx, http.MethodPost, "http://"+addr+"/v1/msg", m, nil)
}

func (c Client) Announce(ctx context.Context, addr string, topics []string) error {
	if topics == nil {
		topics = []string{}
	}
	return c.do(ctx, http.MethodPut, "http://"+addr+"/v1/msg/subscriptions", map[string][]string{"topics": topics}, nil)
}

func (c Client) History(ctx context.Context, addr, topic string, after uint64) ([]Message, error) {
	var out []Message
	q := url.Values{"topic": {topic}, "after": {strconv.FormatUint(after, 10)}}
	err := c.do(ctx, http.MethodGet, "http://"+addr+"/v1/msg/history?"+q.Encode(), nil, &out)
	return out, err
}

// Serve answers other members' messaging calls. caller is the member's
// node ID, already checked by the node server. It reports false if the
// request isn't a messaging call.
func (e *Engine) Serve(w http.ResponseWriter, r *http.Request, caller string) bool {
	switch {
	case r.URL.Path == "/v1/msg" && r.Method == http.MethodPost:
		var m Message
		if err := json.NewDecoder(io.LimitReader(r.Body, MaxBody*2+4096)).Decode(&m); err != nil {
			http.Error(w, "bad message", http.StatusBadRequest)
			return true
		}
		if err := e.Receive(caller, m); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return true
		}
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Path == "/v1/msg/subscriptions" && r.Method == http.MethodPut:
		var req struct {
			Topics []string `json:"topics"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 128<<10)).Decode(&req); err != nil {
			http.Error(w, "bad subscriptions", http.StatusBadRequest)
			return true
		}
		if err := e.Announced(caller, req.Topics); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return true
		}
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Path == "/v1/msg/history" && r.Method == http.MethodGet:
		after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
		msgs, err := e.History(r.URL.Query().Get("topic"), after)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return true
		}
		if msgs == nil {
			msgs = []Message{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(msgs)
	default:
		return false
	}
	return true
}
