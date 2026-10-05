package gateway

import (
	"bytes"
	"io"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The examples from the AWS S3 Signature Version 4 documentation.
const (
	exampleKey    = "AKIAIOSFODNN7EXAMPLE"
	exampleSecret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
)

func exampleGateway(t *testing.T) *Gateway {
	t.Helper()
	dir := t.TempDir()
	writeJSON(filepath.Join(dir, "customers.json"), []Customer{{ID: "ex", Name: "Example", AccessKey: exampleKey, Secret: exampleSecret}})
	g, err := New(dir, OpenCustomers(filepath.Join(dir, "customers.json")), nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

var exampleTime = time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)

func TestSignatureGetObjectExample(t *testing.T) {
	g := exampleGateway(t)
	r := httptest.NewRequest("GET", "http://examplebucket.s3.amazonaws.com/test.txt", nil)
	r.Header.Set("Range", "bytes=0-9")
	r.Header.Set("X-Amz-Content-Sha256", emptySHA256)
	r.Header.Set("X-Amz-Date", "20130524T000000Z")
	r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request,SignedHeaders=host;range;x-amz-content-sha256;x-amz-date,Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41")
	if _, e := g.authenticate(r, exampleTime); e != nil {
		t.Fatal(e)
	}
	r.Header.Set("Range", "bytes=0-10")
	if _, e := g.authenticate(r, exampleTime); e == nil || e.code != errSignature.code {
		t.Fatalf("changed header accepted: %v", e)
	}
	if _, e := g.authenticate(r, exampleTime.Add(time.Hour)); e == nil || e.code != errTimeSkewed.code {
		t.Fatalf("old request accepted: %v", e)
	}
}

func TestSignatureChunkedUploadExample(t *testing.T) {
	g := exampleGateway(t)
	sigs := []string{
		"ad80c730a21e5b8d04586a2213dd63b9a0e99e0e2307b0ade35a65485a288648",
		"0055627c9e194cb4542bae2aa5492e3c1575bbb81b612b7d234b86a503ef5497",
		"b6c6ea8a5354eaf15b3cb7646744f4275b71ea724fed81ceb9323e279d449df9",
	}
	var b bytes.Buffer
	b.WriteString("10000;chunk-signature=" + sigs[0] + "\r\n" + strings.Repeat("a", 65536) + "\r\n")
	b.WriteString("400;chunk-signature=" + sigs[1] + "\r\n" + strings.Repeat("a", 1024) + "\r\n")
	b.WriteString("0;chunk-signature=" + sigs[2] + "\r\n\r\n")
	raw := b.Bytes()

	r := httptest.NewRequest("PUT", "http://s3.amazonaws.com/examplebucket/chunkObject.txt", bytes.NewReader(raw))
	r.Header.Set("X-Amz-Date", "20130524T000000Z")
	r.Header.Set("X-Amz-Storage-Class", "REDUCED_REDUNDANCY")
	r.Header.Set("Content-Encoding", "aws-chunked")
	r.Header.Set("X-Amz-Decoded-Content-Length", "66560")
	r.Header.Set("Content-Length", "66824")
	r.Header.Set("X-Amz-Content-Sha256", streamSigned)
	r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request,SignedHeaders=content-encoding;content-length;host;x-amz-content-sha256;x-amz-date;x-amz-decoded-content-length;x-amz-storage-class,Signature=4f232c4386841ef735655705268965c44a0e4690baa4adea153f7db9fa80a0a9")
	if len(raw) != 66824 {
		t.Fatalf("example body is %d bytes", len(raw))
	}
	a, e := g.authenticate(r, exampleTime)
	if e != nil {
		t.Fatal(e)
	}
	rd, n, e := body(r.Body, &a, r.Header.Get, r.ContentLength)
	if e != nil || n != 66560 {
		t.Fatal(e, n)
	}
	got, err := io.ReadAll(rd)
	if err != nil || len(got) != 66560 || strings.Trim(string(got), "a") != "" {
		t.Fatalf("decoded %d bytes: %v", len(got), err)
	}

	// A changed byte breaks that chunk's signature.
	bad := bytes.Replace(raw, []byte("aaaa\r\n400"), []byte("aaab\r\n400"), 1)
	rd, _, _ = body(bytes.NewReader(bad), &a, r.Header.Get, int64(len(bad)))
	if _, err := io.ReadAll(rd); err != errChunkSig {
		t.Fatalf("tampered chunk: %v", err)
	}
	// So does dropping the last chunk.
	short := raw[:len(raw)-len("0;chunk-signature=")-64-4]
	rd, _, _ = body(bytes.NewReader(short), &a, r.Header.Get, int64(len(short)))
	if _, err := io.ReadAll(rd); err == nil {
		t.Fatal("truncated body accepted")
	}
}

func TestUnsignedTrailerBody(t *testing.T) {
	data := "hello, trailer"
	raw := "e\r\n" + data + "\r\n0\r\nx-amz-checksum-crc32:AAAAAA==\r\n\r\n"
	a := auth{payload: streamUnsigned}
	h := map[string]string{"X-Amz-Decoded-Content-Length": "14"}
	rd, n, e := body(strings.NewReader(raw), &a, func(k string) string { return h[k] }, int64(len(raw)))
	if e != nil || n != 14 {
		t.Fatal(e)
	}
	got, err := io.ReadAll(rd)
	if err != nil || string(got) != data {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestPayloadHashChecked(t *testing.T) {
	a := auth{payload: sha256Hex([]byte("right"))}
	rd, _, _ := body(strings.NewReader("wrong"), &a, func(string) string { return "" }, 5)
	if _, err := io.ReadAll(rd); err != errPayloadHash {
		t.Fatalf("wrong body: %v", err)
	}
	rd, _, _ = body(strings.NewReader("righ"), &a, func(string) string { return "" }, 5)
	if _, err := io.ReadAll(rd); err != errBodyTooShort {
		t.Fatalf("short body: %v", err)
	}
}
