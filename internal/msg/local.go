package msg

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The local API lets programs on this machine (the yggstore command, the
// dashboard, anything people build) use the node's messaging. It listens on
// loopback only and needs the token in the node's token file, which only
// this user can read.
//
//	GET  /v1/status
//	GET  /v1/messages?after=N&topic=T&limit=L   (or latest=L for the last L)
//	GET  /v1/stream?after=N&topic=T             server-sent events, one per message
//	POST /v1/send       {"to": NODE, "type": T, "body": TEXT}
//	POST /v1/publish    {"topic": T, "type": T, "body": TEXT}
//	POST /v1/subscribe  {"topic": T}
//	POST /v1/unsubscribe {"topic": T}
//
// Requests carry "Authorization: Bearer TOKEN".

// LoadOrCreateToken reads the local API token, creating it if missing.
func LoadOrCreateToken(path string) (string, error) {
	if data, err := os.ReadFile(path); err == nil {
		if t := strings.TrimSpace(string(data)); len(t) >= 32 {
			return t, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	b := make([]byte, 24)
	rand.Read(b)
	t := hex.EncodeToString(b)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	return t, os.WriteFile(path, []byte(t+"\n"), 0o600)
}

// ReadToken reads the local API token.
func ReadToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading the messaging token: %w (is the node running on this machine?)", err)
	}
	return strings.TrimSpace(string(data)), nil
}

func loopbackHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type sendReq struct {
	To    string `json:"to,omitempty"`
	Topic string `json:"topic,omitempty"`
	Type  string `json:"type,omitempty"`
	Body  string `json:"body"`
}

// LocalHandler serves the local API.
func (e *Engine) LocalHandler(token string) http.Handler {
	mux := http.NewServeMux()
	jsonOut := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}
	readReq := func(w http.ResponseWriter, r *http.Request) (sendReq, bool) {
		var req sendReq
		if err := json.NewDecoder(io.LimitReader(r.Body, MaxBody*2+4096)).Decode(&req); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return req, false
		}
		return req, true
	}
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) { jsonOut(w, e.Status()) })
	mux.HandleFunc("GET /v1/messages", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if n, err := strconv.Atoi(q.Get("latest")); err == nil && n > 0 {
			jsonOut(w, orEmpty(e.Latest(q.Get("topic"), min(n, 1000))))
			return
		}
		after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
		limit, _ := strconv.Atoi(q.Get("limit"))
		jsonOut(w, orEmpty(e.Messages(after, q.Get("topic"), limit)))
	})
	mux.HandleFunc("GET /v1/stream", func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		q := r.URL.Query()
		after, err := strconv.ParseInt(q.Get("after"), 10, 64)
		if err != nil {
			after = e.Status().Last // from now on
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		fl.Flush()
		for {
			ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
			got := e.Wait(ctx, after, q.Get("topic"))
			cancel()
			if r.Context().Err() != nil {
				return
			}
			if len(got) == 0 {
				fmt.Fprint(w, ": still here\n\n") // keeps proxies from closing it
			}
			for _, s := range got {
				b, _ := json.Marshal(s)
				fmt.Fprintf(w, "id: %d\nevent: message\ndata: %s\n\n", s.N, b)
				after = s.N
			}
			fl.Flush()
		}
	})
	mux.HandleFunc("POST /v1/send", func(w http.ResponseWriter, r *http.Request) {
		req, ok := readReq(w, r)
		if !ok {
			return
		}
		m, err := e.Send(req.To, req.Type, req.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		jsonOut(w, m)
	})
	mux.HandleFunc("POST /v1/publish", func(w http.ResponseWriter, r *http.Request) {
		req, ok := readReq(w, r)
		if !ok {
			return
		}
		m, err := e.Publish(req.Topic, req.Type, req.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		jsonOut(w, m)
	})
	mux.HandleFunc("POST /v1/subscribe", func(w http.ResponseWriter, r *http.Request) {
		req, ok := readReq(w, r)
		if !ok {
			return
		}
		if err := e.Subscribe(req.Topic); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /v1/unsubscribe", func(w http.ResponseWriter, r *http.Request) {
		req, ok := readReq(w, r)
		if !ok {
			return
		}
		e.Unsubscribe(req.Topic)
		w.WriteHeader(http.StatusNoContent)
	})
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Loopback names only, so a web page can't reach this through DNS
		// rebinding; and the token, which web pages can't send.
		if !loopbackHost(r.Host) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			http.Error(w, "the messaging token is missing or wrong", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func orEmpty(s []Stored) []Stored {
	if s == nil {
		return []Stored{}
	}
	return s
}

// Local is a client for the local API.
type Local struct {
	Addr  string // e.g. 127.0.0.1:7401
	Token string
	HTTP  *http.Client
}

func (l Local) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+l.Addr+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+l.Token)
	c := l.HTTP
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("can't reach this machine's node for messaging (%s): %w", l.Addr, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return errors.New(strings.TrimSpace(string(msg)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (l Local) Status(ctx context.Context) (Status, error) {
	var s Status
	return s, l.do(ctx, http.MethodGet, "/v1/status", nil, &s)
}

func (l Local) Latest(ctx context.Context, topic string, n int) ([]Stored, error) {
	var out []Stored
	q := url.Values{"latest": {strconv.Itoa(n)}, "topic": {topic}}
	return out, l.do(ctx, http.MethodGet, "/v1/messages?"+q.Encode(), nil, &out)
}

// Messages returns up to limit messages received after number after, oldest
// first.
func (l Local) Messages(ctx context.Context, after int64, topic string, limit int) ([]Stored, error) {
	var out []Stored
	q := url.Values{"after": {strconv.FormatInt(after, 10)}, "topic": {topic}, "limit": {strconv.Itoa(limit)}}
	return out, l.do(ctx, http.MethodGet, "/v1/messages?"+q.Encode(), nil, &out)
}

func (l Local) Send(ctx context.Context, to, typ, body string) (Message, error) {
	var m Message
	return m, l.do(ctx, http.MethodPost, "/v1/send", sendReq{To: to, Type: typ, Body: body}, &m)
}

func (l Local) Publish(ctx context.Context, topic, typ, body string) (Message, error) {
	var m Message
	return m, l.do(ctx, http.MethodPost, "/v1/publish", sendReq{Topic: topic, Type: typ, Body: body}, &m)
}

func (l Local) Subscribe(ctx context.Context, topic string) error {
	return l.do(ctx, http.MethodPost, "/v1/subscribe", sendReq{Topic: topic}, nil)
}

func (l Local) Unsubscribe(ctx context.Context, topic string) error {
	return l.do(ctx, http.MethodPost, "/v1/unsubscribe", sendReq{Topic: topic}, nil)
}

// Stream calls f for each message after n (n < 0: only new ones) until ctx
// ends or f returns an error.
func (l Local) Stream(ctx context.Context, after int64, topic string, f func(Stored) error) error {
	q := url.Values{"topic": {topic}}
	if after >= 0 {
		q.Set("after", strconv.FormatInt(after, 10))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+l.Addr+"/v1/stream?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+l.Token)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return fmt.Errorf("can't reach this machine's node for messaging (%s): %w", l.Addr, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return errors.New(strings.TrimSpace(string(msg)))
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 1<<20), 4<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var s Stored
		if err := json.Unmarshal([]byte(data), &s); err != nil {
			return err
		}
		if err := f(s); err != nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	return sc.Err()
}
