// Package iot implements the AWS IoT Core control plane without HTTP
// dependencies. Resources belong to an account and region, and keep their
// names as given (IoT names are case-sensitive):
//
//	<scope>/iot/thing/<name>                    (attributes, version, groups, principals)
//	<scope>/iot/thing-type/<name>
//	<scope>/iot/thing-group/<name>              (static groups, with their parent)
//	<scope>/iot/certificate/<ID>                (device certificates by SHA-256)
//	<scope>/iot/ca/<ID>                         (registered CA certificates)
//	<scope>/iot/policy/<name>                   (every version, and the targets)
//	<scope>/iot/principal/<path-escaped ID>     (what a certificate, Cognito identity, or thing group is attached to)
//	<scope>/iot/registration-code/code
//	<account>/iot/endpoint/prefix               (the account's endpoint prefix)
//	aws/iot/endpoint/<prefix>                   (the account of an endpoint prefix)
//
// Certificates that CreateKeysAndCertificate and CreateCertificateFromCsr
// issue are signed by TinyStack's IoT certificate authority, created on first
// use under the partition-wide aws/iot/ca.
package iot

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"regexp"
	"sync"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/iot"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/delivery"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/services/pki"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	service          = "iot"
	kindThing        = "thing"
	kindThingType    = "thing-type"
	kindThingGroup   = "thing-group"
	kindCertificate  = "certificate"
	kindCA           = "ca"
	kindPolicy       = "policy"
	kindPrincipal    = "principal"
	kindRegistration = "registration-code"
	kindEndpoint     = "endpoint"
	caKey            = "aws/" + service + "/ca"
	caCommonName     = "TinyStack IoT Certificate Authority"
	// defaultPage and maxPage bound every list operation's page size.
	defaultPage = 25
	maxPage     = 250
)

var _ api.API = (*Service)(nil)

type Service struct {
	store  storage.Store
	ca     *pki.Lazy
	now    func() time.Time
	logger *slog.Logger
	bg     *background.Group
	// caller runs rule actions; deliveries records them.
	caller     *catalog.Caller
	deliveries *delivery.Log
	// authorizer checks that each action's role may be assumed and may make its call, and that a Lambda action's
	// function allows IoT; see UseServiceAuthorizer.
	authorizer catalog.ServiceAuthorizer
	// roleCredentials issues role alias credentials.
	roleCredentials RoleCredentials
	// brokers are the running MQTT brokers by scope, guarded by brokersMu;
	// nil once stopped.
	brokers map[request.Scope]*broker
	// serverCerts are the MQTT over TLS listener's certificates by name,
	// guarded by serverCertsMu.
	serverCerts map[string]*tls.Certificate
	// defaults is the account and Region of connections that carry no
	// identity, and verify is whether IoT policies are evaluated
	// (identity.mode verify).
	defaults      request.Scope
	brokersMu     sync.Mutex
	serverCertsMu sync.Mutex
	verify        bool
}

// New returns the service. Its MQTT brokers start on first use and run until
// Stop; deliveries records what topic rules' actions send.
func New(store storage.Store, deliveries *delivery.Log, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}

	return &Service{
		store:       store,
		ca:          pki.NewLazy(store, caKey, caCommonName),
		now:         time.Now,
		logger:      logger,
		deliveries:  deliveries,
		bg:          background.New(),
		brokers:     map[request.Scope]*broker{},
		serverCerts: map[string]*tls.Certificate{},
	}
}

// UseCaller lets topic rules run their actions. Call it before serving
// requests.
func (s *Service) UseCaller(caller *catalog.Caller) { s.caller = caller }

// UseServiceAuthorizer makes every action require its role to trust iot.amazonaws.com and to allow the call, or
// for a Lambda action the function's policy to allow it. Call it before serving requests.
func (s *Service) UseServiceAuthorizer(authorizer catalog.ServiceAuthorizer) {
	s.authorizer = authorizer
}

// UseIdentity sets the account and Region of connections without an
// identity, and whether IoT policies are evaluated, as identity.mode verify
// asks. Call it before serving requests.
func (s *Service) UseIdentity(defaults request.Scope, verify bool) {
	s.defaults, s.verify = defaults, verify
}

// Reset forgets the certificate authority and the certificates it signed for
// the MQTT over TLS listener, which a cleared store no longer holds. Connected
// MQTT clients and the brokers' in-memory session state stay.
func (s *Service) Reset(context.Context) {
	s.ca.Reset()
	s.serverCertsMu.Lock()
	defer s.serverCertsMu.Unlock()
	clear(s.serverCerts)
}

// Stop disconnects every MQTT client and stops the brokers and listeners.
func (s *Service) Stop() {
	s.closeBrokers()
	s.bg.Stop()
}

func invalid(format string, args ...any) error {
	return awserror.New("InvalidRequestException", fmt.Sprintf(format, args...))
}

func notFound(format string, args ...any) error {
	return awserror.New("ResourceNotFoundException", fmt.Sprintf(format, args...))
}

func alreadyExists(format string, args ...any) error {
	return awserror.New("ResourceAlreadyExistsException", fmt.Sprintf(format, args...))
}

func conflict(format string, args ...any) error {
	return awserror.New("DeleteConflictException", fmt.Sprintf(format, args...))
}

func versionConflict(what string) error {
	return awserror.New("VersionConflictException", "The version for "+what+" does not match the expected version.")
}

func certificateState(format string, args ...any) error {
	return awserror.New("CertificateStateException", fmt.Sprintf(format, args...))
}

// thingNamePattern is what thing, thing type, and thing group names may be.
var thingNamePattern = regexp.MustCompile(`^[a-zA-Z0-9:_-]{1,128}$`)

func checkName(kind string, name *string) error {
	if !thingNamePattern.MatchString(common.Value(name)) {
		return invalid("The %s name must be 1 to 128 letters, digits, colons, underscores, or hyphens.", kind)
	}

	return nil
}

// arn names a resource of one of the ARN kinds: thing, thingtype,
// thinggroup, cert, cacert, or policy.
func arn(scope request.Scope, kind, name string) string {
	return "arn:aws:iot:" + scope.Region + ":" + scope.AccountID + ":" + kind + "/" + name
}

// view runs fn in a read transaction in the caller's account and region.
func (s *Service) view(ctx context.Context, fn func(storage.ReadTx, request.Scope) error) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	return s.store.View(ctx, func(tx storage.ReadTx) error { return fn(tx, scope) })
}

// update runs fn in a write transaction in the caller's account and region.
func (s *Service) update(ctx context.Context, fn func(storage.WriteTx, request.Scope) error) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error { return fn(tx, scope) })
}

// tagMap converts a request's tags, which must be valid and not reserved.
func tagMap(tags []api.Tag) (map[string]string, error) {
	m, err := tagging.Map(tags, func(t api.Tag) (key, value *string) { return t.Key, new(common.Value(t.Value)) },
		invalid("Every tag needs a Key."),
		func(key string) error { return invalid("The tag key %s appears more than once.", key) })
	if err != nil {
		return nil, err
	}

	if err := tagging.CheckSet(m); err != nil {
		return nil, invalid("%s", err.Error())
	}

	return m, nil
}

func tagList(tags map[string]string) []api.Tag {
	return tagging.List(tags, func(k, v string) api.Tag { return api.Tag{Key: new(k), Value: new(v)} })
}

// page returns the items after the one token names: size of them, the
// default page size when size is unset.
func page[T any](items []T, id func(T) string, token *string, size *int64) (selected []T, next *string, err error) {
	n := int(common.Value(size))
	switch {
	case size == nil:
		n = defaultPage
	case n < 1 || n > maxPage:
		return nil, nil, invalid("The page size must be between 1 and %d.", maxPage)
	}

	return common.Page(items, id, token, n, invalid("The pagination token is invalid."))
}

// exists reports whether key holds a record.
func exists(tx storage.ReadTx, key string) bool {
	_, ok := tx.Get(key)
	return ok
}
