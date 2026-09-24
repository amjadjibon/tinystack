// Package sigv4 parses and verifies AWS Signature Version 4 Authorization headers.
package sigv4

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// Algorithm is the only signing algorithm TinyStack accepts.
	Algorithm = "AWS4-HMAC-SHA256"
	// MaxSkew is how far a request's signing time may be from the server clock.
	MaxSkew    = 15 * time.Minute
	timeFormat = "20060102T150405Z"
	dateFormat = "20060102"
	terminator = "aws4_request"
)

var (
	// ErrMalformed reports an Authorization header or signing input that is not
	// valid SigV4.
	ErrMalformed = errors.New("malformed signature")
	// ErrExpired reports a signing time more than MaxSkew from the server clock.
	ErrExpired = errors.New("signature expired")
	// ErrMismatch reports a well-formed signature that does not match the request.
	ErrMismatch = errors.New("signature does not match")
)

// Authorization is a parsed SigV4 Authorization header.
type Authorization struct {
	AccessKey, Date, Region, Service, Signature string
	SignedHeaders                               []string
}

// Parse parses an Authorization header. SignedHeaders and Signature may be
// empty; Verify rejects them.
func Parse(header string) (*Authorization, error) {
	fields, ok := strings.CutPrefix(header, Algorithm+" ")
	if !ok {
		return nil, errors.New("expected " + Algorithm)
	}

	a := &Authorization{}
	credential := ""
	for field := range strings.SplitSeq(fields, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(field), "=")
		switch k {
		case "Credential":
			credential = v
		case "SignedHeaders":
			a.SignedHeaders = strings.Split(v, ";")
		case "Signature":
			a.Signature = v
		}
	}

	scope := strings.Split(credential, "/")
	if len(scope) != 5 || scope[0] == "" || len(scope[1]) != 8 || scope[2] == "" || scope[3] == "" ||
		scope[4] != terminator {
		return nil, errors.New("malformed credential scope")
	}

	a.AccessKey, a.Date, a.Region, a.Service = scope[0], scope[1], scope[2], scope[3]
	return a, nil
}

func (a *Authorization) scope() string {
	return strings.Join([]string{a.Date, a.Region, a.Service, terminator}, "/")
}

// Verify checks that a signed r with the given payload hash (the hex SHA-256 of
// the body, or a literal such as UNSIGNED-PAYLOAD) was signed with secret at a
// time within MaxSkew of now. S3 signs its URI path as sent; other services
// encode it once more.
func Verify(r *http.Request, a *Authorization, secret, payloadHash string, s3 bool, now time.Time) error {
	if a.Signature == "" || len(a.SignedHeaders) == 0 || !slices.Contains(a.SignedHeaders, "host") {
		return errors.Join(ErrMalformed, errors.New("the Authorization header must sign at least the host header"))
	}

	signedAt, err := requestTime(r, a)
	if err != nil {
		return err
	}

	if skew := now.Sub(signedAt); skew > MaxSkew || skew < -MaxSkew {
		return ErrExpired
	}

	canonical, err := canonicalRequest(r, a.SignedHeaders, payloadHash, s3)
	if err != nil {
		return err
	}

	digest := sha256.Sum256([]byte(canonical))
	stringToSign := strings.Join(
		[]string{Algorithm, signedAt.Format(timeFormat), a.scope(), hex.EncodeToString(digest[:])},
		"\n",
	)
	key := signingKey(secret, a.Date, a.Region, a.Service)
	want := hex.EncodeToString(hmacSHA256(key, stringToSign))
	if !hmac.Equal([]byte(want), []byte(a.Signature)) {
		return ErrMismatch
	}

	return nil
}

// requestTime reads the signing time from X-Amz-Date, or Date when it is
// absent. The header used must be signed and must match the credential date.
func requestTime(r *http.Request, a *Authorization) (time.Time, error) {
	name, value := "x-amz-date", r.Header.Get("X-Amz-Date")
	layout := timeFormat
	if value == "" {
		name, value, layout = "date", r.Header.Get("Date"), http.TimeFormat
	}

	signedAt, err := time.Parse(layout, value)
	if err != nil || !slices.Contains(a.SignedHeaders, name) {
		return time.Time{}, errors.Join(
			ErrMalformed,
			errors.New("the request must sign a valid X-Amz-Date or Date header"),
		)
	}

	if signedAt.UTC().Format(dateFormat) != a.Date {
		return time.Time{}, errors.Join(ErrMalformed, errors.New("the credential date must match the request date"))
	}

	return signedAt.UTC(), nil
}

// canonicalRequest builds the SigV4 canonical request string.
func canonicalRequest(r *http.Request, signedHeaders []string, payloadHash string, s3 bool) (string, error) {
	path := r.URL.EscapedPath()
	if path == "" {
		path = "/"
	}

	if !s3 {
		path = escape(path, false)
	}

	query, err := canonicalQuery(r.URL.RawQuery)
	if err != nil {
		return "", err
	}

	var headers strings.Builder
	for _, name := range signedHeaders {
		headers.WriteString(name)
		headers.WriteByte(':')
		headers.WriteString(headerValue(r, name))
		headers.WriteByte('\n')
	}

	return strings.Join(
		[]string{r.Method, path, query, headers.String(), strings.Join(signedHeaders, ";"), payloadHash},
		"\n",
	), nil
}

// canonicalQuery re-encodes every parameter strictly and sorts them.
func canonicalQuery(raw string) (string, error) {
	values, err := url.ParseQuery(raw)
	if err != nil {
		return "", errors.Join(ErrMismatch, err)
	}

	pairs := make([]string, 0, len(values))
	for k, vs := range values {
		for _, v := range vs {
			pairs = append(pairs, escape(k, true)+"="+escape(v, true))
		}
	}

	sort.Strings(pairs)
	return strings.Join(pairs, "&"), nil
}

// headerValue joins a header's values with commas after trimming them and
// collapsing inner runs of spaces.
func headerValue(r *http.Request, name string) string {
	values := r.Header.Values(name)
	switch {
	case name == "host":
		values = []string{r.Host}
	case name == "content-length" && len(values) == 0 && r.ContentLength >= 0:
		values = []string{strconv.FormatInt(r.ContentLength, 10)}
	}

	cleaned := make([]string, len(values))
	for i, v := range values {
		cleaned[i] = strings.Join(strings.Fields(v), " ")
	}

	return strings.Join(cleaned, ",")
}

// escape percent-encodes every byte outside the RFC 3986 unreserved set,
// keeping "/" unless encodeSlash is set.
func escape(s string, encodeSlash bool) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		unreserved := c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
			c == '-' || c == '_' || c == '.' || c == '~'
		if unreserved || (c == '/' && !encodeSlash) {
			b.WriteByte(c)
			continue
		}

		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&0x0f])
	}

	return b.String()
}

// signingKey derives the SigV4 signing key for a credential scope.
func signingKey(secret, date, region, service string) []byte {
	key := hmacSHA256([]byte("AWS4"+secret), date)
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, service)
	return hmacSHA256(key, terminator)
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}
