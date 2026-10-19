// Package syncthing talks to a Syncthing instance on this machine through
// its REST API: enough for a hub to accept devices and share folders with
// them. Device and folder settings are sent as Syncthing's own defaults with
// a few fields changed, so settings this package doesn't know about keep
// Syncthing's values.
package syncthing

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Client struct {
	URL    string // e.g. http://127.0.0.1:8384
	APIKey string
	HTTP   *http.Client
}

func New(base, apiKey string) Client {
	hc := &http.Client{Timeout: 15 * time.Second}
	if strings.HasPrefix(base, "https://") {
		// Syncthing's own GUI certificate is self-signed; it is on this machine.
		hc.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	}
	return Client{URL: strings.TrimRight(base, "/"), APIKey: apiKey, HTTP: hc}
}

// PendingDevice is a device that tried to connect and isn't known yet.
type PendingDevice struct {
	Name    string    `json:"name"`
	Address string    `json:"address"` // where it connected from, host:port
	Time    time.Time `json:"time"`
}

// FolderDevice is a device a folder is shared with.
type FolderDevice struct {
	DeviceID string `json:"deviceID"`
}

// Folder is the part of a folder's settings the hub sets.
type Folder struct {
	ID      string
	Label   string
	Path    string
	Devices []string // device IDs, this one included
	Paused  bool
	// TrashDays keeps files deleted or replaced by other devices in
	// .stversions for this many days (0 = forever); -1 = no versioning.
	TrashDays int
}

func (c Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.URL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", c.APIKey)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("syncthing %s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(msg))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// MyID is this Syncthing's device ID.
func (c Client) MyID(ctx context.Context) (string, error) {
	var st struct {
		MyID string `json:"myID"`
	}
	err := c.do(ctx, "GET", "/rest/system/status", nil, &st)
	return st.MyID, err
}

// Pending lists devices that tried to connect, by device ID.
func (c Client) Pending(ctx context.Context) (map[string]PendingDevice, error) {
	out := map[string]PendingDevice{}
	err := c.do(ctx, "GET", "/rest/cluster/pending/devices", nil, &out)
	return out, err
}

// DismissPending forgets a pending device, so it isn't offered again until
// it next connects.
func (c Client) DismissPending(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", "/rest/cluster/pending/devices?device="+url.QueryEscape(id), nil, nil)
}

// AddDevice adds (or replaces) a device, found by its announcements.
func (c Client) AddDevice(ctx context.Context, id, name string) error {
	dev := map[string]any{}
	if err := c.do(ctx, "GET", "/rest/config/defaults/device", nil, &dev); err != nil {
		return err
	}
	dev["deviceID"], dev["name"], dev["addresses"] = id, name, []string{"dynamic"}
	return c.do(ctx, "PUT", "/rest/config/devices/"+url.PathEscape(id), dev, nil)
}

func (c Client) RemoveDevice(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", "/rest/config/devices/"+url.PathEscape(id), nil, nil)
}

// AddFolder adds (or replaces) a folder shared with f.Devices.
func (c Client) AddFolder(ctx context.Context, f Folder) error {
	fo := map[string]any{}
	if err := c.do(ctx, "GET", "/rest/config/defaults/folder", nil, &fo); err != nil {
		return err
	}
	devs := []FolderDevice{}
	for _, d := range f.Devices {
		devs = append(devs, FolderDevice{DeviceID: d})
	}
	fo["id"], fo["label"], fo["path"], fo["type"], fo["devices"], fo["paused"] = f.ID, f.Label, f.Path, "sendreceive", devs, f.Paused
	if f.TrashDays >= 0 {
		fo["versioning"] = map[string]any{"type": "trashcan", "params": map[string]string{"cleanoutDays": fmt.Sprint(f.TrashDays)}}
	}
	return c.do(ctx, "PUT", "/rest/config/folders/"+url.PathEscape(f.ID), fo, nil)
}

func (c Client) RemoveFolder(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", "/rest/config/folders/"+url.PathEscape(id), nil, nil)
}

// FolderIDs lists the folders Syncthing has.
func (c Client) FolderIDs(ctx context.Context) (map[string]bool, error) {
	var list []struct {
		ID string `json:"id"`
	}
	if err := c.do(ctx, "GET", "/rest/config/folders", nil, &list); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, f := range list {
		out[f.ID] = true
	}
	return out, nil
}

// FolderShare is a folder and the devices it is shared with.
type FolderShare struct {
	ID      string         `json:"id"`
	Label   string         `json:"label"`
	Devices []FolderDevice `json:"devices"`
}

// Shared reports whether the folder is shared with a device.
func (f FolderShare) Shared(device string) bool {
	for _, d := range f.Devices {
		if d.DeviceID == device {
			return true
		}
	}
	return false
}

// Folders lists the folders Syncthing has, with whom each is shared.
func (c Client) Folders(ctx context.Context) ([]FolderShare, error) {
	var list []FolderShare
	err := c.do(ctx, "GET", "/rest/config/folders", nil, &list)
	return list, err
}

// PauseFolder pauses or resumes a folder.
func (c Client) PauseFolder(ctx context.Context, id string, paused bool) error {
	return c.do(ctx, "PATCH", "/rest/config/folders/"+url.PathEscape(id), map[string]bool{"paused": paused}, nil)
}

// SetOptions changes some of Syncthing's options, leaving the rest.
func (c Client) SetOptions(ctx context.Context, opts map[string]any) error {
	return c.do(ctx, "PATCH", "/rest/config/options", opts, nil)
}

// ConfigPaths are where Syncthing keeps its config.xml for this user, newest
// layout first.
func ConfigPaths() []string {
	home, _ := os.UserHomeDir()
	paths := []string{}
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		paths = append(paths, filepath.Join(d, "syncthing", "config.xml"))
	}
	paths = append(paths, filepath.Join(home, ".local", "state", "syncthing", "config.xml"))
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		paths = append(paths, filepath.Join(d, "syncthing", "config.xml"))
	}
	return append(paths, filepath.Join(home, ".config", "syncthing", "config.xml"),
		filepath.Join(home, "Library", "Application Support", "Syncthing", "config.xml"))
}

// ReadConfig finds the GUI address and API key in a Syncthing config.xml.
// path "" tries ConfigPaths.
func ReadConfig(path string) (base, apiKey string, err error) {
	paths := []string{path}
	if path == "" {
		paths = ConfigPaths()
	}
	for _, p := range paths {
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			err = rerr
			continue
		}
		var cfg struct {
			GUI struct {
				TLS     bool   `xml:"tls,attr"`
				Address string `xml:"address"`
				APIKey  string `xml:"apikey"`
			} `xml:"gui"`
		}
		if err := xml.Unmarshal(data, &cfg); err != nil {
			return "", "", fmt.Errorf("%s: %w", p, err)
		}
		if cfg.GUI.APIKey == "" {
			return "", "", fmt.Errorf("%s has no API key", p)
		}
		scheme := "http"
		if cfg.GUI.TLS {
			scheme = "https"
		}
		return scheme + "://" + cfg.GUI.Address, cfg.GUI.APIKey, nil
	}
	return "", "", fmt.Errorf("no Syncthing config found (%v)", err)
}
