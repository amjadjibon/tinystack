// Package acm implements AWS Certificate Manager without HTTP dependencies.
// Certificates belong to an account and region:
//
//	<scope>/acm/cert/<certificate ID>        (details, tags, and the PEM certificate, chain, and key)
//	<scope>/acm/token/<idempotency token>    (the certificate a token requested, for an hour)
//
// Requested certificates are real X.509 certificates that TinyStack's own
// certificate authority (one per store, under aws/acm/ca) signs. They are
// issued at once, and still report the DNS records or email addresses their
// validation would use. Only certificates requested as exportable keep their
// private key, which ExportCertificate returns encrypted.
package acm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/acm"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	service   = "acm"
	kindCert  = "cert"
	kindToken = "token"
	caKey     = "aws/" + service + "/ca"
)

var _ api.API = (*Service)(nil)

type Service struct {
	store storage.Store
	now   func() time.Time
	// ca is the certificate authority once it is loaded or created.
	ca   *authority
	caMu sync.Mutex
}

func New(store storage.Store) *Service {
	return &Service{store: store, now: time.Now}
}

// certificate is a certificate's record.
type certificate struct {
	Detail api.CertificateDetail
	Tags   map[string]string
	// Body, Chain, and Key are PEM. Only exportable certificates keep their
	// private key; an imported certificate's key is checked and dropped.
	Body     string
	Chain    string `json:",omitempty"`
	Key      string `json:",omitempty"`
	Exported bool   `json:",omitempty"`
}

// token records the certificate an idempotency token requested.
type token struct {
	ID string
	// Request is the request's JSON, which a retry must repeat.
	Request string
	Expires float64
}

func identity(ctx context.Context) (request.Metadata, error) {
	m, ok := request.FromContext(ctx)
	if !ok || m.AccountID == "" || m.Region == "" {
		return request.Metadata{}, fmt.Errorf("missing request identity")
	}

	return m, nil
}

func key(scope request.Scope, kind, name string) string {
	return scope.AccountID + "/" + scope.Region + "/" + service + "/" + kind + "/" + name
}

func certARN(scope request.Scope, id string) string {
	return "arn:aws:acm:" + scope.Region + ":" + scope.AccountID + ":certificate/" + id
}

// certID returns the certificate ID an ARN names in scope.
func certID(scope request.Scope, arn *string) (string, error) {
	if arn == nil || *arn == "" {
		return "", awserror.Validation("The CertificateArn is required.")
	}

	p := "arn:aws:acm:" + scope.Region + ":" + scope.AccountID + ":certificate/"
	id, ok := strings.CutPrefix(*arn, p)
	switch {
	case ok && id != "" && !strings.Contains(id, "/"):
		return id, nil
	case !strings.HasPrefix(*arn, "arn:aws:acm:"):
		return "", awserror.New("InvalidArnException", "The ARN "+*arn+" is not a valid certificate ARN.")
	}

	return "", notFound(*arn)
}

func notFound(arn string) error {
	return awserror.New("ResourceNotFoundException", "Could not find certificate "+arn+".")
}

// load reads the certificate arn names.
func load(tx storage.ReadTx, scope request.Scope, arn *string) (*certificate, error) {
	id, err := certID(scope, arn)
	if err != nil {
		return nil, err
	}

	c, found, err := read[certificate](tx, key(scope, kindCert, id))
	if err == nil && !found {
		err = notFound(*arn)
	}

	return c, err
}

func value[T any](p *T) (v T) {
	if p != nil {
		return *p
	}

	return v
}

func read[T any](tx storage.ReadTx, k string) (v *T, found bool, err error) {
	data, ok := tx.Get(k)
	if !ok {
		return nil, false, nil
	}

	v = new(T)
	if err = json.Unmarshal(data, v); err != nil {
		return nil, false, fmt.Errorf("decode %s: %w", k, err)
	}

	return v, true, nil
}

func write(tx storage.WriteTx, k string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}

	tx.Put(k, data)
	return nil
}

func unix(t time.Time) float64 { return float64(t.UnixMilli()) / 1000 }

func invalidParameter(format string, args ...any) error {
	return awserror.New("InvalidParameterException", fmt.Sprintf(format, args...))
}
