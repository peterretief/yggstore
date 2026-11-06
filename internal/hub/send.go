package hub

import (
	"context"
	"errors"
	"fmt"
	netmail "net/mail"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/mail"
)

// Writing mail from the mail page: someone on the box sends from their
// own group address, never another. What they may send is limited, more
// so for a trial account (trial.go), so that nobody can use the group's
// domain for spam: a message goes to at most SendPerMessage people, and at most
// SendPerDay people a day in all. Every message sent is logged on the box.
const (
	TrialSendPerMessage = 5
	TrialSendPerDay     = 20
	SendPerMessage      = 20
	SendPerDay          = 200
	maxSendText         = 100 << 10
	// TrialSendAfter is how long a trial account waits before it may send
	// mail, so nobody signs up just to send spam from the group's domain.
	TrialSendAfter = 24 * time.Hour
)

// sendLimits are what p may send.
func sendLimits(p Person) (perMessage, perDay int) {
	if p.trial() {
		return TrialSendPerMessage, TrialSendPerDay
	}
	return SendPerMessage, SendPerDay
}

// SendRequest is what the mail page sends.
type SendRequest struct {
	To      string `json:"to"`
	Cc      string `json:"cc"`
	Subject string `json:"subject"`
	Text    string `json:"text"`
	ReplyTo string `json:"reply_to"` // the ID of the message answered, if any
	From    string `json:"from"`     // an address at a domain of their own; "": their address in the group
}

var sendLocal = regexp.MustCompile(`^[a-z0-9][a-z0-9._+-]{0,63}$`)

// chooseFrom is the address a message goes from: want, if it's at one of
// their own domains, or their address in the group. Nobody sends as anyone
// else: the box decides, whatever the page asks for.
func chooseFrom(want, group string, own []string) (string, error) {
	want = strings.ToLower(strings.TrimSpace(want))
	if want == "" || want == group {
		if group == "" {
			return "", errors.New("Say which address it's from.")
		}
		return group, nil
	}
	local, domain, ok := strings.Cut(want, "@")
	if !ok || !sendLocal.MatchString(local) || !slices.Contains(own, domain) {
		return "", fmt.Errorf("You can send from %s, or any address at your own domain.", strings.TrimSpace(group+" "+strings.Join(own, " ")))
	}
	return want, nil
}

// recipients checks a To or Cc list: plain addresses, comma-separated.
func recipients(list string) ([]string, error) {
	if strings.TrimSpace(list) == "" {
		return nil, nil
	}
	as, err := netmail.ParseAddressList(list)
	if err != nil {
		return nil, fmt.Errorf("%q isn't a list of email addresses", list)
	}
	var out []string
	for _, a := range as {
		out = append(out, strings.ToLower(a.Address))
	}
	return out, nil
}

// reserve counts n more people p sends to now, if their limits allow it.
func (s *HubState) reserve(person string, n int, now time.Time) error {
	i := s.person(person)
	if i < 0 || !s.People[i].Active() || s.People[i].leaving() {
		return errors.New("you're no longer on this box")
	}
	p := &s.People[i]
	if from := time.Unix(p.Added, 0).Add(TrialSendAfter); p.trial() && now.Before(from) {
		return fmt.Errorf("New accounts can send mail after their first day: from %s.", from.Format("15:04 on 2 January"))
	}
	per, day := sendLimits(*p)
	if n > per {
		return fmt.Errorf("A message can go to at most %d people.", per)
	}
	p.Sent = slices.DeleteFunc(p.Sent, func(t int64) bool { return now.Sub(time.Unix(t, 0)) >= 24*time.Hour })
	if len(p.Sent)+n > day {
		next := time.Unix(p.Sent[0], 0).Add(24 * time.Hour)
		return fmt.Errorf("You've written to %d people in the last day, the most you may. You can send again from %s.", len(p.Sent), next.Format("15:04 on 2 January"))
	}
	for range n {
		p.Sent = append(p.Sent, now.Unix())
	}
	return nil
}

// send writes and sends p's message from from (their group address), and
// keeps it in their Sent.
func (f *filesPage) send(ctx context.Context, p Person, from string, box *mail.Box, req SendRequest) error {
	to, err := recipients(req.To)
	if err != nil {
		return err
	}
	cc, err := recipients(req.Cc)
	if err != nil {
		return err
	}
	all := append(to, cc...)
	switch {
	case len(to) == 0:
		return errors.New("Say who it's to.")
	case len(req.Text) > maxSendText:
		return errors.New("That's too long for one message.")
	case len(req.Subject) > 200:
		return errors.New("Make the subject shorter.")
	}
	d := mail.Draft{From: from, Name: p.Name, To: strings.Join(to, ", "), Cc: strings.Join(cc, ", "), Subject: req.Subject, Text: req.Text}
	if req.ReplyTo != "" {
		if m, err := box.Read(req.ReplyTo); err == nil && m.MessageID != "" {
			d.InReplyTo, d.References = m.MessageID, strings.TrimSpace(m.References+" "+m.MessageID)
		}
	}
	raw, rcpts, err := d.Compose()
	if err != nil {
		return err
	}
	now := time.Now()
	if _, err := Update(ctx, f.u.cfg.StatePath, func(s *HubState) error { return s.reserve(p.Name, len(rcpts), now) }); err != nil {
		return err
	}
	ps := f.u.cfg.People
	online, err := ps.cfg.Online(ctx)
	if err == nil {
		err = mail.Send(ctx, ps.cfg.Client, online, raw, rcpts)
	}
	if err != nil {
		// Not sent: it doesn't count.
		Update(ctx, f.u.cfg.StatePath, func(s *HubState) error {
			if i := s.person(p.Name); i >= 0 {
				q := &s.People[i]
				for range len(rcpts) {
					if j := slices.Index(q.Sent, now.Unix()); j >= 0 {
						q.Sent = slices.Delete(q.Sent, j, j+1)
					}
				}
			}
			return nil
		})
		return fmt.Errorf("It couldn't be sent just now: %w", err)
	}
	box.KeepSent(ctx, ps.cfg.Client, online, raw)
	f.u.cfg.Logf("hub: %s sent mail from %s to %s (%q)", p.Name, from, strings.Join(all, ", "), req.Subject)
	return nil
}
