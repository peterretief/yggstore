package gateway

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peterretief/yggstore/internal/client"
	"github.com/peterretief/yggstore/internal/localstore"
	"github.com/peterretief/yggstore/internal/peers"
	"github.com/peterretief/yggstore/internal/server"
	"github.com/peterretief/yggstore/internal/transport"
)

type testGroup struct {
	list []peers.Peer
	dirs []string
}

// startGroup starts storage nodes on loopback; optIn says which take
// customer data.
func startGroup(t *testing.T, optIn ...bool) *testGroup {
	t.Helper()
	g := &testGroup{}
	for i, ok := range optIn {
		dir := t.TempDir()
		h := server.Handler(localstore.New(dir), server.Options{Name: "n", Transport: transport.Loopback{},
			Allowed: map[string]bool{"127.0.0.1": true}, Customers: ok})
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		g.list = append(g.list, peers.Peer{Name: string(rune('a' + i)), Addr: strings.TrimPrefix(srv.URL, "http://"), Owner: []string{"Anna", "Ben", "Cleo"}[i%3]})
		g.dirs = append(g.dirs, dir)
	}
	return g
}

func (tg *testGroup) shards(t *testing.T) int {
	n := 0
	for _, d := range tg.dirs {
		filepath.WalkDir(d, func(path string, e os.DirEntry, err error) error {
			if err == nil && !e.IsDir() && len(e.Name()) == 64 {
				n++
			}
			return nil
		})
	}
	return n
}

func startGateway(t *testing.T, tg *testGroup) (*Gateway, *httptest.Server, Customer, string) {
	t.Helper()
	dir := t.TempDir()
	cs := OpenCustomers(filepath.Join(dir, "customers.json"))
	cu, err := cs.Add("Acme", "ops@acme.example", "test", 0)
	if err != nil {
		t.Fatal(err)
	}
	backend := &Group{Client: client.New(), Peers: func() []peers.Peer { return tg.list }}
	g, err := New(dir, cs, backend, Options{Peers: func() []peers.Peer { return tg.list }, Log: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	return g, srv, cu, dir
}

func randomFile(t *testing.T, path string, size int) []byte {
	t.Helper()
	data := make([]byte, size)
	rand.Read(data)
	os.MkdirAll(filepath.Dir(path), 0o700)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return data
}

// TestRclone uses rclone, as a customer would, for everything it does
// against S3: buckets, small and multipart uploads, listing, checksums,
// server-side copies and moves, setting modification times, ranged reads,
// deletes, and its encrypting "crypt" remote on top.
func TestRclone(t *testing.T) {
	rclone, err := exec.LookPath("rclone")
	if err != nil {
		t.Skip("rclone not installed")
	}
	tg := startGroup(t, true, true, true, true, true, true)
	g, srv, cu, _ := startGateway(t, tg)
	home := t.TempDir()
	obscured, err := exec.Command(rclone, "obscure", "correct horse battery staple").Output()
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(home, "rclone.conf"), nil, 0o600)
	env := append(os.Environ(),
		"HOME="+home, "RCLONE_CONFIG="+filepath.Join(home, "rclone.conf"),
		"RCLONE_CONFIG_GW_TYPE=s3", "RCLONE_CONFIG_GW_PROVIDER=Other",
		"RCLONE_CONFIG_GW_ACCESS_KEY_ID="+cu.AccessKey, "RCLONE_CONFIG_GW_SECRET_ACCESS_KEY="+cu.Secret,
		"RCLONE_CONFIG_GW_ENDPOINT="+srv.URL, "RCLONE_CONFIG_GW_FORCE_PATH_STYLE=true",
		"RCLONE_CONFIG_SAFE_TYPE=crypt", "RCLONE_CONFIG_SAFE_REMOTE=gw:backup/safe",
		"RCLONE_CONFIG_SAFE_PASSWORD="+strings.TrimSpace(string(obscured)),
	)
	run := func(args ...string) string {
		t.Helper()
		args = append([]string{"--s3-upload-cutoff", "5M", "--s3-chunk-size", "5M", "--retries", "1", "--low-level-retries", "1"}, args...)
		cmd := exec.Command(rclone, args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("rclone %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}

	src := filepath.Join(t.TempDir(), "src")
	randomFile(t, filepath.Join(src, "empty.txt"), 0)
	randomFile(t, filepath.Join(src, "small.bin"), 1000)
	big := randomFile(t, filepath.Join(src, "photos/big.bin"), 12<<20+333) // 3 parts
	randomFile(t, filepath.Join(src, "photos/2026/a b+c&d.jpg"), 70000)

	run("mkdir", "gw:backup")
	run("copy", src, "gw:backup/plain")
	run("check", src, "gw:backup/plain") // sizes and MD5s (multipart: sizes)
	if out := run("lsf", "-R", "gw:backup/plain"); !strings.Contains(out, "photos/2026/a b+c&d.jpg") {
		t.Fatalf("listing:\n%s", out)
	}
	if out := run("lsf", "gw:backup/plain"); strings.Count(out, "\n") != 3 { // empty.txt, small.bin, photos/
		t.Fatalf("top-level listing:\n%s", out)
	}
	// A ranged read across a part boundary.
	if out := run("cat", "--offset", "5242870", "--count", "20", "gw:backup/plain/photos/big.bin"); out != string(big[5242870:5242890]) {
		t.Fatal("ranged read returned the wrong bytes")
	}
	// Server-side copy shares the stored data; the copy outlives the original.
	before := tg.shards(t)
	run("copyto", "gw:backup/plain/photos/big.bin", "gw:backup/copy.bin")
	if tg.shards(t) != before {
		t.Fatal("a server-side copy stored the data again")
	}
	run("deletefile", "gw:backup/plain/photos/big.bin")
	g.Wait()
	dst := t.TempDir()
	run("copyto", "gw:backup/copy.bin", filepath.Join(dst, "copy.bin"))
	if got, _ := os.ReadFile(filepath.Join(dst, "copy.bin")); !bytes.Equal(got, big) {
		t.Fatal("copy reads back wrong after deleting the original")
	}
	run("moveto", "gw:backup/copy.bin", "gw:backup/moved.bin")
	run("touch", "-t", "2020-01-02T03:04:05", "gw:backup/moved.bin") // rclone shows it in local time
	if out := run("lsl", "gw:backup/moved.bin"); !strings.Contains(out, "2020-01-02 ") || !strings.Contains(out, ":04:05") {
		t.Fatalf("modification time not kept:\n%s", out)
	}

	// Encrypted on the customer's side: the gateway only sees ciphertext.
	run("copy", src, "safe:")
	run("cryptcheck", src, "safe:")
	if out := run("lsf", "-R", "gw:backup/safe"); strings.Contains(out, "photos") || strings.Contains(out, "small") {
		t.Fatalf("names readable through crypt:\n%s", out)
	}

	run("purge", "gw:backup")
	g.Wait()
	if n := tg.shards(t); n != 0 {
		t.Fatalf("%d shards left after deleting everything", n)
	}
	if out := run("lsd", "gw:"); strings.Contains(out, "backup") {
		t.Fatalf("bucket still listed:\n%s", out)
	}
}

// signedRequest signs a request the way S3 clients do.
func signedRequest(t *testing.T, cu Customer, method, url string, body []byte, hdr map[string]string) *http.Request {
	t.Helper()
	r, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	date := now.Format(amzDateFormat)
	r.Header.Set("X-Amz-Date", date)
	r.Header.Set("X-Amz-Content-Sha256", sha256Hex(body))
	signed := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	r.Host = r.URL.Host
	scope := date[:8] + "/us-east-1/s3/aws4_request"
	key := signingKey(cu.Secret, date[:8], "us-east-1", "s3")
	sig := hex.EncodeToString(hmacSHA256(key, stringToSign(date, scope, canonicalRequest(r, signed, r.URL.Query(), "", sha256Hex(body)))))
	r.Header.Set("Authorization", sigAlgorithm+" Credential="+cu.AccessKey+"/"+scope+", SignedHeaders="+strings.Join(signed, ";")+", Signature="+sig)
	return r
}

func do(t *testing.T, r *http.Request) (int, string) {
	t.Helper()
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestQuotaAndSuspension(t *testing.T) {
	tg := startGroup(t, true, true, true)
	g, srv, cu, dir := startGateway(t, tg)
	cs := OpenCustomers(filepath.Join(dir, "customers.json"))
	cs.Update(cu.ID, func(c *Customer) { c.QuotaBytes = 1500 })
	cu, _ = cs.Find(cu.ID)

	if st, b := do(t, signedRequest(t, cu, "PUT", srv.URL+"/bk1", nil, nil)); st != 200 {
		t.Fatalf("create bucket: %d %s", st, b)
	}
	if st, b := do(t, signedRequest(t, cu, "PUT", srv.URL+"/bk1/one", make([]byte, 1000), nil)); st != 200 {
		t.Fatalf("put within quota: %d %s", st, b)
	}
	if st, b := do(t, signedRequest(t, cu, "PUT", srv.URL+"/bk1/two", make([]byte, 1000), nil)); st != 403 || !strings.Contains(b, "QuotaExceeded") {
		t.Fatalf("put over quota: %d %s", st, b)
	}
	// Replacing an object only counts the difference... after the old one goes.
	if st, b := do(t, signedRequest(t, cu, "PUT", srv.URL+"/bk1/one", make([]byte, 400), nil)); st != 200 {
		t.Fatalf("overwrite: %d %s", st, b)
	}
	g.Wait()

	cs.Update(cu.ID, func(c *Customer) { c.Suspended = true })
	if st, b := do(t, signedRequest(t, cu, "GET", srv.URL+"/bk1/one", nil, nil)); st != 403 || !strings.Contains(b, "AccountProblem") {
		t.Fatalf("suspended account: %d %s", st, b)
	}
	other := cu
	other.Secret = "wrong"
	if st, b := do(t, signedRequest(t, other, "GET", srv.URL+"/", nil, nil)); st != 403 || !strings.Contains(b, "SignatureDoesNotMatch") {
		t.Fatalf("wrong secret: %d %s", st, b)
	}
}

func TestOnlyOptedInNodesHoldCustomerData(t *testing.T) {
	tg := startGroup(t, true, false, true, false, true, true)
	_, srv, cu, _ := startGateway(t, tg)
	do(t, signedRequest(t, cu, "PUT", srv.URL+"/bkt", nil, nil))
	if st, b := do(t, signedRequest(t, cu, "PUT", srv.URL+"/bkt/x", make([]byte, 50000), nil)); st != 200 {
		t.Fatalf("put: %d %s", st, b)
	}
	for i, d := range tg.dirs {
		entries, _ := os.ReadDir(d)
		has := false
		for _, e := range entries {
			has = has || len(e.Name()) == 64
		}
		if opted := i != 1 && i != 3; has && !opted {
			t.Fatalf("node %d holds customer data without opting in", i)
		}
	}

	few := startGroup(t, true, false, false, true)
	_, srv2, cu2, _ := startGateway(t, few)
	do(t, signedRequest(t, cu2, "PUT", srv2.URL+"/bkt", nil, nil))
	if st, b := do(t, signedRequest(t, cu2, "PUT", srv2.URL+"/bkt/x", []byte("hi"), nil)); st != 503 || !strings.Contains(b, "not enough storage nodes") {
		t.Fatalf("with 2 opted-in machines: %d %s", st, b)
	}
}

func TestMeteringAndReport(t *testing.T) {
	dir := t.TempDir()
	m := newMeter(dir)
	list := []peers.Peer{{Name: "a", Addr: "[200::1]:7400", Owner: "Anna"}, {Name: "b", Addr: "[200::2]:7400", Owner: "Ben"}}
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	stored := map[string]int64{"c1": 3e9}
	held := map[string]int64{"[200::1]:7400": 3e9, "[200::2]:7400": 1.5e9}
	m.sample(t0, stored, held, list)
	m.ingress("c1", 3e9)
	m.egress("c1", 1e9)
	m.request("c1")
	for h := 1; h <= 744; h++ { // all of October, hourly
		m.sample(t0.Add(time.Duration(h)*time.Hour-time.Second), stored, held, list)
	}
	mo, err := LoadMonth(filepath.Dir(dir), "2026-10")
	if err == nil {
		t.Fatal("LoadMonth should read DIR/usage") // the meter's dir here is the usage dir itself
	}
	if err := readJSON(filepath.Join(dir, "2026-10.json"), &mo); err != nil {
		t.Fatal(err)
	}
	gbm := mo.Customers["c1"].ByteHours / hoursIn("2026-10") / 1e9
	if gbm < 2.99 || gbm > 3.01 {
		t.Fatalf("3 GB all month metered as %.3f GB-months", gbm)
	}
	if mo.Customers["c1"].Uploaded != 3e9 || mo.Customers["c1"].Downloaded != 1e9 || mo.Customers["c1"].Requests != 1 {
		t.Fatalf("counters: %+v", mo.Customers["c1"])
	}
	var out bytes.Buffer
	Report(&out, mo, []Customer{{ID: "c1", Name: "Acme", Plan: "10 GB"}})
	for _, want := range []string{"Acme", "3.000", "Anna", "66.7%", "Ben", "33.3%"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("report lacks %q:\n%s", want, out.String())
		}
	}
	// A long gap (the gateway was off) is capped, not billed in full.
	m.sample(t0.AddDate(0, 0, 40), stored, held, list)
	var nov Month
	readJSON(filepath.Join(dir, "2026-11.json"), &nov)
	if h := nov.Customers["c1"].ByteHours / 3e9; h > maxGap.Hours()+0.01 {
		t.Fatalf("gap billed as %.1f hours", h)
	}
}

// TestBoto3 runs testdata/boto3_check.py with the Python in
// $YGGSTORE_TEST_PYTHON, which needs boto3 installed.
func TestBoto3(t *testing.T) {
	python := os.Getenv("YGGSTORE_TEST_PYTHON")
	if python == "" {
		t.Skip("set YGGSTORE_TEST_PYTHON to a Python with boto3")
	}
	tg := startGroup(t, true, true, true, true, true, true)
	g, srv, cu, _ := startGateway(t, tg)
	cmd := exec.Command(python, "testdata/boto3_check.py")
	cmd.Env = append(os.Environ(), "ENDPOINT="+srv.URL, "ACCESS_KEY="+cu.AccessKey, "SECRET_KEY="+cu.Secret)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	g.Wait()
	if n := tg.shards(t); n != 0 {
		t.Fatalf("%d shards left after deleting everything", n)
	}
}
