// Package mailbridge lets a mail program (Thunderbird, a phone's mail app on
// the same machine) use the group's email, like Proton Bridge: an IMAP
// server for reading and filing the mailbox, and an SMTP server for
// sending through the web nodes. Both listen on this machine only and take
// a password made for them; messages are opened here, so they never leave
// the machine unsealed.
package mailbridge

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"log"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend"
	"github.com/emersion/go-imap/backend/backendutil"
	imapserver "github.com/emersion/go-imap/server"
	"github.com/emersion/go-message"
	"github.com/emersion/go-message/textproto"

	"github.com/peterretief/yggstore/internal/mail"
)

// Bridge serves one mailbox.
type Bridge struct {
	Box      *mail.Box
	Password string
	// Keep files a message in a folder (APPEND, COPY): sealed in the
	// mailbox and stored in the group.
	Keep func(ctx context.Context, raw []byte, folder string, flags []string, date time.Time) error
	// Delete removes a message for good, from the group too.
	Delete func(ctx context.Context, id string) error
	// Send hands a message to the web nodes for delivery.
	Send func(ctx context.Context, raw []byte, rcpts []string) error
	Log  func(string, ...any)

	updates chan backend.Update
	syncMu  sync.Mutex
	told    map[string][]uint32 // per folder, the UIDs mail programs know of
	cacheMu sync.Mutex
	cache   map[string][]byte // opened messages, by ID; they don't change
}

func (b *Bridge) logf(format string, args ...any) {
	if b.Log != nil {
		b.Log(format, args...)
	}
}

// login checks the user name (the mailbox's address) and password.
func (b *Bridge) login(username, password string) error {
	addr := b.Box.Address()
	if addr == "" {
		return errors.New("this mailbox has no address yet (yggstore mail address you@example.org)")
	}
	if !strings.EqualFold(strings.TrimSpace(username), addr) ||
		subtle.ConstantTimeCompare([]byte(password), []byte(b.Password)) != 1 {
		return backend.ErrInvalidCredentials
	}
	return nil
}

// Loopback refuses addresses other machines could reach: the bridge talks
// plain text, which is only fine on this machine.
func Loopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return errors.New(addr + ": the mail bridge only listens on this machine (127.0.0.1)")
	}
	return nil
}

// ServeIMAP answers IMAP on addr until ctx ends.
func (b *Bridge) ServeIMAP(ctx context.Context, addr string) error {
	if err := Loopback(addr); err != nil {
		return err
	}
	b.updates = make(chan backend.Update, 64)
	for _, f := range b.Box.Folders() { // what clients see from the start
		b.sync(f)
	}
	s := imapserver.New((*imapBackend)(b))
	s.Addr = addr
	s.AllowInsecureAuth = true // on this machine only (see Loopback)
	s.ErrorLog = log.New(io.Discard, "", 0)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		s.Close()
	}()
	go b.watch(ctx)
	err = s.Serve(ln)
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// watch tells mail programs about changes made elsewhere: mail the node
// collects in the background, messages deleted from the dashboard.
func (b *Bridge) watch(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
		for _, f := range b.Box.Folders() {
			b.sync(f)
		}
	}
}

// sync compares a folder with what mail programs were last told and tells
// them what changed: EXPUNGE for each message gone, EXISTS if there are
// new ones. Every change, the bridge's own or another's, is told here
// once, so none is told twice.
func (b *Bridge) sync(folder string) {
	entries, err := b.Box.Folder(folder)
	if err != nil {
		return
	}
	now := make([]uint32, len(entries))
	for i, e := range entries {
		now[i] = e.UID
	}
	b.syncMu.Lock()
	defer b.syncMu.Unlock()
	was, known := b.told[folder]
	if b.told == nil {
		b.told = map[string][]uint32{}
	}
	b.told[folder] = now
	if !known {
		return
	}
	for i := len(was) - 1; i >= 0; i-- { // highest first: each shifts those after it
		if _, found := slices.BinarySearch(now, was[i]); !found {
			b.send(&backend.ExpungeUpdate{Update: backend.NewUpdate("", folder), SeqNum: uint32(i + 1)})
		}
	}
	if len(now) > 0 && (len(was) == 0 || now[len(now)-1] > was[len(was)-1]) {
		status := imap.NewMailboxStatus(folder, []imap.StatusItem{imap.StatusMessages})
		status.Messages = uint32(len(now))
		b.send(&backend.MailboxUpdate{Update: backend.NewUpdate("", folder), MailboxStatus: status})
	}
}

// known is the UIDs mail programs have been told of in a folder.
func (b *Bridge) known(folder string) []uint32 {
	b.syncMu.Lock()
	told, ok := b.told[folder]
	b.syncMu.Unlock()
	if !ok { // a folder made since: start telling
		b.sync(folder)
		b.syncMu.Lock()
		told = b.told[folder]
		b.syncMu.Unlock()
	}
	return told
}

func (b *Bridge) send(u backend.Update) {
	if b.updates == nil {
		return
	}
	done := u.Done()                     // made before the server sees it: go-imap makes it lazily, unlocked
	stuck := time.After(5 * time.Second) // mail programs resync when they next open the folder
	select {
	case b.updates <- u:
	case <-stuck:
		return
	}
	select { // written before the command's OK, as IMAP expects
	case <-done:
	case <-stuck:
	}
}

// raw is a message, opened.
func (b *Bridge) raw(id string) ([]byte, error) {
	b.cacheMu.Lock()
	data, ok := b.cache[id]
	b.cacheMu.Unlock()
	if ok {
		return data, nil
	}
	data, err := b.Box.Raw(id)
	if err != nil {
		return nil, err
	}
	b.cacheMu.Lock()
	if b.cache == nil || len(b.cache) > 500 {
		b.cache = map[string][]byte{}
	}
	b.cache[id] = data
	b.cacheMu.Unlock()
	return data, nil
}

type imapBackend Bridge

// Updates feeds go-imap's broadcaster. (go-imap v1 reads each connection's
// state there without a lock, which the race detector reports; yggmail
// runs the same way. The worst case is a client missing a notice and
// catching up on its next check.)
func (ib *imapBackend) Updates() <-chan backend.Update { return ib.updates }

func (ib *imapBackend) Login(_ *imap.ConnInfo, username, password string) (backend.User, error) {
	b := (*Bridge)(ib)
	if err := b.login(username, password); err != nil {
		b.logf("mail bridge: IMAP login refused for %q", username)
		return nil, err
	}
	return &user{b: b, name: b.Box.Address()}, nil
}

type user struct {
	b    *Bridge
	name string
}

func (u *user) Username() string { return u.name }

func (u *user) ListMailboxes(bool) ([]backend.Mailbox, error) {
	var out []backend.Mailbox
	for _, f := range u.b.Box.Folders() {
		out = append(out, &mailbox{b: u.b, name: f})
	}
	return out, nil
}

func (u *user) GetMailbox(name string) (backend.Mailbox, error) {
	for _, f := range u.b.Box.Folders() {
		if f == name || (f == mail.Inbox && strings.EqualFold(name, mail.Inbox)) {
			return &mailbox{b: u.b, name: f}, nil
		}
	}
	return nil, backend.ErrNoSuchMailbox
}

func (u *user) CreateMailbox(name string) error { return u.b.Box.CreateFolder(name) }
func (u *user) DeleteMailbox(name string) error { return u.b.Box.DeleteFolder(name) }
func (u *user) RenameMailbox(string, string) error {
	return errors.New("folders can't be renamed; make a new one and move the messages")
}
func (u *user) Logout() error { return nil }

type mailbox struct {
	b    *Bridge
	name string
}

func (m *mailbox) Name() string { return m.name }

// specialUse tells mail programs which folder is which.
var specialUse = map[string]string{mail.Sent: `\Sent`, mail.Drafts: `\Drafts`, mail.Trash: `\Trash`, mail.Archive: `\Archive`, mail.Junk: `\Junk`}

func (m *mailbox) Info() (*imap.MailboxInfo, error) {
	info := &imap.MailboxInfo{Delimiter: "/", Name: m.name}
	if a, ok := specialUse[m.name]; ok {
		info.Attributes = []string{a}
	}
	return info, nil
}

func (m *mailbox) entries() ([]mail.Entry, error) { return m.b.Box.Folder(m.name) }

func (m *mailbox) Status(items []imap.StatusItem) (*imap.MailboxStatus, error) {
	entries, _, err := m.numbered(true)
	if err != nil {
		return nil, err
	}
	st := imap.NewMailboxStatus(m.name, items)
	st.Flags = []string{imap.SeenFlag, imap.AnsweredFlag, imap.FlaggedFlag, imap.DeletedFlag, imap.DraftFlag}
	st.PermanentFlags = append(slices.Clone(st.Flags), `\*`)
	unseen := uint32(0)
	for i, e := range entries {
		if !slices.Contains(e.Flags, imap.SeenFlag) {
			if st.UnseenSeqNum == 0 {
				st.UnseenSeqNum = uint32(i + 1)
			}
			unseen++
		}
	}
	for _, it := range items {
		switch it {
		case imap.StatusMessages:
			st.Messages = uint32(len(entries))
		case imap.StatusUidNext:
			if st.UidNext, err = m.b.Box.UIDNext(m.name); err != nil {
				return nil, err
			}
		case imap.StatusUidValidity:
			st.UidValidity = m.b.Box.Validity()
		case imap.StatusUnseen:
			st.Unseen = unseen
		case imap.StatusRecent:
			st.Recent = 0
		}
	}
	return st, nil
}

func (m *mailbox) SetSubscribed(bool) error { return nil }
func (m *mailbox) Check() error             { return nil }

// numbered returns the messages mail programs have been told of, with
// their sequence numbers. Plain FETCH, STORE and SEARCH mustn't be answered
// with EXPUNGE (RFC 3501 7.4.1), so they number messages as last told; UID
// commands may, so for them changes are told first.
func (m *mailbox) numbered(uid bool) ([]mail.Entry, []uint32, error) {
	if uid {
		m.b.sync(m.name)
	}
	entries, err := m.entries()
	if err != nil {
		return nil, nil, err
	}
	told := m.b.known(m.name)
	var out []mail.Entry
	var seqs []uint32
	for _, e := range entries {
		if i, ok := slices.BinarySearch(told, e.UID); ok { // others: not told of yet
			out = append(out, e)
			seqs = append(seqs, uint32(i+1))
		}
	}
	return out, seqs, nil
}

// pick returns the entries in seqset (UIDs if uid), with their sequence
// numbers.
func (m *mailbox) pick(uid bool, seqset *imap.SeqSet) ([]mail.Entry, []uint32, error) {
	entries, seqs, err := m.numbered(uid)
	if err != nil {
		return nil, nil, err
	}
	var out []mail.Entry
	var outSeqs []uint32
	for i, e := range entries {
		id := seqs[i]
		if uid {
			id = e.UID
		}
		if seqset.Contains(id) {
			out = append(out, e)
			outSeqs = append(outSeqs, seqs[i])
		}
	}
	return out, outSeqs, nil
}

func (m *mailbox) ListMessages(uid bool, seqset *imap.SeqSet, items []imap.FetchItem, ch chan<- *imap.Message) error {
	defer close(ch)
	entries, seqs, err := m.pick(uid, seqset)
	if err != nil {
		return err
	}
	for i, e := range entries {
		msg, err := m.fetch(e, seqs[i], items)
		if err != nil {
			m.b.logf("mail bridge: %s: %v", e.ID, err)
			continue
		}
		ch <- msg
	}
	return nil
}

func (m *mailbox) fetch(e mail.Entry, seq uint32, items []imap.FetchItem) (*imap.Message, error) {
	out := imap.NewMessage(seq, items)
	var raw []byte
	load := func() (textproto.Header, *bufio.Reader, error) {
		if raw == nil {
			var err error
			if raw, err = m.b.raw(e.ID); err != nil {
				return textproto.Header{}, nil, err
			}
		}
		r := bufio.NewReader(bytes.NewReader(raw))
		h, err := textproto.ReadHeader(r)
		return h, r, err
	}
	for _, item := range items {
		switch item {
		case imap.FetchUid:
			out.Uid = e.UID
		case imap.FetchFlags:
			out.Flags = e.Flags
		case imap.FetchInternalDate:
			out.InternalDate = time.UnixMilli(e.Received)
		case imap.FetchRFC822Size:
			if _, _, err := load(); err != nil {
				return nil, err
			}
			out.Size = uint32(len(raw))
		case imap.FetchEnvelope:
			h, _, err := load()
			if err != nil {
				return nil, err
			}
			out.Envelope, _ = backendutil.FetchEnvelope(h)
		case imap.FetchBody, imap.FetchBodyStructure:
			h, r, err := load()
			if err != nil {
				return nil, err
			}
			out.BodyStructure, _ = backendutil.FetchBodyStructure(h, r, item == imap.FetchBodyStructure)
		default:
			section, err := imap.ParseBodySectionName(item)
			if err != nil {
				continue
			}
			h, r, err := load()
			if err != nil {
				return nil, err
			}
			l, _ := backendutil.FetchBodySection(h, r, section)
			out.Body[section] = l
			if !section.Peek && !slices.Contains(e.Flags, imap.SeenFlag) {
				e.Flags = append(e.Flags, imap.SeenFlag)
				m.b.Box.SetFlags(e.ID, e.Flags)
				out.Items[imap.FetchFlags] = nil // a FETCH that sets \Seen says so (RFC 3501 6.4.5)
				out.Flags = e.Flags
			}
		}
	}
	return out, nil
}

func (m *mailbox) SearchMessages(uid bool, c *imap.SearchCriteria) ([]uint32, error) {
	entries, seqs, err := m.numbered(uid)
	if err != nil {
		return nil, err
	}
	opens := needsContent(c)
	var out []uint32
	for i, e := range entries {
		ent, _ := message.New(message.Header{}, strings.NewReader("")) // enough for flags, dates and numbers
		if opens {
			raw, err := m.b.raw(e.ID)
			if err != nil {
				continue
			}
			ent, err = message.Read(bytes.NewReader(raw))
			if err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err) {
				continue
			}
		}
		ok, err := backendutil.Match(ent, seqs[i], e.UID, time.UnixMilli(e.Received), e.Flags, c)
		if err != nil || !ok {
			continue
		}
		if uid {
			out = append(out, e.UID)
		} else {
			out = append(out, seqs[i])
		}
	}
	return out, nil
}

// needsContent says whether a search looks inside messages. Mail programs
// search often for flags alone (UNSEEN, DELETED), which needn't open every
// message.
func needsContent(c *imap.SearchCriteria) bool {
	if len(c.Header) > 0 || len(c.Body) > 0 || len(c.Text) > 0 || c.Larger > 0 || c.Smaller > 0 ||
		!c.SentSince.IsZero() || !c.SentBefore.IsZero() {
		return true
	}
	for _, n := range c.Not {
		if needsContent(n) {
			return true
		}
	}
	for _, o := range c.Or {
		if needsContent(o[0]) || needsContent(o[1]) {
			return true
		}
	}
	return false
}

func (m *mailbox) CreateMessage(flags []string, date time.Time, body imap.Literal) error {
	raw, err := io.ReadAll(io.LimitReader(body, mail.MaxSize+1))
	if err != nil {
		return err
	}
	if len(raw) > mail.MaxSize {
		return errors.New("message too large")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	err = m.b.Keep(ctx, raw, m.name, flags, date)
	m.b.sync(m.name)
	return err
}

func (m *mailbox) UpdateMessagesFlags(uid bool, seqset *imap.SeqSet, op imap.FlagsOp, flags []string) error {
	entries, seqs, err := m.pick(uid, seqset)
	if err != nil {
		return err
	}
	for i, e := range entries {
		e.Flags = backendutil.UpdateFlags(e.Flags, op, flags)
		if err := m.b.Box.SetFlags(e.ID, e.Flags); err != nil {
			return err
		}
		upd := imap.NewMessage(seqs[i], []imap.FetchItem{imap.FetchFlags, imap.FetchUid})
		upd.Flags, upd.Uid = e.Flags, e.UID
		m.b.send(&backend.MessageUpdate{Update: backend.NewUpdate("", m.name), Message: upd})
	}
	return nil
}

func (m *mailbox) CopyMessages(uid bool, seqset *imap.SeqSet, dest string) error {
	entries, _, err := m.pick(uid, seqset)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for _, e := range entries {
		raw, err := m.b.raw(e.ID)
		if err != nil {
			return err
		}
		if err := m.b.Keep(ctx, raw, dest, e.Flags, time.UnixMilli(e.Received)); err != nil {
			m.b.sync(dest)
			return err
		}
	}
	m.b.sync(dest)
	return nil
}

func (m *mailbox) MoveMessages(uid bool, seqset *imap.SeqSet, dest string) error {
	entries, _, err := m.pick(uid, seqset)
	if err != nil {
		return err
	}
	defer m.b.sync(dest)
	defer m.b.sync(m.name)
	for _, e := range entries {
		if err := m.b.Box.Move(e.ID, dest); err != nil {
			return err
		}
	}
	return nil
}

// Expunge deletes the messages marked \Deleted for good, from the group
// too. (Deleting in a mail program usually moves to Trash first; emptying
// Trash expunges.)
func (m *mailbox) Expunge() error {
	entries, err := m.entries()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var failed error
	for _, e := range entries {
		if !slices.Contains(e.Flags, imap.DeletedFlag) {
			continue
		}
		if err := m.b.Delete(ctx, e.ID); err != nil {
			failed = err
		}
	}
	m.b.sync(m.name)
	return failed
}

// Where the bridge listens unless told otherwise. (yggmail, which a member
// may also run, takes 1143 and 1025.)
const (
	DefaultIMAP = "127.0.0.1:2143"
	DefaultSMTP = "127.0.0.1:2587"
)
