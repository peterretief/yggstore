package site

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildMarkdown(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(src, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}
	write("index.md", "# Harbour Kitchen\n\nSee [the menu](menu.md#fish) and [about us](about/index.md).\n")
	write("menu.md", "---\ntitle: Our menu\n---\n## Fish {#fish}\n\n![Hake](hake.jpg)\n\n| Dish | Price |\n|---|---|\n| Hake | R120 |\n")
	write("about/index.md", "# About\n\n## Who we are\n\n[Home](../index.md) <b>raw html</b>\n")
	write("hake.jpg", "jpeg")
	write("style.css", "body{}")
	write("README.md", "notes for the developer")
	write("_drafts/next.md", "# Not yet")
	write(".hidden", "x")
	write("node_modules/x/index.js", "x")
	write("menu/index.html", "replaced by menu.md")
	if !HasMarkdown(src) {
		t.Fatal("HasMarkdown missed the pages")
	}
	AddNotice(src, "Open on Sunday", "From 10.", time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC))

	pages, files, err := BuildMarkdown(src, dst, "harbourkitchen.example.org", true)
	if err != nil || pages != 3 {
		t.Fatalf("built %d pages, %d files: %v", pages, files, err)
	}
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		return string(b)
	}
	home := read("index.html")
	for _, want := range []string{"<title>Harbour Kitchen</title>", `href="/menu/#fish"`, `href="/about/"`, "Open on Sunday", ContactPath} {
		if !strings.Contains(home, want) {
			t.Errorf("home page lacks %q:\n%s", want, home)
		}
	}
	menu := read("menu/index.html")
	for _, want := range []string{"<title>Our menu</title>", `id="fish"`, `src="/hake.jpg"`, "<table>", "R120"} {
		if !strings.Contains(menu, want) {
			t.Errorf("menu lacks %q:\n%s", want, menu)
		}
	}
	if strings.Contains(menu, "replaced by menu.md") || strings.Contains(menu, "Open on Sunday") {
		t.Error("the page didn't win over the file, or notices went on a page that isn't home")
	}
	about := read("about/index.html")
	if !strings.Contains(about, `href="/"`) || !strings.Contains(about, `id="who-we-are"`) || !strings.Contains(about, "<b>raw html</b>") {
		t.Errorf("about:\n%s", about)
	}
	read("hake.jpg")
	read("style.css")
	read(NoticesFile)
	for _, gone := range []string{"README.md", "_drafts/next.md", "_drafts/next/index.html", ".hidden", "node_modules/x/index.js", "index.md"} {
		if _, err := os.Stat(filepath.Join(dst, gone)); err == nil {
			t.Errorf("%s was published", gone)
		}
	}

	// A layout of the site's own.
	write("_layout.html", "<html><head><title>{{title}} | {{site}}</title></head><body><main>{{content}}</main></body></html>")
	dst2 := t.TempDir()
	if _, _, err := BuildMarkdown(src, dst2, "harbourkitchen.example.org", false); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dst2, "menu", "index.html"))
	if !strings.HasPrefix(string(b), "<html><head><title>Our menu | harbourkitchen.example.org</title>") {
		t.Errorf("own layout:\n%s", b)
	}

	// A folder of HTML only isn't built at all.
	plain := t.TempDir()
	os.WriteFile(filepath.Join(plain, "index.html"), []byte("<h1>x</h1>"), 0o644)
	os.WriteFile(filepath.Join(plain, "README.md"), []byte("x"), 0o644)
	if HasMarkdown(plain) {
		t.Error("README.md alone counted as Markdown pages")
	}
}
