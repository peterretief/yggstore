// Command codemap writes one HTML page that maps a Go module: every
// package, what it builds on within the module, and the functions in each
// of its files, searchable and, for a module on GitHub, linked to the
// source. Tests are left out.
//
//	go run ./tools/codemap -o docs/code-map.html     # yggstore's own
//	go run github.com/peterretief/yggstore/tools/codemap@main -root ~/src/other -o other-map.html
package main

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

type fn struct {
	Name string `json:"n"`
	Recv string `json:"r,omitempty"`
	Line int    `json:"l"`
	Len  int    `json:"s"`
	Exp  bool   `json:"e,omitempty"`
	Doc  string `json:"d,omitempty"`
}

type file struct {
	Name  string `json:"name"`
	Lines int    `json:"lines"`
	Funcs []fn   `json:"funcs"`
}

type pkg struct {
	Path    string   `json:"path"` // as shown: the directory, without internal/
	Dir     string   `json:"dir"`  // the directory in the module ("" at its root)
	Doc     string   `json:"doc,omitempty"`
	Imports []string `json:"imports"` // other packages in the module, by Path
	Files   []file   `json:"files"`
}

// page is what the page's script is given.
type page struct {
	Module string `json:"module"`
	Name   string `json:"name"`             // the module's last element, for the title
	Source string `json:"source,omitempty"` // https://github.com/OWNER/REPO/blob/REV/, for links
	Rev    string `json:"rev,omitempty"`
	Pkgs   []*pkg `json:"pkgs"`
}

//go:embed head.html
var head string

//go:embed page.js
var script []byte

func main() {
	root := flag.String("root", ".", "the module's folder (where its go.mod is)")
	out := flag.String("o", "code-map.html", "the page to write")
	flag.Parse()
	if err := run(*root, *out); err != nil {
		fmt.Fprintln(os.Stderr, "codemap:", err)
		os.Exit(1)
	}
}

func run(root, out string) error {
	mod, err := modulePath(filepath.Join(root, "go.mod"))
	if err != nil {
		return err
	}
	pg := page{Module: mod, Name: mod[strings.LastIndex(mod, "/")+1:], Pkgs: read(root, mod)}
	if len(pg.Pkgs) == 0 {
		return errors.New("no Go packages under " + root)
	}
	if b, err := exec.Command("git", "-C", root, "rev-parse", "--short", "HEAD").Output(); err == nil {
		pg.Rev = strings.TrimSpace(string(b))
	}
	if parts := strings.Split(mod, "/"); len(parts) >= 3 && parts[0] == "github.com" && pg.Rev != "" {
		// The module's folder in its repository, if it isn't the root.
		sub := ""
		if top, err := exec.Command("git", "-C", root, "rev-parse", "--show-prefix").Output(); err == nil {
			sub = strings.TrimSpace(string(top))
		}
		pg.Source = "https://github.com/" + parts[1] + "/" + parts[2] + "/blob/" + pg.Rev + "/" + sub
	}
	data, _ := json.Marshal(pg)
	var b bytes.Buffer
	b.WriteString(strings.ReplaceAll(head, "{{NAME}}", html.EscapeString(pg.Name)))
	b.WriteString(`<script type="application/json" id="data">`)
	b.Write(bytes.ReplaceAll(data, []byte("</"), []byte(`<\/`)))
	b.WriteString("</script>\n<script>\n")
	b.Write(script)
	b.WriteString("</script>\n</body>\n</html>\n")
	if err := os.WriteFile(out, b.Bytes(), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s: %s, %d packages\n", out, mod, len(pg.Pkgs))
	return nil
}

// modulePath is the module line of a go.mod.
func modulePath(gomod string) (string, error) {
	f, err := os.Open(gomod)
	if err != nil {
		return "", fmt.Errorf("%w (run it in a Go module's folder, or give -root)", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if m, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "module "); ok {
			return strings.Trim(strings.TrimSpace(m), `"`), nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", errors.New(gomod + " has no module line")
}

// shown is how a package's directory is shown: without internal/, and
// the module's own name at its root.
func shown(dir, name string) string {
	switch dir {
	case "", ".":
		return name
	case "internal":
		return "internal"
	}
	return strings.TrimPrefix(dir, "internal/")
}

// read lists the module's packages, tests left out. Folders with their
// own go.mod are other modules, and left out too.
func read(root, mod string) []*pkg {
	name := mod[strings.LastIndex(mod, "/")+1:]
	byDir := map[string]*pkg{}
	fset := token.NewFileSet()
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			n := d.Name()
			if p != root {
				if strings.HasPrefix(n, ".") || strings.HasPrefix(n, "_") || n == "testdata" || n == "vendor" || n == "node_modules" {
					return filepath.SkipDir
				}
				if _, err := os.Stat(filepath.Join(p, "go.mod")); err == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, parser.ParseComments)
		if err != nil {
			return nil
		}
		dir, _ := filepath.Rel(root, filepath.Dir(p))
		dir = filepath.ToSlash(dir)
		if dir == "." {
			dir = ""
		}
		pk := byDir[dir]
		if pk == nil {
			pk = &pkg{Path: shown(dir, name), Dir: dir, Imports: []string{}}
			byDir[dir] = pk
		}
		if f.Doc != nil && pk.Doc == "" {
			pk.Doc = firstSentence(f.Doc.Text())
		}
		for _, im := range f.Imports {
			v := strings.Trim(im.Path.Value, `"`)
			t, ok := strings.CutPrefix(v, mod+"/")
			if v == mod {
				t, ok = "", true
			}
			if t = shown(t, name); ok && !slices.Contains(pk.Imports, t) {
				pk.Imports = append(pk.Imports, t)
			}
		}
		fl := file{Name: filepath.Base(p), Lines: fset.File(f.Pos()).LineCount(), Funcs: []fn{}}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			x := fn{Name: fd.Name.Name, Line: fset.Position(fd.Pos()).Line, Exp: fd.Name.IsExported()}
			x.Len = fset.Position(fd.End()).Line - x.Line + 1
			if fd.Recv != nil && len(fd.Recv.List) > 0 {
				t := fd.Recv.List[0].Type
				if s, ok := t.(*ast.StarExpr); ok {
					t = s.X
				}
				switch ix := t.(type) {
				case *ast.IndexExpr:
					t = ix.X
				case *ast.IndexListExpr:
					t = ix.X
				}
				if id, ok := t.(*ast.Ident); ok {
					x.Recv = id.Name
				}
			}
			if fd.Doc != nil {
				x.Doc = firstSentence(fd.Doc.Text())
			}
			fl.Funcs = append(fl.Funcs, x)
		}
		pk.Files = append(pk.Files, fl)
		return nil
	})
	var out []*pkg
	for _, p := range byDir {
		sort.Strings(p.Imports)
		sort.Slice(p.Files, func(i, j int) bool { return p.Files[i].Name < p.Files[j].Name })
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func firstSentence(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if i := strings.Index(s, ". "); i > 0 {
		s = s[:i+1]
	}
	if r := []rune(s); len(r) > 160 {
		s = string(r[:157]) + "…"
	}
	return s
}
