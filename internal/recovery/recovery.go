// Package recovery keeps a copy of what a person needs to carry on without
// their machine, stored in the group like any other item: the outbox's
// stubs and history, the sharing key, contacts, mail and sites, and the
// member list. Without the machine, the stubs are the only way to the
// files, so losing it would otherwise lose everything stored.
//
// The copy is refreshed a little after something changes, and the last few
// are kept. Each copy's stub is also sealed with the recovery key, ready
// for the group's web nodes to hold: an emailed link carries half of that
// key and the web nodes the other half, so a lost machine needs only the
// email to get everything back, and no web node can open a copy on its own.
package recovery

import (
	"archive/tar"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/peterretief/yggstore/internal/atomicfile"
	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/files"
	"github.com/peterretief/yggstore/internal/manifest"
	"github.com/peterretief/yggstore/internal/peers"
)

// Source is a folder (or one file) that goes into the copy, under Name.
type Source struct {
	Name string
	Path string
	// Keep says which paths inside a folder go in, by their path with /.
	// A folder it turns down is left out whole. nil keeps everything.
	Keep func(rel string, dir bool) bool
}

type Config struct {
	Dir     string // where the state and the copies' stubs are kept
	Sources []Source
	Client  client.Client
	Online  func(context.Context) ([]peers.Peer, error)
	Event   func(kind, msg string)

	// Holding, if set, says whether this place holds the working copy:
	// copies are made only there. OnCopy hears of each new copy.
	Holding func() bool
	OnCopy  func(context.Context, manifest.Manifest)

	Check  time.Duration // how often to look for changes (1 minute)
	Settle time.Duration // how long nothing must change before a copy (2 minutes)
	Keep   int           // copies kept (5)
	Now    func() time.Time
}

// State is what is kept in Dir/state.json.
type State struct {
	Off   bool   `json:"off,omitempty"`   // the person turned recovery off
	Email string `json:"email,omitempty"` // where the recovery link goes
	// ID names this person's sealed copy to the web nodes. LinkHalf goes in
	// the emailed link and never to the group; GroupHalf goes to the web
	// nodes with the sealed copy. Together they make the key.
	ID        string `json:"id"`
	LinkHalf  string `json:"link_half"`
	GroupHalf string `json:"group_half"`
	Copies    []Copy `json:"copies,omitempty"` // newest last
	Problem   string `json:"problem,omitempty"`
}

// Copy is one stored recovery copy.
type Copy struct {
	At    int64  `json:"at"`
	Stub  string `json:"stub"` // file name in Dir
	Files int    `json:"files"`
	Bytes int64  `json:"bytes"`
	Print string `json:"print"` // fingerprint of what went in
}

// Status is what the dashboard shows.
type Status struct {
	On      bool   `json:"on"`
	Email   string `json:"email,omitempty"`
	Last    *Copy  `json:"last,omitempty"`
	Copies  int    `json:"copies"`
	Problem string `json:"problem,omitempty"`
}

type Keeper struct {
	cfg    Config
	saveMu sync.Mutex // one copy at a time
	mu     sync.Mutex
	st     State
	wake   chan struct{}

	seen    string    // the fingerprint last looked at
	changed time.Time // when it last changed
}

func New(cfg Config) *Keeper {
	if cfg.Check <= 0 {
		cfg.Check = time.Minute
	}
	if cfg.Settle <= 0 {
		cfg.Settle = 2 * time.Minute
	}
	if cfg.Keep <= 0 {
		cfg.Keep = 5
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Event == nil {
		cfg.Event = func(string, string) {}
	}
	return &Keeper{cfg: cfg, wake: make(chan struct{}, 1)}
}

func (k *Keeper) statePath() string { return filepath.Join(k.cfg.Dir, "state.json") }

// load reads the state, making the key the first time.
func (k *Keeper) load() error {
	if err := os.MkdirAll(k.cfg.Dir, 0o700); err != nil {
		return err
	}
	var st State
	if b, err := os.ReadFile(k.statePath()); err == nil {
		if err := json.Unmarshal(b, &st); err != nil {
			return fmt.Errorf("%s: %w", k.statePath(), err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if st.ID == "" || st.LinkHalf == "" || st.GroupHalf == "" {
		st.ID, st.LinkHalf, st.GroupHalf = randomText(16), randomText(32), randomText(32)
	}
	k.mu.Lock()
	k.st = st
	k.mu.Unlock()
	return k.save()
}

func (k *Keeper) save() error {
	k.mu.Lock()
	b, err := json.MarshalIndent(k.st, "", "  ")
	k.mu.Unlock()
	if err != nil {
		return err
	}
	return atomicfile.Replace(k.statePath(), append(b, '\n'), 0o600)
}

func randomText(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Status says whether recovery is on and how recent the copy is.
func (k *Keeper) Status() Status {
	k.mu.Lock()
	defer k.mu.Unlock()
	s := Status{On: !k.st.Off, Email: k.st.Email, Copies: len(k.st.Copies), Problem: k.st.Problem}
	if n := len(k.st.Copies); n > 0 {
		c := k.st.Copies[n-1]
		s.Last = &c
	}
	return s
}

// Set turns recovery on or off and sets the address the link goes to.
func (k *Keeper) Set(on bool, email string) error {
	email = strings.TrimSpace(email)
	if email != "" && (!strings.Contains(email, "@") || strings.ContainsAny(email, " <>\"")) {
		return fmt.Errorf("%q isn't an email address", email)
	}
	k.mu.Lock()
	k.st.Off, k.st.Email = !on, email
	k.mu.Unlock()
	if err := k.save(); err != nil {
		return err
	}
	select {
	case k.wake <- struct{}{}:
	default:
	}
	return nil
}

// Run keeps the copy fresh until ctx ends.
func (k *Keeper) Run(ctx context.Context) error {
	if err := k.load(); err != nil {
		return err
	}
	t := time.NewTicker(k.cfg.Check)
	defer t.Stop()
	for {
		k.Step(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		case <-k.wake:
		}
	}
}

// Step stores a new copy if what goes in has changed and then settled.
func (k *Keeper) Step(ctx context.Context) {
	k.mu.Lock()
	off := k.st.Off
	var last Copy
	if n := len(k.st.Copies); n > 0 {
		last = k.st.Copies[n-1]
	}
	k.mu.Unlock()
	if off || k.cfg.Holding != nil && !k.cfg.Holding() {
		return
	}
	k.reseal(last)
	print, err := k.fingerprint()
	if err != nil {
		k.problem("can't read what goes into the recovery copy: " + err.Error())
		return
	}
	now := k.cfg.Now()
	if print != k.seen {
		k.seen, k.changed = print, now
	}
	if print == last.Print || now.Sub(k.changed) < k.cfg.Settle && last.Print != "" {
		return
	}
	k.saveMu.Lock()
	err = k.store(ctx, print)
	k.saveMu.Unlock()
	switch {
	case errors.Is(err, errNoNodes):
		// Just started, or offline: tried again next time, quietly.
	case err != nil:
		k.problem("recovery copy not saved: " + err.Error())
	}
}

func (k *Keeper) problem(msg string) {
	k.mu.Lock()
	same := k.st.Problem == msg
	k.st.Problem = msg
	k.mu.Unlock()
	if !same {
		k.cfg.Event("warn", msg)
		k.save()
	}
}

// walk calls f for every file and folder that goes into the copy, with its
// path in the copy.
func (k *Keeper) walk(f func(name, path string, info fs.FileInfo) error) error {
	for _, s := range k.cfg.Sources {
		info, err := os.Stat(s.Path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			if err := f(s.Name, s.Path, info); err != nil {
				return err
			}
			continue
		}
		err = filepath.WalkDir(s.Path, func(path string, e fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil // gone while we looked
				}
				return err
			}
			rel, _ := filepath.Rel(s.Path, path)
			rel = filepath.ToSlash(rel)
			if rel != "." && s.Keep != nil && !s.Keep(rel, e.IsDir()) {
				if e.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if !e.IsDir() && !e.Type().IsRegular() {
				return nil
			}
			info, err := e.Info()
			if err != nil {
				return nil
			}
			name := s.Name
			if rel != "." {
				name += "/" + rel
			}
			return f(name, path, info)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// fingerprint sums up the names, sizes and times of what goes in.
func (k *Keeper) fingerprint() (string, error) {
	h := sha256.New()
	err := k.walk(func(name, _ string, info fs.FileInfo) error {
		fmt.Fprintf(h, "%s\x00%d\x00%d\x00", name, info.Size(), info.ModTime().UnixNano())
		return nil
	})
	return hex.EncodeToString(h.Sum(nil)), err
}

// store makes a new copy, seals its stub, and drops the oldest beyond Keep.
func (k *Keeper) store(ctx context.Context, print string) error {
	online, err := k.cfg.Online(ctx)
	if err != nil {
		return err
	}
	if len(online) == 0 {
		return errNoNodes
	}
	pr, pw := io.Pipe()
	var count int
	var size int64
	go func() { pw.CloseWithError(k.writeTar(pw, &count, &size)) }()
	m, chal, _, err := files.PutReader(ctx, k.cfg.Client, pr, "recovery", online, files.PutOptions{Challenges: 2})
	pr.CloseWithError(errors.New("upload stopped"))
	if err != nil {
		return err
	}
	m.Kind, m.FileCount, m.ContentBytes = files.KindFolder, count, size

	now := k.cfg.Now()
	name := "recovery-" + now.UTC().Format("20060102-150405") + files.StubExt
	stub := filepath.Join(k.cfg.Dir, name)
	if err := files.WriteJSON(stub, m); err != nil {
		files.Delete(context.WithoutCancel(ctx), k.cfg.Client, m)
		return err
	}
	os.Chmod(stub, 0o600)
	files.WriteJSON(files.ChallengesPath(stub), chal)
	if err := k.seal(m); err != nil {
		return err
	}

	k.mu.Lock()
	k.st.Copies = append(k.st.Copies, Copy{At: now.Unix(), Stub: name, Files: count, Bytes: size, Print: print})
	var drop []Copy
	if n := len(k.st.Copies) - k.cfg.Keep; n > 0 {
		drop = append(drop, k.st.Copies[:n]...)
		k.st.Copies = append([]Copy(nil), k.st.Copies[n:]...)
	}
	k.st.Problem = ""
	k.mu.Unlock()
	if err := k.save(); err != nil {
		return err
	}
	k.cfg.Event("ok", fmt.Sprintf("recovery copy saved: %d files, %s", count, human(size)))
	if k.cfg.OnCopy != nil {
		k.cfg.OnCopy(ctx, m)
	}
	for _, c := range drop {
		k.forget(ctx, c)
	}
	return nil
}

var errNoNodes = errors.New("no node answers")

// forget deletes an old copy from the group, and its stub.
func (k *Keeper) forget(ctx context.Context, c Copy) {
	stub := filepath.Join(k.cfg.Dir, c.Stub)
	m, err := files.ReadStub(stub)
	if err == nil {
		files.Delete(ctx, k.cfg.Client, m) // what a node that is down keeps, its lease reports
	}
	os.Remove(stub)
	os.Remove(files.ChallengesPath(stub))
}

func (k *Keeper) writeTar(w io.Writer, count *int, size *int64) error {
	tw := tar.NewWriter(w)
	err := k.walk(func(name, path string, info fs.FileInfo) error {
		if info.IsDir() {
			return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: name + "/", Mode: int64(info.Mode().Perm()), ModTime: info.ModTime()})
		}
		f, err := os.Open(path)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		defer f.Close()
		// Files can change while the copy is made (a stub rewritten by
		// repair): copy what is there now, at its size now.
		b, err := io.ReadAll(f)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: int64(info.Mode().Perm()), ModTime: info.ModTime(), Size: int64(len(b))}); err != nil {
			return err
		}
		if _, err := tw.Write(b); err != nil {
			return err
		}
		*count++
		*size += int64(len(b))
		return nil
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// Key is the recovery key: the link's half and the group's half together.
func Key(linkHalf, groupHalf string) ([]byte, error) {
	a, err := base64.RawURLEncoding.DecodeString(linkHalf)
	if err != nil || len(a) != 32 {
		return nil, errors.New("the link's half of the key isn't right")
	}
	b, err := base64.RawURLEncoding.DecodeString(groupHalf)
	if err != nil || len(b) != 32 {
		return nil, errors.New("the group's half of the key isn't right")
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = a[i] ^ b[i]
	}
	return key, nil
}

// SealedPath is where the newest copy's sealed stub is kept, for the web
// nodes.
func (k *Keeper) SealedPath() string { return filepath.Join(k.cfg.Dir, "sealed") }

// seal writes the copy's stub encrypted with the recovery key.
func (k *Keeper) seal(m manifest.Manifest) error {
	k.mu.Lock()
	link, group := k.st.LinkHalf, k.st.GroupHalf
	k.mu.Unlock()
	key, err := Key(link, group)
	if err != nil {
		return err
	}
	plain, err := json.Marshal(m)
	if err != nil {
		return err
	}
	sealed, err := Seal(key, plain)
	if err != nil {
		return err
	}
	return atomicfile.Replace(k.SealedPath(), sealed, 0o600)
}

// reseal seals the newest copy's stub again if repair has rewritten it
// (shards moved to other nodes) since it was sealed.
func (k *Keeper) reseal(last Copy) {
	if last.Stub == "" {
		return
	}
	stub := filepath.Join(k.cfg.Dir, last.Stub)
	si, err := os.Stat(stub)
	if err != nil {
		return
	}
	if sealed, err := os.Stat(k.SealedPath()); err == nil && !si.ModTime().After(sealed.ModTime()) {
		return
	}
	if m, err := files.ReadStub(stub); err == nil {
		k.seal(m)
	}
}

// Seal encrypts a stub with the recovery key (AES-256-GCM, the nonce first).
func Seal(key, plain []byte) ([]byte, error) {
	aead, err := gcm(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	rand.Read(nonce)
	return aead.Seal(nonce, nonce, plain, []byte("yggstore recovery")), nil
}

// Open is Seal's reverse.
func Open(key, sealed []byte) ([]byte, error) {
	aead, err := gcm(key)
	if err != nil {
		return nil, err
	}
	if len(sealed) < aead.NonceSize() {
		return nil, errors.New("sealed copy too short")
	}
	n := aead.NonceSize()
	plain, err := aead.Open(nil, sealed[:n], sealed[n:], []byte("yggstore recovery"))
	if err != nil {
		return nil, errors.New("the key doesn't open this copy")
	}
	return plain, nil
}

func gcm(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// SaveNow makes a copy straight away, if anything changed since the last
// one, and returns the newest copy's stub: the final copy when the working
// copy moves to another place.
func (k *Keeper) SaveNow(ctx context.Context) (manifest.Manifest, error) {
	k.saveMu.Lock()
	defer k.saveMu.Unlock()
	print, err := k.fingerprint()
	if err != nil {
		return manifest.Manifest{}, err
	}
	k.mu.Lock()
	var last Copy
	if n := len(k.st.Copies); n > 0 {
		last = k.st.Copies[n-1]
	}
	k.mu.Unlock()
	if print != last.Print {
		if err := k.store(ctx, print); err != nil {
			return manifest.Manifest{}, err
		}
	}
	return k.Latest()
}

// Latest is the newest copy's stub, for restoring on this machine.
func (k *Keeper) Latest() (manifest.Manifest, error) {
	k.mu.Lock()
	n := len(k.st.Copies)
	var c Copy
	if n > 0 {
		c = k.st.Copies[n-1]
	}
	k.mu.Unlock()
	if n == 0 {
		return manifest.Manifest{}, errors.New("no recovery copy yet")
	}
	return files.ReadStub(filepath.Join(k.cfg.Dir, c.Stub))
}

func human(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

// OutboxKeep is what of an outbox goes into the copy: stubs and sealed
// shares in files/, whole/ and received/, and the outbox's own records
// (history, restored stubs). Restored files, files not yet stored,
// websites/ (stored as site versions) and the drop folders stay out, and so
// do the stubs' challenges: they are most of an outbox's size, and only
// spot-check the nodes, which reading a file back does too.
func OutboxKeep(rel string, dir bool) bool {
	top, _, _ := strings.Cut(rel, "/")
	switch top {
	case ".yggstore":
		// The activity log changes all the time, and is no use elsewhere.
		return rel != ".yggstore/activity.log" && !strings.HasSuffix(rel, files.ChallengesExt)
	case "files", "whole", "received":
		if dir {
			return true
		}
		return strings.HasSuffix(rel, files.StubExt) || strings.HasSuffix(rel, ".ysend")
	}
	return false
}

// FoldersKeep is OutboxKeep for a folder of outboxes, such as a box's
// teams/: what of each one goes in.
func FoldersKeep(rel string, dir bool) bool {
	_, rest, nested := strings.Cut(rel, "/")
	if !nested {
		return dir
	}
	return OutboxKeep(rest, dir)
}

// homeKeep is what of ~/.yggstore goes in: the person's own things, not the
// node's shards, tokens and passwords, nor the recovery folder itself.
var homeKeep = map[string]bool{
	"sharing.key": true, "contacts.json": true, "names.json": true, "invites.json": true,
	"devices.json": true, "me.name": true, "mail": true, "sites": true, "own-sites": true, "notes": true,
}

// HomeKeep is what of ~/.yggstore goes into the copy. Mail goes in as its
// stubs (each message is stored in the group) and its folders and flags,
// not the copies of the messages: the mailbox fetches those back.
func HomeKeep(rel string, dir bool) bool {
	top, _, _ := strings.Cut(rel, "/")
	if top == "mail" && strings.HasSuffix(rel, ".sealed") {
		return false
	}
	return homeKeep[top]
}
