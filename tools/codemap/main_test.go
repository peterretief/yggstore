package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAnyModule(t *testing.T) {
	root := t.TempDir()
	write := func(p, s string) {
		os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0o755)
		os.WriteFile(filepath.Join(root, p), []byte(s), 0o644)
	}
	write("go.mod", "module example.org/thing\n\ngo 1.24\n")
	write("thing.go", "// Package thing does it.\npackage thing\n\nimport \"example.org/thing/internal/part\"\n\n// Do does it. More.\nfunc Do() { part.Help() }\n")
	write("internal/part/part.go", "package part\n\ntype T[K any] struct{}\n\nfunc (T[K]) Get() {}\n\nfunc Help() {}\n")
	write("internal/part/empty.go", "package part\n")
	write("internal/part/part_test.go", "package part\n\nfunc TestX() {}\n")
	write("other/go.mod", "module example.org/other\n")
	write("other/o.go", "package other\n")
	pkgs := read(root, "example.org/thing")
	if len(pkgs) != 2 {
		t.Fatalf("packages: %+v", pkgs)
	}
	part, top := pkgs[0], pkgs[1]
	if top.Path != "thing" || top.Dir != "" || top.Doc != "Package thing does it." || strings.Join(top.Imports, ",") != "part" {
		t.Fatalf("the module's root: %+v", top)
	}
	if part.Path != "part" || part.Dir != "internal/part" || len(part.Files) != 2 || part.Files[0].Funcs == nil {
		t.Fatalf("internal/part: %+v", part)
	}
	got := map[string]string{}
	for _, f := range part.Files[1].Funcs {
		got[f.Name] = f.Recv
	}
	if got["Get"] != "T" || len(got) != 2 {
		t.Fatalf("functions: %v", got)
	}
	out := filepath.Join(t.TempDir(), "map.html")
	if err := run(root, out); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	if !strings.Contains(string(b), "<title>thing code map</title>") || !strings.Contains(string(b), `"module":"example.org/thing"`) {
		t.Fatal("the page doesn't name the module")
	}
}
