// Package devices keeps the list of devices that may open a dashboard over
// Yggdrasil. A device is known by its Yggdrasil address, which only the
// holder of its private key can use, so no password is needed. The list is
// a file on the dashboard's own machine: the group's admins can't add to it.
package devices

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/peterretief/yggstore/internal/atomicfile"
)

// Device is one device allowed in.
type Device struct {
	Addr  string    `json:"addr"`
	Name  string    `json:"name,omitempty"`
	Added time.Time `json:"added"`
	// LastSeen is when it last opened the page, where that is kept (a box
	// keeps it for its people's devices).
	LastSeen time.Time `json:"last_seen,omitzero"`
	// Pages are the dashboard's pages it may open (see Pages); none listed
	// means all of them, as for devices added before there were pages.
	Pages []string `json:"pages,omitempty"`
}

// Pages are the dashboard's pages, each let in on its own.
var Pages = []string{"files", "web", "messages", "mail", "admin"}

// Can reports whether the device may open page.
func (d Device) Can(page string) bool {
	return len(d.Pages) == 0 || slices.Contains(d.Pages, page)
}

// Load reads the list at path; a missing file is an empty list.
func Load(path string) ([]Device, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var list []Device
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return list, nil
}

func save(path string, list []Device) error {
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	owner := ownerOf(path)
	if err := atomicfile.Replace(path, append(b, '\n'), 0o600); err != nil {
		return err
	}
	// Run as root (say over SSH), keep the file the dashboard's user's, or
	// it could no longer read it and would let nobody in.
	owner.restore(path)
	return nil
}

// Add allows addr, a Yggdrasil address, under name. Adding it again renames
// it.
func Add(path, addr, name string) (Device, error) {
	ip, err := Parse(addr)
	if err != nil {
		return Device{}, err
	}
	list, err := Load(path)
	if err != nil {
		return Device{}, err
	}
	d := Device{Addr: ip, Name: name, Added: time.Now().UTC().Truncate(time.Second)}
	for i := range list {
		if list[i].Addr == ip {
			list[i].Name = name
			return list[i], save(path, list)
		}
	}
	return d, save(path, append(list, d))
}

// SetPages sets the pages the device with this address or name may open.
func SetPages(path, key string, pages []string) (Device, error) {
	var keep []string
	for _, p := range Pages { // in the usual order, each once
		if slices.Contains(pages, p) {
			keep = append(keep, p)
		}
	}
	for _, p := range pages {
		if !slices.Contains(Pages, p) {
			return Device{}, fmt.Errorf("no page %q: the pages are %s", p, strings.Join(Pages, ", "))
		}
	}
	if len(keep) == 0 {
		return Device{}, errors.New("give it at least one page, or remove the device")
	}
	list, err := Load(path)
	if err != nil {
		return Device{}, err
	}
	ip, _ := Parse(key)
	for i, d := range list {
		if d.Addr == ip || (d.Name != "" && d.Name == key) {
			list[i].Pages = keep
			return list[i], save(path, list)
		}
	}
	return Device{}, fmt.Errorf("no device %q in %s", key, path)
}

// Remove stops allowing the device with this address or name.
func Remove(path, key string) (Device, error) {
	list, err := Load(path)
	if err != nil {
		return Device{}, err
	}
	ip, _ := Parse(key)
	for i, d := range list {
		if d.Addr == ip || (d.Name != "" && d.Name == key) {
			return d, save(path, append(list[:i], list[i+1:]...))
		}
	}
	return Device{}, fmt.Errorf("no device %q in %s", key, path)
}

// Parse checks addr is a Yggdrasil address (200::/7, which holds the
// 300::/8 subnets too) and returns it in its usual form.
func Parse(addr string) (string, error) {
	ip := net.ParseIP(strings.Trim(addr, "[]"))
	if ip == nil || ip.To4() != nil || ip[0]&0xfe != 0x02 {
		return "", fmt.Errorf("%q is not a Yggdrasil address (they start with 2 or 3, like 200:1f12::1)", addr)
	}
	return ip.String(), nil
}

// Watch answers whether a device is allowed, rereading the file when it
// changes, so `yggstore devices add` takes effect at once.
type Watch struct {
	path string

	mu    sync.Mutex
	mtime time.Time
	size  int64
	addrs map[string]Device
}

func NewWatch(path string) *Watch { return &Watch{path: path} }

// Path is the file the list is kept in.
func (w *Watch) Path() string { return w.path }

// Allowed reports whether ip may open the dashboard, any page of it. If
// the file can't be read, nobody is let in.
func (w *Watch) Allowed(ip string) bool {
	_, ok := w.Device(ip)
	return ok
}

// Can reports whether ip may open page.
func (w *Watch) Can(ip, page string) bool {
	d, ok := w.Device(ip)
	return ok && d.Can(page)
}

// Device is the allowed device at ip, if it is one.
func (w *Watch) Device(ip string) (Device, bool) {
	norm, err := Parse(ip)
	if err != nil {
		return Device{}, false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	info, err := os.Stat(w.path)
	if err != nil {
		w.addrs, w.mtime, w.size = nil, time.Time{}, 0
		return Device{}, false
	}
	if w.addrs == nil || !info.ModTime().Equal(w.mtime) || info.Size() != w.size {
		list, err := Load(w.path)
		if err != nil {
			w.addrs = nil
			return Device{}, false
		}
		w.addrs = map[string]Device{}
		for _, d := range list {
			w.addrs[d.Addr] = d
		}
		w.mtime, w.size = info.ModTime(), info.Size()
	}
	d, ok := w.addrs[norm]
	return d, ok
}
