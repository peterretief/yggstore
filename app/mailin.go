package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	netmail "net/mail"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/mail"
	"github.com/peterretief/yggstore/internal/share"
)

// Mail-in (docs/app-workflow.md in yggstore): mail to an app's address
// becomes a row of one of its tables.
//
//	CREATE TABLE requests (
//	  -- mail-in: leave@example.org, members only, acknowledge
//	  id TEXT PRIMARY KEY,
//	  login TEXT,          -- from: address
//	  sender_name TEXT,    -- from: name
//	  subject TEXT,        -- mail: subject
//	  body LONGTEXT,       -- mail: text
//	  received DATETIME,   -- mail: received
//	  member BOOLEAN       -- mail: in the group
//	);
//
// The app's address is given with "yggstore app address NAME LABEL", and
// the app started with -mail-in and the folder that prints: the machine's
// node puts the app's mail there, sealed for the app's own key. "members
// only" refuses mail that didn't come from a member through the group's own
// mail (anyone can put any From on mail from outside); "acknowledge" answers
// the sender once, with -mail. Attachments go into the app's documents,
// under Mail in. A message that can't become a row is kept in the folder's
// "refused", and said in the log.

type mailIn struct {
	Address                  string
	MembersOnly, Acknowledge bool
}

var mailInLine = regexp.MustCompile(`(?i)--\s*mail-in:\s*(.+)$`)

func readMailIn(t *table, text string) error {
	parts := strings.Split(text, ",")
	m := &mailIn{Address: strings.ToLower(strings.TrimSpace(parts[0]))}
	if a, err := netmail.ParseAddress(m.Address); err != nil || a.Address != m.Address {
		return fmt.Errorf("mail-in: %q isn't an address", m.Address)
	}
	for _, p := range parts[1:] {
		switch strings.ToLower(strings.Join(strings.Fields(p), " ")) {
		case "members only":
			m.MembersOnly = true
		case "acknowledge":
			m.Acknowledge = true
		default:
			return fmt.Errorf("mail-in: %q isn't \"members only\" or \"acknowledge\"", strings.TrimSpace(p))
		}
	}
	t.MailIn = m
	return nil
}

// mailNote is a column's "from: address" or "mail: subject", and so on.
func mailNote(c *column, kind, what string) error {
	w := strings.ToLower(strings.Join(strings.Fields(what), " "))
	w, _, _ = strings.Cut(w, " (") // "address (owner)" reads as address
	switch {
	case kind == "from" && (w == "address" || w == "name"):
		c.Mail = "from " + w
	case kind == "mail" && (w == "subject" || w == "text" || w == "received" || w == "in the group"):
		c.Mail = w
	default:
		return fmt.Errorf("%s: %s isn't one of from: address, from: name, mail: subject, mail: text, mail: received, mail: in the group", kind, what)
	}
	return nil
}

// mailFrom is the address a table's emails come from: its own, if it takes
// mail, so answers come back to it; otherwise the node's ("").
func (t table) mailFrom() string {
	if t.MailIn != nil {
		return t.MailIn.Address
	}
	return ""
}

// sameAddress says whether two addresses are one, counting an address and
// its internal twin (anna@example.org and anna@example.internal) as one.
func sameAddress(a, b string) bool {
	la, da, _ := strings.Cut(a, "@")
	lb, db, _ := strings.Cut(b, "@")
	if la != lb {
		return false
	}
	first := func(d string) string { l, _, _ := strings.Cut(d, "."); return l }
	return da == db || ((strings.HasSuffix(da, ".internal") || strings.HasSuffix(db, ".internal")) && first(da) == first(db))
}

// mailTable is the table mail to an address goes to.
func mailTable(tables []table, to string) *table {
	var only *table
	n := 0
	for i := range tables {
		if m := tables[i].MailIn; m != nil {
			if sameAddress(m.Address, to) {
				return &tables[i]
			}
			only = &tables[i]
			n++
		}
	}
	if n == 1 { // the app's one address, under another name
		return only
	}
	return nil
}

// insert makes a row from fields, starting its steps if it has them, and
// returns its key.
func (d *data) insert(t table, fields map[string]any) (string, error) {
	if t.Steps != nil {
		fields[statusColumn] = t.Start
	}
	if d.sync != nil {
		return d.saveEntry(t, "", fields)
	}
	var names, marks []string
	var args []any
	if c := t.col(t.Key); c != nil && c.Random {
		names, marks, args = append(names, quote(t.Key)), append(marks, "?"), append(args, newID())
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		names, marks, args = append(names, quote(k)), append(marks, "?"), append(args, fields[k])
	}
	res, err := d.db.Exec("INSERT INTO "+quote(t.Name)+" ("+strings.Join(names, ", ")+") VALUES ("+strings.Join(marks, ", ")+")", args...)
	if err != nil {
		return "", err
	}
	return d.madeID(t, res, names, args), nil
}

// noAnswer is mail not to answer: automatic, or from nobody who reads it.
func noAnswer(h netmail.Header, from string) bool {
	if a := strings.ToLower(h.Get("Auto-Submitted")); a != "" && a != "no" {
		return true
	}
	switch strings.ToLower(h.Get("Precedence")) {
	case "bulk", "list", "junk":
		return true
	}
	local, _, _ := strings.Cut(strings.ToLower(from), "@")
	return from == "" || h.Get("List-Id") != "" || strings.Contains(local, "noreply") || strings.Contains(local, "no-reply") || local == "mailer-daemon" || local == "postmaster"
}

// takeMail makes rows of the messages in an app folder's inbox.
func (d *data) takeMail(dir string, dc *docs) (taken int, err error) {
	key, err := share.Load(filepath.Join(dir, "sharing.key"))
	if err != nil {
		return 0, err
	}
	list, _ := filepath.Glob(filepath.Join(dir, "inbox", "*.sealed"))
	sort.Strings(list)
	for _, path := range list {
		base := strings.TrimSuffix(path, ".sealed")
		if err := d.takeOne(key, base, dc); err != nil {
			log.Printf("mail-in: %s: %v (kept in %s)", filepath.Base(base), err, filepath.Join(dir, "refused"))
			os.MkdirAll(filepath.Join(dir, "refused"), 0o700)
			for _, ext := range []string{".sealed", ".json"} {
				os.Rename(base+ext, filepath.Join(dir, "refused", filepath.Base(base)+ext))
			}
			os.WriteFile(filepath.Join(dir, "refused", filepath.Base(base)+".why"), []byte(err.Error()+"\n"), 0o600)
			continue
		}
		os.Remove(base + ".json")
		os.Remove(base + ".sealed")
		taken++
	}
	return taken, nil
}

func (d *data) takeOne(key *share.Identity, base string, dc *docs) error {
	var meta mail.Meta
	if b, err := os.ReadFile(base + ".json"); err == nil {
		json.Unmarshal(b, &meta)
	}
	sealed, err := os.ReadFile(base + ".sealed")
	if err != nil {
		return err
	}
	raw, err := mail.Open(key, sealed)
	if err != nil {
		return errors.New("it isn't sealed for this app")
	}
	hdr, err := netmail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return errors.New("it isn't an email message")
	}
	m, err := mail.Parse(raw)
	if err != nil {
		return err
	}
	tables, err := d.design()
	if err != nil {
		return err
	}
	to := strings.ToLower(strings.TrimSpace(hdr.Header.Get("Delivered-To")))
	t := mailTable(tables, to)
	if t == nil {
		return fmt.Errorf("no table takes mail to %q", to)
	}
	sender, name := "", ""
	if a, err := netmail.ParseAddress(m.From); err == nil {
		sender, name = strings.ToLower(a.Address), a.Name
	}
	if meta.InGroup != "" {
		sender = meta.InGroup // checked by the group, unlike a From
	} else if t.MailIn.MembersOnly {
		return fmt.Errorf("from %s, outside the group: %s takes members' mail only", sender, t.MailIn.Address)
	}
	received := time.UnixMilli(meta.Received)
	if meta.Received == 0 {
		received = time.Now()
	}
	fields := map[string]any{}
	for _, c := range t.Cols {
		var v any
		switch c.Mail {
		case "":
			continue
		case "from address":
			v = sender
		case "from name":
			v = name
		case "subject":
			v = m.Subject
		case "text":
			v = m.Text
		case "received":
			switch c.kind() {
			case "date":
				v = received.Format(time.DateOnly)
			case "datetime-local":
				v = received.Format("2006-01-02T15:04")
			default:
				v = received.Format("2006-01-02 15:04")
			}
		case "in the group":
			if k := c.kind(); k == "checkbox" || k == "number" {
				v = map[bool]int{true: 1, false: 0}[meta.InGroup != ""]
			} else {
				v = map[bool]string{true: "yes", false: "no"}[meta.InGroup != ""]
			}
		}
		fields[c.Name] = v
	}
	id, err := d.insert(*t, fields)
	if err != nil {
		return errors.New(plainError(err))
	}
	log.Printf("mail-in: %s from %s is %s %s", t.MailIn.Address, sender, t.Name, id)
	d.made(*t, id, sender)
	if dc != nil {
		for _, a := range m.Attachments {
			n := strings.Map(func(r rune) rune {
				if strings.ContainsRune(`/\:*?"<>|`, r) || r < 32 {
					return '_'
				}
				return r
			}, a.Name)
			if !validDocName(n) {
				n = "attachment"
			}
			if err := dc.store("", "Mail in/"+human(t.Name)+"/"+id+"/"+n, sender, bytes.NewReader(a.Data())); err != nil {
				log.Printf("mail-in: %s %s: keeping %s: %v", t.Name, id, a.Name, err)
			}
		}
	}
	if t.MailIn.Acknowledge && d.notify.send != nil && !noAnswer(hdr.Header, sender) {
		subject := strings.TrimSpace(m.Subject)
		if subject == "" {
			subject = "your message"
		}
		body := fmt.Sprintf("Thank you: we have your message, \"%s\", and will be in touch.\n", subject)
		go func() {
			if err := d.notify.send(t.mailFrom(), []string{sender}, "Re: "+subject, body); err != nil {
				log.Printf("mail-in: answering %s: %v", sender, err)
			}
		}()
	}
	return nil
}

// takeMailEvery makes rows of an app folder's mail, every so often, for good.
func (d *data) takeMailEvery(dir string, dc *docs, every time.Duration) {
	for {
		if _, err := d.takeMail(dir, dc); err != nil {
			log.Printf("mail-in: %v", err)
		}
		time.Sleep(every)
	}
}
