package site

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/atomicfile"
	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/files"
	"github.com/peterretief/yggstore/internal/manifest"
	"github.com/peterretief/yggstore/internal/peers"
)

// Announcer sends a message to the topic; the node's messaging does it.
type Announcer interface {
	Publish(ctx context.Context, topic, typ, body string) error
}

// Publisher keeps the sites published from this machine: every version's
// stub, under Dir/NAME/.
type Publisher struct {
	Dir      string
	Client   client.Client
	Announce Announcer
	Log      func(string, ...any)
}

// Version is one stored version of a site.
type Version struct {
	ID       string `json:"id"` // the item's FileID
	StoredAt int64  `json:"stored_at"`
	Bytes    int64  `json:"bytes"`
	Files    int    `json:"files"`
	Current  bool   `json:"current"`
	stub     string
	written  time.Time // stub's mtime, to order versions stored in the same second
}

type state struct {
	Current string `json:"current"`           // FileID being served
	Contact string `json:"contact,omitempty"` // sharing code the contact form's messages go to; "" if off
}

func (p *Publisher) siteDir(name string) string { return filepath.Join(p.Dir, name) }

func (p *Publisher) readState(name string) state {
	var s state
	if b, err := os.ReadFile(filepath.Join(p.siteDir(name), "site.json")); err == nil {
		json.Unmarshal(b, &s)
	}
	return s
}

func (p *Publisher) writeState(name string, s state) error {
	b, _ := json.MarshalIndent(s, "", "  ")
	return atomicfile.Replace(filepath.Join(p.siteDir(name), "site.json"), append(b, '\n'), 0o600)
}

// Sites lists the sites published from here.
func (p *Publisher) Sites() []string {
	entries, _ := os.ReadDir(p.Dir)
	var out []string
	for _, e := range entries {
		if e.IsDir() && ValidName(e.Name()) == nil {
			out = append(out, e.Name())
		}
	}
	return out
}

// Versions lists a site's versions, newest first.
func (p *Publisher) Versions(name string) ([]Version, error) {
	dir := filepath.Join(p.siteDir(name), "versions")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no site %s was published from this machine", name)
	}
	if err != nil {
		return nil, err
	}
	cur := p.readState(name).Current
	var out []Version
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), files.StubExt) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		m, err := files.ReadStub(path)
		if err != nil {
			continue
		}
		v := Version{ID: m.FileID, StoredAt: m.StoredAt, Bytes: m.ContentBytes, Files: m.FileCount, Current: m.FileID == cur, stub: path}
		if fi, err := e.Info(); err == nil {
			v.written = fi.ModTime()
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StoredAt != out[j].StoredAt {
			return out[i].StoredAt > out[j].StoredAt
		}
		return out[i].written.After(out[j].written)
	})
	return out, nil
}

// Publish stores folder as a new version of the site and announces it.
// Unchanged content makes no new version; it is announced again, so web
// nodes that missed it catch up.
func (p *Publisher) Publish(ctx context.Context, name, folder string, online []peers.Peer) (Version, int, error) {
	if err := ValidName(name); err != nil {
		return Version{}, 0, err
	}
	if fi, err := os.Stat(folder); err != nil || !fi.IsDir() {
		return Version{}, 0, fmt.Errorf("%s is not a folder", folder)
	}
	if _, err := os.Stat(filepath.Join(folder, "index.html")); err != nil {
		p.logf("note: %s has no index.html, so its home page will be empty", folder)
	}
	opts := files.PutOptions{Log: p.Log}
	var prev *manifest.Manifest
	if vs, _ := p.Versions(name); len(vs) > 0 {
		for _, v := range vs {
			if v.Current {
				if m, err := files.ReadStub(v.stub); err == nil {
					prev = &m
				}
			}
		}
		opts.Previous = prev
	}
	m, _, _, err := files.PutFolder(ctx, p.Client, folder, online, opts)
	if err != nil {
		return Version{}, 0, err
	}
	reused := 0
	if prev != nil {
		reused = files.Reused(m, *prev)
		if reused == len(m.Chunks) && len(m.Chunks) == len(prev.Chunks) {
			// Nothing changed: drop the new item (its shards are the old
			// ones) and announce the current version again.
			v, err := p.announce(ctx, name, *prev)
			return v, reused, err
		}
		m.Lineage = manifest.LineageOf(*prev)
	}
	m.StoredAt = time.Now().Unix()
	dir := filepath.Join(p.siteDir(name), "versions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Version{}, 0, err
	}
	stub := filepath.Join(dir, time.Unix(m.StoredAt, 0).UTC().Format("20060102T150405Z")+"-"+m.FileID[:8]+files.StubExt)
	if err := files.WriteJSON(stub, m); err != nil {
		return Version{}, 0, err
	}
	v, err := p.announce(ctx, name, m)
	return v, reused, err
}

// Rollback serves an earlier version again: the one with the ID (or ID
// prefix) given, or with "" the one before the current.
func (p *Publisher) Rollback(ctx context.Context, name, id string) (Version, error) {
	vs, err := p.Versions(name)
	if err != nil {
		return Version{}, err
	}
	var pick *Version
	for i, v := range vs {
		if id == "" && v.Current && i+1 < len(vs) {
			pick = &vs[i+1]
			break
		}
		if id != "" && strings.HasPrefix(v.ID, id) {
			pick = &vs[i]
			break
		}
	}
	if pick == nil {
		if id == "" {
			return Version{}, errors.New("there is no earlier version")
		}
		return Version{}, fmt.Errorf("no version %s of %s", id, name)
	}
	m, err := files.ReadStub(pick.stub)
	if err != nil {
		return Version{}, err
	}
	return p.announce(ctx, name, m)
}

// AnnounceAll announces every site's current version again, for web nodes
// that joined after it was published.
func (p *Publisher) AnnounceAll(ctx context.Context) (int, error) {
	n := 0
	for _, name := range p.Sites() {
		vs, err := p.Versions(name)
		if err != nil {
			return n, err
		}
		for _, v := range vs {
			if v.Current {
				m, err := files.ReadStub(v.stub)
				if err != nil {
					return n, err
				}
				if _, err := p.announce(ctx, name, m); err != nil {
					return n, err
				}
				n++
			}
		}
	}
	return n, nil
}

// Remove stops every web node serving the site, then deletes all its
// versions from the group.
func (p *Publisher) Remove(ctx context.Context, name string) error {
	vs, err := p.Versions(name)
	if err != nil {
		return err
	}
	typ, body, err := encode(Announcement{Site: name})
	if err != nil {
		return err
	}
	if err := p.Announce.Publish(ctx, Topic, typ, body); err != nil {
		return fmt.Errorf("telling the web nodes: %w", err)
	}
	seen := map[string]bool{}
	for _, v := range vs {
		m, err := files.ReadStub(v.stub)
		if err != nil {
			continue
		}
		// Versions share unchanged chunks; delete each shard once.
		for i := range m.Chunks {
			kept := m.Chunks[i].Shards[:0]
			for _, s := range m.Chunks[i].Shards {
				if !seen[s.Hash] {
					seen[s.Hash] = true
					kept = append(kept, s)
				}
			}
			m.Chunks[i].Shards = kept
		}
		if res := files.Delete(ctx, p.Client, m); res.Err() != nil {
			p.logf("deleting version %s: %v", v.ID[:8], res.Err())
		}
	}
	return os.RemoveAll(p.siteDir(name))
}

// SetContact turns a site's contact form on, its messages sealed for code
// (this machine's sharing code), or off with "", and announces the current
// version again so web nodes know.
func (p *Publisher) SetContact(ctx context.Context, name, code string) error {
	vs, err := p.Versions(name)
	if err != nil {
		return err
	}
	s := p.readState(name)
	s.Contact = code
	if err := p.writeState(name, s); err != nil {
		return err
	}
	for _, v := range vs {
		if v.Current {
			m, err := files.ReadStub(v.stub)
			if err != nil {
				return err
			}
			_, err = p.announce(ctx, name, m)
			return err
		}
	}
	return nil
}

// Contact is the sharing code a site's contact form goes to; "" if off.
func (p *Publisher) Contact(name string) string { return p.readState(name).Contact }

func (p *Publisher) announce(ctx context.Context, name string, m manifest.Manifest) (Version, error) {
	s := p.readState(name)
	typ, body, err := encode(Announcement{Site: name, Stub: &m, Contact: s.Contact})
	if err != nil {
		return Version{}, err
	}
	if err := p.Announce.Publish(ctx, Topic, typ, body); err != nil {
		return Version{}, fmt.Errorf("stored, but the web nodes could not be told: %w (run: yggstore site announce)", err)
	}
	s.Current = m.FileID
	if err := p.writeState(name, s); err != nil {
		return Version{}, err
	}
	return Version{ID: m.FileID, StoredAt: m.StoredAt, Bytes: m.ContentBytes, Files: m.FileCount, Current: true}, nil
}

func (p *Publisher) logf(format string, args ...any) {
	if p.Log != nil {
		p.Log(format, args...)
	}
}
