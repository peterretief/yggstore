package site

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// PullData copies a site's data (its entries) from the group's web nodes
// into a folder's local web node (yggstore site dev), so the site can be
// worked on with real data. What the folder's local web node had for the
// site is kept aside, not merged. The web nodes give a site's entries
// only to each other and to the machine that publishes it, so this runs
// there. It returns how many entries it got, and from how many web nodes.
func PullData(ctx context.Context, c *http.Client, webNodes []string, name, folder string) (entries, from int, err error) {
	dir := filepath.Join(folder, DevDir, "data", name)
	if _, err := os.Stat(dir); err == nil {
		aside := dir + ".before-pull-" + time.Now().UTC().Format("20060102T150405Z")
		if err := os.Rename(dir, aside); err != nil {
			return 0, 0, err
		}
	}
	d, err := openSiteData(dir)
	if err != nil {
		return 0, 0, err
	}
	defer d.close()
	var refused []string
	for _, addr := range webNodes {
		n, ok, err := pullAll(ctx, c, addr, name, d)
		switch {
		case err != nil:
			return entries, from, err
		case !ok:
			refused = append(refused, addr)
			continue
		}
		entries += n
		from++
	}
	if from == 0 {
		if len(refused) > 0 {
			return 0, 0, fmt.Errorf("no web node gave %s's data: run this on the machine that publishes it", name)
		}
		return 0, 0, fmt.Errorf("no web node answered")
	}
	return entries, from, nil
}

// pullAll takes every entry one web node has for a site. ok is false if
// it refused or didn't answer.
func pullAll(ctx context.Context, c *http.Client, addr, name string, d *siteData) (n int, ok bool, err error) {
	var got int64
	for range 1000 {
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		req, _ := http.NewRequestWithContext(rctx, http.MethodGet, "http://"+addr+dataPath+"entries?site="+name+"&got="+strconv.FormatInt(got, 10), nil)
		resp, rerr := c.Do(req)
		if rerr != nil {
			cancel()
			return n, n > 0, nil
		}
		var in struct {
			Entries []Entry `json:"entries"`
			Got     int64   `json:"got"`
		}
		derr := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&in)
		resp.Body.Close()
		cancel()
		if resp.StatusCode != http.StatusOK || derr != nil {
			return n, n > 0, nil
		}
		fresh, err := d.add(in.Entries, time.Now())
		if err != nil {
			return n, true, err
		}
		n += len(fresh)
		got = in.Got
		if len(in.Entries) < pullBatch {
			return n, true, nil
		}
	}
	return n, true, nil
}
