package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/peterretief/yggstore/internal/files"
	"github.com/peterretief/yggstore/internal/manifest"
)

// Object is a stored object; its manifest (with the key) is in its file.
type Object struct {
	Key      string            `json:"key"`
	Size     int64             `json:"size"`
	ETag     string            `json:"etag"` // without quotes
	Modified time.Time         `json:"modified"`
	Headers  map[string]string `json:"headers,omitempty"` // Content-Type and the like
	Meta     map[string]string `json:"meta,omitempty"`    // x-amz-meta-*, lower-case names
	FileID   string            `json:"-"`
	Manifest manifest.Manifest `json:"manifest"`
}

type bucket struct {
	Created time.Time          `json:"created"`
	objects map[string]*Object // without manifests
	sorted  []string           // nil when it needs re-sorting
}

func (b *bucket) keys() []string {
	if b.sorted == nil {
		b.sorted = make([]string, 0, len(b.objects))
		for k := range b.objects {
			b.sorted = append(b.sorted, k)
		}
		sort.Strings(b.sorted)
	}
	return b.sorted
}

// Upload is a multipart upload in progress.
type Upload struct {
	ID       string            `json:"id"`
	Bucket   string            `json:"bucket"`
	Key      string            `json:"key"`
	Started  time.Time         `json:"started"`
	Headers  map[string]string `json:"headers,omitempty"`
	Meta     map[string]string `json:"meta,omitempty"`
	CryptKey []byte            `json:"crypt_key"` // every part uses it, so parts can be joined
	parts    map[int]*Part
}

type Part struct {
	Number   int               `json:"number"`
	Size     int64             `json:"size"`
	ETag     string            `json:"etag"`
	Modified time.Time         `json:"modified"`
	Manifest manifest.Manifest `json:"manifest"`
}

type account struct {
	buckets map[string]*bucket
	uploads map[string]*Upload
	used    int64 // object and part bytes
	pending int64 // bytes of uploads under way
}

// fileRef counts the objects using one stored item (copies share it), and
// what each peer holds of it.
type fileRef struct {
	refs int
	held map[string]int64
}

// store is the gateway's index: customers' buckets and objects on disk,
// and in memory without the manifests.
type store struct {
	dir string
	mu  sync.Mutex
	acc map[string]*account
	fid map[string]*fileRef
}

func (s *store) objPath(cust, bucket, key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(s.dir, "objects", cust, bucket, hex.EncodeToString(sum[:])+".json")
}

func (s *store) bucketPath(cust, bucket string) string {
	return filepath.Join(s.dir, "objects", cust, bucket, ".bucket.json")
}

func (s *store) uploadDir(cust, id string) string {
	return filepath.Join(s.dir, "uploads", cust, id)
}

func (s *store) trashDir() string { return filepath.Join(s.dir, "trash") }

func openStore(dir string) (*store, error) {
	s := &store{dir: dir, acc: map[string]*account{}, fid: map[string]*fileRef{}}
	custDirs, _ := os.ReadDir(filepath.Join(dir, "objects"))
	for _, cd := range custDirs {
		if !cd.IsDir() {
			continue
		}
		a := s.account(cd.Name())
		bucketDirs, _ := os.ReadDir(filepath.Join(dir, "objects", cd.Name()))
		for _, bd := range bucketDirs {
			var b bucket
			if err := readJSON(s.bucketPath(cd.Name(), bd.Name()), &b); err != nil {
				continue
			}
			b.objects = map[string]*Object{}
			a.buckets[bd.Name()] = &b
			entries, _ := os.ReadDir(filepath.Join(dir, "objects", cd.Name(), bd.Name()))
			for _, e := range entries {
				if !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), ".") {
					continue
				}
				var o Object
				if err := readJSON(filepath.Join(dir, "objects", cd.Name(), bd.Name(), e.Name()), &o); err != nil {
					return nil, err
				}
				s.addRef(o.Manifest)
				o.FileID, o.Manifest = o.Manifest.FileID, manifest.Manifest{}
				b.objects[o.Key] = &o
				a.used += o.Size
			}
		}
	}
	upDirs, _ := os.ReadDir(filepath.Join(dir, "uploads"))
	for _, cd := range upDirs {
		a := s.account(cd.Name())
		ids, _ := os.ReadDir(filepath.Join(dir, "uploads", cd.Name()))
		for _, id := range ids {
			var u Upload
			if err := readJSON(filepath.Join(s.uploadDir(cd.Name(), id.Name()), "upload.json"), &u); err != nil {
				continue
			}
			u.parts = map[int]*Part{}
			parts, _ := filepath.Glob(filepath.Join(s.uploadDir(cd.Name(), id.Name()), "part-*.json"))
			for _, pp := range parts {
				var p Part
				if err := readJSON(pp, &p); err != nil {
					return nil, err
				}
				s.addRef(p.Manifest)
				p.Manifest = manifest.Manifest{FileID: p.Manifest.FileID}
				u.parts[p.Number] = &p
				a.used += p.Size
			}
			a.uploads[u.ID] = &u
		}
	}
	return s, nil
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func (s *store) account(cust string) *account {
	a := s.acc[cust]
	if a == nil {
		a = &account{buckets: map[string]*bucket{}, uploads: map[string]*Upload{}}
		s.acc[cust] = a
	}
	return a
}

func (s *store) addRef(m manifest.Manifest) {
	f := s.fid[m.FileID]
	if f == nil {
		f = &fileRef{held: files.ShardBytes(m)}
		s.fid[m.FileID] = f
	}
	f.refs++
}

// dropRef forgets one use of an item and reports whether it was the last,
// so its shards should be deleted.
func (s *store) dropRef(fileID string) bool {
	f := s.fid[fileID]
	if f == nil {
		return false
	}
	if f.refs--; f.refs > 0 {
		return false
	}
	delete(s.fid, fileID)
	return true
}

// held is what each peer holds for customers, counting shared items once.
func (s *store) held() map[string]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]int64{}
	for _, f := range s.fid {
		for p, n := range f.held {
			out[p] += n
		}
	}
	return out
}

// usage is the bytes each customer stores.
func (s *store) usage() map[string]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]int64{}
	for c, a := range s.acc {
		out[c] = a.used
	}
	return out
}

// reserve makes room for n more bytes within quota (0 = unlimited), until
// release is called.
func (s *store) reserve(cust string, n, quota int64) (release func(), err *s3Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.account(cust)
	if quota > 0 && a.used+a.pending+n > quota {
		return nil, errf(errQuotaExceeded, fmt.Sprintf("this would take you over your plan's %s (%s in use)", gb(quota), gb(a.used+a.pending)))
	}
	a.pending += n
	return func() {
		s.mu.Lock()
		a.pending -= n
		s.mu.Unlock()
	}, nil
}

func gb(n int64) string { return fmt.Sprintf("%.2f GB", float64(n)/1e9) }

var errNotEmpty = errors.New("bucket not empty")

func (s *store) createBucket(cust, name string, maxBuckets int) *s3Error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.account(cust)
	if a.buckets[name] != nil {
		return errf(errBucketExists, "you already have this bucket")
	}
	if len(a.buckets) >= maxBuckets {
		return errf(errTooManyBuckets, fmt.Sprintf("an account can have %d buckets", maxBuckets))
	}
	b := &bucket{Created: time.Now().UTC().Truncate(time.Second), objects: map[string]*Object{}}
	if err := writeJSON(s.bucketPath(cust, name), b); err != nil {
		return errf(errInternal, err.Error())
	}
	a.buckets[name] = b
	return nil
}

func (s *store) deleteBucket(cust, name string) *s3Error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.account(cust)
	b := a.buckets[name]
	if b == nil {
		return errf(errNoSuchBucket, "no such bucket")
	}
	if len(b.objects) > 0 {
		return errf(errBucketNotEmpty, "delete the objects in the bucket first")
	}
	for _, u := range a.uploads {
		if u.Bucket == name {
			return errf(errBucketNotEmpty, "the bucket has unfinished multipart uploads; abort them first")
		}
	}
	if err := os.RemoveAll(filepath.Join(s.dir, "objects", cust, name)); err != nil {
		return errf(errInternal, err.Error())
	}
	delete(a.buckets, name)
	return nil
}

type bucketInfo struct {
	Name    string
	Created time.Time
}

func (s *store) buckets(cust string) []bucketInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []bucketInfo
	for n, b := range s.account(cust).buckets {
		out = append(out, bucketInfo{n, b.Created})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *store) hasBucket(cust, name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.account(cust).buckets[name] != nil
}

// head returns an object's details, without its manifest.
func (s *store) head(cust, bucketName, key string) (Object, *s3Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.account(cust).buckets[bucketName]
	if b == nil {
		return Object{}, errf(errNoSuchBucket, "no such bucket")
	}
	o := b.objects[key]
	if o == nil {
		return Object{}, errf(errNoSuchKey, "no such key")
	}
	return *o, nil
}

// get returns an object with its manifest.
func (s *store) get(cust, bucketName, key string) (Object, *s3Error) {
	if _, e := s.head(cust, bucketName, key); e != nil {
		return Object{}, e
	}
	var o Object
	if err := readJSON(s.objPath(cust, bucketName, key), &o); err != nil {
		if errors.Is(err, os.ErrNotExist) { // deleted meanwhile
			return Object{}, errf(errNoSuchKey, "no such key")
		}
		return Object{}, errf(errInternal, err.Error())
	}
	o.FileID = o.Manifest.FileID
	return o, nil
}

// put records o (with its manifest, already stored in the group) and
// returns the manifest of an item no longer used, to delete, if any.
func (s *store) put(cust, bucketName string, o Object, newRef bool) (*manifest.Manifest, *s3Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.account(cust)
	b := a.buckets[bucketName]
	if b == nil {
		return nil, errf(errNoSuchBucket, "the bucket was deleted during the upload")
	}
	if !newRef && s.fid[o.Manifest.FileID] == nil {
		return nil, errf(errNoSuchKey, "the source object was deleted during the copy")
	}
	path := s.objPath(cust, bucketName, o.Key)
	var old Object
	oldErr := readJSON(path, &old)
	if err := writeJSON(path, o); err != nil {
		return nil, errf(errInternal, err.Error())
	}
	if newRef {
		s.addRef(o.Manifest)
	} else {
		s.fid[o.Manifest.FileID].refs++
	}
	mem := o
	mem.FileID, mem.Manifest = o.Manifest.FileID, manifest.Manifest{}
	if prev := b.objects[o.Key]; prev != nil {
		a.used -= prev.Size
	} else {
		b.sorted = nil
	}
	b.objects[o.Key] = &mem
	a.used += o.Size
	if oldErr == nil && s.dropRef(old.Manifest.FileID) {
		return &old.Manifest, nil
	}
	return nil, nil
}

// remove deletes an object's record and returns its manifest if no other
// object uses the stored item.
func (s *store) remove(cust, bucketName, key string) (*manifest.Manifest, *s3Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.account(cust)
	b := a.buckets[bucketName]
	if b == nil {
		return nil, errf(errNoSuchBucket, "no such bucket")
	}
	o := b.objects[key]
	if o == nil {
		return nil, nil // S3 deletes of missing keys succeed
	}
	path := s.objPath(cust, bucketName, key)
	var full Object
	if err := readJSON(path, &full); err != nil {
		return nil, errf(errInternal, err.Error())
	}
	if err := os.Remove(path); err != nil {
		return nil, errf(errInternal, err.Error())
	}
	delete(b.objects, key)
	b.sorted = nil
	a.used -= o.Size
	if s.dropRef(o.FileID) {
		return &full.Manifest, nil
	}
	return nil, nil
}

type listing struct {
	objects   []Object
	prefixes  []string
	truncated bool
	next      string // the last key or prefix returned
}

// list lists a bucket the S3 way: keys after marker that start with prefix,
// with keys sharing a part up to delimiter rolled into one common prefix.
func (s *store) list(cust, bucketName, prefix, delimiter, marker string, max int) (listing, *s3Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.account(cust).buckets[bucketName]
	if b == nil {
		return listing{}, errf(errNoSuchBucket, "no such bucket")
	}
	keys := b.keys()
	i := sort.SearchStrings(keys, prefix)
	if marker > prefix {
		i = sort.Search(len(keys), func(j int) bool { return keys[j] > marker })
	}
	var out listing
	lastPrefix := ""
	for ; i < len(keys); i++ {
		k := keys[i]
		if !strings.HasPrefix(k, prefix) {
			break
		}
		if delimiter != "" && strings.HasSuffix(marker, delimiter) && strings.HasPrefix(k, marker) {
			continue // inside a common prefix already returned
		}
		cp := ""
		if delimiter != "" {
			if j := strings.Index(k[len(prefix):], delimiter); j >= 0 {
				cp = k[:len(prefix)+j+len(delimiter)]
			}
		}
		if cp != "" && cp == lastPrefix {
			continue
		}
		if len(out.objects)+len(out.prefixes) == max {
			out.truncated = true
			break
		}
		if cp != "" {
			out.prefixes = append(out.prefixes, cp)
			lastPrefix, out.next = cp, cp
		} else {
			out.objects = append(out.objects, *b.objects[k])
			out.next = k
		}
	}
	return out, nil
}

func (s *store) newUpload(cust string, u *Upload) *s3Error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.account(cust)
	if a.buckets[u.Bucket] == nil {
		return errf(errNoSuchBucket, "no such bucket")
	}
	if err := writeJSON(filepath.Join(s.uploadDir(cust, u.ID), "upload.json"), u); err != nil {
		return errf(errInternal, err.Error())
	}
	u.parts = map[int]*Part{}
	a.uploads[u.ID] = u
	return nil
}

func (s *store) upload(cust, id, bucketName, key string) (*Upload, *s3Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.account(cust).uploads[id]
	if u == nil || u.Bucket != bucketName || u.Key != key {
		return nil, errf(errNoSuchUpload, "no such upload; it may have been completed or aborted")
	}
	return u, nil
}

// putPart records a stored part, returning a replaced part's manifest.
func (s *store) putPart(cust string, u *Upload, p Part) (*manifest.Manifest, *s3Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.account(cust)
	if a.uploads[u.ID] != u {
		return &p.Manifest, errf(errNoSuchUpload, "the upload was completed or aborted meanwhile")
	}
	path := filepath.Join(s.uploadDir(cust, u.ID), fmt.Sprintf("part-%05d.json", p.Number))
	var old Part
	oldErr := readJSON(path, &old)
	if err := writeJSON(path, p); err != nil {
		return &p.Manifest, errf(errInternal, err.Error())
	}
	s.addRef(p.Manifest)
	if prev := u.parts[p.Number]; prev != nil {
		a.used -= prev.Size
	}
	mem := p
	mem.Manifest = manifest.Manifest{FileID: p.Manifest.FileID}
	u.parts[p.Number] = &mem
	a.used += p.Size
	if oldErr == nil && s.dropRef(old.Manifest.FileID) {
		return &old.Manifest, nil
	}
	return nil, nil
}

func (s *store) parts(u *Upload) []Part {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Part
	for _, p := range u.parts {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out
}

func (s *store) partManifest(cust string, u *Upload, n int) (manifest.Manifest, error) {
	var p Part
	err := readJSON(filepath.Join(s.uploadDir(cust, u.ID), fmt.Sprintf("part-%05d.json", n)), &p)
	return p.Manifest, err
}

// endUpload forgets an upload. Its parts' items are returned for deletion,
// except those in keep (joined into the finished object).
func (s *store) endUpload(cust string, u *Upload, keep map[int]bool) []manifest.Manifest {
	s.mu.Lock()
	a := s.account(cust)
	if a.uploads[u.ID] != u {
		s.mu.Unlock()
		return nil
	}
	delete(a.uploads, u.ID)
	var drop []int
	for n, p := range u.parts {
		a.used -= p.Size
		if s.dropRef(p.Manifest.FileID) && !keep[n] {
			drop = append(drop, n)
		}
	}
	s.mu.Unlock()
	var out []manifest.Manifest
	for _, n := range drop {
		if m, err := s.partManifest(cust, u, n); err == nil {
			out = append(out, m)
		}
	}
	os.RemoveAll(s.uploadDir(cust, u.ID))
	return out
}

func (s *store) uploads(cust, bucketName string) []*Upload {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Upload
	for _, u := range s.account(cust).uploads {
		if u.Bucket == bucketName {
			out = append(out, u)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key != out[j].Key {
			return out[i].Key < out[j].Key
		}
		return out[i].Started.Before(out[j].Started)
	})
	return out
}

// staleUploads lists uploads started before cutoff, for every customer.
func (s *store) staleUploads(cutoff time.Time) map[string][]*Upload {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string][]*Upload{}
	for c, a := range s.acc {
		for _, u := range a.uploads {
			if u.Started.Before(cutoff) {
				out[c] = append(out[c], u)
			}
		}
	}
	return out
}
