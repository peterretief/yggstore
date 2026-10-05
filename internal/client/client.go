// Package client talks to other nodes' shard servers.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/peterretief/yggstore/internal/challenge"
	"github.com/peterretief/yggstore/internal/invite"
	"github.com/peterretief/yggstore/internal/manifest"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/server"
)

type Client struct{ HTTP *http.Client }

func New() Client { return Client{HTTP: &http.Client{Timeout: 60 * time.Second}} }

func ShardURL(addr, hash string) string { return "http://" + addr + "/shards/" + hash }

func (c Client) Info(ctx context.Context, addr string) (server.Info, error) {
	var info server.Info
	err := c.doJSON(ctx, http.MethodGet, "http://"+addr+"/v1/info", nil, &info)
	return info, err
}

func (c Client) Put(ctx context.Context, addr string, shard []byte) (string, error) {
	hash := manifest.Hash(shard)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, ShardURL(addr, hash), bytes.NewReader(shard))
	if err != nil {
		return "", err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return "", statusError(resp)
	}
	return hash, nil
}

// Get fetches a shard and rejects it unless its bytes hash to the requested ID.
func (c Client) Get(ctx context.Context, addr, hash string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ShardURL(addr, hash), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, statusError(resp)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, server.DefaultMaxShardBytes+1))
	if err != nil {
		return nil, err
	}
	if manifest.Hash(data) != hash {
		return nil, fmt.Errorf("shard %s from %s failed hash check", hash[:12], addr)
	}
	return data, nil
}

// Has asks a peer whether it holds a shard, without transferring it.
func (c Client) Has(ctx context.Context, addr, hash string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, ShardURL(addr, hash), nil)
	if err != nil {
		return false, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return false, err
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	}
	return false, fmt.Errorf("%s", resp.Status)
}

func (c Client) Delete(ctx context.Context, addr, hash string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, ShardURL(addr, hash), nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		return statusError(resp)
	}
	return nil
}

func (c Client) Challenge(ctx context.Context, addr string, req challenge.Request) (string, error) {
	var resp challenge.Response
	if err := c.doJSON(ctx, http.MethodPost, "http://"+addr+"/v1/challenge", req, &resp); err != nil {
		return "", err
	}
	return resp.Proof, nil
}

func (c Client) doJSON(ctx context.Context, method, url string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return statusError(resp)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

func statusError(resp *http.Response) error {
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(msg))
}

// PushPeers sends a node a new peer list. Only admin nodes may.
func (c Client) PushPeers(ctx context.Context, addr string, list []peers.Peer) error {
	b, err := json.Marshal(list)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://"+addr+"/v1/peers", bytes.NewReader(b))
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return statusError(resp)
	}
	return nil
}

// Join asks an admin node to add this machine, presenting an invite.
func (c Client) Join(ctx context.Context, addr string, req invite.Request) (invite.Response, error) {
	var resp invite.Response
	err := c.doJSON(ctx, http.MethodPost, "http://"+addr+"/v1/join", req, &resp)
	return resp, err
}
