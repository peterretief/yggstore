package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/hub"
	"github.com/peterretief/yggstore/internal/peers"
)

// yggstore admin is the box's admin on its own screen (docs/box-setup.md,
// Settings). Logging in on the box is the key: it's run with sudo after
// logging in on one of its consoles. It does what the dashboard's admin
// page does, through the dashboard on this machine, so the two never
// disagree, and it stops after five idle minutes.

const adminIdle = 5 * time.Minute

const adminUsage = `yggstore admin: the box's admin, on its own screen

  sudo yggstore admin [-dashboard http://127.0.0.1:7480]

Devices that may open the dashboard (and ones that were refused), people
on the box, websites, and services, after logging in on the box.
`

func cmdAdmin(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("admin", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, adminUsage) }
	base := fs.String("dashboard", "http://127.0.0.1:7480", "the dashboard on this machine")
	fs.Parse(args)
	if os.Geteuid() != 0 {
		return errors.New("run it with sudo: sudo yggstore admin")
	}
	restore, err := rawTerminal(os.Stdin)
	if err != nil {
		return err
	}
	defer restore()
	fmt.Print("\033[?25l")
	defer fmt.Print("\033[?25h\033[2J\033[H")
	keys := make(chan byte)
	go readKeys(keys)
	box, _ := os.Hostname()
	err = runAdmin(ctx, keyReader{keys}, box, dash{strings.TrimSuffix(*base, "/")})
	if errors.Is(err, errIdle) || errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

// keyReader hands out key presses, with a time limit.
type keyReader struct{ ch <-chan byte }

var errIdle = errors.New("idle")

// key waits for one key. Escape sequences (arrow keys and the like) come
// back as 0, so they're ignored rather than read as Escape.
func (k keyReader) key(ctx context.Context) (byte, error) {
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-time.After(adminIdle):
		return 0, errIdle
	case b, ok := <-k.ch:
		if !ok {
			return 0, io.EOF
		}
		if b != 27 {
			return b, nil
		}
		seq := false
		for {
			select {
			case <-k.ch:
				seq = true
				continue
			case <-time.After(30 * time.Millisecond):
			}
			break
		}
		if seq {
			return 0, nil
		}
		return 27, nil
	}
}

// line reads typed text until Enter. Escape gives ok false.
func (k keyReader) line(ctx context.Context, show func(typed string), secret bool) (string, bool, error) {
	var buf []byte
	for {
		typed := string(buf)
		if secret {
			typed = strings.Repeat("*", len(buf))
		}
		show(typed)
		b, err := k.key(ctx)
		switch {
		case err != nil:
			return "", false, err
		case b == 27:
			return "", false, nil
		case b == '\r' || b == '\n':
			return string(buf), true, nil
		case b == 127 || b == 8:
			if len(buf) > 0 {
				buf = buf[:len(buf)-1]
			}
		case b >= 32 && b < 127 && len(buf) < 80:
			buf = append(buf, b)
		}
	}
}

func readKeys(keys chan<- byte) {
	b := make([]byte, 1)
	for {
		if _, err := os.Stdin.Read(b); err != nil {
			close(keys)
			return
		}
		keys <- b[0]
	}
}

// adminDraw draws one admin screen: a title, its lines, a message (the
// result of the last action) and what the keys do.
func adminDraw(box, title string, lines []string, msg, keys string) {
	const bold, dim, off = "\033[1m", "\033[2m", "\033[0m"
	var b strings.Builder
	b.WriteString("\033[2J\033[H\n")
	fmt.Fprintf(&b, "  %sLocalBox  %s%s    %s\n\n", bold, box, off, title)
	if msg != "" {
		b.WriteString("  " + msg + "\n\n")
	}
	for _, l := range lines {
		b.WriteString("  " + l + "\n")
	}
	b.WriteString("\n  " + dim + keys + off + "\n")
	fmt.Print(b.String())
}

// dash is the dashboard on this machine.
type dash struct{ base string }

func (d dash) get(path string, out any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, d.base+path, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("the dashboard isn't answering: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return errors.New(strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (d dash) post(path string, body any) error { return d.call(path, body, nil) }

// call posts body to path and reads the answer into out, if out isn't nil.
func (d dash) call(path string, body, out any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, d.base+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Yggstore", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("the dashboard isn't answering: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return errors.New(strings.TrimSpace(string(b)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "~"
}

// pick turns a number key into an index into n items (1 to 9).
func pick(b byte, n int) (int, bool) {
	i := int(b) - '1'
	return i, i >= 0 && i < n && i < 9
}

// adminGate opens the admin from the status screen: the box's owner (its
// first person) types their name and password, as they set them up. Five
// wrong tries close it for an hour, for as long as the screen runs.
type adminGate struct {
	wrong int
	until time.Time
}

func (g *adminGate) login(ctx context.Context, k keyReader, box, statePath string) (bool, error) {
	const title = "Admin"
	back := func(lines ...string) (bool, error) {
		adminDraw(box, title, lines, "", "Any key to go back")
		_, err := k.key(ctx)
		return false, err
	}
	st, err := hub.Load(statePath)
	if err != nil {
		return back("Can't read who is on this box: " + err.Error())
	}
	var owner *hub.Person
	for i := range st.People {
		if st.People[i].Active() {
			owner = &st.People[i]
			break
		}
	}
	switch {
	case owner == nil:
		return back("Nobody is on this box yet.", "", "Press Enter on the status screen to set it up.")
	case time.Now().Before(g.until):
		return back("Too many wrong tries.", "", "Try again after "+g.until.Format("15:04")+".")
	}
	name, ok, err := k.line(ctx, func(t string) {
		adminDraw(box, title, []string{"Your name: " + t}, "", "Enter when done    Esc to go back")
	}, false)
	if err != nil || !ok {
		return false, err
	}
	pw, ok, err := k.line(ctx, func(t string) {
		adminDraw(box, title, []string{"Your name: " + name, "Your password: " + t}, "", "Enter when done    Esc to go back")
	}, true)
	if err != nil || !ok {
		return false, err
	}
	adminDraw(box, title, []string{"Checking..."}, "", "")
	if strings.EqualFold(strings.TrimSpace(name), owner.Name) && owner.Password.Live(time.Now()) && owner.Password.Matches(pw) {
		g.wrong = 0
		return true, nil
	}
	if g.wrong++; g.wrong >= 5 {
		g.wrong, g.until = 0, time.Now().Add(time.Hour)
	}
	return back("That isn't the box owner's name and password.")
}

// runAdmin is the admin menu. It returns when the owner leaves, or after
// five idle minutes.
func runAdmin(ctx context.Context, k keyReader, box string, d dash) error {
	msg := ""
	for {
		adminDraw(box, "Admin", []string{
			"1  Devices that may open the dashboard",
			"2  People on this box: add, limits, passwords",
			"3  Websites",
			"4  Services",
			"5  Wi-Fi",
		}, msg, "Number to choose    Esc to leave")
		msg = ""
		b, err := k.key(ctx)
		if err != nil {
			return err
		}
		switch b {
		case 27:
			return nil
		case '1':
			err = adminDevices(ctx, k, box, d)
		case '2':
			err = adminPeople(ctx, k, box, d)
		case '3':
			err = adminSites(ctx, k, box, d)
		case '4':
			err = adminServices(ctx, k, box)
		case '5':
			err = adminWiFi(ctx, k, box)
		}
		if err != nil {
			return err
		}
	}
}

// confirm asks a yes or no question.
func confirm(ctx context.Context, k keyReader, box, title, question string) (bool, error) {
	adminDraw(box, title, []string{question}, "", "Y for yes, any other key for no")
	b, err := k.key(ctx)
	return b == 'y' || b == 'Y', err
}

func adminDevices(ctx context.Context, k keyReader, box string, d dash) error {
	const title = "Devices"
	msg := ""
	for {
		var st struct {
			On      bool
			Devices []struct {
				Addr, Name string
				Pages      []string
			}
			Asks []struct {
				Addr, Name string
				At         time.Time
			}
		}
		var lines []string
		if err := d.get("/api/devices", &st); err != nil {
			lines = []string{"Can't read them: " + err.Error()}
		} else if !st.On {
			lines = []string{"The dashboard here only opens on this machine (it isn't", "started with -remote), so there are no devices to list."}
		}
		n := 0
		if len(st.Devices) > 0 {
			lines = append(lines, "May open it:")
			for _, dv := range st.Devices {
				n++
				pages := "every page"
				if len(dv.Pages) > 0 {
					pages = strings.Join(dv.Pages, ", ")
				}
				lines = append(lines, fmt.Sprintf("%d  %-14s %s", n, cut(dv.Name, 14), cut(pages, 36)))
			}
		}
		if len(st.Asks) > 0 {
			lines = append(lines, "", "Tried to open it, and were refused:")
			for _, a := range st.Asks {
				n++
				lines = append(lines, fmt.Sprintf("%d  %s  at %s", n, a.Addr, a.At.Local().Format("15:04")))
			}
		}
		if st.On && n == 0 {
			lines = append(lines, "No devices yet. One that tries to open the dashboard", "shows up here, to be let in.")
		}
		keys := "Number to choose    Esc to go back"
		if st.On {
			keys = "Number to choose    N to add by address    Esc to go back"
		}
		adminDraw(box, title, lines, msg, keys)
		msg = ""
		b, err := k.key(ctx)
		if err != nil {
			return err
		}
		if b == 27 {
			return nil
		}
		add := func(addr string) error {
			name, ok, err := k.line(ctx, func(typed string) {
				adminDraw(box, title, []string{"Letting in " + addr, "", "A name for it, such as \"Anna's phone\": " + typed}, "", "Enter to let it in    Esc to cancel")
			}, false)
			if err != nil || !ok {
				return err
			}
			if e := d.post("/api/devices/add", map[string]string{"addr": addr, "name": name}); e != nil {
				msg = "Not let in: " + e.Error()
			} else {
				msg = "Let in: it may open every page but Admin."
			}
			return nil
		}
		if (b == 'n' || b == 'N') && st.On {
			addr, ok, err := k.line(ctx, func(typed string) {
				adminDraw(box, title, []string{"The device's Yggdrasil address (it's shown on the", "page it gets when it's refused):", "", typed}, "", "Enter when done    Esc to cancel")
			}, false)
			if err != nil {
				return err
			}
			if ok && strings.TrimSpace(addr) != "" {
				if err := add(strings.TrimSpace(addr)); err != nil {
					return err
				}
			}
			continue
		}
		i, ok := pick(b, n)
		if !ok {
			continue
		}
		if i < len(st.Devices) {
			dv := st.Devices[i]
			yes, err := confirm(ctx, k, box, title, "Stop "+dv.Name+" ("+dv.Addr+") opening the dashboard?")
			if err != nil {
				return err
			}
			if yes {
				if e := d.post("/api/devices/remove", map[string]string{"addr": dv.Addr}); e != nil {
					msg = "Not removed: " + e.Error()
				} else {
					msg = dv.Name + " can't open the dashboard any more."
				}
			}
			continue
		}
		if err := add(st.Asks[i-len(st.Devices)].Addr); err != nil {
			return err
		}
	}
}

func adminPeople(ctx context.Context, k keyReader, box string, d dash) error {
	const title = "People on this box"
	msg := ""
	for {
		var st struct {
			On     bool
			People []boxPersonView
		}
		var lines []string
		if err := d.get("/api/box/people", &st); err != nil {
			lines = []string{"Can't read them: " + err.Error()}
		} else if !st.On {
			lines = []string{"This box keeps no one else's files: it has no hub."}
		}
		var shown []boxPersonView
		for _, p := range st.People {
			if !p.Removed {
				shown = append(shown, p)
				lines = append(lines, fmt.Sprintf("%d  %-16s %s", len(shown), cut(p.Name, 16), p.summary()))
			}
		}
		if st.On && len(shown) == 0 {
			lines = append(lines, "No one yet.")
		}
		keys := "Esc to go back"
		if st.On {
			keys = "A to add someone    Number to choose    Esc to go back"
		}
		adminDraw(box, title, lines, msg, keys)
		msg = ""
		b, err := k.key(ctx)
		if err != nil {
			return err
		}
		switch {
		case b == 27:
			return nil
		case (b == 'a' || b == 'A') && st.On:
			name, ok, err := k.line(ctx, func(typed string) {
				adminDraw(box, title, []string{"Their first name, as they'd sign in with it: " + typed}, "", "Enter to add them    Esc to cancel")
			}, false)
			if err != nil {
				return err
			}
			if !ok || strings.TrimSpace(name) == "" {
				continue
			}
			var out passwordOut
			if e := d.call("/api/box/people/add", map[string]string{"name": strings.TrimSpace(name)}, &out); e != nil {
				msg = "Not added: " + e.Error()
				continue
			}
			// If they're at the box, they type their own; otherwise a
			// temporary one to give them.
			chosen, m, err := choosePassword(ctx, k, box, title, d, out.Name, personPath(d, out.Name), len(shown) == 0,
				out.Name+" is on the box now. If they're here, they type the password they'll use; otherwise press Esc for a temporary one to give them.")
			if err != nil {
				return err
			}
			if chosen {
				msg = m
			} else if err := showPassword(ctx, k, box, title, out.Name+" is on the box now.", out); err != nil {
				return err
			}
		default:
			if i, ok := pick(b, len(shown)); ok {
				if msg, err = adminPerson(ctx, k, box, d, shown[i], i == 0); err != nil {
					return err
				}
			}
		}
	}
}

type boxPersonView struct {
	Name       string
	Bytes      int64
	Quota      int64
	MaxDevices int `json:"max_devices"`
	Suspended  bool
	Removed    bool
	Syncthing  int
	Path       string
	Site       string // their name in the group, once given
	Asked      string // the name asked for, while it waits
}

func (p boxPersonView) summary() string {
	s := size(p.Bytes)
	if p.Quota > 0 {
		s += " of " + size(p.Quota)
	} else {
		s += ", no limit"
	}
	if p.Suspended {
		s += ", shut out for now"
	}
	return s
}

type passwordOut struct{ Name, Login, Password, At string }

// showPassword shows a temporary password until a key is pressed. It's on
// the box's own screen, for the owner to give by phone or in person.
func showPassword(ctx context.Context, k keyReader, box, title, head string, out passwordOut) error {
	lines := []string{head, "", "Give them, by phone or in person:"}
	if out.At != "" {
		lines = append(lines, "  sign in at   "+out.At)
	}
	lines = append(lines, "  as           "+out.Login, "  password     "+out.Password, "",
		"They choose their own password when they first sign in.")
	adminDraw(box, title, lines, "", "Any key when you've noted it (it isn't shown again)")
	_, err := k.key(ctx)
	return err
}

// personPath is where name's folder is on the box.
func personPath(d dash, name string) string {
	var st struct{ People []boxPersonView }
	d.get("/api/box/people", &st)
	for _, p := range st.People {
		if strings.EqualFold(p.Name, name) && !p.Removed {
			return p.Path
		}
	}
	return ""
}

// choosePassword lets the person at the box type their own password,
// twice. chosen is false if they pressed Esc.
// The box's owner (its first person) also gets a login at the box with
// it, and so its admin.
func choosePassword(ctx context.Context, k keyReader, box, title string, d dash, name, path string, owner bool, head string) (chosen bool, msg string, err error) {
	note := ""
	for {
		pw, ok, err := k.line(ctx, func(typed string) {
			adminDraw(box, title, []string{head, "", "At least 10 characters; a few words together is easiest.", "",
				name + "'s password: " + typed}, note, "Enter when done    Esc to skip")
		}, true)
		if err != nil || !ok {
			return false, "", err
		}
		again, ok, err := k.line(ctx, func(typed string) {
			adminDraw(box, title, []string{"The same password again: " + typed}, "", "Enter when done    Esc to skip")
		}, true)
		if err != nil || !ok {
			return false, "", err
		}
		if again != pw {
			note = "The two didn't match. Try again."
			continue
		}
		if e := d.call("/api/box/people/set-password", map[string]string{"name": name, "password": pw}, nil); e != nil {
			note = e.Error()
			continue
		}
		msg = name + " has the password they chose."
		if owner {
			if e := boxLogin(name, pw); e != nil {
				msg += " No login at the box: " + e.Error() + "."
			} else {
				msg += " It's also their login at the box."
			}
		}
		if sambaOn() {
			if e := sambaShare(name, path, pw); e != nil {
				msg += " Their network drive isn't set up: " + e.Error()
			} else {
				msg += " On this network their folder opens as " + sambaAddress(name) + ", with the same name and password."
			}
		}
		return true, msg, nil
	}
}

// adminPerson is one person's choices. It returns what was done.
func adminPerson(ctx context.Context, k keyReader, box string, d dash, p boxPersonView, owner bool) (string, error) {
	title := p.Name
	limit := "no limit"
	if p.Quota > 0 {
		limit = size(p.Quota)
	}
	shut := "S  shut them out for now (their files stay)"
	if p.Suspended {
		shut = "S  let them use the box again"
	}
	web := "W  a website and email address for them"
	switch {
	case p.Site != "":
		web = "Website https://" + p.Site + ", email " + peers.MailAddress(p.Site)
	case p.Asked != "":
		web = "Website " + p.Asked + ": asked for, waiting for the group's admin"
	}
	adminDraw(box, title, []string{
		fmt.Sprintf("Uses %s, limit %s, %d devices syncing", size(p.Bytes), limit, p.Syncthing),
		web, "",
		"L  change their limit",
		"C  choose their password now (they're here)",
		"P  a new temporary password, to give them",
		shut,
		"R  remove them from the box (their files stay)",
	}, "", "Letter to choose    Esc to go back")
	b, err := k.key(ctx)
	if err != nil {
		return "", err
	}
	name := map[string]any{"name": p.Name}
	switch b {
	case 'l', 'L':
		gb, ok, err := k.line(ctx, func(typed string) {
			adminDraw(box, title, []string{"Their limit in GB, or 0 for no limit: " + typed}, "", "Enter to set it    Esc to cancel")
		}, false)
		if err != nil || !ok {
			return "", err
		}
		var n float64
		if _, e := fmt.Sscanf(strings.TrimSpace(gb), "%g", &n); e != nil || n < 0 {
			return "Not changed: give a number of GB.", nil
		}
		name["quota"] = int64(n * (1 << 30))
		if e := d.call("/api/box/people/limit", name, nil); e != nil {
			return "Not changed: " + e.Error(), nil
		}
		return "Changed " + p.Name + "'s limit.", nil
	case 'w', 'W':
		if p.Site != "" || p.Asked != "" {
			return "", nil
		}
		list, _ := peers.Load(defaultPeers())
		label, ok, err := askWebName(ctx, k, box, title, "A name for "+p.Name+"'s website and email: ", list)
		if err != nil || !ok {
			return "", err
		}
		if e := d.call("/api/box/people/name", map[string]string{"name": p.Name, "label": label}, nil); e != nil {
			return "Not asked: " + e.Error(), nil
		}
		if q, ok := personNamed(d, p.Name); ok && q.Site != "" {
			return p.Name + " has https://" + q.Site + " and " + peers.MailAddress(q.Site) + " now.", nil
		}
		return label + " is asked of the group: the website and email start once the group's admin gives it.", nil
	case 'c', 'C':
		_, m, err := choosePassword(ctx, k, box, title, d, p.Name, p.Path, owner, "They type the password they'll use. Their old one, and the browsers it signed in, stop working.")
		return m, err
	case 'p', 'P':
		yes, err := confirm(ctx, k, box, title, "A new temporary password for "+p.Name+"? Their old one, and the browsers it signed in, stop working.")
		if err != nil || !yes {
			return "", err
		}
		var out passwordOut
		if e := d.call("/api/box/people/password", name, &out); e != nil {
			return "Not changed: " + e.Error(), nil
		}
		return "", showPassword(ctx, k, box, title, "A new temporary password for "+p.Name+".", out)
	case 's', 'S':
		action, q := "suspend", "Shut "+p.Name+" out for now? Their files stay."
		if p.Suspended {
			action, q = "resume", "Let "+p.Name+" use the box again?"
		}
		yes, err := confirm(ctx, k, box, title, q)
		if err != nil || !yes {
			return "", err
		}
		if e := d.call("/api/box/people/"+action, name, nil); e != nil {
			return "Not changed: " + e.Error(), nil
		}
		sambaEnable(p.Name, action == "resume")
		return "Done.", nil
	case 'r', 'R':
		yes, err := confirm(ctx, k, box, title, "Remove "+p.Name+" from the box? Their devices are shut out; their files stay.")
		if err != nil || !yes {
			return "", err
		}
		if e := d.call("/api/box/people/remove", name, nil); e != nil {
			return "Not removed: " + e.Error(), nil
		}
		sambaRemove(p.Name)
		return p.Name + " is removed.", nil
	}
	return "", nil
}

// adminWiFi joins a Wi-Fi network through NetworkManager. Raspberry Pi OS
// keeps Wi-Fi off until a country is set, so it sets South Africa's first.
func adminWiFi(ctx context.Context, k keyReader, box string) error {
	const title = "Wi-Fi"
	if _, err := exec.LookPath("nmcli"); err != nil {
		adminDraw(box, title, []string{"This box has no NetworkManager, so Wi-Fi can't be set here."}, "", "Any key to go back")
		_, err := k.key(ctx)
		return err
	}
	if out, _ := exec.Command("nmcli", "-t", "-f", "TYPE", "dev").Output(); !strings.Contains(string(out), "wifi") {
		adminDraw(box, title, []string{"This box has no Wi-Fi."}, "", "Any key to go back")
		_, err := k.key(ctx)
		return err
	}
	adminDraw(box, title, []string{"Looking for networks..."}, "", "")
	if _, err := exec.LookPath("raspi-config"); err == nil {
		exec.Command("raspi-config", "nonint", "do_wifi_country", "ZA").Run()
	}
	exec.Command("rfkill", "unblock", "wifi").Run()
	exec.Command("nmcli", "radio", "wifi", "on").Run()
	msg := ""
	for {
		// --rescan yes waits for a fresh scan, which a radio just
		// switched on needs.
		out, err := exec.Command("nmcli", "-t", "-e", "no", "-f", "IN-USE,SIGNAL,SECURITY,SSID", "dev", "wifi", "list", "--rescan", "yes").Output()
		var nets, lines []string
		if err != nil {
			lines = []string{"Can't list networks: " + err.Error()}
		}
		seen := map[string]bool{}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			f := strings.SplitN(line, ":", 4)
			if len(f) < 4 || f[3] == "" || seen[f[3]] || len(nets) == 9 {
				continue
			}
			seen[f[3]] = true
			nets = append(nets, f[3])
			now := ""
			if f[0] == "*" {
				now = "  connected"
			}
			lines = append(lines, fmt.Sprintf("%d  %-28s signal %3s%%%s", len(nets), cut(f[3], 28), f[1], now))
		}
		if err == nil && len(nets) == 0 {
			lines = append(lines, "No networks found.")
		}
		adminDraw(box, title, lines, msg, "Number to join    Esc to go back")
		msg = ""
		b, err := k.key(ctx)
		if err != nil {
			return err
		}
		if b == 27 {
			return nil
		}
		i, ok := pick(b, len(nets))
		if !ok {
			continue
		}
		pw, ok, err := k.line(ctx, func(typed string) {
			adminDraw(box, title, []string{"Joining " + nets[i], "", "Its password (blank if it has none): " + typed}, "", "Enter to join    Esc to cancel")
		}, true)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		adminDraw(box, title, []string{"Joining " + nets[i] + "..."}, "", "")
		args := []string{"dev", "wifi", "connect", nets[i]}
		if pw != "" {
			args = append(args, "password", pw)
		}
		if out, e := exec.Command("nmcli", args...).CombinedOutput(); e != nil {
			msg = "Not joined: " + strings.TrimSpace(string(out))
		} else {
			msg = "Joined " + nets[i] + ". The cable is still used when it's plugged in."
		}
	}
}

func adminSites(ctx context.Context, k keyReader, box string, d dash) error {
	const title = "Websites"
	msg := ""
	for {
		var st struct {
			Error string
			Sites []struct {
				Site         string
				Contact, Ygg bool
			}
		}
		var lines []string
		if err := d.get("/api/sites", &st); err != nil {
			lines = []string{"Can't read them: " + err.Error()}
		} else if st.Error != "" {
			lines = []string{st.Error}
		}
		on := map[bool]string{true: "on", false: "off"}
		for i, s := range st.Sites {
			lines = append(lines, fmt.Sprintf("%d  %-30s contact form %-3s  ygg button %s", i+1, cut(s.Site, 30), on[s.Contact], on[s.Ygg]))
		}
		if st.Error == "" && len(st.Sites) == 0 {
			lines = append(lines, "No websites yet.")
		}
		adminDraw(box, title, lines, msg, "Number to change one    Esc to go back")
		msg = ""
		b, err := k.key(ctx)
		if err != nil {
			return err
		}
		if b == 27 {
			return nil
		}
		i, ok := pick(b, len(st.Sites))
		if !ok {
			continue
		}
		s := st.Sites[i]
		adminDraw(box, title, []string{s.Site, "",
			"C  turn the contact form " + on[!s.Contact],
			"Y  turn the ygg button " + on[!s.Ygg]}, "", "Letter to choose    Esc to go back")
		b, err = k.key(ctx)
		if err != nil {
			return err
		}
		action, value := "", false
		switch b {
		case 'c', 'C':
			action, value = "contact", !s.Contact
		case 'y', 'Y':
			action, value = "ygg", !s.Ygg
		default:
			continue
		}
		adminDraw(box, title, []string{"Changing " + s.Site + "..."}, "", "")
		if e := d.post("/api/sites/"+action, map[string]any{"site": s.Site, "on": value}); e != nil {
			msg = "Not changed: " + e.Error()
		} else {
			msg = "Done: the home page is published again."
		}
	}
}

func adminServices(ctx context.Context, k keyReader, box string) error {
	const title = "Services"
	msg := ""
	for {
		out, err := exec.Command("systemctl", "list-units", "--type=service", "--all", "--plain", "--no-legend",
			"yggstore*", "yggdrasil*", "syncthing*").Output()
		var units, lines []string
		if err != nil {
			lines = []string{"Can't list them: " + err.Error()}
		}
		for _, line := range strings.Split(string(out), "\n") {
			f := strings.Fields(line)
			// Timer-started ones that finished aren't services to restart,
			// and the screen doesn't restart itself.
			if len(f) < 4 || f[0] == "yggstore-screen.service" || (f[2] == "inactive" && f[3] == "dead") {
				continue
			}
			units = append(units, f[0])
			state := f[3]
			if f[2] == "failed" {
				state = "FAILED"
			}
			lines = append(lines, fmt.Sprintf("%d  %-40s %s", len(units), cut(strings.TrimSuffix(f[0], ".service"), 40), state))
		}
		adminDraw(box, title, lines, msg, "Number to restart one    Esc to go back")
		msg = ""
		b, err := k.key(ctx)
		if err != nil {
			return err
		}
		if b == 27 {
			return nil
		}
		i, ok := pick(b, len(units))
		if !ok {
			continue
		}
		u := strings.TrimSuffix(units[i], ".service")
		yes, err := confirm(ctx, k, box, title, "Restart "+u+"?")
		if err != nil {
			return err
		}
		if yes {
			adminDraw(box, title, []string{"Restarting " + u + "..."}, "", "")
			if out, e := exec.Command("systemctl", "restart", units[i]).CombinedOutput(); e != nil {
				msg = "Didn't restart: " + strings.TrimSpace(string(out))
			} else {
				msg = u + " restarted."
			}
		}
	}
}
