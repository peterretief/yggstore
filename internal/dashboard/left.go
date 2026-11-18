package dashboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/peterretief/yggstore/internal/atomicfile"
	"github.com/peterretief/yggstore/internal/peers"
)

// Nodes that left the group. Their shards can stay on other nodes for a
// while (until leases or repair clear them), so the usage table needs a
// name for an uploader that is no longer on the list.

// leftNode is what is kept about a node that left, by its Yggdrasil IP.
type leftNode struct {
	Name string `json:"name"`
	Left int64  `json:"left"` // unix seconds, when it was taken off the list
}

// leftPath keeps the nodes that left, next to the names file.
func (d *Dashboard) leftPath() string {
	if d.cfg.NamesPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(d.cfg.NamesPath), "left.json")
}

func (d *Dashboard) leftNodes() map[string]leftNode {
	out := map[string]leftNode{}
	if path := d.leftPath(); path != "" {
		if b, err := os.ReadFile(path); err == nil {
			json.Unmarshal(b, &out)
		}
	}
	return out
}

// recordLeft remembers nodes just taken off the list.
func (d *Dashboard) recordLeft(left []peers.Peer, at time.Time) error {
	path := d.leftPath()
	if path == "" {
		return nil
	}
	all := d.leftNodes()
	for _, p := range left {
		all[p.IP()] = leftNode{Name: p.Name, Left: at.Unix()}
	}
	b, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Replace(path, append(b, '\n'), 0o600)
}
