package gateway

import (
	"encoding/xml"
	"net/http"
)

type s3Code struct {
	code   string
	status int
}

var (
	errAccessDenied     = s3Code{"AccessDenied", http.StatusForbidden}
	errAccountProblem   = s3Code{"AccountProblem", http.StatusForbidden}
	errAuthMalformed    = s3Code{"AuthorizationHeaderMalformed", http.StatusBadRequest}
	errBadDigest        = s3Code{"BadDigest", http.StatusBadRequest}
	errBucketExists     = s3Code{"BucketAlreadyOwnedByYou", http.StatusConflict}
	errBucketNotEmpty   = s3Code{"BucketNotEmpty", http.StatusConflict}
	errEntityTooLarge   = s3Code{"EntityTooLarge", http.StatusBadRequest}
	errIncompleteBody   = s3Code{"IncompleteBody", http.StatusBadRequest}
	errInternal         = s3Code{"InternalError", http.StatusInternalServerError}
	errInvalidAccessKey = s3Code{"InvalidAccessKeyId", http.StatusForbidden}
	errInvalidArgument  = s3Code{"InvalidArgument", http.StatusBadRequest}
	errInvalidBucket    = s3Code{"InvalidBucketName", http.StatusBadRequest}
	errInvalidPart      = s3Code{"InvalidPart", http.StatusBadRequest}
	errInvalidPartOrder = s3Code{"InvalidPartOrder", http.StatusBadRequest}
	errInvalidRange     = s3Code{"InvalidRange", http.StatusRequestedRangeNotSatisfiable}
	errInvalidRequest   = s3Code{"InvalidRequest", http.StatusBadRequest}
	errKeyTooLong       = s3Code{"KeyTooLongError", http.StatusBadRequest}
	errMalformedXML     = s3Code{"MalformedXML", http.StatusBadRequest}
	errMethodNotAllowed = s3Code{"MethodNotAllowed", http.StatusMethodNotAllowed}
	errMissingLength    = s3Code{"MissingContentLength", http.StatusLengthRequired}
	errNoSuchBucket     = s3Code{"NoSuchBucket", http.StatusNotFound}
	errNoSuchKey        = s3Code{"NoSuchKey", http.StatusNotFound}
	errNoSuchUpload     = s3Code{"NoSuchUpload", http.StatusNotFound}
	errNotImplemented   = s3Code{"NotImplemented", http.StatusNotImplemented}
	errPreconditionFail = s3Code{"PreconditionFailed", http.StatusPreconditionFailed}
	errQuotaExceeded    = s3Code{"QuotaExceeded", http.StatusForbidden}
	errSHA256Mismatch   = s3Code{"XAmzContentSHA256Mismatch", http.StatusBadRequest}
	errSignature        = s3Code{"SignatureDoesNotMatch", http.StatusForbidden}
	errSlowDown         = s3Code{"SlowDown", http.StatusServiceUnavailable}
	errTimeSkewed       = s3Code{"RequestTimeTooSkewed", http.StatusForbidden}
	errTooManyBuckets   = s3Code{"TooManyBuckets", http.StatusBadRequest}
	errUnavailable      = s3Code{"ServiceUnavailable", http.StatusServiceUnavailable}
)

type s3Error struct {
	s3Code
	msg string
}

func (e *s3Error) Error() string { return e.code + ": " + e.msg }

func errf(c s3Code, msg string) *s3Error { return &s3Error{c, msg} }

type errorBody struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	Resource  string   `xml:"Resource,omitempty"`
	RequestID string   `xml:"RequestId"`
}

func writeError(w http.ResponseWriter, r *http.Request, e *s3Error, reqID string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(e.status)
	if r.Method == http.MethodHead {
		return
	}
	w.Write([]byte(xml.Header))
	xml.NewEncoder(w).Encode(errorBody{Code: e.code, Message: e.msg, Resource: r.URL.Path, RequestID: reqID})
}

func writeXML(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	w.Write([]byte(xml.Header))
	xml.NewEncoder(w).Encode(v)
}

const s3NS = "http://s3.amazonaws.com/doc/2006-03-01/"
