// Command codemap writes docs/code-map.html: every package in the
// repository, what it builds on, and the functions in each of its files.
//
//	go run ./tools/codemap            # from the repository's root
package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
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
	Path    string   `json:"path"`
	Doc     string   `json:"doc,omitempty"`
	Imports []string `json:"imports"`
	Files   []file   `json:"files"`
}

//go:embed head.html
var head []byte

//go:embed page.js
var script []byte

func main() {
	root := flag.String("root", ".", "the repository")
	out := flag.String("o", "docs/code-map.html", "the page to write")
	flag.Parse()
	pkgs := read(*root, "github.com/peterretief/yggstore")
	rev, _ := exec.Command("git", "-C", *root, "rev-parse", "--short", "HEAD").Output()
	data, _ := json.Marshal(pkgs)
	var b bytes.Buffer
	b.Write(head)
	fmt.Fprintf(&b, "<script type=\"application/json\" id=\"data\" data-rev=\"%s\">", strings.TrimSpace(string(rev)))
	b.Write(bytes.ReplaceAll(data, []byte("</"), []byte("<\\/")))
	b.WriteString("</script>\n<script>\n")
	b.Write(script)
	b.WriteString("</script>\n</body>\n</html>\n")
	if err := os.WriteFile(*out, b.Bytes(), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s: %d packages\n", *out, len(pkgs))
}

// read lists root's packages (tests left out), named without internal/.
func read(root, mod string) []*pkg {
	byDir := map[string]*pkg{}
	fset := token.NewFileSet()
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			n := d.Name()
			if p != root && (strings.HasPrefix(n, ".") || n == "testdata" || n == "node_modules" || n == "docs" || n == "get" || n == "bin" || n == "tools") {
				return filepath.SkipDir
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
		pk := byDir[dir]
		if pk == nil {
			pk = &pkg{Path: filepath.ToSlash(dir)}
			byDir[dir] = pk
		}
		if f.Doc != nil && pk.Doc == "" {
			pk.Doc = firstSentence(f.Doc.Text())
		}
		for _, im := range f.Imports {
			v := strings.Trim(im.Path.Value, `"`)
			if strings.HasPrefix(v, mod+"/") {
				t := strings.TrimPrefix(v, mod+"/")
				if !contains(pk.Imports, t) {
					pk.Imports = append(pk.Imports, t)
				}
			}
		}
		fl := file{Name: filepath.Base(p), Lines: fset.File(f.Pos()).LineCount()}
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
				if ix, ok := t.(*ast.IndexExpr); ok {
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
		if p.Imports == nil {
			p.Imports = []string{}
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	for _, p := range out {
		p.Path = strings.TrimPrefix(p.Path, "internal/")
		for i, im := range p.Imports {
			p.Imports[i] = strings.TrimPrefix(im, "internal/")
		}
	}
	return out
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func firstSentence(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if i := strings.Index(s, ". "); i > 0 {
		s = s[:i+1]
	}
	if len(s) > 160 {
		s = s[:157] + "…"
	}
	return s
}
