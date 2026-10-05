// Package gateway lets people who don't run a storage box use the group's
// storage for a monthly fee. It speaks the S3 protocol, so customers use
// tools they already have (rclone, Cyberduck, backup programs), and it
// stores their files in the group like any other upload, on the nodes whose
// owners opted in. It meters what each customer stores and downloads, and
// what each member holds for customers, for billing and credit.
package gateway

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Customer is one paying account.
type Customer struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Email      string `json:"email,omitempty"`
	Plan       string `json:"plan,omitempty"` // free text, e.g. "100 GB, R50/month"
	QuotaBytes int64  `json:"quota_bytes"`
	AccessKey  string `json:"access_key"`
	Secret     string `json:"secret"`
	Suspended  bool   `json:"suspended,omitempty"`
	TrialEnds  int64  `json:"trial_ends,omitempty"` // unix seconds; 0 = not a trial
	Closed     int64  `json:"closed,omitempty"`     // when it was closed: its files are deleted
	Created    int64  `json:"created"`
	Note       string `json:"note,omitempty"`
}

// Customers is the accounts file. The gateway only reads it, reloading it
// when it changes; the admin commands write it.
type Customers struct {
	path  string
	mu    sync.Mutex
	mtime time.Time
	list  []Customer
}

func OpenCustomers(path string) *Customers { return &Customers{path: path} }

func (c *Customers) load() error {
	info, err := os.Stat(c.path)
	if errors.Is(err, os.ErrNotExist) {
		c.list, c.mtime = nil, time.Time{}
		return nil
	}
	if err != nil {
		return err
	}
	if info.ModTime().Equal(c.mtime) && c.list != nil {
		return nil
	}
	data, err := os.ReadFile(c.path)
	if err != nil {
		return err
	}
	var list []Customer
	if err := json.Unmarshal(data, &list); err != nil {
		return fmt.Errorf("%s: %w", c.path, err)
	}
	c.list, c.mtime = list, info.ModTime()
	return nil
}

// ByAccessKey finds the customer with that access key.
func (c *Customers) ByAccessKey(key string) (Customer, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.load(); err != nil {
		return Customer{}, false
	}
	for _, cu := range c.list {
		if cu.AccessKey == key {
			return cu, true
		}
	}
	return Customer{}, false
}

// List returns every customer.
func (c *Customers) List() ([]Customer, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.load(); err != nil {
		return nil, err
	}
	return append([]Customer(nil), c.list...), nil
}

func (c *Customers) save(list []Customer) error {
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFile(c.path, append(b, '\n')); err != nil {
		return err
	}
	c.list, c.mtime = nil, time.Time{}
	return nil
}

// Add creates an account with fresh keys.
func (c *Customers) Add(name, email, plan string, quotaBytes int64) (Customer, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Customer{}, errors.New("give the customer a name")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.load(); err != nil {
		return Customer{}, err
	}
	cu := Customer{ID: randomString(idChars, 8), Name: name, Email: strings.TrimSpace(email), Plan: plan,
		QuotaBytes: quotaBytes, AccessKey: "YGG" + randomString(keyChars, 17),
		Secret: randomString(secretChars, 40), Created: time.Now().Unix()}
	return cu, c.save(append(c.list, cu))
}

// ErrNoCustomer means no account matched.
var ErrNoCustomer = errors.New("no such customer")

// Find returns the customer whose ID, access key or exact name is who.
func (c *Customers) Find(who string) (Customer, error) {
	list, err := c.List()
	if err != nil {
		return Customer{}, err
	}
	i, err := match(list, who)
	if err != nil {
		return Customer{}, err
	}
	return list[i], nil
}

func match(list []Customer, who string) (int, error) {
	found := -1
	for i, cu := range list {
		if cu.ID == who || cu.AccessKey == who || cu.Name == who {
			if found >= 0 {
				return -1, fmt.Errorf("%q matches more than one customer; use the ID", who)
			}
			found = i
		}
	}
	if found < 0 {
		return -1, ErrNoCustomer
	}
	return found, nil
}

// Update changes the customer whose ID, access key or exact name is who.
func (c *Customers) Update(who string, change func(*Customer)) (Customer, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.load(); err != nil {
		return Customer{}, err
	}
	list := append([]Customer(nil), c.list...)
	found, err := match(list, who)
	if err != nil {
		return Customer{}, err
	}
	change(&list[found])
	return list[found], c.save(list)
}

// writeFile replaces path atomically, private to the user.
func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(path, append(b, '\n'))
}

const (
	idChars     = "abcdefghijklmnopqrstuvwxyz0123456789"
	keyChars    = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	secretChars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
)

func randomString(chars string, n int) string {
	out := make([]byte, n)
	buf := make([]byte, 1)
	for i := 0; i < n; {
		rand.Read(buf)
		if int(buf[0]) >= 256-256%len(chars) { // no modulo bias
			continue
		}
		out[i] = chars[int(buf[0])%len(chars)]
		i++
	}
	return string(out)
}

// WithNewSecret returns c with a fresh secret key.
func WithNewSecret(c Customer) Customer {
	c.Secret = randomString(secretChars, 40)
	return c
}
