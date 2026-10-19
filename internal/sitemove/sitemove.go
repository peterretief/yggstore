// Package sitemove moves a website from the machine that publishes it to
// another (see docs/move-site.md). The group's admin starts a move; the old
// holder hands the site's versions over, sealed for the new holder; the
// admin gives the new holder the name; the new holder announces the version
// being served, and the old holder lets go. The site stays online
// throughout, and so does its contact form.
//
// The messages, all direct:
//
//	site-move     admin → old and new holder: Move
//	site-want     new → old: Want, the sharing code to seal the versions for
//	site-handover old → new: Handover, the versions, sealed
//	site-ready    new → admin: Ready, the versions are here
//	site-moved    new → old and admin: Done, the new holder serves it
//	site-refused  either → admin: Refused, why the move can't go on
//
// The versions travel as one item stored in the group, whose stub is sealed
// for the new holder, so a site of any size fits in one message.
package sitemove

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/atomicfile"
	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/files"
	"github.com/peterretief/yggstore/internal/manifest"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/share"
)

const (
	TypeMove     = "site-move"
	TypeWant     = "site-want"
	TypeHandover = "site-handover"
	TypeReady    = "site-ready"
	TypeMoved    = "site-moved"
	TypeRefused  = "site-refused"
)

// Move is a move the admin started. From and To are node IDs; a person is
// someone on that node's box ("" for the node itself).
type Move struct {
	Site       string `json:"site"`
	From       string `json:"from"`
	FromPerson string `json:"from_person,omitempty"`
	To         string `json:"to"`
	ToPerson   string `json:"to_person,omitempty"`
	Time       int64  `json:"time"` // unix ms, when the admin started it
}

// Want asks the old holder for the versions, sealed for Code.
type Want struct {
	Site string `json:"site"`
	Code string `json:"code"`
}

// Handover carries the versions: a .ysend of the bundle's stub.
type Handover struct {
	Site   string `json:"site"`
	Sealed string `json:"sealed"`
}

// Ready tells the admin the new holder has the versions, and the sharing
// code the name goes with.
type Ready struct {
	Site   string `json:"site"`
	Person string `json:"person,omitempty"`
	Code   string `json:"code"`
}

// Done says the new holder serves the site.
type Done struct {
	Site string `json:"site"`
}

// Refused says why a move can't go on.
type Refused struct {
	Site   string `json:"site"`
	Reason string `json:"reason"`
}

// Body is v as a message body.
func Body(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// Bundle is everything a publisher keeps of a site: its versions' stubs
// (and their challenges), the version being served and whether its contact
// form is on.
type Bundle struct {
	Site    string            `json:"site"`
	Current string            `json:"current"`
	Contact bool              `json:"contact"`
	Files   map[string][]byte `json:"files"` // versions/NAME → contents
	// Times are the files' modification times (unix ns), which order
	// versions stored in the same second.
	Times map[string]int64 `json:"times,omitempty"`
}

// publisher's files (see site.Publisher).
type state struct {
	Current string `json:"current"`
	Contact string `json:"contact,omitempty"`
}

func siteDir(pubDir, site string) string { return filepath.Join(pubDir, site) }

// Export reads a site's versions from a publisher's folder.
func Export(pubDir, site string) (Bundle, error) {
	dir := siteDir(pubDir, site)
	var st state
	b, err := os.ReadFile(filepath.Join(dir, "site.json"))
	if err != nil {
		return Bundle{}, fmt.Errorf("%s isn't published from here: %w", site, err)
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return Bundle{}, err
	}
	out := Bundle{Site: site, Current: st.Current, Contact: st.Contact != "", Files: map[string][]byte{}, Times: map[string]int64{}}
	ents, err := os.ReadDir(filepath.Join(dir, "versions"))
	if err != nil {
		return Bundle{}, err
	}
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, "versions", e.Name()))
		if err != nil {
			return Bundle{}, err
		}
		out.Files[e.Name()] = data
		if fi, err := e.Info(); err == nil {
			out.Times[e.Name()] = fi.ModTime().UnixNano()
		}
	}
	if len(out.Files) == 0 {
		return Bundle{}, fmt.Errorf("%s has no versions here", site)
	}
	return out, nil
}

// Import files a bundle in a publisher's folder, with its contact form's
// messages sealed for contactCode if the form was on. A site already
// published there is refused.
func Import(pubDir string, b Bundle, contactCode string) error {
	dir := siteDir(pubDir, b.Site)
	if _, err := os.Stat(filepath.Join(dir, "site.json")); err == nil {
		return fmt.Errorf("%s is published from here already", b.Site)
	}
	if err := os.MkdirAll(filepath.Join(dir, "versions"), 0o700); err != nil {
		return err
	}
	for name, data := range b.Files {
		if name != filepath.Base(name) || strings.HasPrefix(name, ".") {
			return fmt.Errorf("a version called %q", name)
		}
		path := filepath.Join(dir, "versions", name)
		if err := atomicfile.Replace(path, data, 0o600); err != nil {
			return err
		}
		if ns, ok := b.Times[name]; ok {
			os.Chtimes(path, time.Unix(0, ns), time.Unix(0, ns))
		}
	}
	st := state{Current: b.Current}
	if b.Contact {
		st.Contact = contactCode
	}
	data, _ := json.MarshalIndent(st, "", "  ")
	return atomicfile.Replace(filepath.Join(dir, "site.json"), append(data, '\n'), 0o600) // last: the site is here
}

// Pack stores a bundle in the group and seals its stub for toCode.
func Pack(ctx context.Context, c client.Client, online []peers.Peer, from *share.Identity, fromName, toCode string, b Bundle) (string, error) {
	data, err := json.Marshal(b)
	if err != nil {
		return "", err
	}
	m, _, _, err := files.PutReader(ctx, c, bytes.NewReader(data), b.Site+".site-move.json", online, files.PutOptions{})
	if err != nil {
		return "", fmt.Errorf("storing %s's versions for the move: %w", b.Site, err)
	}
	stub, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	sealed, err := from.Seal(toCode, stub, fromName, "the website "+b.Site)
	if err != nil {
		return "", err
	}
	return string(sealed), nil
}

// Unpack opens a handover sealed for id and fetches the bundle. The stored
// bundle is deleted from the group once read: the versions are filed by
// then.
func Unpack(ctx context.Context, c client.Client, id *share.Identity, sealed string) (Bundle, manifest.Manifest, error) {
	rec, err := id.Open([]byte(sealed))
	if err != nil {
		return Bundle{}, manifest.Manifest{}, err
	}
	var m manifest.Manifest
	if err := json.Unmarshal(rec.Stub, &m); err != nil {
		return Bundle{}, manifest.Manifest{}, fmt.Errorf("the handover is damaged: %w", err)
	}
	var buf bytes.Buffer
	if err := files.Get(ctx, c, m, &buf, nil); err != nil {
		return Bundle{}, m, fmt.Errorf("fetching the versions: %w", err)
	}
	var b Bundle
	if err := json.Unmarshal(buf.Bytes(), &b); err != nil {
		return Bundle{}, m, fmt.Errorf("the versions are damaged: %w", err)
	}
	return b, m, nil
}

// Forget deletes a read bundle from the group.
func Forget(ctx context.Context, c client.Client, m manifest.Manifest) {
	files.Delete(ctx, c, m)
}

// MovedDir keeps the sites moved away from a publisher's folder, for 30
// days, in case a move is undone.
const MovedDir = ".moved"

// Keep is how long a moved site's versions stay.
const Keep = 30 * 24 * time.Hour

type moved struct {
	Site string `json:"site"`
	To   string `json:"to"` // where it went, as people know it
	At   int64  `json:"at"` // unix
	Dir  string `json:"dir"`
}

// LetGo stops a publisher keeping a site: its folder moves to .moved/,
// with a note of where the site went, so it is no longer published or
// renewed from here.
func LetGo(pubDir, site, to string, now time.Time) error {
	dir := siteDir(pubDir, site)
	if _, err := os.Stat(dir); err != nil {
		return err
	}
	keep := filepath.Join(pubDir, MovedDir)
	if err := os.MkdirAll(keep, 0o700); err != nil {
		return err
	}
	name := site + "-" + now.UTC().Format("20060102T150405Z")
	if err := os.Rename(dir, filepath.Join(keep, name)); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(moved{Site: site, To: to, At: now.Unix(), Dir: name}, "", "  ")
	return atomicfile.Replace(filepath.Join(keep, site+".json"), append(data, '\n'), 0o600)
}

// Moved says where a site published from here went, if it moved away.
func Moved(pubDir, site string) (to string, at time.Time, ok bool) {
	b, err := os.ReadFile(filepath.Join(pubDir, MovedDir, site+".json"))
	if err != nil {
		return "", time.Time{}, false
	}
	var m moved
	if json.Unmarshal(b, &m) != nil {
		return "", time.Time{}, false
	}
	return m.To, time.Unix(m.At, 0), true
}

// Unmoved forgets that a site moved away, when it comes back.
func Unmoved(pubDir, site string) {
	os.Remove(filepath.Join(pubDir, MovedDir, site+".json"))
}

// Prune deletes the versions of sites moved away more than Keep ago.
func Prune(pubDir string, now time.Time) {
	keep := filepath.Join(pubDir, MovedDir)
	ents, err := os.ReadDir(keep)
	if err != nil {
		return
	}
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(keep, e.Name()))
		var m moved
		if err != nil || json.Unmarshal(b, &m) != nil || m.Dir == "" || strings.ContainsAny(m.Dir, `/\`) {
			continue
		}
		if now.Sub(time.Unix(m.At, 0)) > Keep {
			os.RemoveAll(filepath.Join(keep, m.Dir))
		}
	}
}

// ErrNotForMe is a message about a move that isn't this node's part.
var ErrNotForMe = errors.New("not this node's move")
