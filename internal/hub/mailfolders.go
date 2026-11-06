package hub

import (
	"bytes"
	netmail "net/mail"
	"regexp"
	"slices"
	"strings"

	"github.com/peterretief/yggstore/internal/mail"
)

// A domain of someone's own is one mailbox, so mail is sorted by the
// address it came to: info@acme.com lands in a folder called info, made the
// first time. Spammers try made-up addresses, so at most maxPrefixFolders
// are made; past that a new address stays in the Inbox.
const maxPrefixFolders = 20

var prefixName = regexp.MustCompile(`^[a-z0-9][a-z0-9._+-]{0,30}$`)

// prefixFolder is the folder for a message: the part before the @ of the
// address it was delivered to, at one of own. "": it stays in the Inbox.
func prefixFolder(raw []byte, own []string) string {
	m, err := netmail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return ""
	}
	local, domain, ok := strings.Cut(strings.ToLower(strings.TrimSpace(m.Header.Get("Delivered-To"))), "@")
	if !ok || !slices.Contains(own, domain) || !prefixName.MatchString(local) {
		return ""
	}
	if slices.ContainsFunc(mail.StandardFolders, func(f string) bool { return strings.EqualFold(f, local) }) {
		return "" // inbox@ and sent@ aren't the folders of those names
	}
	return local
}

// fileByPrefix moves message id into its address's folder, if it has one.
func fileByPrefix(box *mail.Box, id string, raw []byte, own []string) error {
	folder := prefixFolder(raw, own)
	if folder == "" {
		return nil
	}
	have := box.Folders()
	if !slices.ContainsFunc(have, func(f string) bool { return strings.EqualFold(f, folder) }) {
		if len(have)-len(mail.StandardFolders) >= maxPrefixFolders {
			return nil
		}
		if err := box.CreateFolder(folder); err != nil {
			return err
		}
	}
	return box.Move(id, folder)
}
