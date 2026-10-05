// Package invite lets someone join a group with one string. An admin makes
// an invite on their dashboard; it carries the admin node's address, some
// Yggdrasil peers to connect through, the inviter's sharing code and a
// one-time secret. The newcomer runs "yggstore join INVITE": their machine
// asks the admin node to be added, presenting the secret, and gets the
// member list back. The dashboard's list sync then tells every node.
package invite

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/peterretief/yggstore/internal/contacts"
	"github.com/peterretief/yggstore/internal/peers"
)

const prefix = "yggjoin1:"

// TTL is how long an invite can be used.
const TTL = 7 * 24 * time.Hour

// Invite is what the newcomer is given.
type Invite struct {
	Group       string     `json:"group"`
	From        string     `json:"from"`         // the inviter's name
	SharingCode string     `json:"sharing_code"` // the inviter's, so you can share at once
	Admin       peers.Peer `json:"admin"`        // the node that accepts the join
	YggPeers    []string   `json:"ygg_peers"`    // Yggdrasil peers to connect through
	Token       string     `json:"token"`
	Expires     int64      `json:"expires"`
}

// Encode makes the invite string.
func (inv Invite) Encode() string {
	b, _ := json.Marshal(inv)
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

// Decode reads an invite string, ignoring spaces and line breaks that email
// may add.
func Decode(s string) (Invite, error) {
	s = strings.Join(strings.Fields(s), "")
	var inv Invite
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, prefix))
	if !strings.HasPrefix(s, prefix) || err != nil || json.Unmarshal(raw, &inv) != nil {
		return inv, errors.New(`not an invite (they start with "yggjoin1:")`)
	}
	if !peers.IsOverlay(inv.Admin.Addr) || inv.Token == "" {
		return inv, errors.New("the invite is incomplete")
	}
	if time.Now().Unix() > inv.Expires {
		return inv, errors.New("the invite has expired; ask for a new one")
	}
	return inv, nil
}

// pending is an invite as the admin keeps it: only a hash of the secret.
type pending struct {
	TokenHash string `json:"token_hash"`
	For       string `json:"for,omitempty"` // who it was made for, as a reminder
	Created   int64  `json:"created"`
	Expires   int64  `json:"expires"`
	UsedBy    string `json:"used_by,omitempty"`
	UsedAt    int64  `json:"used_at,omitempty"`
}

func hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

var mu sync.Mutex // the dashboard and the node may share the invites file

func load(path string) []pending {
	var out []pending
	if data, err := os.ReadFile(path); err == nil {
		json.Unmarshal(data, &out)
	}
	return out
}

func save(path string, list []pending) error {
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Create records a new invite in the invites file and returns it; fill in
// the rest of it (group, admin, peers) before encoding.
func Create(path, forWhom string) (Invite, error) {
	secret := make([]byte, 18)
	if _, err := rand.Read(secret); err != nil {
		return Invite{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(secret)
	now := time.Now()
	mu.Lock()
	defer mu.Unlock()
	list := load(path)
	// Forget invites that expired more than a month ago.
	keep := list[:0]
	for _, p := range list {
		if now.Unix() < p.Expires+30*86400 {
			keep = append(keep, p)
		}
	}
	keep = append(keep, pending{TokenHash: hash(token), For: strings.TrimSpace(forWhom), Created: now.Unix(), Expires: now.Add(TTL).Unix()})
	if err := save(path, keep); err != nil {
		return Invite{}, err
	}
	return Invite{Token: token, Expires: now.Add(TTL).Unix()}, nil
}

// Pending lists invites not used yet and not expired, for the dashboard.
type Pending struct {
	For     string `json:"for"`
	Created int64  `json:"created"`
	Expires int64  `json:"expires"`
	UsedBy  string `json:"used_by,omitempty"`
}

func List(path string) []Pending {
	mu.Lock()
	defer mu.Unlock()
	var out []Pending
	now := time.Now().Unix()
	for _, p := range load(path) {
		if p.UsedBy != "" || now < p.Expires {
			out = append(out, Pending{For: p.For, Created: p.Created, Expires: p.Expires, UsedBy: p.UsedBy})
		}
	}
	return out
}

// ErrBadToken covers unknown, used and expired invites alike, so a guesser
// learns nothing.
var ErrBadToken = errors.New("this invite is not valid (used already, expired, or not from this group)")

func redeem(path, token, by string) error {
	mu.Lock()
	defer mu.Unlock()
	list := load(path)
	h := hash(token)
	now := time.Now().Unix()
	for i, p := range list {
		if subtle.ConstantTimeCompare([]byte(p.TokenHash), []byte(h)) == 1 {
			if p.UsedBy != "" || now > p.Expires {
				return ErrBadToken
			}
			list[i].UsedBy, list[i].UsedAt = by, now
			return save(path, list)
		}
	}
	return ErrBadToken
}

// Request is what a joining machine sends the admin node.
type Request struct {
	Token       string `json:"token"`
	Node        string `json:"node"`   // name for the new node, e.g. "anna-laptop"
	Person      string `json:"person"` // who runs it, e.g. "Anna"
	Addr        string `json:"addr"`   // the new node's [overlay IP]:port
	SharingCode string `json:"sharing_code"`
}

// Response is the member list, so the new node can start with it.
type Response struct {
	Peers []peers.Peer `json:"peers"`
	Node  string       `json:"node"` // the name the node was given
}

// Acceptor handles join requests on an admin node.
type Acceptor struct {
	InvitesPath  string
	PeersPath    string // the admin's member list, which the dashboard syncs
	ContactsPath string // the newcomer is added here
	OwnCode      string
	Refresh      func() // reload the node's own allow-list after a change
}

// Accept checks a join request from caller (the overlay IP it came from)
// and adds the new node to the member list.
func (a Acceptor) Accept(caller string, req Request) (Response, error) {
	if _, err := os.Stat(a.InvitesPath); err != nil {
		return Response{}, ErrBadToken // this node gives no invites
	}
	host, port, ok := strings.Cut(strings.TrimPrefix(req.Addr, "["), "]:")
	if !ok || host != caller || port == "" {
		return Response{}, errors.New("the address must be the joining machine's own")
	}
	req.Node, req.Person = strings.TrimSpace(req.Node), strings.TrimSpace(req.Person)
	if req.Node == "" || strings.ContainsAny(req.Node, " /\\\"<>") || len(req.Node) > 40 || len(req.Person) > 60 {
		return Response{}, errors.New("give the node a short name without spaces")
	}
	list, err := peers.Load(a.PeersPath)
	if err != nil {
		return Response{}, err
	}
	p := peers.Peer{Name: req.Node, Addr: fmt.Sprintf("[%s]:%s", host, port), Owner: req.Person}
	for _, o := range list {
		if o.Addr == p.Addr {
			return Response{}, errors.New("this node is already a member")
		}
		if o.IP() == host {
			p.Host = o.Machine() // another node on the same machine: they fail together
		}
		if o.Name == p.Name {
			p.Name = fmt.Sprintf("%s-%d", req.Node, len(list)+1)
		}
	}
	if err := redeem(a.InvitesPath, req.Token, p.Name); err != nil {
		return Response{}, err
	}
	list = append(list, p)
	if err := peers.Validate(list); err != nil {
		return Response{}, err
	}
	if err := peers.Write(a.PeersPath, list); err != nil {
		return Response{}, err
	}
	if a.Refresh != nil {
		a.Refresh()
	}
	if a.ContactsPath != "" && req.SharingCode != "" && req.Person != "" {
		contacts.Add(a.ContactsPath, contacts.Contact{Name: req.Person, Code: req.SharingCode}, a.OwnCode)
	}
	return Response{Peers: list, Node: p.Name}, nil
}
