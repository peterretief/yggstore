package hub

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/peterretief/yggstore/internal/mail"
)

func msgTo(addr string) []byte {
	return []byte("Delivered-To: " + addr + "\r\nReturn-Path: <a@b.net>\r\nFrom: a@b.net\r\nSubject: hi\r\n\r\nhello\r\n")
}

func TestPrefixFolder(t *testing.T) {
	own := []string{"acme.com"}
	for addr, want := range map[string]string{
		"info@acme.com": "info", "SALES@Acme.com": "sales", "first.last@acme.com": "first.last",
		"tommy@withcare.example": "", "sent@acme.com": "", "INBOX@acme.com": "", "trash@acme.com": "",
		"../x@acme.com": "", "": "",
	} {
		if got := prefixFolder(msgTo(addr), own); got != want {
			t.Errorf("%q: %q, want %q", addr, got, want)
		}
	}
	if got := prefixFolder([]byte("Subject: no address\r\n\r\nhi\r\n"), own); got != "" {
		t.Errorf("no Delivered-To: %q", got)
	}
}

func TestFileByPrefix(t *testing.T) {
	box := &mail.Box{Dir: t.TempDir()}
	put := func(id string) {
		os.WriteFile(filepath.Join(box.Dir, id+".json"), []byte(`{"received":1}`), 0o600)
		os.WriteFile(filepath.Join(box.Dir, id+".sealed"), []byte("x"), 0o600)
	}
	own := []string{"acme.com"}
	in := func(folder, id string) bool {
		es, err := box.Folder(folder)
		return err == nil && slices.ContainsFunc(es, func(e mail.Entry) bool { return e.ID == id })
	}
	put("a")
	if err := fileByPrefix(box, "a", msgTo("info@acme.com"), own); err != nil {
		t.Fatal(err)
	}
	put("b")
	fileByPrefix(box, "b", msgTo("info@acme.com"), own) // the same folder again
	if !in("info", "a") || !in("info", "b") {
		t.Fatal("not filed under info")
	}
	// Spam to made-up addresses stops making folders.
	for i := range maxPrefixFolders + 5 {
		id := fmt.Sprintf("s%d", i)
		put(id)
		if err := fileByPrefix(box, id, msgTo(fmt.Sprintf("x%d@acme.com", i)), own); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(box.Folders()) - len(mail.StandardFolders); n != maxPrefixFolders {
		t.Fatalf("%d folders made", n)
	}
	last := fmt.Sprintf("s%d", maxPrefixFolders+4)
	if !in(mail.Inbox, last) {
		t.Fatal("past the limit, it should stay in the Inbox")
	}
	// An existing folder still takes mail past the limit.
	put("c")
	fileByPrefix(box, "c", msgTo("INFO@acme.com"), own)
	if !in("info", "c") {
		t.Fatal("info should still take mail")
	}
}
