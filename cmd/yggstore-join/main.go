// yggstore-join is the installer a box hands out for Windows ("Connect to
// BOX.exe"): double-clicked, it connects the computer to the box with
// Syncthing and opens the person's folder. See internal/join and
// docs/one-click-join.md.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/peterretief/yggstore/internal/join"
)

func main() {
	noWait := flag.Bool("no-wait", false, "don't wait for Enter before closing (tests)")
	noService := flag.Bool("no-service", false, "don't start Syncthing at sign-in (tests)")
	syncthing := flag.String("syncthing", "", "the Syncthing to use (tests)")
	flag.Parse()
	ok := run(*syncthing, *noService)
	if runtime.GOOS == "windows" && !*noWait {
		// Double-clicked, the window would close before anyone could read it.
		fmt.Print("\nPress Enter to close this window.")
		bufio.NewReader(os.Stdin).ReadString('\n')
	}
	if !ok {
		os.Exit(1)
	}
}

func run(syncthing string, noService bool) bool {
	exe, err := os.Executable()
	if err == nil {
		var b join.Box
		if b, err = join.Read(exe); err == nil {
			if a := os.Getenv("YGGSTORE_BOX_ADDRS"); a != "" {
				b.Addrs = strings.Fields(a) // for tests
			}
			fmt.Printf("Connecting this computer to the box %s.\nIt takes a minute or two; leave this window open.\n\n", b.Name)
			var folders []join.Folder
			var me string
			folders, me, err = join.Connect(context.Background(), b, join.Computer{Syncthing: syncthing, NoService: noService, Out: os.Stdout})
			if err == nil {
				fmt.Printf("\nDone. This computer is connected to the box %s.\n", b.Name)
				for _, f := range folders {
					if f.Mine {
						fmt.Printf("Your folder is %s. Put files in its files folder to keep them safe;\neach becomes a small .ystub file. Copy a .ystub into restore to get it back.\n", f.Path)
						join.OpenFolder(f.Path)
						break
					}
				}
				fmt.Printf("This computer's Syncthing ID (for Work from anywhere): %s\n", me)
				return true
			}
		}
	}
	fmt.Printf("\nIt didn't work: %v\n", err)
	return false
}
