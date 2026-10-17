package dashboard

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	netmail "net/mail"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/atomicfile"
	"github.com/peterretief/yggstore/internal/mail"
	"github.com/peterretief/yggstore/internal/peers"
)

// Forwarding a website's contact form: each message its form sends to this
// mailbox also goes to an ordinary email address, through the group's mail
// relay, once that address has confirmed with a code sent to it. It's per
// site, so the messages of a site run for someone else can go to them while
// the rest of the mail stays here. (People on a box forward all their mail
// instead; see hub/personsite.go.)

type siteForward struct {
	To        string   `json:"to"`
	CodeHash  string   `json:"code_hash,omitempty"` // of the code sent to To, until it is confirmed
	SentAt    int64    `json:"sent_at,omitempty"`   // when the code was sent (unix)
	Tries     int      `json:"tries,omitempty"`     // wrong codes typed
	Confirmed bool     `json:"confirmed,omitempty"` // the code came back
	Since     int64    `json:"since,omitempty"`     // unix ms: messages from then on go
	Done      []string `json:"done,omitempty"`      // messages forwarded already
}

// forwardInfo is what the page shows of it.
type forwardInfo struct {
	To        string `json:"to"`
	Confirmed bool   `json:"confirmed,omitempty"`
}

const (
	fwdCodeFor   = time.Hour   // how long a code works
	fwdCodeEvery = time.Minute // the least time between two codes
	fwdCodeTries = 5
	fwdMax       = 20  // messages forwarded in one pass
	fwdKeep      = 500 // forwarded messages remembered
)

// ForwardEvery is how often new contact messages are forwarded.
var ForwardEvery = 30 * time.Second

func (d *Dashboard) forwardPath(site string) string {
	return filepath.Join(d.cfg.SitesDir, site, "forward.json")
}

func (d *Dashboard) loadForward(site string) *siteForward {
	if d.cfg.SitesDir == "" {
		return nil
	}
	b, err := os.ReadFile(d.forwardPath(site))
	if err != nil {
		return nil
	}
	var f siteForward
	if json.Unmarshal(b, &f) != nil || f.To == "" {
		return nil
	}
	return &f
}

func (d *Dashboard) saveForward(site string, f *siteForward) error {
	if f == nil {
		err := os.Remove(d.forwardPath(site))
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
		return err
	}
	if err := os.MkdirAll(filepath.Dir(d.forwardPath(site)), 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(f)
	return atomicfile.Replace(d.forwardPath(site), b, 0o600)
}

func codeHash(code string) string {
	h := sha256.Sum256([]byte(code))
	return hex.EncodeToString(h[:])
}

// sendForwardCode starts forwarding a site's contact messages to an
// address: a code goes to it, to be typed on the dashboard.
func (d *Dashboard) sendForwardCode(ctx context.Context, site, to string) error {
	box, err := d.mailBox()
	if err != nil {
		return err
	}
	from := box.Address()
	if from == "" {
		return errors.New("this mailbox has no address to send from: set yours with yggstore mail address you@example.org")
	}
	a, err := netmail.ParseAddress(strings.TrimSpace(to))
	if err != nil || !strings.Contains(a.Address, "@") {
		return errors.New("that isn't an email address")
	}
	if strings.EqualFold(a.Address, from) {
		return errors.New("that's this mailbox's own address: the messages come here anyway")
	}
	if f := d.loadForward(site); f != nil && !f.Confirmed && time.Since(time.Unix(f.SentAt, 0)) < fwdCodeEvery {
		return errors.New("a code was sent a moment ago: wait a minute before asking for another")
	}
	n, _ := rand.Int(rand.Reader, big.NewInt(1000000))
	code := fmt.Sprintf("%06d", n.Int64())
	text := fmt.Sprintf("Your code is %s\n\n%s asked for the messages from the contact form on %s to be sent here too. "+
		"Give them this code to start. It works for an hour.\n\nIf you didn't expect this, ignore this message: nothing is sent here without the code.",
		code, d.me(), site)
	raw, rcpts, err := mail.Draft{From: from, Name: d.me(), To: a.Address, Subject: "Your code for messages from " + site, Text: text}.Compose()
	if err != nil {
		return err
	}
	list, err := peers.Load(d.cfg.PeersPath)
	if err != nil {
		return err
	}
	if err := mail.Send(ctx, d.cfg.Client, list, raw, rcpts); err != nil {
		return fmt.Errorf("the code could not be sent: %w", err)
	}
	return d.saveForward(site, &siteForward{To: a.Address, CodeHash: codeHash(code), SentAt: time.Now().Unix()})
}

// confirmForward checks the code sent to the address, and starts forwarding.
func (d *Dashboard) confirmForward(site, code string, now time.Time) error {
	f := d.loadForward(site)
	switch {
	case f == nil || f.CodeHash == "":
		return errors.New("ask for a code first")
	case f.Confirmed:
		return nil
	case now.Sub(time.Unix(f.SentAt, 0)) > fwdCodeFor:
		return errors.New("that code has run out: ask for a new one")
	case f.Tries >= fwdCodeTries:
		return errors.New("too many wrong codes: ask for a new one")
	}
	if codeHash(strings.TrimSpace(code)) != f.CodeHash {
		f.Tries++
		d.saveForward(site, f)
		return errors.New("that isn't the code that was sent")
	}
	f.Confirmed, f.CodeHash, f.Tries, f.Since = true, "", 0, now.UnixMilli()
	if err := d.saveForward(site, f); err != nil {
		return err
	}
	d.event("ok", fmt.Sprintf("website %s: its contact form's messages go to %s too now", site, f.To))
	return nil
}

// runForwards sends on new contact messages for every site that forwards.
func (d *Dashboard) runForwards(ctx context.Context) {
	t := time.NewTicker(ForwardEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		d.forwardAll(ctx)
	}
}

func (d *Dashboard) forwardAll(ctx context.Context) {
	pub, err := d.publisher()
	if err != nil {
		return
	}
	for _, site := range pub.Sites() {
		if f := d.loadForward(site); f != nil && f.Confirmed {
			if err := d.forwardSite(ctx, site, f); err != nil {
				d.logOnce("forward "+site, fmt.Sprintf("website %s: forwarding its messages to %s: %v (will try again)", site, f.To, err))
			} else {
				d.logOnce("forward "+site, "")
			}
		}
	}
}

// logOnce shows a problem once, until it changes or goes ("" clears it).
func (d *Dashboard) logOnce(key, msg string) {
	d.mu.Lock()
	if d.fwdErr == nil {
		d.fwdErr = map[string]string{}
	}
	same := d.fwdErr[key] == msg
	d.fwdErr[key] = msg
	d.mu.Unlock()
	if !same && msg != "" {
		d.event("warn", msg)
	}
}

func (d *Dashboard) forwardSite(ctx context.Context, site string, f *siteForward) error {
	box, err := d.mailBox()
	if err != nil {
		return err
	}
	from := box.Address()
	if from == "" {
		return errors.New("this mailbox has no address to send from")
	}
	msgs, err := box.List()
	if err != nil {
		return err
	}
	done := map[string]bool{}
	for _, id := range f.Done {
		done[id] = true
	}
	var list []peers.Peer
	n := 0
	for _, s := range msgs {
		if done[s.ID] || s.Sent || s.Error != "" || s.Received < f.Since || n >= fwdMax ||
			!strings.Contains(strings.ToLower(s.From), "contact-form@"+site) {
			continue
		}
		m, err := box.Read(s.ID)
		if err != nil {
			continue
		}
		if list == nil {
			if list, err = peers.Load(d.cfg.PeersPath); err != nil {
				return err
			}
		}
		replyTo := m.ReplyTo
		if _, err := netmail.ParseAddress(replyTo); err != nil {
			replyTo = ""
		}
		text := fmt.Sprintf("A message from the contact form on %s.\n\n%s", site, m.Text)
		raw, rcpts, err := mail.Draft{From: from, Name: "Contact form on " + site, To: f.To,
			Subject: m.Subject, Text: text, ReplyTo: replyTo}.Compose()
		if err == nil {
			err = mail.Send(ctx, d.cfg.Client, list, raw, rcpts)
		}
		if err != nil {
			return err
		}
		n++
		f.Done = append(f.Done, s.ID)
		if len(f.Done) > fwdKeep {
			f.Done = f.Done[len(f.Done)-fwdKeep:]
		}
		if err := d.saveForward(site, f); err != nil {
			return err
		}
	}
	if n > 0 {
		d.event("info", fmt.Sprintf("website %s: forwarded %d message(s) to %s", site, n, f.To))
	}
	return nil
}
