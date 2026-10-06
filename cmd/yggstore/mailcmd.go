package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/mail"
	"github.com/peterretief/yggstore/internal/share"
	"github.com/peterretief/yggstore/internal/transport"
)

const mailUsage = `yggstore mail: the group's email (see docs/mail.md)

  yggstore mail address ADDRESS   this machine's entry for the mail Worker's MAILBOXES
  yggstore mail token   FILE      make the Worker's token (if FILE is missing) and print it
  yggstore mail list              your mailbox, newest first
  yggstore mail read    ID        one message (-raw for it as it arrived)

Options: -mailbox ~/.yggstore/mail, -sharing-key ~/.yggstore/sharing.key.
`

func cmdMail(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, mailUsage)
		os.Exit(2)
	}
	sub := args[0]
	fs := flag.NewFlagSet("mail "+sub, flag.ExitOnError)
	boxDir := fs.String("mailbox", filepath.Join(yggstoreHome(), "mail"), "your mailbox")
	keyPath := fs.String("sharing-key", filepath.Join(yggstoreHome(), "sharing.key"), "your sharing key")
	tname := fs.String("transport", "ygg", "ygg or loopback (address)")
	raw := fs.Bool("raw", false, "print the message as it arrived (read)")
	fs.Parse(args[1:])
	box := func() (*mail.Box, error) {
		id, err := share.Load(*keyPath)
		if err != nil {
			return nil, fmt.Errorf("sharing key: %w", err)
		}
		return &mail.Box{Dir: *boxDir, ID: id}, nil
	}

	switch sub {
	case "address":
		if fs.NArg() != 1 || !strings.Contains(fs.Arg(0), "@") {
			return errors.New("usage: yggstore mail address you@example.org")
		}
		id, err := share.LoadOrCreate(*keyPath)
		if err != nil {
			return fmt.Errorf("sharing key: %w", err)
		}
		t, err := transport.ByName(*tname)
		if err != nil {
			return err
		}
		ip, err := t.LocalIP()
		if err != nil {
			return err
		}
		entry, _ := json.Marshal(map[string]string{"node": ip.String(), "code": id.Code()})
		fmt.Printf("%q: %s\n", strings.ToLower(fs.Arg(0)), entry)
		return nil

	case "token":
		if fs.NArg() != 1 {
			return errors.New("usage: yggstore mail token FILE")
		}
		path := fs.Arg(0)
		if b, err := os.ReadFile(path); err == nil {
			fmt.Println(strings.TrimSpace(string(b)))
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		secret := make([]byte, 32)
		rand.Read(secret)
		token := base64.RawURLEncoding.EncodeToString(secret)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		if _, err := f.WriteString(token + "\n"); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		fmt.Println(token)
		return nil

	case "list":
		b, err := box()
		if err != nil {
			return err
		}
		list, err := b.List()
		if err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Println("no mail")
		}
		for _, s := range list {
			if s.Error != "" {
				fmt.Printf("%s  (%s)\n", s.ID, s.Error)
				continue
			}
			fmt.Printf("%s  %s  %-30.30s  %s\n", s.ID, time.UnixMilli(s.Received).Format("2006-01-02 15:04"), s.From, s.Subject)
		}
		return nil

	case "read":
		if fs.NArg() != 1 {
			return errors.New("usage: yggstore mail read ID")
		}
		b, err := box()
		if err != nil {
			return err
		}
		if *raw {
			data, err := b.Raw(fs.Arg(0))
			if err != nil {
				return err
			}
			os.Stdout.Write(data)
			return nil
		}
		m, err := b.Read(fs.Arg(0))
		if err != nil {
			return err
		}
		fmt.Printf("From:    %s\nTo:      %s\n", m.From, m.To)
		if m.Cc != "" {
			fmt.Printf("Cc:      %s\n", m.Cc)
		}
		if m.Date != 0 {
			fmt.Printf("Date:    %s\n", time.UnixMilli(m.Date).Format("Mon 2 Jan 2006 15:04"))
		}
		fmt.Printf("Subject: %s\n\n%s\n", m.Subject, m.Text)
		for _, a := range m.Attachments {
			fmt.Printf("\n[attachment: %s, %s, %s]", a.Name, a.Type, size(int64(a.Size)))
		}
		if len(m.Attachments) > 0 {
			fmt.Println()
		}
		return nil
	}
	fmt.Fprint(os.Stderr, mailUsage)
	os.Exit(2)
	return nil
}
