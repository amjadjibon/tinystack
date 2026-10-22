// Package sigv4 parses and verifies AWS Signature Version 4 Authorization
// headers and presigned URLs.
package sigv4

import (
	"cmp"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	// Algorithm is the only signing algorithm TinyStack accepts.
	Algorithm = "AWS4-HMAC-SHA256"
	// MaxSkew is how far a request's signing time may be from the server clock.
	MaxSkew = 15 * time.Minute
	// MaxExpires is the longest a presigned URL may be valid.
	MaxExpires = 7 * 24 * time.Hour
	timeFormat = "20060102T150405Z"
	dateFormat = "20060102"
	terminator = "aws4_request"
	// Query parameters of a presigned URL.
	queryAlgorithm     = "X-Amz-Algorithm"
	queryCredential    = "X-Amz-Credential" // #nosec G101 -- a parameter name, not a credential.
	queryDate          = "X-Amz-Date"
	queryExpires       = "X-Amz-Expires"
	querySignedHeaders = "X-Amz-SignedHeaders"
	querySignature     = "X-Amz-Signature"
	// QuerySecurityToken carries a presigned URL's session token.
	QuerySecurityToken = "X-Amz-Security-Token" // #nosec G101 -- a parameter name, not a credential.
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

// Authorization is a parsed SigV4 Authorization header or presigned URL.
type Authorization struct {
	AccessKey, Date, Region, Service, Signature string
	SignedHeaders                               []string
	// Expires is how long a presigned URL is valid; it is zero for a header.
	Expires time.Duration
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

	err := a.setCredential(credential)
	return a, err
}

// setCredential parses a credential scope,
// "<access key>/<date>/<region>/<service>/aws4_request".
func (a *Authorization) setCredential(credential string) error {
	scope := strings.Split(credential, "/")
	if len(scope) != 5 || scope[0] == "" || len(scope[1]) != 8 || scope[2] == "" || scope[3] == "" ||
		scope[4] != terminator {
		return errors.New("malformed credential scope")
	}

	a.AccessKey, a.Date, a.Region, a.Service = scope[0], scope[1], scope[2], scope[3]
	return nil
}

// Presigned reports whether query holds a presigned URL's signature.
func Presigned(query url.Values) bool {
	return query.Has(queryAlgorithm) || query.Has(queryCredential)
}

// QueryParameter reports whether name is a signature parameter of a presigned
// URL rather than a parameter of the operation.
func QueryParameter(name string) bool {
	switch name {
	case queryAlgorithm, queryCredential, queryDate, queryExpires, querySignedHeaders, querySignature,
		QuerySecurityToken:
		return true
	default:
		return false
	}
}

// ParseQuery parses the signature of a presigned URL. Signature may be empty;
// Verify rejects it.
func ParseQuery(query url.Values, missingExpires time.Duration) (*Authorization, error) {
	if query.Get(queryAlgorithm) != Algorithm {
		return nil, errors.New("expected " + queryAlgorithm + "=" + Algorithm)
	}

	expires := missingExpires
	if raw := query.Get(queryExpires); raw != "" || missingExpires == 0 {
		seconds, err := strconv.Atoi(raw)
		if err != nil || seconds < 1 || time.Duration(seconds)*time.Second > MaxExpires {
			return nil, errors.New(queryExpires + " must be between 1 and 604800 seconds")
		}

		expires = time.Duration(seconds) * time.Second
	}

	a := &Authorization{Signature: query.Get(querySignature), Expires: expires}
	if headers := query.Get(querySignedHeaders); headers != "" {
		a.SignedHeaders = strings.Split(headers, ";")
	}

	err := a.setCredential(query.Get(queryCredential))
	return a, err
}

func (a *Authorization) scope() string {
	return strings.Join([]string{a.Date, a.Region, a.Service, terminator}, "/")
}

// Verify checks that a signed r with the given payload hash (the hex SHA-256 of
// the body, or a literal such as UNSIGNED-PAYLOAD) was signed with secret. A
// header signature must be from within MaxSkew of now; a presigned URL is
// valid from its signing time until it expires. S3 signs its URI path as sent;
// other services encode it once more.
func Verify(r *http.Request, a *Authorization, secret, payloadHash string, s3 bool, now time.Time) error {
	if a.Signature == "" || len(a.SignedHeaders) == 0 || !slices.Contains(a.SignedHeaders, "host") {
		return errors.Join(ErrMalformed, errors.New("the signature must sign at least the host header"))
	}

	query := r.URL.RawQuery
	var signedAt time.Time
	var err error
	if a.Expires == 0 {
		if signedAt, err = requestTime(r, a); err != nil {
			return err
		}

		if skew := now.Sub(signedAt); skew > MaxSkew || skew < -MaxSkew {
			return ErrExpired
		}
	} else {
		values := r.URL.Query()
		if signedAt, err = time.Parse(timeFormat, values.Get(queryDate)); err != nil ||
			signedAt.Format(dateFormat) != a.Date {
			return errors.Join(ErrMalformed, errors.New("X-Amz-Date must be the credential date's signing time"))
		}

		if now.Before(signedAt.Add(-MaxSkew)) || now.After(signedAt.Add(a.Expires)) {
			return ErrExpired
		}

		// The signature signs every other parameter.
		values.Del(querySignature)
		query = values.Encode()
	}

	canonical, err := canonicalRequest(r, query, a.SignedHeaders, payloadHash, s3)
	if err != nil {
		return err
	}

	digest := sha256.Sum256([]byte(canonical))
	stringToSign := strings.Join(
		[]string{Algorithm, signedAt.Format(timeFormat), a.scope(), hex.EncodeToString(digest[:])},
		"\n",
	)
	key := SigningKey(secret, a.Date, a.Region, a.Service)
	want := hex.EncodeToString(HMACSHA256(key, stringToSign))
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

// canonicalRequest builds the SigV4 canonical request string of r with the
// given raw query.
func canonicalRequest(
	r *http.Request,
	rawQuery string,
	signedHeaders []string,
	payloadHash string,
	s3 bool,
) (string, error) {
	path := r.URL.EscapedPath()
	if path == "" {
		path = "/"
	}

	if !s3 {
		path = escape(path, false)
	}

	query, err := canonicalQuery(rawQuery)
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

	// Parameters sort by encoded name, then value, so "a" precedes "a-b"
	// even though "-" sorts before "=".
	type pair struct{ key, value string }
	pairs := make([]pair, 0, len(values))
	for k, vs := range values {
		for _, v := range vs {
			pairs = append(pairs, pair{escape(k, true), escape(v, true)})
		}
	}

	slices.SortFunc(pairs, func(a, b pair) int {
		return cmp.Or(strings.Compare(a.key, b.key), strings.Compare(a.value, b.value))
	})

	joined := make([]string, len(pairs))
	for i, p := range pairs {
		joined[i] = p.key + "=" + p.value
	}

	return strings.Join(joined, "&"), nil
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

// SigningKey derives the SigV4 signing key for a credential scope.
func SigningKey(secret, date, region, service string) []byte {
	key := HMACSHA256([]byte("AWS4"+secret), date)
	key = HMACSHA256(key, region)
	key = HMACSHA256(key, service)
	return HMACSHA256(key, terminator)
}

// HMACSHA256 computes HMAC-SHA256.
func HMACSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}
