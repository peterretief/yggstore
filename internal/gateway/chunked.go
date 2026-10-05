package gateway

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"strconv"
	"strings"
)

// maxChunk bounds one aws-chunked chunk, so a client can't make us buffer
// much. SDKs use 64 KiB to 8 MiB.
const maxChunk = 16 << 20

var (
	errBadChunk     = errors.New("malformed aws-chunked body")
	errChunkSig     = errors.New("chunk signature does not match")
	errPayloadHash  = errors.New("body does not match x-amz-content-sha256")
	errBodyTooShort = errors.New("body is shorter than its declared length")
	errBodyTooLong  = errors.New("body is longer than its declared length")
)

// chunkedReader decodes an aws-chunked body ("SIZE;chunk-signature=SIG\r\n
// DATA\r\n" ... "0...\r\n" trailers "\r\n"), checking each chunk's signature
// when signed.
type chunkedReader struct {
	r       *bufio.Reader
	a       *auth // nil when unsigned
	prevSig string
	buf     []byte
	done    bool
}

func newChunkedReader(r io.Reader, a *auth) *chunkedReader {
	c := &chunkedReader{r: bufio.NewReaderSize(r, 64<<10), a: a}
	if a != nil {
		c.prevSig = a.seed
	}
	return c
}

func (c *chunkedReader) line() (string, error) {
	l, err := c.r.ReadString('\n')
	if err != nil {
		if err == io.EOF {
			return "", io.ErrUnexpectedEOF
		}
		return "", err
	}
	if len(l) > 4096 {
		return "", errBadChunk
	}
	return strings.TrimRight(l, "\r\n"), nil
}

func (c *chunkedReader) Read(p []byte) (int, error) {
	for len(c.buf) == 0 {
		if c.done {
			return 0, io.EOF
		}
		if err := c.next(); err != nil {
			return 0, err
		}
	}
	n := copy(p, c.buf)
	c.buf = c.buf[n:]
	return n, nil
}

func (c *chunkedReader) next() error {
	head, err := c.line()
	if err != nil {
		return err
	}
	sizeHex, ext, _ := strings.Cut(head, ";")
	size, err := strconv.ParseInt(strings.TrimSpace(sizeHex), 16, 64)
	if err != nil || size < 0 || size > maxChunk {
		return errBadChunk
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(c.r, data); err != nil {
		return io.ErrUnexpectedEOF
	}
	if c.a != nil {
		sig, ok := strings.CutPrefix(ext, "chunk-signature=")
		if !ok {
			return errBadChunk
		}
		sts := "AWS4-HMAC-SHA256-PAYLOAD\n" + c.a.date + "\n" + c.a.scope + "\n" + c.prevSig + "\n" + emptySHA256 + "\n" + sha256Hex(data)
		want := hex.EncodeToString(hmacSHA256(c.a.key, sts))
		if !hmac.Equal([]byte(want), []byte(sig)) {
			return errChunkSig
		}
		c.prevSig = want
	}
	if size == 0 {
		// Trailers (checksums, a trailer signature), then an empty line.
		for {
			l, err := c.line()
			if err != nil {
				return err
			}
			if l == "" {
				break
			}
		}
		c.done = true
		return nil
	}
	if l, err := c.line(); err != nil || l != "" {
		return errBadChunk
	}
	c.buf = data
	return nil
}

// exactReader passes exactly n bytes through, failing if the body is
// shorter or longer, and checks its SHA-256 at the end when want is set.
type exactReader struct {
	r    io.Reader
	n    int64
	h    hash.Hash
	want string
}

func (e *exactReader) Read(p []byte) (int, error) {
	if e.n <= 0 {
		// Read on to the end: that checks the final chunk's signature, and
		// that nothing follows.
		var one [1]byte
		k, err := io.ReadFull(e.r, one[:])
		if k > 0 {
			return 0, errBodyTooLong
		}
		if err != nil && err != io.EOF {
			return 0, err
		}
		if e.h != nil && hex.EncodeToString(e.h.Sum(nil)) != e.want {
			return 0, errPayloadHash
		}
		return 0, io.EOF
	}
	if int64(len(p)) > e.n {
		p = p[:e.n]
	}
	k, err := e.r.Read(p)
	e.n -= int64(k)
	if e.h != nil {
		e.h.Write(p[:k])
	}
	if err == io.EOF {
		if e.n > 0 {
			return k, errBodyTooShort
		}
		err = nil
	}
	return k, err
}

// body decodes the request body as its signature says and returns it with
// its real length. The reader fails, rather than ending early, if the body
// is short, long, or doesn't match its hash or chunk signatures.
func body(r io.Reader, a *auth, header func(string) string, contentLength int64) (io.Reader, int64, *s3Error) {
	switch a.payload {
	case streamSigned, streamSignedTr, streamUnsigned:
		n, err := strconv.ParseInt(header("X-Amz-Decoded-Content-Length"), 10, 64)
		if err != nil || n < 0 {
			return nil, 0, errf(errMissingLength, "x-amz-decoded-content-length is required")
		}
		signer := a
		if a.payload == streamUnsigned {
			signer = nil
		}
		return &exactReader{r: newChunkedReader(r, signer), n: n}, n, nil
	case unsignedBody:
		if contentLength < 0 {
			return nil, 0, errf(errMissingLength, "Content-Length is required")
		}
		return &exactReader{r: r, n: contentLength}, contentLength, nil
	default:
		if len(a.payload) != 64 {
			return nil, 0, errf(errNotImplemented, "unsupported x-amz-content-sha256 mode "+a.payload)
		}
		if contentLength < 0 {
			return nil, 0, errf(errMissingLength, "Content-Length is required")
		}
		return &exactReader{r: r, n: contentLength, h: sha256.New(), want: strings.ToLower(a.payload)}, contentLength, nil
	}
}
