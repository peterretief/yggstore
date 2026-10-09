package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/peterretief/yggstore/internal/devices"
)

const devicesUsage = `yggstore devices: the devices that may open your dashboard over Yggdrasil

  yggstore devices                     list them
  yggstore devices add ADDR NAME       allow the device with this Yggdrasil address
  yggstore devices remove ADDR|NAME    stop allowing it

Run these on the machine the dashboard runs on; it must be started with
-remote. A device that isn't allowed is shown its address when it opens the
dashboard. All take -file. See docs/remote-dashboard.md.
`

func cmdDevices(args []string) error {
	fs := flag.NewFlagSet("devices", flag.ExitOnError)
	path := fs.String("file", filepath.Join(yggstoreHome(), "devices.json"), "the list of devices")
	fs.Usage = func() { fmt.Fprint(os.Stderr, devicesUsage) }
	fs.Parse(reorder(args))
	switch cmd := fs.Arg(0); {
	case cmd == "":
		list, err := devices.Load(*path)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Println("No devices yet: only this machine can open the dashboard.")
			return nil
		}
		fmt.Printf("%-42s %-20s %s\n", "ADDRESS", "NAME", "ADDED")
		for _, d := range list {
			fmt.Printf("%-42s %-20s %s\n", d.Addr, d.Name, d.Added.Format("2006-01-02"))
		}
	case cmd == "add" && fs.NArg() == 3:
		d, err := devices.Add(*path, fs.Arg(1), fs.Arg(2))
		if err != nil {
			return err
		}
		fmt.Printf("%s (%s) may open the dashboard now.\n", d.Name, d.Addr)
	case cmd == "remove" && fs.NArg() == 2:
		d, err := devices.Remove(*path, fs.Arg(1))
		if err != nil {
			return err
		}
		fmt.Printf("%s (%s) may no longer open the dashboard.\n", d.Name, d.Addr)
	default:
		fs.Usage()
		return errors.New("unknown devices command")
	}
	return nil
}
