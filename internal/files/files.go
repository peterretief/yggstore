// Package files is the put/get/verify pipeline: encrypt per chunk, erasure
// code, place shards on peers, and write a stub holding the manifest.
package files

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/peterretief/yggstore/internal/challenge"
	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/cryptofile"
	"github.com/peterretief/yggstore/internal/erasure"
	"github.com/peterretief/yggstore/internal/manifest"
	"github.com/peterretief/yggstore/internal/peers"
)

const (
	StubExt       = ".ystub"
	ChallengesExt = ".challenges.json"
)

// Layout is 4 data + 2 parity, as in the credit spec (1.5x raw per logical GB).
func Layout() erasure.Layout { return erasure.Layout{DataShards: 4, ParityShards: 2} }

type PutOptions struct {
	ChunkSize  int // plaintext bytes per chunk
	Challenges int // precomputed challenges per shard
	Log        func(format string, args ...any)
}

// Challenges is the owner's private list of precomputed challenges, keyed by shard hash.
type Challenges struct {
	FileID string                          `json:"file_id"`
	Shards map[string][]challenge.Prepared `json:"shards"`
}

// Put stores path on the online peers and returns its manifest and challenges.
func Put(ctx context.Context, c client.Client, path string, online []peers.Peer, opts PutOptions) (manifest.Manifest, Challenges, error) {
	f, err := os.Open(path)
	if err != nil {
		return manifest.Manifest{}, Challenges{}, err
	}
	defer f.Close()
	m, chal, _, err := PutReader(ctx, c, f, filepath.Base(path), online, opts)
	return m, chal, err
}

// PutReader stores everything read from r under name. It also returns the
// SHA-256 of the plaintext so callers can check a read-back.
func PutReader(ctx context.Context, c client.Client, r io.Reader, name string, online []peers.Peer, opts PutOptions) (manifest.Manifest, Challenges, string, error) {
	h := sha256.New()
	m, chal, err := putReader(ctx, c, io.TeeReader(r, h), name, online, opts)
	return m, chal, hex.EncodeToString(h.Sum(nil)), err
}

func putReader(ctx context.Context, c client.Client, r io.Reader, name string, online []peers.Peer, opts PutOptions) (manifest.Manifest, Challenges, error) {
	layout := Layout()
	if len(online) == 0 {
		return manifest.Manifest{}, Challenges{}, errors.New("no peers online")
	}
	if opts.ChunkSize <= 0 {
		opts.ChunkSize = 4 << 20
	}
	logf := opts.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if len(online) < layout.TotalShards() {
		logf("warning: only %d peers online; some peers will hold more than one shard of a chunk", len(online))
	}

	key := make([]byte, cryptofile.KeySize)
	if _, err := rand.Read(key); err != nil {
		return manifest.Manifest{}, Challenges{}, err
	}
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return manifest.Manifest{}, Challenges{}, err
	}
	fileID := hex.EncodeToString(idBytes)
	chal := Challenges{FileID: fileID, Shards: map[string][]challenge.Prepared{}}

	// Chunks are read, encrypted and encoded in order, then uploaded with up to
	// inflightChunks in flight, so slow links overlap instead of adding up.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		firstErr error
		chunks   = map[int]manifest.Chunk{}
	)
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
		mu.Unlock()
	}
	failed := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return firstErr != nil
	}
	sem := make(chan struct{}, inflightChunks)
	lim := newPeerLimit()
	total, nChunks := 0, 0
	for idx := 0; ; idx++ {
		if failed() {
			break
		}
		if err := ctx.Err(); err != nil {
			fail(err)
			break
		}
		buf := make([]byte, opts.ChunkSize)
		n, rerr := io.ReadFull(r, buf)
		if rerr == io.EOF && idx > 0 {
			break
		}
		if rerr != nil && rerr != io.EOF && rerr != io.ErrUnexpectedEOF {
			fail(rerr)
			break
		}
		cipherText, nonce, err := cryptofile.EncryptChunk(buf[:n], key)
		if err != nil {
			fail(err)
			break
		}
		shards, err := layout.Encode(cipherText)
		if err != nil {
			fail(err)
			break
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			fail(ctx.Err())
			break
		}
		total += n
		nChunks++
		ctLen := len(cipherText)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			refs, err := place(ctx, c, lim, shards, online, idx, layout.ParityShards)
			if err != nil {
				fail(fmt.Errorf("chunk %d: %w", idx, err))
				return
			}
			prepared := map[string][]challenge.Prepared{}
			if opts.Challenges > 0 {
				for i, s := range shards {
					p, err := challenge.Prepare(refs[i].Hash, s, opts.Challenges)
					if err != nil {
						fail(err)
						return
					}
					prepared[refs[i].Hash] = p
				}
			}
			mu.Lock()
			for h, p := range prepared {
				chal.Shards[h] = p
			}
			chunks[idx] = manifest.Chunk{PlaintextSize: n, CiphertextSize: ctLen, Nonce: nonce, Shards: refs}
			mu.Unlock()
			progress(ctx, n)
			logf("chunk %d: %d bytes -> %d shards", idx, n, len(shards))
		}()
		if rerr != nil { // short read: last chunk
			break
		}
	}
	wg.Wait()
	if firstErr != nil {
		return manifest.Manifest{}, Challenges{}, firstErr
	}
	ordered := make([]manifest.Chunk, nChunks)
	for i := range ordered {
		ordered[i] = chunks[i]
	}
	m, err := manifest.NewChunked(layout, fileID, name, total, opts.ChunkSize, key, ordered)
	return m, chal, err
}

// inflightChunks is how many chunks upload at once (6 shards each). 6 was no
// faster than 3 against the Pis (2026-10-05), and 3 is gentler on them.
const inflightChunks = 3

// targets picks a peer (an index into online) for each of n shards. Peers
// are filled in rounds, one shard each per round, starting at start so load
// rotates between chunks. No machine (see peers.Peer.Host) gets more than max
// shards, so losing one machine never costs more than the parity covers.
// Slow peers get one shard while the others have room for the rest, and up
// to max only when they don't. Only with too few machines for that is max
// exceeded.
func targets(online []peers.Peer, n, start, max int) []int {
	out := make([]int, 0, n)
	count := make([]int, len(online))
	onMachine := map[string]int{}
	for _, slowMax := range []int{1, max} {
		for round := 0; round < max && len(out) < n; round++ {
			for k := range online {
				i := (start + k) % len(online)
				limit := max
				if online[i].Slow {
					limit = slowMax
				}
				m := online[i].Machine()
				if len(out) < n && count[i] == round && round < limit && onMachine[m] < max {
					out = append(out, i)
					count[i]++
					onMachine[m]++
				}
			}
		}
	}
	for k := 0; len(out) < n; k++ {
		out = append(out, (start+k)%len(online))
	}
	return out
}

// place uploads a chunk's shards in parallel to the peers targets picks.
// Shards whose peer fails then fall back, one at a time, to the next peer,
// avoiding peers this chunk already uses while unused ones remain.
func place(ctx context.Context, c client.Client, lim *peerLimit, shards [][]byte, online []peers.Peer, start, max int) ([]manifest.ShardRef, error) {
	put := func(p peers.Peer, s []byte) (string, error) {
		if err := lim.acquire(ctx, p.Addr); err != nil {
			return "", err
		}
		defer lim.release(p.Addr)
		return c.Put(ctx, p.Addr, s)
	}
	refs := make([]manifest.ShardRef, len(shards))
	errs := make([]error, len(shards))
	to := targets(online, len(shards), start, max)
	var wg sync.WaitGroup
	for i, s := range shards {
		p := online[to[i]]
		wg.Add(1)
		go func() {
			defer wg.Done()
			hash, err := put(p, s)
			if err != nil {
				errs[i] = fmt.Errorf("%s: %w", p.Name, err)
				return
			}
			refs[i] = manifest.ShardRef{Hash: hash, Peer: p.Addr, URL: client.ShardURL(p.Addr, hash)}
		}()
	}
	wg.Wait()

	used := map[string]bool{}
	onMachine := map[string]int{}
	for i := range refs {
		if errs[i] == nil {
			used[refs[i].Peer] = true
			onMachine[online[to[i]].Machine()]++
		}
	}
	for i, s := range shards {
		if errs[i] == nil {
			continue
		}
		lastErr := errs[i]
		placed := false
		// First try peers this chunk doesn't use yet, on machines below max;
		// then anything that answers.
		for pass := 0; pass < 2 && !placed; pass++ {
			for k := 1; k <= len(online); k++ {
				p := online[(start+i+k)%len(online)]
				if pass == 0 && (used[p.Addr] || onMachine[p.Machine()] >= max) {
					continue
				}
				hash, err := put(p, s)
				if err != nil {
					lastErr = fmt.Errorf("%s: %w", p.Name, err)
					continue
				}
				refs[i] = manifest.ShardRef{Hash: hash, Peer: p.Addr, URL: client.ShardURL(p.Addr, hash)}
				used[p.Addr] = true
				onMachine[p.Machine()]++
				placed = true
				break
			}
		}
		if !placed {
			return nil, fmt.Errorf("shard %d could not be placed: %w", i, lastErr)
		}
	}
	return refs, nil
}

// Get rebuilds the file from its manifest, writing plaintext to w. Up to
// getAhead chunks are fetched at once, and each is rebuilt from the first
// DataShards of its shards to arrive, so slow nodes are skipped when the
// others have enough.
func Get(ctx context.Context, c client.Client, m manifest.Manifest, w io.Writer, logf func(string, ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	layout, err := erasure.NewLayout(m.DataShards, m.ParityShards)
	if err != nil {
		return err
	}
	if m.Version != 2 {
		return fmt.Errorf("only chunked (version 2) manifests are supported")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	lim := newPeerLimit()
	type result struct {
		plain []byte
		have  int
		err   error
	}
	results := make([]chan result, len(m.Chunks))
	for i := range results {
		results[i] = make(chan result, 1)
	}
	sem := make(chan struct{}, getAhead)
	go func() {
		for idx, ch := range m.Chunks {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			go func() {
				plain, have, err := getChunk(ctx, c, lim, layout, m, idx, ch, logf)
				results[idx] <- result{plain, have, err}
			}()
		}
	}()
	for idx := range m.Chunks {
		var r result
		select {
		case r = <-results[idx]:
		case <-ctx.Done():
			return ctx.Err()
		}
		<-sem
		if r.err != nil {
			return r.err
		}
		if _, err := w.Write(r.plain); err != nil {
			return err
		}
		progress(ctx, len(r.plain))
		logf("chunk %d: rebuilt from the first %d of %d shards to arrive", idx, r.have, len(m.Chunks[idx].Shards))
	}
	return nil
}

// getAhead is how many chunks Get fetches at once.
const getAhead = 4

func getChunk(ctx context.Context, c client.Client, lim *peerLimit, layout erasure.Layout, m manifest.Manifest, idx int, ch manifest.Chunk, logf func(string, ...any)) ([]byte, int, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel() // stops the shards still on their way once enough are in
	type got struct {
		i    int
		data []byte
		err  error
	}
	arrived := make(chan got, len(ch.Shards))
	for i, ref := range ch.Shards {
		go func() {
			if err := lim.acquire(ctx, ref.Peer); err != nil {
				arrived <- got{i, nil, err}
				return
			}
			defer lim.release(ref.Peer)
			data, err := c.Get(ctx, ref.Peer, ref.Hash)
			arrived <- got{i, data, err}
		}()
	}
	shards := make([][]byte, len(ch.Shards))
	have := 0
	for range ch.Shards {
		g := <-arrived
		if g.err != nil {
			logf("chunk %d shard %d unavailable: %v", idx, g.i, g.err)
			continue
		}
		shards[g.i] = g.data
		if have++; have == layout.DataShards {
			break
		}
	}
	if have < layout.DataShards {
		return nil, have, fmt.Errorf("chunk %d: only %d of %d shards reachable, need %d", idx, have, len(ch.Shards), layout.DataShards)
	}
	cipherText, err := layout.Decode(shards, ch.CiphertextSize)
	if err != nil {
		return nil, have, fmt.Errorf("chunk %d: %w", idx, err)
	}
	plain, err := cryptofile.Decrypt(cipherText, m.Key, ch.Nonce)
	if err != nil {
		return nil, have, fmt.Errorf("chunk %d: %w", idx, err)
	}
	return plain, have, nil
}

type ShardResult struct {
	Chunk, Index int
	Peer, Hash   string
	Err          error // nil = passed
}

// Verify spends one unused challenge per shard and reports pass/fail.
func Verify(ctx context.Context, c client.Client, m manifest.Manifest, chal *Challenges) []ShardResult {
	var out []ShardResult
	var mu sync.Mutex
	var wg sync.WaitGroup
	for ci, ch := range m.Chunks {
		for si, ref := range ch.Shards {
			list := chal.Shards[ref.Hash]
			pick := -1
			for k := range list {
				if !list[k].Used {
					pick = k
					break
				}
			}
			res := ShardResult{Chunk: ci, Index: si, Peer: ref.Peer, Hash: ref.Hash}
			if pick < 0 {
				res.Err = errors.New("no unused challenges left")
				out = append(out, res)
				continue
			}
			list[pick].Used = true
			p := list[pick]
			wg.Add(1)
			go func() {
				defer wg.Done()
				proof, err := c.Challenge(ctx, ref.Peer, p.Request)
				if err == nil && proof != p.Expect {
					err = errors.New("wrong proof")
				}
				res.Err = err
				mu.Lock()
				out = append(out, res)
				mu.Unlock()
			}()
		}
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Chunk != out[j].Chunk {
			return out[i].Chunk < out[j].Chunk
		}
		return out[i].Index < out[j].Index
	})
	return out
}

func WriteJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func ReadStub(path string) (manifest.Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return manifest.Manifest{}, err
	}
	return manifest.Unmarshal(data)
}

func ReadChallenges(path string) (Challenges, error) {
	var c Challenges
	data, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(data, &c)
}

// ChallengesPath is where a stub's private challenges live: a hidden file next
// to it. A visible legacy "<stub>.challenges.json" is used if it exists.
func ChallengesPath(stub string) string {
	legacy := stub + ChallengesExt
	if _, err := os.Stat(legacy); err == nil {
		return legacy
	}
	return filepath.Join(filepath.Dir(stub), "."+filepath.Base(stub)+ChallengesExt)
}
