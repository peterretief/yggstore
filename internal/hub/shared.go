package hub

import (
	"context"
	"encoding/json"
	"fmt"
	netmail "net/mail"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/atomicfile"
	"github.com/peterretief/yggstore/internal/mail"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/share"
)

// Shared mailboxes: every box has support, sales and accounts (the hub's
// -shared-mail), whose local addresses (support@local.4 on localbox-004) take
// mail from inside the group. Anyone on the box's own network opens them
// from its info page (http://mail.local/info) with no password; nobody
// opens them from anywhere else (webmail.go).
//
// A shared mailbox can also pass a copy of what comes in on to an address
// for the whole group (-shared-copy support=signs-support@example.internal):
// a Notes-style app there makes each one a record, saying which box it came
// through. The copy is marked as passed on (Auto-Submitted), and a box never
// passes on mail a program sent, or mail from where it copies to, so the
// app's own emails back to the box don't go round again.

// DefaultShared are the shared mailboxes a box has unless told otherwise.
const DefaultShared = "support,sales,accounts"

// SharedNames reads a comma-separated list of mailbox names.
func SharedNames(list string) []string {
	var out []string
	for _, n := range strings.Split(list, ",") {
		if n = strings.ToLower(strings.TrimSpace(n)); n != "" && !unsafeName.MatchString(n) && strings.Trim(n, ".-") == n {
			out = append(out, n)
		}
	}
	return out
}

// EnsureShared adds the shared mailboxes the box hasn't got, and gives
// back the ones it added and the ones it couldn't because a person there
// has the name already.
func (s *HubState) EnsureShared(names []string, dir string, now time.Time) (added, taken []string) {
	for _, n := range names {
		if i := s.person(n); i >= 0 {
			if !s.People[i].Shared {
				taken = append(taken, n)
			}
			continue
		}
		s.People = append(s.People, Person{Name: n, Folder: "yggstore-p-" + shortHash(n), Path: filepath.Join(dir, n),
			Added: now.Unix(), Shared: true})
		added = append(added, n)
	}
	return added, taken
}

// SharedPeople are the box's shared mailboxes that are on.
func (s HubState) SharedPeople() []Person {
	var out []Person
	for _, p := range s.People {
		if p.Shared && p.Active() {
			out = append(out, p)
		}
	}
	return out
}

// SharedCopies reads -shared-copy: "support=a@x,sales=b@x".
func SharedCopies(list string) (map[string]string, error) {
	out := map[string]string{}
	for _, kv := range strings.Split(list, ",") {
		if kv = strings.TrimSpace(kv); kv == "" {
			continue
		}
		k, v, ok := strings.Cut(kv, "=")
		k = strings.ToLower(strings.TrimSpace(k))
		a, err := netmail.ParseAddress(strings.TrimSpace(v))
		if !ok || k == "" || err != nil {
			return nil, fmt.Errorf("%q: give it as MAILBOX=ADDRESS", kv)
		}
		out[k] = strings.ToLower(a.Address)
	}
	return out, nil
}

// SetCopies sets where each shared mailbox passes its mail on to; one
// not named stops. A new address starts from now, not with the old mail.
func (s *HubState) SetCopies(to map[string]string, now time.Time) (changed []string) {
	for i, p := range s.People {
		if !p.Shared {
			continue
		}
		if a := to[strings.ToLower(p.Name)]; a != p.CopyTo {
			s.People[i].CopyTo, s.People[i].CopySince = a, now.UnixMilli()
			changed = append(changed, p.Name)
		}
	}
	return changed
}

const copiedFile = "copied.json"

// sendShared sends a copy (a test's own, in tests).
var sendShared = mail.Send

// sameMailbox says whether two addresses are one, an address and its
// .internal twin counted as the same.
func sameMailbox(a, b string) bool {
	la, da, _ := strings.Cut(strings.ToLower(a), "@")
	lb, db, _ := strings.Cut(strings.ToLower(b), "@")
	label := func(d string) string { l, _, _ := strings.Cut(d, "."); return l }
	return la == lb && (da == db || ((strings.HasSuffix(da, ".internal") || strings.HasSuffix(db, ".internal")) && label(da) == label(db)))
}

// copyShared passes a shared mailbox's new mail on to its CopyTo, from
// its local address, with the sender kept for replies (Reply-To) and in
// X-Original-From, and the box in X-Box.
func (ps *PeopleSites) copyShared(ctx context.Context, list []peers.Peer, p Person, key *share.Identity) {
	from := localAddress(list, ps.cfg.Self, p.Name)
	boxName := ""
	for _, q := range list {
		if q.IP() == ps.cfg.Self {
			boxName = q.Machine()
		}
	}
	if from == "" {
		return
	}
	box := &mail.Box{Dir: ps.mailDir(p), ID: key}
	path := filepath.Join(ps.cfg.Dir, p.Folder, copiedFile)
	var done forwarded
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &done)
	}
	sent := map[string]bool{}
	for _, id := range done.IDs {
		sent[id] = true
	}
	msgs, err := box.List()
	if err != nil {
		return
	}
	var online []peers.Peer
	n := 0
	for _, s := range msgs {
		if sent[s.ID] || s.Sent || s.Received < p.CopySince || s.Error != "" || n >= forwardMax {
			continue
		}
		m, err := box.Read(s.ID)
		if err != nil {
			continue
		}
		skip := m.Auto != "" // a program's: an app's email, an auto-reply, a copy
		if a, err := netmail.ParseAddress(m.From); err != nil || sameMailbox(a.Address, p.CopyTo) || sameMailbox(a.Address, from) {
			skip = true
		}
		if !skip {
			if online == nil {
				if online, err = ps.cfg.Online(ctx); err != nil {
					return
				}
			}
			replyTo := m.ReplyTo
			if replyTo == "" {
				replyTo = m.From
			}
			text := m.Text
			if len(m.Attachments) > 0 {
				text += fmt.Sprintf("\n\n(%d attachment(s): in %s on %s.)", len(m.Attachments), from, boxName)
			}
			raw, rcpts, err := mail.Draft{From: from, Name: strings.ToUpper(p.Name[:1]) + p.Name[1:] + " at " + boxName, To: p.CopyTo,
				Subject: m.Subject, Text: text, ReplyTo: replyTo, Auto: "auto-forwarded",
				Extra: []string{"X-Box: " + boxName, "X-Original-From: " + m.From, "X-Original-To: " + m.To}}.Compose()
			if err == nil {
				err = sendShared(ctx, ps.cfg.Client, online, raw, rcpts)
			}
			if err != nil {
				ps.logOnce(p.Name, "passing its mail on to %s: %v (will try again)", p.CopyTo, err)
				return
			}
			n++
		}
		done.IDs = append(done.IDs, s.ID)
		b, _ := json.Marshal(done)
		atomicfile.Replace(path, b, 0o600)
	}
	if n > 0 {
		ps.cfg.Logf("%s: passed %d message(s) on to %s", p.Name, n, p.CopyTo)
	}
}
