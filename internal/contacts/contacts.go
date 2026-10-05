// Package contacts is the list of people you share with
// (~/.yggstore/contacts.json).
package contacts

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"

	"github.com/peterretief/yggstore/internal/share"
)

// Contact is someone you can share with.
type Contact struct {
	Name string `json:"name"`
	Code string `json:"code"`
}

// One process may write from several goroutines; files are replaced atomically.
var mu sync.Mutex

// Load reads the list; a missing file is an empty list.
func Load(path string) []Contact {
	out := []Contact{}
	if data, err := os.ReadFile(path); err == nil {
		json.Unmarshal(data, &out)
	}
	return out
}

func save(path string, list []Contact) error {
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ErrExists means a contact with that name or code is already listed.
var ErrExists = errors.New("already in your contacts")

// Add appends c after checking its code, unless own (your code) is c's.
func Add(path string, c Contact, own string) error {
	c.Name, c.Code = strings.TrimSpace(c.Name), strings.TrimSpace(c.Code)
	if c.Name == "" {
		return errors.New("give the contact a name")
	}
	if _, err := share.ParseCode(c.Code); err != nil {
		return err
	}
	if c.Code == own {
		return errors.New("that is your own sharing code")
	}
	mu.Lock()
	defer mu.Unlock()
	list := Load(path)
	for _, o := range list {
		if strings.EqualFold(o.Name, c.Name) || o.Code == c.Code {
			return ErrExists
		}
	}
	return save(path, append(list, c))
}

// Remove drops the contact with that code.
func Remove(path, code string) error {
	mu.Lock()
	defer mu.Unlock()
	keep := []Contact{}
	for _, c := range Load(path) {
		if c.Code != code {
			keep = append(keep, c)
		}
	}
	return save(path, keep)
}
