package site

import (
	"bytes"
	"fmt"
	"html"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	gmhtml "github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// Markdown pages, for people who write and people who build alike: a
// site's folder with .md files in it is built into pages when it's
// published, by the rules of the desktop's Markdown build (YGGSITES
// scripts/md.mjs), so the same folder works either way:
//
//	index.md          -> index.html          (the home page)
//	about.md          -> about/index.html    (https://SITE/about/)
//	courses/intro.md  -> courses/intro/index.html
//
// _layout.html is the page around each one ({{title}}, {{content}},
// {{site}}); without one, a plain layout is used. A page may start with
// title: and layout: between --- lines. Links to .md files become page
// addresses, and links and pictures beside a page keep working once it's
// in a folder of its own. Headings get ids ("## Start {#start}", or made
// from the words). Files and folders starting with _ or ., README.md and
// node_modules aren't published; every other file is, as it is. A folder
// with no .md pages is published exactly as it is.

// defaultLayout is the page around Markdown pages of a site with no
// _layout.html of its own.
const defaultLayout = `<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{title}}</title>
<style>
body{font-family:system-ui,sans-serif;max-width:42em;margin:2.5em auto;padding:0 1em;line-height:1.6;color:#1b1f24;background:#fff}
img{max-width:100%;height:auto} a{color:#2f5fbf} pre{overflow-x:auto;background:#f3f4f6;padding:.8em;border-radius:6px}
table{border-collapse:collapse} th,td{border:1px solid #ddd;padding:.3em .6em}
nav{margin-bottom:2em;font-size:.95em} nav a{margin-right:1em}
@media (prefers-color-scheme: dark){body{color:#e6e9ed;background:#111418} a{color:#79a2f2} pre{background:#1a1e24} th,td{border-color:#333}}
</style>
</head>
<body>
<nav><a href="/">Home</a></nav>
{{content}}
</body>
</html>
`

// mdPage is where a .md file is published: "about.md" -> "about/",
// "index.md" -> "", "courses/index.md" -> "courses/".
func mdPage(rel string) string {
	p := strings.TrimSuffix(rel, ".md")
	switch {
	case p == "index":
		return ""
	case strings.HasSuffix(p, "/index"):
		return strings.TrimSuffix(p, "index")
	}
	return p + "/"
}

func mdSkipped(name string) bool {
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "node_modules"
}

// HasMarkdown is whether folder has Markdown pages to build.
func HasMarkdown(folder string) bool {
	found := false
	filepath.WalkDir(folder, func(p string, e os.DirEntry, err error) error {
		if err != nil || found {
			return filepath.SkipAll
		}
		if p != folder && mdSkipped(e.Name()) {
			if e.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") && p != filepath.Join(folder, "README.md") {
			found = true
		}
		return nil
	})
	return found
}

var frontMatter = regexp.MustCompile(`(?s)\A---\r?\n(.*?)\r?\n---\r?\n`)
var metaLine = regexp.MustCompile(`^(\w+):\s*(.*)$`)
var firstHeading = regexp.MustCompile(`(?m)^#\s+(.+)$`)
var hasScheme = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`)

// localLinks rewrites a page's links and pictures: one to a .md file goes
// to that page's address; one to another file beside the page is made to
// start at the site's top, since the page moves into a folder of its own.
type localLinks struct{ here string } // the .md file, from the site's top

func (l localLinks) Transform(doc *ast.Document, _ text.Reader, _ parser.Context) {
	ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n := n.(type) {
		case *ast.Link:
			n.Destination = []byte(l.rewrite(string(n.Destination)))
		case *ast.Image:
			n.Destination = []byte(l.rewrite(string(n.Destination)))
		}
		return ast.WalkContinue, nil
	})
}

func (l localLinks) rewrite(dest string) string {
	if dest == "" || strings.HasPrefix(dest, "/") || strings.HasPrefix(dest, "#") || hasScheme.MatchString(dest) {
		return dest
	}
	p, frag, _ := strings.Cut(dest, "#")
	p, query, _ := strings.Cut(p, "?")
	if frag != "" {
		frag = "#" + frag
	}
	if query != "" {
		query = "?" + query
	}
	target := path.Clean(path.Join(path.Dir(l.here), p))
	if strings.HasPrefix(target, "../") || target == ".." {
		return dest
	}
	if strings.HasSuffix(target, ".md") {
		return "/" + mdPage(target) + frag
	}
	return "/" + target + query + frag
}

// BuildMarkdown builds the site in src into dst: Markdown pages, and every
// other file as it is. On the home page, contact puts the contact link
// and the site's notices go in, as they would on an index.html.
func BuildMarkdown(src, dst, siteName string, contact bool) (pages, files int, err error) {
	layouts := map[string]string{}
	layout := func(name string) (string, error) {
		if l, ok := layouts[name]; ok {
			return l, nil
		}
		b, err := os.ReadFile(filepath.Join(src, name))
		switch {
		case err == nil:
			layouts[name] = string(b)
		case os.IsNotExist(err) && name == "_layout.html":
			layouts[name] = defaultLayout
		default:
			return "", fmt.Errorf("the layout %s: %w", name, err)
		}
		return layouts[name], nil
	}
	var mds []string
	err = filepath.WalkDir(src, func(p string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == src {
			return nil
		}
		if mdSkipped(e.Name()) {
			if e.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel := filepath.ToSlash(must(filepath.Rel(src, p)))
		switch {
		case e.IsDir():
			return nil
		case rel == "README.md":
			return nil
		case strings.HasSuffix(rel, ".md"):
			mds = append(mds, rel)
			return nil
		case !e.Type().IsRegular():
			return nil
		}
		files++
		return copyFile(p, filepath.Join(dst, filepath.FromSlash(rel)))
	})
	if err != nil {
		return 0, 0, err
	}
	// Pages last, so a page wins over a file of the same name.
	for _, rel := range mds {
		raw, err := os.ReadFile(filepath.Join(src, filepath.FromSlash(rel)))
		if err != nil {
			return 0, 0, err
		}
		body, meta := string(raw), map[string]string{}
		if m := frontMatter.FindStringSubmatch(body); m != nil {
			for _, line := range strings.Split(m[1], "\n") {
				if kv := metaLine.FindStringSubmatch(strings.TrimSpace(line)); kv != nil {
					meta[kv[1]] = strings.TrimSpace(kv[2])
				}
			}
			body = body[len(m[0]):]
		}
		title := meta["title"]
		if title == "" {
			if m := firstHeading.FindStringSubmatch(body); m != nil {
				title = strings.TrimSpace(m[1])
			} else {
				title = strings.TrimSuffix(path.Base(rel), ".md")
			}
		}
		md := goldmark.New(
			goldmark.WithExtensions(extension.GFM),
			goldmark.WithParserOptions(parser.WithAutoHeadingID(), parser.WithAttribute(),
				parser.WithASTTransformers(util.Prioritized(localLinks{here: rel}, 100))),
			goldmark.WithRendererOptions(gmhtml.WithUnsafe()), // the owner's own page, as their HTML is
		)
		var out bytes.Buffer
		if err := md.Convert([]byte(body), &out); err != nil {
			return 0, 0, fmt.Errorf("%s: %w", rel, err)
		}
		name := meta["layout"]
		if name == "" {
			name = "_layout.html"
		}
		l, err := layout(name)
		if err != nil {
			return 0, 0, fmt.Errorf("%s: %w", rel, err)
		}
		page := strings.ReplaceAll(l, "{{title}}", html.EscapeString(title))
		page = strings.ReplaceAll(page, "{{site}}", html.EscapeString(siteName))
		page = strings.Replace(page, "{{content}}", out.String(), 1)
		if mdPage(rel) == "" {
			if list, _ := Notices(src); len(list) > 0 {
				page = WithNotices(page, list)
			}
			if contact {
				page = WithContactLink(page)
			}
		}
		to := filepath.Join(dst, filepath.FromSlash(mdPage(rel)), "index.html")
		if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
			return 0, 0, err
		}
		if err := os.WriteFile(to, []byte(page), 0o644); err != nil {
			return 0, 0, err
		}
		pages++
	}
	return pages, files, nil
}

func must(s string, err error) string {
	if err != nil {
		return ""
	}
	return s
}

func copyFile(from, to string) error {
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	if os.Link(from, to) == nil {
		return nil
	}
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
