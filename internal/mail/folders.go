package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/peterretief/yggstore/internal/atomicfile"
	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/files"
	"github.com/peterretief/yggstore/internal/peers"
)

// Folders: a message's folder and flags live in its .json beside it, with
// a number (UID) that a mail program uses to tell messages apart. Mail the
// node collects lands in the Inbox; messages written here go to Sent.

// The folders every mailbox has. Others can be made (see CreateFolder).
const (
	Inbox   = "INBOX"
	Sent    = "Sent"
	Drafts  = "Drafts"
	Trash   = "Trash"
	Archive = "Archives"
	Junk    = "Junk"
)

// StandardFolders are always there, in this order.
var StandardFolders = []string{Inbox, Sent, Drafts, Trash, Archive, Junk}

// Entry is a message in a folder.
type Entry struct {
	ID string
	Meta
}

// folderState is the mailbox's folder bookkeeping (folders.json).
type folderState struct {
	Validity uint32            `json:"validity"` // UIDVALIDITY: changes only if UIDs are reset
	Next     map[string]uint32 `json:"next"`     // next UID per folder
	Custom   []string          `json:"custom,omitempty"`
}

var metaMu sync.Map // per mailbox dir: *sync.Mutex, shared by every Box on it

func (b *Box) lock() func() {
	m, _ := metaMu.LoadOrStore(filepath.Clean(b.Dir), &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (b *Box) statePath() string { return filepath.Join(b.Dir, "folders.json") }

// loadFolders reads folders.json; the caller holds the lock.
func (b *Box) loadFolders() folderState {
	var st folderState
	if data, err := os.ReadFile(b.statePath()); err == nil {
		json.Unmarshal(data, &st)
	}
	if st.Validity == 0 {
		st.Validity = uint32(time.Now().Unix())
	}
	if st.Next == nil {
		st.Next = map[string]uint32{}
	}
	return st
}

func (b *Box) saveFolders(st folderState) error {
	if err := os.MkdirAll(b.Dir, 0o700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(st, "", "  ")
	return atomicfile.Replace(b.statePath(), append(data, '\n'), 0o600)
}

func folderOf(m Meta) string {
	switch {
	case m.Folder != "":
		return m.Folder
	case m.Sent:
		return Sent
	}
	return Inbox
}

// canonical is a folder's name as the mailbox knows it: INBOX in capitals,
// others as made.
func (b *Box) canonical(st folderState, name string) (string, bool) {
	if strings.EqualFold(name, Inbox) {
		return Inbox, true
	}
	for _, f := range append(slices.Clone(StandardFolders), st.Custom...) {
		if f == name {
			return f, true
		}
	}
	return "", false
}

// Folders lists the mailbox's folders.
func (b *Box) Folders() []string {
	defer b.lock()()
	return append(slices.Clone(StandardFolders), b.loadFolders().Custom...)
}

// Validity is the mailbox's UIDVALIDITY.
func (b *Box) Validity() uint32 {
	defer b.lock()()
	st := b.loadFolders()
	b.saveFolders(st)
	return st.Validity
}

// ErrNoFolder means there is no folder by that name.
var ErrNoFolder = errors.New("no such folder")

// CreateFolder makes a folder.
func (b *Box) CreateFolder(name string) error {
	defer b.lock()()
	st := b.loadFolders()
	if _, ok := b.canonical(st, name); ok {
		return errors.New("that folder exists")
	}
	if name == "" || strings.ContainsAny(name, "/\\\r\n\"*%") || len(name) > 100 {
		return errors.New("folder names can't contain / \\ \" * % or line breaks")
	}
	st.Custom = append(st.Custom, name)
	return b.saveFolders(st)
}

// DeleteFolder removes an empty folder that isn't a standard one.
func (b *Box) DeleteFolder(name string) error {
	defer b.lock()()
	st := b.loadFolders()
	i := slices.Index(st.Custom, name)
	if i < 0 {
		return errors.New("only empty folders you made can be removed")
	}
	entries, err := b.entries(&st)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if folderOf(e.Meta) == name {
			return errors.New("the folder isn't empty")
		}
	}
	st.Custom = slices.Delete(st.Custom, i, i+1)
	return b.saveFolders(st)
}

// entries reads every message's meta, giving each without a UID one (new
// mail, oldest first). The caller holds the lock.
func (b *Box) entries(st *folderState) ([]Entry, error) {
	ents, err := os.ReadDir(b.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var out []Entry
	for _, e := range ents {
		id, ok := strings.CutSuffix(e.Name(), ".sealed")
		if !ok || !validID.MatchString(id) {
			continue
		}
		var m Meta
		if data, err := os.ReadFile(filepath.Join(b.Dir, id+".json")); err == nil {
			json.Unmarshal(data, &m)
		}
		out = append(out, Entry{id, m})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Received != out[j].Received {
			return out[i].Received < out[j].Received
		}
		return out[i].ID < out[j].ID
	})
	changed := false
	for i := range out {
		f := folderOf(out[i].Meta)
		if _, ok := b.canonical(*st, f); !ok {
			out[i].Folder = Inbox // its folder was removed by hand
			f = Inbox
			out[i].UID = 0
		}
		if u := out[i].UID; u != 0 && u >= st.Next[f] { // folders.json was lost: never reuse a UID
			st.Next[f] = u + 1
			changed = true
		}
	}
	for i := range out {
		f := folderOf(out[i].Meta)
		if out[i].UID == 0 {
			if st.Next[f] == 0 {
				st.Next[f] = 1
			}
			out[i].UID = st.Next[f]
			st.Next[f]++
			b.writeMeta(out[i])
			changed = true
		}
	}
	if changed {
		if err := b.saveFolders(*st); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (b *Box) writeMeta(e Entry) error {
	data, _ := json.Marshal(e.Meta)
	return atomicfile.Replace(filepath.Join(b.Dir, e.ID+".json"), data, 0o600)
}

// Folder lists a folder's messages in UID order.
func (b *Box) Folder(name string) ([]Entry, error) {
	defer b.lock()()
	st := b.loadFolders()
	name, ok := b.canonical(st, name)
	if !ok {
		return nil, ErrNoFolder
	}
	all, err := b.entries(&st)
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, e := range all {
		if folderOf(e.Meta) == name {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out, nil
}

// UIDNext is the UID the folder's next message will get. It never goes
// down, even when the newest message is moved or deleted.
func (b *Box) UIDNext(name string) (uint32, error) {
	defer b.lock()()
	st := b.loadFolders()
	name, ok := b.canonical(st, name)
	if !ok {
		return 0, ErrNoFolder
	}
	if _, err := b.entries(&st); err != nil {
		return 0, err
	}
	return max(st.Next[name], 1), nil
}

// SetFlags replaces a message's flags (\Seen, \Flagged, ...).
func (b *Box) SetFlags(id string, flags []string) error {
	defer b.lock()()
	m, err := b.meta(id)
	if err != nil {
		return err
	}
	m.Flags = flags
	return b.writeMeta(Entry{id, m})
}

// MarkSeen sets \Seen on a message.
func (b *Box) MarkSeen(id string) error {
	defer b.lock()()
	m, err := b.meta(id)
	if err != nil {
		return err
	}
	if slices.Contains(m.Flags, `\Seen`) {
		return nil
	}
	m.Flags = append(m.Flags, `\Seen`)
	return b.writeMeta(Entry{id, m})
}

func (b *Box) meta(id string) (Meta, error) {
	if !validID.MatchString(id) {
		return Meta{}, ErrNoMessage
	}
	if _, err := os.Stat(filepath.Join(b.Dir, id+".sealed")); err != nil {
		return Meta{}, ErrNoMessage
	}
	var m Meta
	if data, err := os.ReadFile(filepath.Join(b.Dir, id+".json")); err == nil {
		json.Unmarshal(data, &m)
	}
	return m, nil
}

// Move puts a message in another folder, under a new UID there.
func (b *Box) Move(id, folder string) error {
	defer b.lock()()
	st := b.loadFolders()
	folder, ok := b.canonical(st, folder)
	if !ok {
		return ErrNoFolder
	}
	m, err := b.meta(id)
	if err != nil {
		return err
	}
	if st.Next[folder] == 0 {
		st.Next[folder] = 1
	}
	m.Folder, m.UID = folder, st.Next[folder]
	st.Next[folder]++
	if err := b.saveFolders(st); err != nil {
		return err
	}
	return b.writeMeta(Entry{id, m})
}

// Keep puts a message in a folder, sealed for the owner, and stores a copy
// in the group. The message is kept even if the group copy fails; that
// error is returned with its ID.
func (b *Box) Keep(ctx context.Context, c client.Client, list []peers.Peer, raw []byte, folder string, flags []string, received time.Time) (string, error) {
	if b.ID == nil {
		return "", errors.New("no sharing key to keep mail with")
	}
	unlock := b.lock()
	st := b.loadFolders()
	folder, ok := b.canonical(st, folder)
	unlock()
	if !ok {
		return "", ErrNoFolder
	}
	sealed, err := Seal(b.ID.Code(), raw)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(b.Dir, 0o700); err != nil {
		return "", err
	}
	id := "kept-" + randomID()
	if folder == Sent {
		id = "sent-" + randomID()
	}
	if received.IsZero() {
		received = time.Now()
	}
	m := Meta{Received: received.UnixMilli(), Sent: folder == Sent, Folder: folder, Flags: flags}
	unlock = b.lock()
	st = b.loadFolders()
	if st.Next[folder] == 0 {
		st.Next[folder] = 1
	}
	m.UID = st.Next[folder]
	st.Next[folder]++
	err = b.saveFolders(st)
	if err == nil {
		err = b.writeMeta(Entry{id, m})
	}
	if err == nil {
		err = atomicfile.Replace(filepath.Join(b.Dir, id+".sealed"), sealed, 0o600)
	}
	unlock()
	if err != nil {
		return "", err
	}
	stub, _, _, err := files.PutReader(ctx, c, bytes.NewReader(sealed), "mail-"+id, online(ctx, c, list), files.PutOptions{})
	if err != nil {
		return id, fmt.Errorf("kept here, but not stored in the group: %w", err)
	}
	return id, files.WriteJSON(filepath.Join(b.Dir, id+".ystub"), stub)
}
