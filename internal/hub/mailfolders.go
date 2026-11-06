package hub

import (
	"bytes"
	netmail "net/mail"
	"regexp"
	"slices"
	"strings"

	"github.com/peterretief/yggstore/internal/mail"
)

// Mail is sorted by the address it came to: info@acme.com lands in a
// folder called info, made the first time, as does tommy@ the group's
// domain in tommy, and contact form messages in ContactFolder. The mail page
// shows folders and All mail, with no Inbox. Spammers try made-up addresses,
// so at most maxPrefixFolders are made; past that a new address stays in
// the Inbox, which the page calls Unsorted.
const (
	maxPrefixFolders = 20
	ContactFolder    = "Contact forms"
)

var prefixName = regexp.MustCompile(`^[a-z0-9][a-z0-9._+-]{0,30}$`)

// prefixFolder is the folder for a message: ContactFolder, or the part
// before the @ of the address it was delivered to, at one of own or the
// group's domain. "": it stays in the Inbox.
func prefixFolder(raw []byte, own []string, group string) string {
	m, err := netmail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return ""
	}
	if m.Header.Get("X-Yggstore-Contact-Form") != "" {
		return ContactFolder
	}
	local, domain, ok := strings.Cut(strings.ToLower(strings.TrimSpace(m.Header.Get("Delivered-To"))), "@")
	if !ok || !(slices.Contains(own, domain) || (group != "" && domain == group)) || !prefixName.MatchString(local) {
		return ""
	}
	if slices.ContainsFunc(mail.StandardFolders, func(f string) bool { return strings.EqualFold(f, local) }) {
		return "" // inbox@ and sent@ aren't the folders of those names
	}
	return local
}

// fileByPrefix moves message id into its address's folder, if it has one.
func fileByPrefix(box *mail.Box, id string, raw []byte, own []string, group string) error {
	folder := prefixFolder(raw, own, group)
	if folder == "" {
		return nil
	}
	have := box.Folders()
	if !slices.ContainsFunc(have, func(f string) bool { return strings.EqualFold(f, folder) }) {
		if folder != ContactFolder && len(have)-len(mail.StandardFolders) >= maxPrefixFolders {
			return nil
		}
		if err := box.CreateFolder(folder); err != nil {
			return err
		}
	}
	return box.Move(id, folder)
}
