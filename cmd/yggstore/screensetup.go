package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/peterretief/yggstore/internal/password"
	"github.com/peterretief/yggstore/internal/peers"
)

// Set up this box (docs/box-setup.md): on a box with nobody on it yet,
// Enter on the status screen asks three things (a first name, a password,
// a name for the website and email) and does the rest: the owner is the
// box's first person, with that password for the web, for logging in at
// the box (and so its admin) and for their network drive, and their name
// is asked of the group. Having the box in front of you is the key, so
// it's only offered while nobody is on it.

// boxPeople is who is on the box, from its dashboard; on is false when
// the box has no hub.
func boxPeople(d dash) (on bool, people []boxPersonView, err error) {
	var st struct {
		On     bool
		People []boxPersonView
	}
	err = d.get("/api/box/people", &st)
	for _, p := range st.People {
		if !p.Removed {
			people = append(people, p)
		}
	}
	return st.On, people, err
}

// ownerLines are what the box's first person has, for the status screen.
func ownerLines(p boxPersonView, domain string) []screenLine {
	var out []screenLine
	switch {
	case p.Site != "":
		out = append(out, screenLine{Label: "Website", Text: "https://" + p.Site, OK: true},
			screenLine{Label: "Email", Text: peers.MailAddress(p.Site), OK: true})
	case p.Asked != "":
		out = append(out, screenLine{Label: "Website", Text: p.Asked + ": asked for, waiting", OK: true})
	}
	if sambaOn() {
		if _, err := os.Stat(sambaDir + "/" + strings.ToLower(p.Name) + ".conf"); err == nil {
			out = append(out, screenLine{Label: "Drive", Text: sambaAddress(p.Name), OK: true})
		}
	}
	return out
}

func runSetup(ctx context.Context, k keyReader, box string, d dash, peersPath string) error {
	const title = "Set up this box"
	list, _ := peers.Load(peersPath)
	domain := peers.GroupDomain(list)

	adminDraw(box, title, []string{
		"Three questions, then the box does the rest:",
		"",
		"  1  your first name",
		"  2  a password, for everything on the box",
		"  3  a name for your website and email",
	}, "", "Enter to start    Esc to go back")
	if b, err := k.key(ctx); err != nil || b == 27 {
		return err
	}

	// 1. Their name.
	note := ""
	var name string
	for {
		typed, ok, err := k.line(ctx, func(t string) {
			adminDraw(box, title, []string{"1  Your first name, as you'll sign in with it: " + t}, note, "Enter when done    Esc to stop")
		}, false)
		if err != nil || !ok {
			return err
		}
		name = strings.ToLower(strings.TrimSpace(typed))
		if sambaName.MatchString(name) {
			break
		}
		note = "Letters only (digits and dashes may follow), starting with a letter."
	}

	// 2. A password, twice.
	note = ""
	var pw string
	for {
		typed, ok, err := k.line(ctx, func(t string) {
			adminDraw(box, title, []string{"2  A password for everything on the box: the web, this screen's", "   admin, and your network drive.", "",
				"   At least 10 characters; a few words together is easiest.", "", "   Password: " + t}, note, "Enter when done    Esc to stop")
		}, true)
		if err != nil || !ok {
			return err
		}
		if e := password.OK(typed); e != nil {
			note = e.Error()
			continue
		}
		again, ok, err := k.line(ctx, func(t string) {
			adminDraw(box, title, []string{"2  The same password again: " + t}, "", "Enter when done    Esc to stop")
		}, true)
		if err != nil || !ok {
			return err
		}
		if again != typed {
			note = "The two didn't match. Try again."
			continue
		}
		pw = typed
		break
	}

	// 3. A name for the website and email.
	note = ""
	var label string
	for {
		typed, ok, err := k.line(ctx, func(t string) {
			show := t
			if show == "" {
				show = "yourname"
			}
			lines := []string{"3  A name for your website and email, such as your business's: " + t}
			if domain != "" {
				lines = append(lines, "", "   https://"+show+"."+domain, "   "+show+"@"+domain)
			}
			adminDraw(box, title, lines, note, "Enter when done    Esc to stop")
		}, false)
		if err != nil || !ok {
			return err
		}
		label = strings.ToLower(strings.TrimSpace(typed))
		if e := peers.ValidLabel(label); e != nil {
			note = e.Error()
			continue
		}
		if _, taken := peers.NameOwner(list, label+"."+domain); taken && domain != "" {
			note = label + "." + domain + " is taken; choose another."
			continue
		}
		break
	}

	yes, err := confirm(ctx, k, box, title, "Set up the box for "+name+", with the name "+label+"?")
	if err != nil || !yes {
		return err
	}

	// The rest, a line at a time.
	var done []string
	step := func(what string, err error) bool {
		mark := "[x] "
		if err != nil {
			mark = "[!] "
			what += ": " + err.Error()
		}
		done = append(done, mark+what)
		adminDraw(box, "Setting up "+name+"'s box", done, "", "")
		return err == nil
	}
	adminDraw(box, "Setting up "+name+"'s box", nil, "", "")
	var added passwordOut
	if !step("You're on the box, as its first person", d.call("/api/box/people/add", map[string]string{"name": name}, &added)) {
		return waitAny(ctx, k, box, name, done)
	}
	step("Your password", d.call("/api/box/people/set-password", map[string]string{"name": name, "password": pw}, nil))
	step("Logging in at the box as "+name, boxLogin(name, pw))
	path := personPath(d, name)
	step("Your folder, kept in the group as you add files", nil)
	if sambaOn() {
		step("Your network drive: "+sambaAddress(name), sambaShare(name, path, pw))
	}
	if domain != "" {
		if e := d.call("/api/box/people/name", map[string]string{"name": name, "label": label}, nil); e != nil {
			step("Your name "+label, e)
		} else if p, ok := personNamed(d, name); ok && p.Site != "" {
			step("Your website: https://"+p.Site, nil)
			step("Your email: "+peers.MailAddress(p.Site), nil)
		} else {
			step("Your name "+label+" is asked of the group: your website and email start once it's given", nil)
		}
	}
	return waitAny(ctx, k, box, name, done)
}

func waitAny(ctx context.Context, k keyReader, box, name string, done []string) error {
	adminDraw(box, "Setting up "+name+"'s box", append(done, "",
		"The status screen shows these from now on. Everything else, such as",
		"your phone and PCs, other people and your own domain, is in the admin:",
		"Ctrl+Alt+F2, log in as "+name+", then  sudo yggstore admin"), "", "Any key to finish")
	_, err := k.key(ctx)
	return err
}

func personNamed(d dash, name string) (boxPersonView, bool) {
	_, people, _ := boxPeople(d)
	for _, p := range people {
		if strings.EqualFold(p.Name, name) {
			return p, true
		}
	}
	return boxPersonView{}, false
}

// boxLogin lets the owner log in at the box with their password, and use
// sudo (so its admin). An account that's there already is left alone.
func boxLogin(name, pw string) error {
	if exec.Command("id", "-u", name).Run() == nil {
		return fmt.Errorf("there's a login called %s already, left as it is", name)
	}
	if out, err := exec.Command("useradd", "--create-home", "--shell", "/bin/bash", "--groups", "sudo",
		"--comment", "box owner", name).CombinedOutput(); err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	cmd := exec.Command("chpasswd")
	cmd.Stdin = strings.NewReader(name + ":" + pw + "\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return nil
}
