package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/peterretief/yggstore/internal/site"
)

// Publishing from a developer's machine (docs/sites.md, Publishing with
// git push): yggstore.json in the project says which site, how to build
// it and where the build goes; yggstore site push builds it here and
// sends only the built folder to the box that publishes the site, through
// its dashboard over Yggdrasil, as one of the owner's devices. The git
// hook site init installs does the same on git push: main goes live, a
// branch called preview becomes the site's preview.

const projectFile = "yggstore.json"

type project struct {
	Site   string `json:"site"`            // shop.example.org
	Box    string `json:"box"`             // the dashboard of the box that publishes it
	Build  string `json:"build,omitempty"` // run here first, e.g. "npm run build"
	Output string `json:"output"`          // the built folder, e.g. "dist"; "." for the project itself

	dir string // where yggstore.json is
}

// findProject looks for yggstore.json in dir and the folders above it.
func findProject(dir string) (project, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return project{}, err
	}
	for d := abs; ; d = filepath.Dir(d) {
		b, err := os.ReadFile(filepath.Join(d, projectFile))
		if err == nil {
			var p project
			if err := json.Unmarshal(b, &p); err != nil {
				return project{}, fmt.Errorf("%s: %w", filepath.Join(d, projectFile), err)
			}
			p.dir = d
			if p.Output == "" {
				p.Output = "."
			}
			return p, nil
		}
		if filepath.Dir(d) == d {
			return project{}, fmt.Errorf("no %s here or above: run yggstore site init first", projectFile)
		}
	}
}

const prePushHook = `#!/bin/sh
# yggstore: publish the site on git push (yggstore site init made this).
# main or master goes live; a branch called preview becomes the preview.
# Skip it once with: git push --no-verify
while read local_ref local_sha remote_ref remote_sha; do
	case "$remote_ref" in
	refs/heads/main|refs/heads/master) yggstore site push || exit 1 ;;
	refs/heads/preview) yggstore site push -preview || exit 1 ;;
	esac
done
exit 0
`

func cmdSiteInit(dir, name, box, build, output string) error {
	if name == "" || box == "" {
		return errors.New("usage: yggstore site init [FOLDER] -name DOMAIN -box BOX [-build \"npm run build\"] [-output dist]\n" +
			"BOX is the dashboard of the box that publishes the site, as your browser opens it, e.g. http://[200:...]:7480")
	}
	if err := site.ValidName(site.Normalise(name)); err != nil {
		return err
	}
	u, err := url.Parse(box)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%q isn't the address of a dashboard, such as http://[200:...]:7480", box)
	}
	if output == "" {
		output = "."
	}
	p := project{Site: site.Normalise(name), Box: strings.TrimSuffix(box, "/"), Build: build, Output: output}
	b, _ := json.MarshalIndent(p, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, projectFile), append(b, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Printf("Wrote %s.\n", filepath.Join(dir, projectFile))

	// The local web node's data isn't for git.
	ignore := filepath.Join(dir, ".gitignore")
	if old, _ := os.ReadFile(ignore); !strings.Contains(string(old), site.DevDir) {
		f, err := os.OpenFile(ignore, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			if len(old) > 0 && !bytes.HasSuffix(old, []byte("\n")) {
				f.WriteString("\n")
			}
			f.WriteString(site.DevDir + "/\n")
			f.Close()
		}
	}

	hooks := filepath.Join(dir, ".git", "hooks")
	if fi, err := os.Stat(hooks); err != nil || !fi.IsDir() {
		fmt.Println("No git repository here, so no hook: publish with yggstore site push.")
		return nil
	}
	hook := filepath.Join(hooks, "pre-push")
	if old, err := os.ReadFile(hook); err == nil && !strings.Contains(string(old), "yggstore") {
		return fmt.Errorf("%s exists already; add a line to it that runs yggstore site push", hook)
	}
	if err := os.WriteFile(hook, []byte(prePushHook), 0o755); err != nil {
		return err
	}
	fmt.Println("git push now publishes: main goes live, a branch called preview becomes its preview.")
	return nil
}

func cmdSitePush(ctx context.Context, dir string, preview bool) error {
	p, err := findProject(dir)
	if err != nil {
		return err
	}
	if p.Build != "" {
		fmt.Printf("Building: %s\n", p.Build)
		shell, flag := "sh", "-c"
		if runtime.GOOS == "windows" {
			shell, flag = "cmd", "/c"
		}
		cmd := exec.CommandContext(ctx, shell, flag, p.Build)
		cmd.Dir, cmd.Stdout, cmd.Stderr = p.dir, os.Stderr, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("the build failed (%w): nothing was sent", err)
		}
	}
	out := filepath.Join(p.dir, p.Output)
	var zipped bytes.Buffer
	files, err := zipSite(out, &zipped)
	if err != nil {
		return err
	}
	if files == 0 {
		return fmt.Errorf("%s has no files to send", out)
	}
	fmt.Printf("Sending %d files (%s) to %s ...\n", files, size(int64(zipped.Len())), p.Box)

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, _ := mw.CreateFormFile("zip", "site.zip")
	part.Write(zipped.Bytes())
	mw.Close()
	target := p.Box + "/api/sites/publish?site=" + url.QueryEscape(p.Site)
	if preview {
		target += "&preview=1"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Yggstore", "1")
	resp, err := (&http.Client{Timeout: 30 * time.Minute}).Do(req)
	if err != nil {
		return fmt.Errorf("the box didn't answer (%w): is Yggdrasil running here, and is this one of the owner's devices?", err)
	}
	defer resp.Body.Close()
	reply, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		msg := strings.TrimSpace(string(reply))
		if resp.StatusCode == http.StatusForbidden && strings.Contains(strings.ToLower(msg), "device") {
			msg += "\nLet this machine in on the box: its admin, Devices."
		}
		return fmt.Errorf("the box didn't publish it: %s", msg)
	}
	var r struct {
		PreviewLink string `json:"preview_link"`
		Folder      string `json:"folder"`
		Version     struct {
			ID string `json:"id"`
		} `json:"version"`
	}
	json.Unmarshal(reply, &r)
	switch {
	case r.PreviewLink != "":
		fmt.Printf("%s: the preview, for whoever has the link:\n\n  %s\n\n", p.Site, r.PreviewLink)
	case r.Folder != "" && preview:
		fmt.Printf("%s: the box makes it the preview within a minute; the link is in the file beside its folder, and in the box's admin.\n", p.Site)
	case r.Folder != "":
		fmt.Printf("%s: the box publishes it within a minute.\n", p.Site)
	default:
		fmt.Printf("%s: published (version %.8s).\n", p.Site, r.Version.ID)
	}
	return nil
}

// zipSite zips a built folder as publishing would take it: no hidden files
// or folders, no local web node, no yggstore.json or node_modules.
func zipSite(dir string, w io.Writer) (int, error) {
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return 0, fmt.Errorf("%s isn't a folder: is output in %s right, and did the build run?", dir, projectFile)
	}
	zw := zip.NewWriter(w)
	n := 0
	err := filepath.WalkDir(dir, func(p string, e os.DirEntry, err error) error {
		if err != nil || p == dir {
			return err
		}
		name := e.Name()
		if (strings.HasPrefix(name, ".") && name != ".well-known") || name == "node_modules" || name == site.DevDir || name == projectFile {
			if e.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if e.IsDir() || !e.Type().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		fw, err := zw.Create(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		if _, err := io.Copy(fw, bufio.NewReader(f)); err != nil {
			return err
		}
		n++
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, zw.Close()
}
