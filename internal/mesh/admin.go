// Package mesh keeps this machine's Yggdrasil linked directly to the other
// members' machines. Members list where they can be reached (ygg_listen in
// the peer list); every minute the node asks its own Yggdrasil which links
// are up and opens the missing ones. So the group stays connected when one
// path (a tunnel, a VPN, a public peer) goes away, and traffic between
// members takes the shortest way.
package mesh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// Admin talks to Yggdrasil's admin socket.
type Admin struct {
	// Endpoint is "unix:///path/to/yggdrasil.sock" or "tcp://127.0.0.1:9001".
	Endpoint string
}

// DefaultEndpoints are where Yggdrasil's packages put the admin socket.
var DefaultEndpoints = []string{
	"unix:///var/run/yggdrasil/yggdrasil.sock",
	"unix:///var/run/yggdrasil.sock",
	"tcp://localhost:9001",
}

// Find returns the admin endpoint to use: endpoint itself, or with "auto"
// the first default that exists.
func Find(endpoint string) Admin {
	if endpoint != "auto" {
		return Admin{Endpoint: endpoint}
	}
	for _, e := range DefaultEndpoints {
		if path, ok := strings.CutPrefix(e, "unix://"); ok {
			if _, err := os.Stat(path); err == nil {
				return Admin{Endpoint: e}
			}
		}
	}
	return Admin{Endpoint: DefaultEndpoints[len(DefaultEndpoints)-1]}
}

// ErrNoAccess means the socket exists but this user may not use it.
var ErrNoAccess = errors.New("no access to Yggdrasil's admin socket")

func (a Admin) call(ctx context.Context, request string, args, out any) error {
	network, addr := "tcp", strings.TrimPrefix(a.Endpoint, "tcp://")
	if path, ok := strings.CutPrefix(a.Endpoint, "unix://"); ok {
		network, addr = "unix", path
	}
	var d net.Dialer
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := d.DialContext(dctx, network, addr)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("%w %s", ErrNoAccess, addr)
		}
		return fmt.Errorf("Yggdrasil's admin socket (%s): %w", a.Endpoint, err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	req := map[string]any{"request": request}
	if args != nil {
		req["arguments"] = args
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return err
	}
	var resp struct {
		Status   string          `json:"status"`
		Error    string          `json:"error"`
		Response json.RawMessage `json:"response"`
	}
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return fmt.Errorf("Yggdrasil's admin socket: %w", err)
	}
	if resp.Status != "success" {
		return errors.New(resp.Error)
	}
	if out != nil {
		return json.Unmarshal(resp.Response, out)
	}
	return nil
}

// Self is this machine's Yggdrasil identity.
type Self struct {
	Key     string `json:"key"`
	Address string `json:"address"`
}

func (a Admin) Self(ctx context.Context) (Self, error) {
	var s Self
	err := a.call(ctx, "getself", nil, &s)
	return s, err
}

// Link is one of Yggdrasil's peerings, up or still trying.
type Link struct {
	URI       string `json:"remote"`
	Up        bool   `json:"up"`
	Inbound   bool   `json:"inbound"`
	Address   string `json:"address"`
	Key       string `json:"key"`
	LastError string `json:"last_error"`
}

func (a Admin) Links(ctx context.Context) ([]Link, error) {
	var r struct {
		Peers []Link `json:"peers"`
	}
	err := a.call(ctx, "getpeers", nil, &r)
	return r.Peers, err
}

// errAlreadyConfigured is Yggdrasil's answer when the peering exists.
const errAlreadyConfigured = "peer is already configured"

// AddLink peers with uri; added is false if Yggdrasil already had it.
func (a Admin) AddLink(ctx context.Context, uri string) (added bool, err error) {
	err = a.call(ctx, "addpeer", map[string]string{"uri": uri}, nil)
	if err != nil && strings.Contains(err.Error(), errAlreadyConfigured) {
		return false, nil
	}
	return err == nil, err
}

func (a Admin) RemoveLink(ctx context.Context, uri string) error {
	err := a.call(ctx, "removepeer", map[string]string{"uri": uri}, nil)
	if err != nil && strings.Contains(err.Error(), "not configured") {
		return nil
	}
	return err
}
