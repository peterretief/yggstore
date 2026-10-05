package gateway

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/peterretief/yggstore/internal/files"
	"github.com/peterretief/yggstore/internal/manifest"
	"github.com/peterretief/yggstore/internal/peers"
)

const (
	maxObjectSize = 5 << 30 // one PUT or one part, as in S3
	maxParts      = 10000
	maxKeyLen     = 1024
	maxXMLBody    = 1 << 20
	uploadTTL     = 7 * 24 * time.Hour // unfinished multipart uploads are aborted after this
)

type Options struct {
	// Domain, if set, also accepts virtual-hosted requests: BUCKET.Domain.
	Domain string
	// Region is what a bucket's location query answers; "" is us-east-1.
	Region string
	// Peers is the group's member list, for crediting members.
	Peers func() []peers.Peer
	// TrustProxy takes the client's address from X-Forwarded-For, for
	// rate-limiting failed sign-ins behind a reverse proxy.
	TrustProxy bool
	MaxBuckets int
	Log        func(format string, args ...any)
}

// Gateway is the S3 service.
type Gateway struct {
	Customers *Customers
	backend   Backend
	opts      Options
	store     *store
	meter     *meter
	fails     *failures
	bg        sync.WaitGroup
}

// New opens the gateway kept in dir.
func New(dir string, customers *Customers, backend Backend, opts Options) (*Gateway, error) {
	if opts.MaxBuckets <= 0 {
		opts.MaxBuckets = 100
	}
	if opts.Log == nil {
		opts.Log = func(string, ...any) {}
	}
	if opts.Peers == nil {
		opts.Peers = func() []peers.Peer { return nil }
	}
	s, err := openStore(dir)
	if err != nil {
		return nil, err
	}
	return &Gateway{Customers: customers, backend: backend, opts: opts, store: s,
		meter: newMeter(filepath.Join(dir, "usage")), fails: &failures{m: map[string]*failCount{}}}, nil
}

// Run does the background work: metering, retrying deletes that failed,
// and aborting abandoned multipart uploads. It returns when ctx ends.
func (g *Gateway) Run(ctx context.Context) {
	g.sample(time.Now())
	meter := time.NewTicker(10 * time.Minute)
	chores := time.NewTicker(time.Hour)
	defer meter.Stop()
	defer chores.Stop()
	g.chores(ctx)
	for {
		select {
		case <-ctx.Done():
			g.sample(time.Now())
			g.bg.Wait()
			return
		case now := <-meter.C:
			g.sample(now)
		case <-chores.C:
			g.chores(ctx)
		}
	}
}

func (g *Gateway) sample(now time.Time) {
	if err := g.meter.sample(now, g.store.usage(), g.store.held(), g.opts.Peers()); err != nil {
		g.opts.Log("metering: %v", err)
	}
}

func (g *Gateway) chores(ctx context.Context) {
	for cust, list := range g.store.staleUploads(time.Now().Add(-uploadTTL)) {
		for _, u := range list {
			g.opts.Log("aborting multipart upload %s (%s/%s), unfinished for a week", u.ID, u.Bucket, u.Key)
			for _, m := range g.store.endUpload(cust, u, nil) {
				g.discard(m)
			}
		}
	}
	entries, _ := os.ReadDir(g.store.trashDir())
	for _, e := range entries {
		path := filepath.Join(g.store.trashDir(), e.Name())
		var m manifest.Manifest
		if readJSON(path, &m) != nil {
			continue
		}
		dctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		err := g.backend.Delete(dctx, m)
		cancel()
		if err == nil {
			os.Remove(path)
		}
	}
}

// discard deletes an item no object uses any more. If that fails it is
// kept in the trash and retried.
func (g *Gateway) discard(m manifest.Manifest) {
	g.bg.Add(1)
	go func() {
		defer g.bg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := g.backend.Delete(ctx, m); err != nil {
			g.opts.Log("deleting item %s: %v; will retry", m.FileID, err)
			if err := writeJSON(filepath.Join(g.store.trashDir(), m.FileID+".json"), m); err != nil {
				g.opts.Log("could not record item %s for retry: %v", m.FileID, err)
			}
		}
	}()
}

// Wait waits for deletes started by requests, for tests and shutdown.
func (g *Gateway) Wait() { g.bg.Wait() }

type request struct {
	w      http.ResponseWriter
	r      *http.Request
	a      auth
	id     string
	bucket string
	key    string
	query  url.Values
}

func (q *request) fail(e *s3Error) { writeError(q.w, q.r, e, q.id) }

func (q *request) cust() string { return q.a.customer.ID }

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (g *Gateway) clientIP(r *http.Request) string {
	if g.opts.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := &request{w: w, r: r, id: randomHex(8), query: r.URL.Query()}
	w.Header().Set("x-amz-request-id", q.id)
	w.Header().Set("Server", "yggstore")
	ip := g.clientIP(r)
	if g.fails.blocked(ip) {
		q.fail(errf(errSlowDown, "too many failed sign-ins from your address; wait a few minutes"))
		return
	}
	a, e := g.authenticate(r, time.Now())
	if e != nil {
		if e.code == errSignature.code || e.code == errInvalidAccessKey.code {
			g.fails.add(ip)
		}
		q.fail(e)
		return
	}
	q.a = a
	g.meter.request(a.customer.ID)
	q.bucket, q.key = g.route(r)
	if q.key != "" && (len(q.key) > maxKeyLen || !utf8.ValidString(q.key)) {
		q.fail(errf(errKeyTooLong, "keys are at most 1024 bytes of UTF-8"))
		return
	}
	g.dispatch(q)
}

// route splits the request into bucket and key, path-style
// (/bucket/key) or virtual-hosted (bucket.domain/key).
func (g *Gateway) route(r *http.Request) (string, string) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	if g.opts.Domain != "" {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if b, ok := strings.CutSuffix(host, "."+g.opts.Domain); ok {
			return b, path
		}
	}
	b, k, _ := strings.Cut(path, "/")
	return b, k
}

var unsupportedSub = []string{"acl", "cors", "lifecycle", "policy", "tagging", "encryption", "logging",
	"notification", "replication", "website", "accelerate", "requestPayment", "object-lock", "ownershipControls",
	"publicAccessBlock", "analytics", "inventory", "metrics", "intelligent-tiering", "policyStatus",
	"retention", "legal-hold", "torrent", "attributes", "restore", "select"}

func (q *request) has(k string) bool { _, ok := q.query[k]; return ok }

func (g *Gateway) dispatch(q *request) {
	r := q.r
	for _, sub := range unsupportedSub {
		if q.has(sub) {
			q.fail(errf(errNotImplemented, "?"+sub+" is not supported by this service"))
			return
		}
	}
	switch {
	case q.bucket == "":
		if r.Method != http.MethodGet {
			q.fail(errf(errMethodNotAllowed, "method not allowed"))
			return
		}
		g.listBuckets(q)
	case q.key == "":
		switch r.Method {
		case http.MethodPut:
			if q.has("versioning") {
				q.fail(errf(errNotImplemented, "versioning is not supported"))
				return
			}
			g.createBucket(q)
		case http.MethodDelete:
			if e := g.store.deleteBucket(q.cust(), q.bucket); e != nil {
				q.fail(e)
				return
			}
			q.w.WriteHeader(http.StatusNoContent)
		case http.MethodHead:
			if !g.store.hasBucket(q.cust(), q.bucket) {
				q.fail(errf(errNoSuchBucket, "no such bucket"))
				return
			}
			q.w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			switch {
			case !g.store.hasBucket(q.cust(), q.bucket):
				q.fail(errf(errNoSuchBucket, "no such bucket"))
			case q.has("location"):
				region := g.opts.Region
				if region == "us-east-1" {
					region = ""
				}
				writeXML(q.w, http.StatusOK, struct {
					XMLName xml.Name `xml:"LocationConstraint"`
					NS      string   `xml:"xmlns,attr"`
					Region  string   `xml:",chardata"`
				}{NS: s3NS, Region: region})
			case q.has("versioning"):
				writeXML(q.w, http.StatusOK, struct {
					XMLName xml.Name `xml:"VersioningConfiguration"`
					NS      string   `xml:"xmlns,attr"`
				}{NS: s3NS})
			case q.has("uploads"):
				g.listUploads(q)
			default:
				g.listObjects(q)
			}
		case http.MethodPost:
			if q.has("delete") {
				g.deleteObjects(q)
				return
			}
			q.fail(errf(errNotImplemented, "not supported"))
		default:
			q.fail(errf(errMethodNotAllowed, "method not allowed"))
		}
	default:
		switch r.Method {
		case http.MethodPut:
			switch {
			case q.has("uploadId") && r.Header.Get("X-Amz-Copy-Source") != "":
				q.fail(errf(errNotImplemented, "copying into a part is not supported; copy the whole object"))
			case q.has("uploadId"):
				g.uploadPart(q)
			case r.Header.Get("X-Amz-Copy-Source") != "":
				g.copyObject(q)
			default:
				g.putObject(q)
			}
		case http.MethodGet, http.MethodHead:
			if q.has("uploadId") && r.Method == http.MethodGet {
				g.listParts(q)
				return
			}
			g.getObject(q)
		case http.MethodDelete:
			if q.has("uploadId") {
				g.abortUpload(q)
				return
			}
			m, e := g.store.remove(q.cust(), q.bucket, q.key)
			if e != nil {
				q.fail(e)
				return
			}
			if m != nil {
				g.discard(*m)
			}
			q.w.WriteHeader(http.StatusNoContent)
		case http.MethodPost:
			switch {
			case q.has("uploads"):
				g.createUpload(q)
			case q.has("uploadId"):
				g.completeUpload(q)
			default:
				q.fail(errf(errNotImplemented, "not supported"))
			}
		default:
			q.fail(errf(errMethodNotAllowed, "method not allowed"))
		}
	}
}

var bucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// readSmall reads a small request body (XML), checked against its signature.
func (q *request) readSmall() ([]byte, *s3Error) {
	if q.r.ContentLength == 0 && q.r.Header.Get("X-Amz-Decoded-Content-Length") == "" {
		return nil, nil
	}
	rd, n, e := body(q.r.Body, &q.a, q.r.Header.Get, q.r.ContentLength)
	if e != nil {
		return nil, e
	}
	if n > maxXMLBody {
		return nil, errf(errInvalidRequest, "request body too large")
	}
	data, err := io.ReadAll(rd)
	if err != nil {
		return nil, bodyError(err)
	}
	return data, nil
}

func bodyError(err error) *s3Error {
	switch {
	case errors.Is(err, errPayloadHash):
		return errf(errSHA256Mismatch, "the body doesn't match x-amz-content-sha256")
	case errors.Is(err, errChunkSig):
		return errf(errSignature, "a chunk signature doesn't match")
	case errors.Is(err, errBadChunk):
		return errf(errInvalidRequest, "malformed aws-chunked body")
	case errors.Is(err, errBodyTooLong):
		return errf(errInvalidRequest, "the body is longer than its Content-Length")
	default:
		return errf(errIncompleteBody, "the body ended before its declared length")
	}
}

func (g *Gateway) createBucket(q *request) {
	if !bucketName.MatchString(q.bucket) || strings.Contains(q.bucket, "..") || net.ParseIP(q.bucket) != nil {
		q.fail(errf(errInvalidBucket, "bucket names are 3-63 lower-case letters, digits, dots and hyphens"))
		return
	}
	if _, e := q.readSmall(); e != nil { // CreateBucketConfiguration; the region is ignored
		q.fail(e)
		return
	}
	if e := g.store.createBucket(q.cust(), q.bucket, g.opts.MaxBuckets); e != nil {
		q.fail(e)
		return
	}
	q.w.Header().Set("Location", "/"+q.bucket)
	q.w.WriteHeader(http.StatusOK)
}

type owner struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName"`
}

func (q *request) owner() owner { return owner{q.a.customer.ID, q.a.customer.Name} }

func (g *Gateway) listBuckets(q *request) {
	type b struct {
		Name         string `xml:"Name"`
		CreationDate string `xml:"CreationDate"`
	}
	var list []b
	for _, x := range g.store.buckets(q.cust()) {
		list = append(list, b{x.Name, x.Created.UTC().Format(time.RFC3339)})
	}
	writeXML(q.w, http.StatusOK, struct {
		XMLName xml.Name `xml:"ListAllMyBucketsResult"`
		NS      string   `xml:"xmlns,attr"`
		Owner   owner    `xml:"Owner"`
		Buckets []b      `xml:"Buckets>Bucket"`
	}{NS: s3NS, Owner: q.owner(), Buckets: list})
}

type xmlObject struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
	Owner        *owner `xml:"Owner,omitempty"`
}

type xmlPrefix struct {
	Prefix string `xml:"Prefix"`
}

func isoTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func (g *Gateway) listObjects(q *request) {
	v2 := q.query.Get("list-type") == "2"
	prefix, delimiter := q.query.Get("prefix"), q.query.Get("delimiter")
	maxKeys := 1000
	if s := q.query.Get("max-keys"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			q.fail(errf(errInvalidArgument, "max-keys must be a number from 0"))
			return
		}
		maxKeys = min(n, 1000)
	}
	enc := func(s string) string { return s }
	encType := q.query.Get("encoding-type")
	if encType == "url" {
		enc = func(s string) string { return uriEncode(s, true) }
	} else if encType != "" {
		q.fail(errf(errInvalidArgument, "encoding-type must be url"))
		return
	}
	marker := q.query.Get("marker")
	token := q.query.Get("continuation-token")
	if v2 {
		marker = q.query.Get("start-after")
		if token != "" {
			raw, err := base64.RawURLEncoding.DecodeString(token)
			if err != nil {
				q.fail(errf(errInvalidArgument, "bad continuation token"))
				return
			}
			marker = string(raw)
		}
	}
	l, e := g.store.list(q.cust(), q.bucket, prefix, delimiter, marker, maxKeys)
	if e != nil {
		q.fail(e)
		return
	}
	withOwner := !v2 || q.query.Get("fetch-owner") == "true"
	var contents []xmlObject
	for _, o := range l.objects {
		x := xmlObject{Key: enc(o.Key), LastModified: isoTime(o.Modified), ETag: `"` + o.ETag + `"`, Size: o.Size, StorageClass: "STANDARD"}
		if withOwner {
			ow := q.owner()
			x.Owner = &ow
		}
		contents = append(contents, x)
	}
	var cps []xmlPrefix
	for _, p := range l.prefixes {
		cps = append(cps, xmlPrefix{enc(p)})
	}
	if v2 {
		next := ""
		if l.truncated {
			next = base64.RawURLEncoding.EncodeToString([]byte(l.next))
		}
		writeXML(q.w, http.StatusOK, struct {
			XMLName               xml.Name    `xml:"ListBucketResult"`
			NS                    string      `xml:"xmlns,attr"`
			Name                  string      `xml:"Name"`
			Prefix                string      `xml:"Prefix"`
			Delimiter             string      `xml:"Delimiter,omitempty"`
			MaxKeys               int         `xml:"MaxKeys"`
			KeyCount              int         `xml:"KeyCount"`
			IsTruncated           bool        `xml:"IsTruncated"`
			ContinuationToken     string      `xml:"ContinuationToken,omitempty"`
			NextContinuationToken string      `xml:"NextContinuationToken,omitempty"`
			StartAfter            string      `xml:"StartAfter,omitempty"`
			EncodingType          string      `xml:"EncodingType,omitempty"`
			Contents              []xmlObject `xml:"Contents"`
			CommonPrefixes        []xmlPrefix `xml:"CommonPrefixes"`
		}{NS: s3NS, Name: q.bucket, Prefix: enc(prefix), Delimiter: enc(delimiter), MaxKeys: maxKeys,
			KeyCount: len(contents) + len(cps), IsTruncated: l.truncated, ContinuationToken: token,
			NextContinuationToken: next, StartAfter: enc(q.query.Get("start-after")), EncodingType: encType,
			Contents: contents, CommonPrefixes: cps})
		return
	}
	next := ""
	if l.truncated {
		next = enc(l.next)
	}
	writeXML(q.w, http.StatusOK, struct {
		XMLName        xml.Name    `xml:"ListBucketResult"`
		NS             string      `xml:"xmlns,attr"`
		Name           string      `xml:"Name"`
		Prefix         string      `xml:"Prefix"`
		Marker         string      `xml:"Marker"`
		NextMarker     string      `xml:"NextMarker,omitempty"`
		Delimiter      string      `xml:"Delimiter,omitempty"`
		MaxKeys        int         `xml:"MaxKeys"`
		IsTruncated    bool        `xml:"IsTruncated"`
		EncodingType   string      `xml:"EncodingType,omitempty"`
		Contents       []xmlObject `xml:"Contents"`
		CommonPrefixes []xmlPrefix `xml:"CommonPrefixes"`
	}{NS: s3NS, Name: q.bucket, Prefix: enc(prefix), Marker: enc(marker), NextMarker: next, Delimiter: enc(delimiter),
		MaxKeys: maxKeys, IsTruncated: l.truncated, EncodingType: encType, Contents: contents, CommonPrefixes: cps})
}

// storedHeaders are the request headers kept with an object and returned
// when it is read.
var storedHeaders = []string{"Content-Type", "Content-Encoding", "Content-Disposition", "Content-Language", "Cache-Control", "Expires"}

func objectHeaders(r *http.Request) (map[string]string, map[string]string) {
	h, meta := map[string]string{}, map[string]string{}
	for _, k := range storedHeaders {
		if v := r.Header.Get(k); v != "" {
			h[k] = v
		}
	}
	for k, vs := range r.Header {
		if name, ok := strings.CutPrefix(strings.ToLower(k), "x-amz-meta-"); ok && name != "" {
			meta[name] = strings.Join(vs, ",")
		}
	}
	return h, meta
}

// errRecorder remembers the first error reading the body, so a failed
// upload can be blamed on the client rather than on storage.
type errRecorder struct {
	r   io.Reader
	err error
	n   int64
}

func (e *errRecorder) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	e.n += int64(n)
	if err != nil && err != io.EOF && e.err == nil {
		e.err = err
	}
	return n, err
}

// store reads the request body into the group, encrypted with key, and
// returns its manifest, size and MD5.
func (g *Gateway) storeBody(q *request, key []byte) (manifest.Manifest, int64, []byte, *s3Error) {
	rd, n, e := body(q.r.Body, &q.a, q.r.Header.Get, q.r.ContentLength)
	if e != nil {
		return manifest.Manifest{}, 0, nil, e
	}
	if n > maxObjectSize {
		return manifest.Manifest{}, 0, nil, errf(errEntityTooLarge, "one upload is at most 5 GiB; use a multipart upload")
	}
	release, e := g.store.reserve(q.cust(), n, q.a.customer.QuotaBytes)
	if e != nil {
		return manifest.Manifest{}, 0, nil, e
	}
	defer release()
	var wantMD5 []byte
	if v := q.r.Header.Get("Content-MD5"); v != "" {
		b, err := base64.StdEncoding.DecodeString(v)
		if err != nil || len(b) != md5.Size {
			return manifest.Manifest{}, 0, nil, errf(errInvalidArgument, "Content-MD5 is not a base64 MD5")
		}
		wantMD5 = b
	}
	h := md5.New()
	rec := &errRecorder{r: io.TeeReader(rd, h)}
	// Not the request's context: if the client goes away, reading fails and
	// the upload cleans up after itself, rather than being cut off midway.
	m, err := g.backend.Put(context.WithoutCancel(q.r.Context()), rec, key)
	if err != nil {
		if rec.err != nil {
			return manifest.Manifest{}, 0, nil, bodyError(rec.err)
		}
		g.opts.Log("storing for %s: %v", q.cust(), err)
		if errors.Is(err, ErrTooFewNodes) {
			return manifest.Manifest{}, 0, nil, errf(errUnavailable, "not enough storage nodes are online; try again later")
		}
		return manifest.Manifest{}, 0, nil, errf(errUnavailable, "the storage nodes could not take the data; try again later")
	}
	sum := h.Sum(nil)
	if wantMD5 != nil && !equalBytes(sum, wantMD5) {
		g.discard(m)
		return manifest.Manifest{}, 0, nil, errf(errBadDigest, "the body doesn't match Content-MD5")
	}
	g.meter.ingress(q.cust(), n)
	return m, n, sum, nil
}

func equalBytes(a, b []byte) bool { return string(a) == string(b) }

func (g *Gateway) putObject(q *request) {
	if !g.store.hasBucket(q.cust(), q.bucket) {
		q.fail(errf(errNoSuchBucket, "no such bucket"))
		return
	}
	m, n, sum, e := g.storeBody(q, nil)
	if e != nil {
		q.fail(e)
		return
	}
	h, meta := objectHeaders(q.r)
	o := Object{Key: q.key, Size: n, ETag: hex.EncodeToString(sum), Modified: time.Now().UTC(), Headers: h, Meta: meta, Manifest: m}
	old, e := g.store.put(q.cust(), q.bucket, o, true)
	if e != nil {
		g.discard(m)
		q.fail(e)
		return
	}
	if old != nil {
		g.discard(*old)
	}
	q.w.Header().Set("ETag", `"`+o.ETag+`"`)
	q.w.WriteHeader(http.StatusOK)
}

// lazyWriter sends the status line with the first byte, so a read that
// fails at once can still get an error response.
type lazyWriter struct {
	w      http.ResponseWriter
	status int
	sent   bool
	n      int64
}

func (l *lazyWriter) Write(p []byte) (int, error) {
	if !l.sent {
		l.w.WriteHeader(l.status)
		l.sent = true
	}
	n, err := l.w.Write(p)
	l.n += int64(n)
	return n, err
}

// parseRange reads a single "bytes=" range; ok is false for anything else,
// which S3 answers with the whole object.
func parseRange(h string, size int64) (start, length int64, ok bool, e *s3Error) {
	spec, found := strings.CutPrefix(h, "bytes=")
	if !found || strings.Contains(spec, ",") {
		return 0, size, false, nil
	}
	a, b, _ := strings.Cut(spec, "-")
	switch {
	case a == "" && b != "": // last b bytes
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n <= 0 {
			return 0, size, false, nil
		}
		if size == 0 {
			return 0, 0, false, errf(errInvalidRange, "the object is empty")
		}
		n = min(n, size)
		return size - n, n, true, nil
	case a != "":
		s, err := strconv.ParseInt(a, 10, 64)
		if err != nil || s < 0 {
			return 0, size, false, nil
		}
		end := size - 1
		if b != "" {
			e, err := strconv.ParseInt(b, 10, 64)
			if err != nil || e < s {
				return 0, size, false, nil
			}
			end = min(e, size-1)
		}
		if s >= size {
			return 0, 0, false, errf(errInvalidRange, "the range starts past the end of the object")
		}
		return s, end - s + 1, true, nil
	}
	return 0, size, false, nil
}

func (g *Gateway) getObject(q *request) {
	head := q.r.Method == http.MethodHead
	var o Object
	var e *s3Error
	if head {
		o, e = g.store.head(q.cust(), q.bucket, q.key)
	} else {
		o, e = g.store.get(q.cust(), q.bucket, q.key)
	}
	if e != nil {
		q.fail(e)
		return
	}
	etag := `"` + o.ETag + `"`
	if m := q.r.Header.Get("If-Match"); m != "" && m != "*" && !etagIn(m, etag) {
		q.fail(errf(errPreconditionFail, "If-Match doesn't match the object"))
		return
	}
	if t, err := http.ParseTime(q.r.Header.Get("If-Unmodified-Since")); err == nil && o.Modified.Truncate(time.Second).After(t) {
		q.fail(errf(errPreconditionFail, "the object was modified"))
		return
	}
	w := q.w
	w.Header().Set("ETag", etag)
	w.Header().Set("Last-Modified", o.Modified.UTC().Format(http.TimeFormat))
	if nm := q.r.Header.Get("If-None-Match"); nm != "" {
		if nm == "*" || etagIn(nm, etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	} else if t, err := http.ParseTime(q.r.Header.Get("If-Modified-Since")); err == nil && !o.Modified.Truncate(time.Second).After(t) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "binary/octet-stream")
	for k, v := range o.Headers {
		w.Header().Set(k, v)
	}
	for k, v := range o.Meta {
		w.Header()["X-Amz-Meta-"+k] = []string{v}
	}
	for _, k := range storedHeaders { // presigned links may override these
		if v := q.query.Get("response-" + strings.ToLower(k)); v != "" {
			w.Header().Set(k, v)
		}
	}
	w.Header().Set("Accept-Ranges", "bytes")
	start, length, partial, re := parseRange(q.r.Header.Get("Range"), o.Size)
	if re != nil {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", o.Size))
		q.fail(re)
		return
	}
	if head {
		w.Header().Set("Content-Length", strconv.FormatInt(o.Size, 10))
		w.WriteHeader(http.StatusOK)
		return
	}
	status := http.StatusOK
	if partial {
		status = http.StatusPartialContent
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, start+length-1, o.Size))
	}
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	lw := &lazyWriter{w: w, status: status}
	err := g.backend.Get(q.r.Context(), o.Manifest, lw, start, length)
	g.meter.egress(q.cust(), lw.n)
	if err != nil {
		if !lw.sent {
			w.Header().Del("Content-Length")
			w.Header().Del("Content-Range")
			g.opts.Log("reading %s/%s for %s: %v", q.bucket, q.key, q.cust(), err)
			q.fail(errf(errUnavailable, "the object can't be read right now: too few of its storage nodes answer"))
			return
		}
		g.opts.Log("reading %s/%s for %s broke off after %d bytes: %v", q.bucket, q.key, q.cust(), lw.n, err)
		panic(http.ErrAbortHandler) // tell the client the body is incomplete
	}
	if !lw.sent {
		w.WriteHeader(status)
	}
}

func etagIn(list, etag string) bool {
	for _, e := range strings.Split(list, ",") {
		e = strings.TrimPrefix(strings.TrimSpace(e), "W/")
		if e == etag || `"`+e+`"` == etag {
			return true
		}
	}
	return false
}

func (g *Gateway) copyObject(q *request) {
	src, err := url.PathUnescape(q.r.Header.Get("X-Amz-Copy-Source"))
	if err != nil {
		q.fail(errf(errInvalidArgument, "bad x-amz-copy-source"))
		return
	}
	src, _, _ = strings.Cut(src, "?versionId=")
	srcBucket, srcKey, ok := strings.Cut(strings.TrimPrefix(src, "/"), "/")
	if !ok || srcKey == "" {
		q.fail(errf(errInvalidArgument, "x-amz-copy-source must be bucket/key"))
		return
	}
	if !g.store.hasBucket(q.cust(), q.bucket) {
		q.fail(errf(errNoSuchBucket, "no such bucket"))
		return
	}
	o, e := g.store.get(q.cust(), srcBucket, srcKey)
	if e != nil {
		q.fail(e)
		return
	}
	if m := q.r.Header.Get("X-Amz-Copy-Source-If-Match"); m != "" && !etagIn(m, `"`+o.ETag+`"`) {
		q.fail(errf(errPreconditionFail, "x-amz-copy-source-if-match doesn't match"))
		return
	}
	replace := strings.EqualFold(q.r.Header.Get("X-Amz-Metadata-Directive"), "REPLACE")
	if srcBucket == q.bucket && srcKey == q.key && !replace {
		q.fail(errf(errInvalidRequest, "copying an object onto itself needs x-amz-metadata-directive: REPLACE"))
		return
	}
	if replace {
		o.Headers, o.Meta = objectHeaders(q.r)
	}
	release, e := g.store.reserve(q.cust(), o.Size, q.a.customer.QuotaBytes)
	if e != nil {
		q.fail(e)
		return
	}
	defer release()
	o.Key, o.Modified = q.key, time.Now().UTC()
	// The copy shares the stored item; it is deleted when neither uses it.
	old, e := g.store.put(q.cust(), q.bucket, o, false)
	if e != nil {
		q.fail(e)
		return
	}
	if old != nil {
		g.discard(*old)
	}
	writeXML(q.w, http.StatusOK, struct {
		XMLName      xml.Name `xml:"CopyObjectResult"`
		NS           string   `xml:"xmlns,attr"`
		LastModified string   `xml:"LastModified"`
		ETag         string   `xml:"ETag"`
	}{NS: s3NS, LastModified: isoTime(o.Modified), ETag: `"` + o.ETag + `"`})
}

func (g *Gateway) deleteObjects(q *request) {
	data, e := q.readSmall()
	if e != nil {
		q.fail(e)
		return
	}
	var req struct {
		Quiet   bool `xml:"Quiet"`
		Objects []struct {
			Key string `xml:"Key"`
		} `xml:"Object"`
	}
	if err := xml.Unmarshal(data, &req); err != nil || len(req.Objects) > 1000 {
		q.fail(errf(errMalformedXML, "the delete request is not valid (at most 1000 keys)"))
		return
	}
	type deleted struct {
		Key string `xml:"Key"`
	}
	type failed struct {
		Key     string `xml:"Key"`
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	var done []deleted
	var errs []failed
	for _, o := range req.Objects {
		m, e := g.store.remove(q.cust(), q.bucket, o.Key)
		if e != nil {
			errs = append(errs, failed{o.Key, e.code, e.msg})
			continue
		}
		if m != nil {
			g.discard(*m)
		}
		if !req.Quiet {
			done = append(done, deleted{o.Key})
		}
	}
	writeXML(q.w, http.StatusOK, struct {
		XMLName xml.Name  `xml:"DeleteResult"`
		NS      string    `xml:"xmlns,attr"`
		Deleted []deleted `xml:"Deleted"`
		Error   []failed  `xml:"Error"`
	}{NS: s3NS, Deleted: done, Error: errs})
}

func (g *Gateway) createUpload(q *request) {
	key := make([]byte, 32)
	rand.Read(key)
	h, meta := objectHeaders(q.r)
	u := &Upload{ID: randomHex(24), Bucket: q.bucket, Key: q.key, Started: time.Now().UTC(), Headers: h, Meta: meta, CryptKey: key}
	if e := g.store.newUpload(q.cust(), u); e != nil {
		q.fail(e)
		return
	}
	writeXML(q.w, http.StatusOK, struct {
		XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
		NS       string   `xml:"xmlns,attr"`
		Bucket   string   `xml:"Bucket"`
		Key      string   `xml:"Key"`
		UploadID string   `xml:"UploadId"`
	}{NS: s3NS, Bucket: q.bucket, Key: q.key, UploadID: u.ID})
}

func (g *Gateway) uploadPart(q *request) {
	n, err := strconv.Atoi(q.query.Get("partNumber"))
	if err != nil || n < 1 || n > maxParts {
		q.fail(errf(errInvalidArgument, "partNumber must be from 1 to 10000"))
		return
	}
	u, e := g.store.upload(q.cust(), q.query.Get("uploadId"), q.bucket, q.key)
	if e != nil {
		q.fail(e)
		return
	}
	m, size, sum, e := g.storeBody(q, u.CryptKey)
	if e != nil {
		q.fail(e)
		return
	}
	p := Part{Number: n, Size: size, ETag: hex.EncodeToString(sum), Modified: time.Now().UTC(), Manifest: m}
	old, e := g.store.putPart(q.cust(), u, p)
	if old != nil {
		g.discard(*old)
	}
	if e != nil {
		q.fail(e)
		return
	}
	q.w.Header().Set("ETag", `"`+p.ETag+`"`)
	q.w.WriteHeader(http.StatusOK)
}

func (g *Gateway) completeUpload(q *request) {
	u, e := g.store.upload(q.cust(), q.query.Get("uploadId"), q.bucket, q.key)
	if e != nil {
		q.fail(e)
		return
	}
	data, e := q.readSmall()
	if e != nil {
		q.fail(e)
		return
	}
	var req struct {
		Parts []struct {
			Number int    `xml:"PartNumber"`
			ETag   string `xml:"ETag"`
		} `xml:"Part"`
	}
	if err := xml.Unmarshal(data, &req); err != nil || len(req.Parts) == 0 {
		q.fail(errf(errMalformedXML, "the part list is not valid"))
		return
	}
	have := map[int]Part{}
	for _, p := range g.store.parts(u) {
		have[p.Number] = p
	}
	var manifests []manifest.Manifest
	keep := map[int]bool{}
	md5s := md5.New()
	var size int64
	for i, rp := range req.Parts {
		if i > 0 && rp.Number <= req.Parts[i-1].Number {
			q.fail(errf(errInvalidPartOrder, "parts must be listed in ascending order"))
			return
		}
		p, ok := have[rp.Number]
		if !ok || strings.Trim(rp.ETag, `"`) != p.ETag {
			q.fail(errf(errInvalidPart, fmt.Sprintf("part %d was not uploaded, or its ETag differs", rp.Number)))
			return
		}
		m, err := g.store.partManifest(q.cust(), u, p.Number)
		if err != nil {
			q.fail(errf(errInternal, err.Error()))
			return
		}
		manifests = append(manifests, m)
		keep[p.Number] = true
		raw, _ := hex.DecodeString(p.ETag)
		md5s.Write(raw)
		size += p.Size
	}
	joined, err := files.Concat("s3-object", manifests)
	if err != nil {
		q.fail(errf(errInternal, err.Error()))
		return
	}
	o := Object{Key: q.key, Size: size, ETag: fmt.Sprintf("%x-%d", md5s.Sum(nil), len(req.Parts)),
		Modified: time.Now().UTC(), Headers: u.Headers, Meta: u.Meta, Manifest: joined}
	old, e := g.store.put(q.cust(), q.bucket, o, true)
	if e != nil {
		q.fail(e)
		return
	}
	if old != nil {
		g.discard(*old)
	}
	for _, m := range g.store.endUpload(q.cust(), u, keep) {
		g.discard(m)
	}
	writeXML(q.w, http.StatusOK, struct {
		XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
		NS       string   `xml:"xmlns,attr"`
		Location string   `xml:"Location"`
		Bucket   string   `xml:"Bucket"`
		Key      string   `xml:"Key"`
		ETag     string   `xml:"ETag"`
	}{NS: s3NS, Location: "/" + q.bucket + "/" + q.key, Bucket: q.bucket, Key: q.key, ETag: `"` + o.ETag + `"`})
}

func (g *Gateway) abortUpload(q *request) {
	u, e := g.store.upload(q.cust(), q.query.Get("uploadId"), q.bucket, q.key)
	if e != nil {
		q.fail(e)
		return
	}
	for _, m := range g.store.endUpload(q.cust(), u, nil) {
		g.discard(m)
	}
	q.w.WriteHeader(http.StatusNoContent)
}

func (g *Gateway) listParts(q *request) {
	u, e := g.store.upload(q.cust(), q.query.Get("uploadId"), q.bucket, q.key)
	if e != nil {
		q.fail(e)
		return
	}
	marker, _ := strconv.Atoi(q.query.Get("part-number-marker"))
	maxParts := 1000
	if n, err := strconv.Atoi(q.query.Get("max-parts")); err == nil && n >= 0 {
		maxParts = min(n, 1000)
	}
	type xmlPart struct {
		PartNumber   int    `xml:"PartNumber"`
		LastModified string `xml:"LastModified"`
		ETag         string `xml:"ETag"`
		Size         int64  `xml:"Size"`
	}
	var list []xmlPart
	truncated, next := false, 0
	for _, p := range g.store.parts(u) {
		if p.Number <= marker {
			continue
		}
		if len(list) == maxParts {
			truncated = true
			break
		}
		list = append(list, xmlPart{p.Number, isoTime(p.Modified), `"` + p.ETag + `"`, p.Size})
		next = p.Number
	}
	writeXML(q.w, http.StatusOK, struct {
		XMLName              xml.Name  `xml:"ListPartsResult"`
		NS                   string    `xml:"xmlns,attr"`
		Bucket               string    `xml:"Bucket"`
		Key                  string    `xml:"Key"`
		UploadID             string    `xml:"UploadId"`
		PartNumberMarker     int       `xml:"PartNumberMarker"`
		NextPartNumberMarker int       `xml:"NextPartNumberMarker"`
		MaxParts             int       `xml:"MaxParts"`
		IsTruncated          bool      `xml:"IsTruncated"`
		StorageClass         string    `xml:"StorageClass"`
		Parts                []xmlPart `xml:"Part"`
	}{NS: s3NS, Bucket: q.bucket, Key: q.key, UploadID: u.ID, PartNumberMarker: marker, NextPartNumberMarker: next,
		MaxParts: maxParts, IsTruncated: truncated, StorageClass: "STANDARD", Parts: list})
}

func (g *Gateway) listUploads(q *request) {
	prefix := q.query.Get("prefix")
	type xmlUpload struct {
		Key       string `xml:"Key"`
		UploadID  string `xml:"UploadId"`
		Initiated string `xml:"Initiated"`
	}
	var list []xmlUpload
	for _, u := range g.store.uploads(q.cust(), q.bucket) {
		if strings.HasPrefix(u.Key, prefix) && len(list) < 1000 {
			list = append(list, xmlUpload{u.Key, u.ID, isoTime(u.Started)})
		}
	}
	writeXML(q.w, http.StatusOK, struct {
		XMLName     xml.Name    `xml:"ListMultipartUploadsResult"`
		NS          string      `xml:"xmlns,attr"`
		Bucket      string      `xml:"Bucket"`
		Prefix      string      `xml:"Prefix"`
		MaxUploads  int         `xml:"MaxUploads"`
		IsTruncated bool        `xml:"IsTruncated"`
		Uploads     []xmlUpload `xml:"Upload"`
	}{NS: s3NS, Bucket: q.bucket, Prefix: prefix, MaxUploads: 1000, Uploads: list})
}

// failures counts failed sign-ins per address, to slow down guessing.
type failures struct {
	mu sync.Mutex
	m  map[string]*failCount
}

type failCount struct {
	n     int
	since time.Time
}

const (
	failLimit  = 20
	failWindow = 10 * time.Minute
)

func (f *failures) add(ip string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.m[ip]
	if c == nil || time.Since(c.since) > failWindow {
		if len(f.m) > 10000 { // forget old entries rather than grow forever
			for k, v := range f.m {
				if time.Since(v.since) > failWindow {
					delete(f.m, k)
				}
			}
		}
		c = &failCount{since: time.Now()}
		f.m[ip] = c
	}
	c.n++
}

func (f *failures) blocked(ip string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.m[ip]
	return c != nil && c.n >= failLimit && time.Since(c.since) <= failWindow
}
