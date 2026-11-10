package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"github.com/peterretief/yggstore/internal/light"
)

// cmdLogin is a member's side of the group login (docs/login.md, step 2),
// asked of their own node over Yggdrasil.
func cmdLogin(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	node := fs.String("node", "", "this machine's node (default: its Yggdrasil address, port 7400)")
	showLink := fs.Bool("link", false, "invite: print the link as well as the QR code")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `yggstore login invite [-link]   a passkey for the group login: a QR code to open on your phone
yggstore login keys              your passkeys for the group login
yggstore login forget ID         forget one (not the last)
`)
	}
	if len(args) == 0 {
		fs.Usage()
		os.Exit(2)
	}
	sub := args[0]
	fs.Parse(args[1:])
	addr := *node
	if addr == "" {
		addr = net.JoinHostPort(boxYggAddr(), "7400")
	}
	ask := func(method, path string, out any) error {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, method, "http://"+addr+light.MemberBase+path, nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return fmt.Errorf("this machine's node isn't answering at %s: %v", addr, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			return errors.New(strings.TrimSpace(string(msg)))
		}
		if out == nil {
			return nil
		}
		return json.NewDecoder(resp.Body).Decode(out)
	}
	switch {
	case sub == "invite" && fs.NArg() == 0:
		var got struct{ Link, Person string }
		if err := ask(http.MethodPost, "/invite", &got); err != nil {
			return err
		}
		q, err := qrcode.New(got.Link, qrcode.Medium)
		if err != nil {
			return err
		}
		fmt.Print(q.ToSmallString(false))
		fmt.Printf("Open this on your phone with its camera, then choose \"Make a passkey instead\".\n"+
			"It signs you in to the group login as %s. It works once, for a week.\n", got.Person)
		if *showLink {
			fmt.Println(got.Link)
		}
		return nil
	case sub == "keys" && fs.NArg() == 0:
		var got struct {
			Person string
			Keys   []light.Key
		}
		if err := ask(http.MethodGet, "/keys", &got); err != nil {
			return err
		}
		if len(got.Keys) == 0 {
			fmt.Printf("%s has no passkeys for the group login yet: yggstore login invite\n", got.Person)
			return nil
		}
		when := func(t int64) string {
			if t == 0 {
				return "never"
			}
			return time.Unix(t, 0).Format("2 Jan 2006 15:04")
		}
		for _, k := range got.Keys {
			kind := ""
			if k.Link {
				kind = " (invite link, no passkey)"
			}
			fmt.Printf("%-24s %s%s, made %s, last used %s\n", k.ID[:min(len(k.ID), 22)], k.Name, kind, when(k.Added), when(k.Used))
		}
		return nil
	case sub == "forget" && fs.NArg() == 1:
		keys := struct{ Keys []light.Key }{}
		if err := ask(http.MethodGet, "/keys", &keys); err != nil {
			return err
		}
		id := ""
		for _, k := range keys.Keys {
			if strings.HasPrefix(k.ID, fs.Arg(0)) {
				if id != "" {
					return errors.New("that matches more than one passkey: give more of its ID")
				}
				id = k.ID
			}
		}
		if id == "" {
			return errors.New("no passkey with that ID: yggstore login keys")
		}
		if err := ask(http.MethodPost, "/forget?id="+id, nil); err != nil {
			return err
		}
		fmt.Println("Forgotten: it won't sign you in any more.")
		return nil
	}
	fs.Usage()
	os.Exit(2)
	return nil
}
