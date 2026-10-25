// Package join connects a computer to a box with one double-click: the
// installer (cmd/yggstore-join) a box hands out. It does what connect.ps1
// and connect.sh do: puts Syncthing on the computer and starts it at
// sign-in, adds the box, and accepts the folders the box offers into the
// person's home folder. The box writes where it is and who it is to the end
// of the file as it sends it (Append), so there's nothing to type.
package join

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// Box is what the installer needs to know about the box it came from.
type Box struct {
	Hub   string   `json:"hub"`   // the box's page, e.g. http://192.168.0.5:7490
	Name  string   `json:"name"`  // what the box is called
	ID    string   `json:"id"`    // its Syncthing ID
	Addrs []string `json:"addrs"` // where its Syncthing is reached, or "dynamic"
}

// marker starts the box's details at the end of the installer. Windows
// ignores what follows a program's last section, so the file still runs.
const marker = "\n#YGGSTORE-BOX#"

// Trailer is what the box appends to the installer for b.
func Trailer(b Box) []byte {
	j, _ := json.Marshal(b)
	return append(append([]byte(marker), j...), '\n')
}

// Read finds the box's details at the end of the file at path.
func Read(path string) (Box, error) {
	f, err := os.Open(path)
	if err != nil {
		return Box{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return Box{}, err
	}
	n := min(fi.Size(), 16<<10)
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, fi.Size()-n); err != nil {
		return Box{}, err
	}
	i := bytes.LastIndex(buf, []byte(marker))
	if i < 0 {
		return Box{}, errors.New("this copy doesn't say which box it's for: download it again from the box's page")
	}
	var b Box
	if err := json.Unmarshal(bytes.TrimSpace(buf[i+len(marker):]), &b); err != nil || b.Hub == "" || !deviceID.MatchString(b.ID) {
		return Box{}, errors.New("this copy's details about the box are damaged: download it again from the box's page")
	}
	return b, nil
}

var deviceID = regexp.MustCompile(`^[A-Z0-9]{7}(-[A-Z0-9]{7}){7}$`)

// Computer is the computer being connected. The zero value is this one,
// for the person running it; tests fill it in.
type Computer struct {
	Home      string // the person's home folder
	Syncthing string // a Syncthing to use, instead of finding or installing one
	NoService bool   // don't start Syncthing at sign-in (tests)
	Out       io.Writer
	HTTP      *http.Client
}

// Folder is a folder the box shares with the computer.
type Folder struct {
	ID, Label, Path string
	Mine            bool // the person's own folder (not websites or a team's)
}

// Connect connects the computer to the box. It reports the folders it
// added or found, and the computer's Syncthing ID.
func Connect(ctx context.Context, b Box, c Computer) (folders []Folder, me string, err error) {
	if c.Out == nil {
		c.Out = io.Discard
	}
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: 2 * time.Minute}
	}
	if c.Home == "" {
		if c.Home, err = os.UserHomeDir(); err != nil {
			return nil, "", err
		}
	}
	say := func(format string, a ...any) { fmt.Fprintf(c.Out, format+"\n", a...) }

	// 1. Syncthing: the one given, the one here, or the latest release.
	st := c.Syncthing
	if st == "" {
		st = findSyncthing()
	}
	if st == "" {
		say("Downloading Syncthing...")
		if st, err = installSyncthing(ctx, c.HTTP, say); err != nil {
			return nil, "", err
		}
	}
	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, st, append([]string{"cli"}, args...)...)
		cmd.Env = append(os.Environ(), "HOME="+c.Home)
		hide(cmd)
		out, err := cmd.Output()
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) && len(ee.Stderr) > 0 {
				err = fmt.Errorf("%v: %s", err, bytes.TrimSpace(ee.Stderr))
			}
			return "", fmt.Errorf("syncthing cli %s: %w", strings.Join(args, " "), err)
		}
		return strings.TrimSpace(string(out)), nil
	}
	lines := func(s string) []string {
		var out []string
		for _, l := range strings.Split(s, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				out = append(out, l)
			}
		}
		return out
	}

	// 2. Running, and started when the person signs in.
	if _, err := run("show", "system"); err != nil {
		if !c.NoService {
			if err := startAtSignIn(st, b.Name); err != nil {
				say("Couldn't set Syncthing to start when you sign in (%v); it runs until the computer restarts.", err)
			}
		}
		say("Starting Syncthing...")
		cmd := exec.Command(st, "serve", "--no-browser", "--no-restart")
		cmd.Env = append(os.Environ(), "HOME="+c.Home, "STNODEFAULTFOLDER=1")
		detach(cmd)
		if err := cmd.Start(); err != nil {
			return nil, "", fmt.Errorf("starting Syncthing: %w", err)
		}
		go cmd.Wait() // it keeps running after this program ends
		for i := 0; ; i++ {
			if _, err := run("show", "system"); err == nil {
				break
			}
			if i >= 60 {
				return nil, "", errors.New("Syncthing didn't start; restart the computer and run this again")
			}
			time.Sleep(time.Second)
		}
	}
	sys, err := run("show", "system")
	if err != nil {
		return nil, "", err
	}
	var s struct {
		MyID string `json:"myID"`
	}
	if json.Unmarshal([]byte(sys), &s) != nil || !deviceID.MatchString(s.MyID) {
		return nil, "", errors.New("couldn't read this computer's Syncthing ID")
	}
	me = s.MyID

	// 3. The box.
	devs, err := run("config", "devices", "list")
	if err != nil {
		return nil, me, err
	}
	if !contains(lines(devs), b.ID) {
		if _, err := run("config", "devices", "add", "--device-id", b.ID, "--name", b.Name); err != nil {
			return nil, me, fmt.Errorf("couldn't add the box to Syncthing: %w", err)
		}
		// One by one: "devices add --addresses" crashes Syncthing 1.29.
		for _, a := range b.Addrs {
			if a != "dynamic" {
				if _, err := run("config", "devices", b.ID, "addresses", "add", a); err != nil {
					return nil, me, err
				}
			}
		}
		say("Added the box %s.", b.Name)
	}

	// 4. The folders it offers: it takes the computer within half a minute
	// of it connecting.
	say("Waiting for the box to take this computer...")
	var offered []string
	for i := 0; ; i++ {
		offered = askFolders(ctx, c.HTTP, b.Hub, me)
		if len(offered) > 0 {
			break
		}
		if i >= 90 {
			return nil, me, fmt.Errorf("the box hasn't taken this computer yet. It may not be taking new devices today: ask whoever looks after it, then run this again (this computer's Syncthing ID is %s)", me)
		}
		select {
		case <-ctx.Done():
			return nil, me, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	haveOut, err := run("config", "folders", "list")
	if err != nil {
		return nil, me, err
	}
	have := lines(haveOut)
	for _, line := range offered {
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		id, label, rel := parts[0], parts[1], parts[2]
		mine := id != "yggstore-websites" && !strings.HasPrefix(id, "yggstore-team-")
		if contains(have, id) {
			with, err := run("config", "folders", id, "devices", "list")
			if err != nil {
				return nil, me, err
			}
			if !contains(lines(with), b.ID) {
				say("Not adding %s: this computer has a folder with the same ID (%s), kept in step with another device. Ask whoever looks after the box.", label, id)
				continue
			}
			path, err := run("config", "folders", id, "path", "get")
			if err != nil {
				return nil, me, err
			}
			folders = append(folders, Folder{id, label, path, mine})
			continue
		}
		path := filepath.Join(c.Home, filepath.FromSlash(rel))
		if rel == "yggstore" && !empty(path) {
			path = filepath.Join(c.Home, "yggstore-"+b.Name) // not into a folder of theirs in use
		}
		if err := os.MkdirAll(path, 0o755); err != nil {
			return nil, me, err
		}
		if _, err := run("config", "folders", "add", "--id", id, "--label", label, "--path", path); err != nil {
			return nil, me, err
		}
		if _, err := run("config", "folders", id, "devices", "add", "--device-id", b.ID); err != nil {
			return nil, me, err
		}
		say("Added %s: %s", label, path)
		folders = append(folders, Folder{id, label, path, mine})
	}
	return folders, me, nil
}

func askFolders(ctx context.Context, hc *http.Client, hub, me string) []string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(hub, "/")+"/api/connect/folders?device="+me, nil)
	if err != nil {
		return nil
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var out []string
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 1<<20))
	for sc.Scan() {
		if l := strings.TrimRight(sc.Text(), "\r"); strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func empty(dir string) bool {
	f, err := os.Open(dir)
	if err != nil {
		return true
	}
	defer f.Close()
	_, err = f.Readdirnames(1)
	return err != nil
}

// installSyncthing downloads the latest Syncthing release for this
// computer, checks it against the release's checksums and puts it in
// syncthingDir.
func installSyncthing(ctx context.Context, hc *http.Client, say func(string, ...any)) (string, error) {
	osName := map[string]string{"windows": "windows", "darwin": "macos", "linux": "linux"}[runtime.GOOS]
	if osName == "" {
		return "", fmt.Errorf("Syncthing has no download for %s", runtime.GOOS)
	}
	arch := runtime.GOARCH
	if osName == "macos" {
		arch = "universal"
	}
	get := func(url string) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		resp, err := hc.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s: %s", url, resp.Status)
		}
		return io.ReadAll(io.LimitReader(resp.Body, 200<<20))
	}
	rel, err := get("https://api.github.com/repos/syncthing/syncthing/releases/latest")
	if err != nil {
		return "", fmt.Errorf("couldn't find Syncthing's latest release: %w", err)
	}
	var latest struct {
		Tag string `json:"tag_name"`
	}
	if json.Unmarshal(rel, &latest) != nil || latest.Tag == "" {
		return "", errors.New("couldn't find Syncthing's latest release")
	}
	name := fmt.Sprintf("syncthing-%s-%s-%s", osName, arch, latest.Tag)
	file := name + ".zip"
	if osName == "linux" {
		return "", errors.New("install Syncthing from your system's packages, then run this again")
	}
	base := "https://github.com/syncthing/syncthing/releases/download/" + latest.Tag + "/"
	data, err := get(base + file)
	if err != nil {
		return "", fmt.Errorf("couldn't download Syncthing: %w", err)
	}
	sums, err := get(base + "sha256sum.txt.asc")
	if err != nil {
		return "", fmt.Errorf("couldn't download Syncthing's checksums: %w", err)
	}
	sum := sha256.Sum256(data)
	if !checksumOK(sums, file, hex.EncodeToString(sum[:])) {
		return "", errors.New("the Syncthing download doesn't match its checksum; stopping")
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", err
	}
	exe := "syncthing"
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	dir := syncthingDir()
	for _, f := range zr.File {
		if f.Name != name+"/"+exe {
			continue
		}
		r, err := f.Open()
		if err != nil {
			return "", err
		}
		defer r.Close()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		dst := filepath.Join(dir, exe)
		w, err := os.OpenFile(dst+".new", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(w, r); err != nil {
			w.Close()
			return "", err
		}
		if err := w.Close(); err != nil {
			return "", err
		}
		if err := os.Rename(dst+".new", dst); err != nil {
			return "", err
		}
		say("Installed Syncthing %s in %s.", latest.Tag, dst)
		return dst, nil
	}
	return "", fmt.Errorf("%s has no %s in it", file, exe)
}

// checksumOK reports whether sums (sha256sum's output, maybe signed) lists
// file with the hash want.
func checksumOK(sums []byte, file, want string) bool {
	for _, l := range strings.Split(string(sums), "\n") {
		f := strings.Fields(l)
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == file {
			return strings.EqualFold(f[0], want)
		}
	}
	return false
}
