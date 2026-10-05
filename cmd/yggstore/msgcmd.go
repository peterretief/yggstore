package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/msg"
)

const msgUsage = `yggstore msg: message the group's nodes, through this machine's node

  yggstore msg send   NODE TEXT          a direct message to one node (TEXT "-" reads stdin)
  yggstore msg pub    TOPIC TEXT         publish to everyone subscribed to TOPIC
  yggstore msg sub    TOPIC              subscribe (recent history arrives too)
  yggstore msg unsub  TOPIC
  yggstore msg read   [-topic T] [-n 20] [-follow]
                                         show messages; "direct" as T for direct ones
  yggstore msg status                    subscriptions, who follows what, undelivered

Options: -api 127.0.0.1:7401, -token ~/.yggstore/msg.token, -type TYPE (send, pub).
`

func cmdMsg(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, msgUsage)
		os.Exit(2)
	}
	sub := args[0]
	fs := flag.NewFlagSet("msg "+sub, flag.ExitOnError)
	api := fs.String("api", "127.0.0.1:7401", "this machine's node's local messaging API")
	tokenPath := fs.String("token", filepath.Join(yggstoreHome(), "msg.token"), "the node's messaging token file")
	typ := fs.String("type", "text/plain", "content type of the message")
	topic := fs.String("topic", "", "only this topic (read)")
	n := fs.Int("n", 20, "how many recent messages (read)")
	follow := fs.Bool("follow", false, "keep showing new messages (read)")
	fs.Parse(args[1:])
	token, err := msg.ReadToken(*tokenPath)
	if err != nil {
		return err
	}
	l := msg.Local{Addr: *api, Token: token}
	text := func(i int) (string, error) {
		t := fs.Arg(i)
		if t == "-" {
			b, err := io.ReadAll(io.LimitReader(os.Stdin, msg.MaxBody+1))
			return string(b), err
		}
		if t == "" {
			return "", errors.New("give the message text, or - to read it from stdin")
		}
		return t, nil
	}
	switch sub {
	case "send":
		body, err := text(1)
		if err != nil {
			return err
		}
		m, err := l.Send(ctx, fs.Arg(0), *typ, body)
		if err != nil {
			return err
		}
		fmt.Printf("queued %s for %s; it is retried until delivered (see yggstore msg status)\n", m.ID[:8], fs.Arg(0))
	case "pub", "publish":
		body, err := text(1)
		if err != nil {
			return err
		}
		m, err := l.Publish(ctx, fs.Arg(0), *typ, body)
		if err != nil {
			return err
		}
		fmt.Printf("published %s as #%d in %s\n", m.ID[:8], m.Seq, m.Topic)
	case "sub", "subscribe":
		if err := l.Subscribe(ctx, fs.Arg(0)); err != nil {
			return err
		}
		fmt.Printf("subscribed to %s; its recent messages will arrive shortly\n", fs.Arg(0))
	case "unsub", "unsubscribe":
		if err := l.Unsubscribe(ctx, fs.Arg(0)); err != nil {
			return err
		}
		fmt.Printf("unsubscribed from %s\n", fs.Arg(0))
	case "read":
		list, err := l.Latest(ctx, *topic, *n)
		if err != nil {
			return err
		}
		last := int64(-1)
		for _, s := range list {
			printMsg(s)
			last = s.N
		}
		if !*follow {
			if len(list) == 0 {
				fmt.Println("no messages")
			}
			return nil
		}
		if last < 0 {
			st, err := l.Status(ctx)
			if err != nil {
				return err
			}
			last = st.Last
		}
		return l.Stream(ctx, last, *topic, func(s msg.Stored) error { printMsg(s); return nil })
	case "status":
		st, err := l.Status(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("This node: %s\n", st.Self)
		fmt.Printf("Subscribed to: %s\n", orNone(strings.Join(st.Subscriptions, ", ")))
		if len(st.Subscribers) > 0 {
			fmt.Println("Who follows what (as told to this node):")
			for t, names := range st.Subscribers {
				fmt.Printf("  %-24s %s\n", t, strings.Join(names, ", "))
			}
		}
		if len(st.Pending) == 0 {
			fmt.Println("Undelivered: none")
		} else {
			fmt.Println("Undelivered (retrying):")
			for _, p := range st.Pending {
				to := p.Name
				if to == "" {
					to = p.To
				}
				what := "direct"
				if p.Msg.Topic != "" {
					what = p.Msg.Topic
				}
				if p.Tries == 0 {
					fmt.Printf("  %s to %s (%s): first try under way\n", p.Msg.ID[:8], to, what)
					continue
				}
				fmt.Printf("  %s to %s (%s), %d tries, next at %s: %s\n", p.Msg.ID[:8], to, what, p.Tries,
					time.UnixMilli(p.Next).Format("15:04:05"), p.Err)
			}
		}
		for _, f := range st.Failed {
			fmt.Printf("Gave up: %s to %s after a week: %s\n", f.Msg.ID[:8], f.To, f.Err)
		}
	default:
		fmt.Fprint(os.Stderr, msgUsage)
		os.Exit(2)
	}
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "nothing"
	}
	return s
}

func printMsg(s msg.Stored) {
	from := s.FromName
	if from == "" {
		from = s.From
	}
	where := "direct"
	if s.Topic != "" {
		where = s.Topic
	}
	t := time.UnixMilli(s.Time).Format("2 Jan 15:04")
	body := s.Body
	if !strings.HasPrefix(s.Type, "text/") && s.Type != "" {
		body = fmt.Sprintf("(%s, %d bytes) %s", s.Type, len(s.Body), s.Body)
	}
	fmt.Printf("%s  %-10s %-12s %s\n", t, from, where, body)
}
