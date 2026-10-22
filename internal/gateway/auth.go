package gateway

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/protocol/rest"
	"github.com/amjadjibon/tinystack/internal/aws/sigv4"
	"github.com/amjadjibon/tinystack/internal/config"
)

// authFailure indexes the per-protocol error codes in authCodes.
type authFailure int

const (
	missingToken authFailure = iota
	unknownKey
	badSignature
	expired
	expiredToken
)

const (
	requestExpired        = "RequestExpired"
	accessDenied          = "AccessDenied"
	signatureDoesNotMatch = "SignatureDoesNotMatch"
	opPostObject          = "PostObject"
)

// authCodes are the error codes each protocol family's services use for
// authentication failures.
var authCodes = map[string][5]string{
	catalog.RESTXML: {
		accessDenied, "InvalidAccessKeyId", signatureDoesNotMatch, "RequestTimeTooSkewed", "ExpiredToken",
	},
	catalog.AWSQuery: {
		"MissingAuthenticationToken", "InvalidClientTokenId", signatureDoesNotMatch, requestExpired, "ExpiredToken",
	},
	catalog.EC2Query: {
		"MissingAuthenticationToken", "AuthFailure", "AuthFailure", requestExpired, requestExpired,
	},
}

// jsonAuthCodes serve AWS JSON and REST JSON services.
var jsonAuthCodes = [5]string{
	"MissingAuthenticationTokenException", "UnrecognizedClientException", "InvalidSignatureException",
	"InvalidSignatureException", "ExpiredTokenException",
}

var authMessages = [5]string{
	"Request is missing Authentication Token",
	"The security token included in the request is invalid.",
	"The request signature we calculated does not match the signature you provided. " +
		"Check your AWS Secret Access Key and signing method.",
	"Signature expired: the signing time must be within 15 minutes of the server time.",
	"The security token included in the request is expired",
}

func authError(protocol string, failure authFailure) error {
	codes, ok := authCodes[protocol]
	if !ok {
		codes = jsonAuthCodes
	}

	status := http.StatusForbidden
	if failure == expiredToken {
		status = http.StatusBadRequest
	}

	return &awserror.Error{Code: codes[failure], Message: authMessages[failure], StatusCode: status}
}

// signature parses the request's SigV4 signature from its Authorization
// header or, for a presigned URL, its query. signed is false for an unsigned
// request.
func signature(r *http.Request) (a *sigv4.Authorization, signed bool, err error) {
	if header := r.Header.Get("Authorization"); header != "" {
		if a, err = sigv4.Parse(header); err != nil {
			return nil, true, awserror.New("IncompleteSignatureException", "Invalid Authorization header: "+err.Error())
		}

		return a, true, nil
	}

	query := r.URL.Query()
	if !sigv4.Presigned(query) {
		return nil, false, nil
	}

	// AWS IoT device SDKs presign MQTT over WebSocket URLs without
	// X-Amz-Expires; IoT Core accepts them for the usual signing window.
	var missingExpires time.Duration
	if r.URL.Path == mqttPath && upgrade(r) {
		missingExpires = mqttExpires
	}

	if a, err = sigv4.ParseQuery(query, missingExpires); err != nil {
		return nil, true, awserror.New("AuthorizationQueryParametersError", "Invalid presigned URL: "+err.Error())
	}

	return a, true, nil
}

// authenticate verifies the request's SigV4 signature in identity.mode verify.
// It runs once the service is known, so S3 can use its path and payload rules.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request, c *call) error {
	if s.cfg.Identity.Mode != config.IdentityVerify {
		return nil
	}

	if c.meta.Operation == opPostObject {
		return nil
	}

	protocol := c.meta.Protocol
	// identify has already parsed the signature.
	a, signed, err := signature(r)
	if err != nil {
		return err
	}

	if !signed {
		// Operations such as Cognito's sign-in are called unsigned; a signed
		// call to one is still verified.
		if c.service != nil && c.service.Operations[c.meta.Operation].Anonymous {
			return nil
		}

		return authError(protocol, missingToken)
	}

	secret, ok := s.cfg.Identity.SecretKeys[a.AccessKey]
	if !ok {
		// An issued key must come with its token: a session's, or none for an
		// IAM user's key.
		token := r.Header.Get("X-Amz-Security-Token")
		if a.Expires != 0 {
			token = cmp.Or(r.URL.Query().Get(sigv4.QuerySecurityToken), token)
		}

		if c.credentials == nil || subtle.ConstantTimeCompare([]byte(token), []byte(c.credentials.Token)) != 1 {
			return authError(protocol, unknownKey)
		}

		secret = c.credentials.Secret
	}

	s3 := c.service.SigningName == "s3"
	payloadHash := "UNSIGNED-PAYLOAD"
	// A presigned S3 URL leaves the payload unsigned.
	if !s3 || a.Expires == 0 {
		if payloadHash, err = s.payloadHash(w, r, c, s3); err != nil {
			return err
		}
	}

	err = sigv4.Verify(r, a, secret, payloadHash, s3, time.Now())
	switch {
	case errors.Is(err, sigv4.ErrExpired) && a.Expires != 0 && protocol == catalog.RESTXML:
		return &awserror.Error{Code: accessDenied, Message: "Request has expired", StatusCode: http.StatusForbidden}
	case errors.Is(err, sigv4.ErrExpired):
		return authError(protocol, expired)
	case errors.Is(err, sigv4.ErrMalformed):
		return awserror.New("IncompleteSignatureException", err.Error())
	case err != nil:
		return authError(protocol, badSignature)
	case c.credentials != nil && !c.credentials.Expires.IsZero() && !time.Now().Before(c.credentials.Expires):
		return authError(protocol, expiredToken)
	}

	return nil
}

type tempFileCloser struct {
	*os.File
	path string
	once sync.Once
}

func newTempFileCloser(f *os.File) *tempFileCloser {
	return &tempFileCloser{File: f, path: f.Name()}
}

func (t *tempFileCloser) Close() error {
	var err error
	t.once.Do(func() {
		err = t.File.Close()
		_ = os.Remove(t.path)
	})
	return err
}

func (s *Server) spoolAndHash(
	w http.ResponseWriter,
	r *http.Request,
	c *call,
	limit int64,
	declared string,
) (res string, retErr error) {
	origBody := r.Body
	if origBody != nil {
		defer func() {
			_ = origBody.Close()
		}()
	}

	if err := r.Context().Err(); err != nil {
		return "", err
	}

	f, err := os.CreateTemp("", "tinystack-upload-*")
	if err != nil {
		return "", err
	}

	closer := newTempFileCloser(f)
	if c != nil {
		c.addCleanup(closer.Close)
	}
	defer func() {
		if retErr != nil {
			_ = closer.Close()
		}
	}()

	h := sha256.New()
	mw := io.MultiWriter(f, h)
	reader := http.MaxBytesReader(w, origBody, limit)
	buf := make([]byte, 32*1024)
	var totalRead int64

	for {
		if err := r.Context().Err(); err != nil {
			retErr = err
			return "", retErr
		}

		n, rerr := reader.Read(buf)
		if err := r.Context().Err(); err != nil {
			return "", err
		}
		if n > 0 {
			totalRead += int64(n)
			if _, werr := mw.Write(buf[:n]); werr != nil {
				retErr = werr
				return "", retErr
			}
		}

		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			if err := r.Context().Err(); err != nil {
				retErr = err
				return "", retErr
			}
			if errors.Is(rerr, context.Canceled) || errors.Is(rerr, context.DeadlineExceeded) {
				retErr = rerr
				return "", retErr
			}
			if _, tooLarge := errors.AsType[*http.MaxBytesError](rerr); tooLarge {
				retErr = rest.TooLarge()
				return "", retErr
			}
			if errors.Is(rerr, io.ErrUnexpectedEOF) {
				retErr = awserror.New(
					"IncompleteBody",
					"You did not provide the number of bytes specified by the Content-Length HTTP header.",
				)
				return "", retErr
			}
			retErr = awserror.Validation("Cannot read request body: %v", rerr)
			return "", retErr
		}
	}

	if r.ContentLength >= 0 && totalRead != r.ContentLength {
		retErr = awserror.New(
			"IncompleteBody",
			"You did not provide the number of bytes specified by the Content-Length HTTP header.",
		)
		return "", retErr
	}

	computed := hex.EncodeToString(h.Sum(nil))
	if declared != "" && declared != computed {
		retErr = awserror.New(
			"XAmzContentSHA256Mismatch",
			"The provided 'x-amz-content-sha256' header does not match what was computed.",
		)
		return "", retErr
	}

	if _, seekErr := f.Seek(0, io.SeekStart); seekErr != nil {
		retErr = seekErr
		return "", retErr
	}

	r.Body = closer
	return computed, nil
}

// payloadHash returns the payload hash the client signed. S3 may sign an
// unsigned-payload literal; everything else signs the SHA-256 of the body,
// which must match any X-Amz-Content-Sha256 the client sent.
func (s *Server) payloadHash(w http.ResponseWriter, r *http.Request, c *call, s3 bool) (string, error) {
	declared := r.Header.Get("X-Amz-Content-Sha256")
	if s3 && (declared == "UNSIGNED-PAYLOAD" || declared == "STREAMING-UNSIGNED-PAYLOAD-TRAILER") {
		return declared, nil
	}

	if s3 && strings.HasPrefix(declared, "STREAMING-") {
		return "", rest.NotImplemented("TinyStack does not verify signed streaming (aws-chunked) payloads.")
	}

	if s3 && (c.meta.Operation == opPutObject || c.meta.Operation == opUploadPart) {
		return s.spoolAndHash(w, r, c, s.restBodyLimit(c), declared)
	}

	body, err := s.readBody(w, r, c)
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(body)
	computed := hex.EncodeToString(sum[:])
	if declared == "" || declared == computed {
		return computed, nil
	}

	if s3 {
		return "", awserror.New(
			"XAmzContentSHA256Mismatch",
			"The provided 'x-amz-content-sha256' header does not match what was computed.",
		)
	}

	return "", authError(c.meta.Protocol, badSignature)
}

// readBody returns the request body and leaves an equivalent one in place for
// decoding. AWS Query bodies were already read to classify the request.
func (s *Server) readBody(w http.ResponseWriter, r *http.Request, c *call) ([]byte, error) {
	if formProtocol(c.meta.Protocol) {
		return c.body, nil
	}

	limit := int64(maxBody)
	isREST := restProtocol(c.service) == c.meta.Protocol
	if isREST {
		limit = s.restBodyLimit(c)
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge && isREST {
			return nil, rest.TooLarge()
		}

		return nil, awserror.Validation("Cannot read request body: %v", err)
	}

	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}
