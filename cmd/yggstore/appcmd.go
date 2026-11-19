package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/share"
)

const appUsage = `yggstore app: an app on this machine with an email address of its own
(docs/app-workflow.md). Mail to the address is sealed for the app's own key
and put in the app's inbox, not your mailbox; the app makes a row of each.

  yggstore app address NAME LABEL   give the app NAME the address LABEL@ the group's domain
                                    (at once on the admin's machine; otherwise the admin is asked)
  yggstore app folder NAME          the app's folder, for its -mail-in

Options: -dashboard http://127.0.0.1:7480, -apps ~/.yggstore/apps
`

func cmdApp(args []string) error {
	fs := flag.NewFlagSet("app", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, appUsage) }
	base := fs.String("dashboard", "http://127.0.0.1:7480", "the dashboard on this machine")
	apps := fs.String("apps", filepath.Join(yggstoreHome(), "apps"), "where apps' keys and inboxes are")
	if len(args) == 0 {
		fs.Usage()
		return flag.ErrHelp
	}
	sub := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		fs.Usage()
		return flag.ErrHelp
	}
	name := strings.ToLower(fs.Arg(0))
	if peers.ValidLabel(name) != nil {
		return fmt.Errorf("an app's name is like leave or help-desk, not %q", fs.Arg(0))
	}
	dir := filepath.Join(*apps, name)
	if err := os.MkdirAll(filepath.Join(dir, "inbox"), 0o700); err != nil {
		return err
	}
	id, err := share.LoadOrCreate(filepath.Join(dir, "sharing.key"))
	if err != nil {
		return err
	}
	switch sub {
	case "folder":
		fmt.Println(dir)
		return nil
	case "address":
		if fs.NArg() != 2 {
			fs.Usage()
			return flag.ErrHelp
		}
		var out struct{ Message string }
		if err := (dash{strings.TrimSuffix(*base, "/")}).call("/api/names/app",
			map[string]string{"app": name, "label": fs.Arg(1), "code": id.Code()}, &out); err != nil {
			return err
		}
		fmt.Println(out.Message)
		fmt.Printf("Its mail goes to %s; start the app with -mail-in %s\n", filepath.Join(dir, "inbox"), dir)
		return nil
	}
	fs.Usage()
	return flag.ErrHelp
}
