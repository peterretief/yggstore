package gateway

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// AWS Signature Version 4, as S3 uses it.
// https://docs.aws.amazon.com/AmazonS3/latest/API/sig-v4-authenticating-requests.html

const (
	sigAlgorithm   = "AWS4-HMAC-SHA256"
	amzDateFormat  = "20060102T150405Z"
	emptySHA256    = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	unsignedBody   = "UNSIGNED-PAYLOAD"
	streamSigned   = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
	streamSignedTr = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER"
	streamUnsigned = "STREAMING-UNSIGNED-PAYLOAD-TRAILER"
	maxSkew        = 15 * time.Minute
)

// auth is a request that passed signature checks.
type auth struct {
	customer Customer
	payload  string // x-amz-content-sha256: a hex hash or one of the modes above
	key      []byte // signing key, for chunk signatures
	date     string // amz date
	scope    string
	seed     string // the request's signature, which chunk signatures chain from
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func signingKey(secret, day, region, service string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), day)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	return hmacSHA256(k, "aws4_request")
}

// uriEncode is S3's URI encoding: everything but unreserved characters is
// %XX-escaped, and '/' too unless keepSlash.
func uriEncode(s string, keepSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && keepSlash:
			b.WriteByte(c)
		default:
			b.WriteString("%" + strings.ToUpper(hex.EncodeToString([]byte{c})))
		}
	}
	return b.String()
}

func canonicalQuery(q url.Values, skip string) string {
	var parts []string
	for k, vs := range q {
		if k == skip {
			continue
		}
		for _, v := range vs {
			parts = append(parts, uriEncode(k, false)+"="+uriEncode(v, false))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "&")
}

func headerValue(r *http.Request, name string) string {
	switch name {
	case "host":
		return r.Host
	case "content-length":
		if v := r.Header.Get("Content-Length"); v != "" {
			return v
		}
		if r.ContentLength >= 0 {
			return strconv.FormatInt(r.ContentLength, 10)
		}
	case "transfer-encoding":
		if len(r.TransferEncoding) > 0 {
			return strings.Join(r.TransferEncoding, ",")
		}
	}
	vals := r.Header.Values(name)
	for i, v := range vals {
		vals[i] = strings.Join(strings.Fields(v), " ")
	}
	return strings.Join(vals, ",")
}

func canonicalRequest(r *http.Request, signed []string, query url.Values, skipQuery, payload string) string {
	var b strings.Builder
	b.WriteString(r.Method + "\n")
	b.WriteString(uriEncode(r.URL.Path, true) + "\n")
	b.WriteString(canonicalQuery(query, skipQuery) + "\n")
	for _, h := range signed {
		b.WriteString(h + ":" + headerValue(r, h) + "\n")
	}
	b.WriteString("\n" + strings.Join(signed, ";") + "\n")
	b.WriteString(payload)
	return b.String()
}

func stringToSign(date, scope, canonical string) string {
	return sigAlgorithm + "\n" + date + "\n" + scope + "\n" + sha256Hex([]byte(canonical))
}

// authenticate checks a request's signature, in the Authorization header or
// in the query string (a presigned URL).
func (g *Gateway) authenticate(r *http.Request, now time.Time) (auth, *s3Error) {
	query := r.URL.Query()
	var credential, signedHeaders, signature, date, payload, skipQuery string
	presigned := false
	if h := r.Header.Get("Authorization"); h != "" {
		rest, ok := strings.CutPrefix(h, sigAlgorithm+" ")
		if !ok {
			return auth{}, errf(errAccessDenied, "only AWS Signature Version 4 is supported")
		}
		for _, f := range strings.Split(rest, ",") {
			k, v, _ := strings.Cut(strings.TrimSpace(f), "=")
			switch k {
			case "Credential":
				credential = v
			case "SignedHeaders":
				signedHeaders = v
			case "Signature":
				signature = v
			}
		}
		date = r.Header.Get("X-Amz-Date")
		if date == "" {
			if t, err := http.ParseTime(r.Header.Get("Date")); err == nil {
				date = t.UTC().Format(amzDateFormat)
			}
		}
		payload = r.Header.Get("X-Amz-Content-Sha256")
		if payload == "" {
			return auth{}, errf(errInvalidRequest, "missing x-amz-content-sha256")
		}
	} else if query.Get("X-Amz-Algorithm") == sigAlgorithm {
		presigned = true
		credential = query.Get("X-Amz-Credential")
		signedHeaders = query.Get("X-Amz-SignedHeaders")
		signature = query.Get("X-Amz-Signature")
		date = query.Get("X-Amz-Date")
		payload, skipQuery = unsignedBody, "X-Amz-Signature"
	} else if query.Get("AWSAccessKeyId") != "" && query.Get("Signature") != "" {
		return g.authenticateV2Link(r, query, now)
	} else {
		return auth{}, errf(errAccessDenied, "requests must be signed with an access key")
	}

	parts := strings.Split(credential, "/")
	if len(parts) != 5 || parts[3] != "s3" || parts[4] != "aws4_request" || signature == "" || signedHeaders == "" {
		return auth{}, errf(errAuthMalformed, "the authorization is malformed")
	}
	t, err := time.Parse(amzDateFormat, date)
	if err != nil || parts[1] != date[:8] {
		return auth{}, errf(errAuthMalformed, "the request date is missing or doesn't match the credential")
	}
	if presigned {
		expires, err := strconv.Atoi(query.Get("X-Amz-Expires"))
		if err != nil || expires < 1 || expires > 7*24*3600 {
			return auth{}, errf(errAuthMalformed, "X-Amz-Expires must be between 1 second and 7 days")
		}
		if now.Before(t.Add(-maxSkew)) || now.After(t.Add(time.Duration(expires)*time.Second)) {
			return auth{}, errf(errAccessDenied, "the link has expired")
		}
	} else if d := now.Sub(t); d > maxSkew || d < -maxSkew {
		return auth{}, errf(errTimeSkewed, "the difference between the request time and this server's time is too large; check your clock")
	}
	signed := strings.Split(signedHeaders, ";")
	if !sort.StringsAreSorted(signed) || !contains(signed, "host") {
		return auth{}, errf(errAuthMalformed, "signed headers must be sorted and include host")
	}

	cu, ok := g.Customers.ByAccessKey(parts[0])
	if !ok {
		return auth{}, errf(errInvalidAccessKey, "the access key is not known here")
	}
	key := signingKey(cu.Secret, parts[1], parts[2], "s3")
	scope := strings.Join(parts[1:], "/")
	canonical := canonicalRequest(r, signed, query, skipQuery, payload)
	want := hex.EncodeToString(hmacSHA256(key, stringToSign(date, scope, canonical)))
	if !hmac.Equal([]byte(want), []byte(strings.ToLower(signature))) {
		return auth{}, errf(errSignature, "the signature doesn't match; check the secret key")
	}
	if cu.Suspended {
		return auth{}, errf(errAccountProblem, "this account is suspended; contact the group's organiser")
	}
	return auth{customer: cu, payload: payload, key: key, date: date, scope: scope, seed: want}, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// v2Subresources are the query parameters Signature Version 2 signs.
var v2Subresources = []string{"acl", "delete", "lifecycle", "location", "logging", "notification", "partNumber",
	"policy", "requestPayment", "response-cache-control", "response-content-disposition", "response-content-encoding",
	"response-content-language", "response-content-type", "response-expires", "tagging", "torrent", "uploadId",
	"uploads", "versionId", "versioning", "versions", "website"}

// authenticateV2Link checks a download link presigned with the older
// Signature Version 2, which boto3 and others still make by default for
// endpoints other than AWS. Only reads are allowed this way.
func (g *Gateway) authenticateV2Link(r *http.Request, query url.Values, now time.Time) (auth, *s3Error) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return auth{}, errf(errAccessDenied, "Signature Version 2 links can only download; sign with Version 4")
	}
	expires, err := strconv.ParseInt(query.Get("Expires"), 10, 64)
	if err != nil {
		return auth{}, errf(errAuthMalformed, "the link has no valid Expires")
	}
	if now.Unix() > expires {
		return auth{}, errf(errAccessDenied, "the link has expired")
	}
	if time.Unix(expires, 0).After(now.Add(7 * 24 * time.Hour)) {
		return auth{}, errf(errAccessDenied, "links can be valid for at most 7 days")
	}
	cu, ok := g.Customers.ByAccessKey(query.Get("AWSAccessKeyId"))
	if !ok {
		return auth{}, errf(errInvalidAccessKey, "the access key is not known here")
	}
	var sub []string
	for _, k := range v2Subresources {
		if vs, ok := query[k]; ok {
			if vs[0] == "" {
				sub = append(sub, k)
			} else {
				sub = append(sub, k+"="+vs[0])
			}
		}
	}
	resource := uriEncode(r.URL.Path, true)
	if len(sub) > 0 {
		resource += "?" + strings.Join(sub, "&")
	}
	var amz []string
	for k := range r.Header {
		if lk := strings.ToLower(k); strings.HasPrefix(lk, "x-amz-") {
			amz = append(amz, lk+":"+strings.Join(r.Header.Values(k), ","))
		}
	}
	sort.Strings(amz)
	sts := r.Method + "\n" + r.Header.Get("Content-Md5") + "\n" + r.Header.Get("Content-Type") + "\n" +
		query.Get("Expires") + "\n"
	for _, h := range amz {
		sts += h + "\n"
	}
	sts += resource
	mac := hmac.New(sha1.New, []byte(cu.Secret))
	mac.Write([]byte(sts))
	want := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(query.Get("Signature"))) {
		return auth{}, errf(errSignature, "the link's signature doesn't match")
	}
	if cu.Suspended {
		return auth{}, errf(errAccountProblem, "this account is suspended; contact the group's organiser")
	}
	return auth{customer: cu, payload: unsignedBody}, nil
}
