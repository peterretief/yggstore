package site

import (
	"strings"
	"testing"
)

func TestYggButton(t *testing.T) {
	page := "<html><body><h1>Menu</h1></body></html>"
	on := WithYggButton(page, "Kitchen.Example.org")
	if !strings.Contains(on, YggGivePage+"?to=kitchen.example.org") {
		t.Fatalf("no button for the site: %s", on)
	}
	if !strings.HasSuffix(on, "</body></html>") {
		t.Fatalf("button not before </body>: %s", on)
	}
	if again := WithYggButton(on, "kitchen.example.org"); strings.Count(again, yggButtonStart) != 1 {
		t.Fatalf("added twice: %s", again)
	}
	if off := WithoutYggButton(on); off != page {
		t.Fatalf("not taken off cleanly:\n%s", off)
	}
	both := WithContactLink(on)
	if WithoutYggButton(both) != WithContactLink(page) || WithoutContactLink(both) != on {
		t.Fatal("the button and the contact link disturb each other")
	}
}
