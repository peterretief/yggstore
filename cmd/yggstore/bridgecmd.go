package main

import (
	"context"
	"log"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/dashboard"
	"github.com/peterretief/yggstore/internal/mail"
	"github.com/peterretief/yggstore/internal/mailbridge"
	"github.com/peterretief/yggstore/internal/msg"
	"github.com/peterretief/yggstore/internal/peers"
)

// startMailBridge serves the dashboard's mailbox to mail programs on this
// machine (see package mailbridge).
func startMailBridge(ctx context.Context, d *dashboard.Dashboard, peersPath, pwPath, imapAddr, smtpAddr string) {
	box, err := d.MailBox()
	if err != nil {
		log.Printf("mail bridge: not started: %v", err)
		return
	}
	pw, err := msg.LoadOrCreateToken(pwPath)
	if err != nil {
		log.Printf("mail bridge: not started: password file: %v", err)
		return
	}
	c := client.New()
	c.HTTP.Timeout = 2 * time.Minute
	list := func() []peers.Peer {
		l, _ := peers.Load(peersPath)
		return l
	}
	br := &mailbridge.Bridge{Box: box, Password: pw, Log: log.Printf,
		Keep: func(ctx context.Context, raw []byte, folder string, flags []string, date time.Time) error {
			id, err := box.Keep(ctx, c, list(), raw, folder, flags, date)
			if err != nil && id != "" {
				log.Printf("mail bridge: %s: %v (will be kept here only)", id, err)
				return nil
			}
			return err
		},
		Delete: func(ctx context.Context, id string) error { return box.Delete(ctx, c, id) },
		Send: func(ctx context.Context, raw []byte, rcpts []string) error {
			return mail.Send(ctx, c, list(), raw, rcpts)
		},
	}
	for _, s := range []struct {
		addr  string
		serve func(context.Context, string) error
		what  string
	}{{imapAddr, br.ServeIMAP, "IMAP"}, {smtpAddr, br.ServeSMTP, "SMTP"}} {
		if s.addr == "" {
			continue
		}
		go func() {
			log.Printf("mail bridge: %s for mail programs on %s", s.what, s.addr)
			if err := s.serve(ctx, s.addr); err != nil {
				log.Printf("mail bridge: %s: %v", s.what, err)
			}
		}()
	}
}
