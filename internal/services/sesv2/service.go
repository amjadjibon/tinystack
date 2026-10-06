// Package sesv2 implements Amazon SES (API v2) without HTTP dependencies.
// Resources belong to an account and region:
//
//	<scope>/sesv2/identity/<lowercased email address or domain>
//	<scope>/sesv2/template/<name>
//	<scope>/sesv2/configuration-set/<name>  (with its event destinations)
//	<scope>/sesv2/account/sending           (hourly send counts for GetAccount)
//
// Identities are verified as soon as they are created. TinyStack never sends
// email: SendEmail and SendBulkEmail record each message in the delivery.Log
// and, after their transactions commit, publish its events (send, delivery,
// and, for the mailbox simulator's addresses, bounce and complaint, or a
// template's rendering failure) to the destinations of its configuration set
// through a catalog.Caller.
package sesv2

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/sesv2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/delivery"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	service          = "sesv2"
	kindIdentity     = "identity"
	kindTemplate     = "template"
	kindConfigSet    = "configuration-set"
	kindAccount      = "account"
	identityEmail    = "EMAIL_ADDRESS"
	identityDomain   = "DOMAIN"
	statusSuccess    = "SUCCESS"
	statusNotStarted = "NOT_STARTED"
)

var _ api.API = (*Service)(nil)

type Service struct {
	store      storage.Store
	deliveries *delivery.Log
	// caller reaches the SNS topics, event buses, and delivery streams that
	// configuration sets publish events to.
	caller *catalog.Caller
	now    func() time.Time
}

// New returns the service, which records the email it would send in
// deliveries.
func New(store storage.Store, deliveries *delivery.Log) *Service {
	return &Service{store: store, deliveries: deliveries, now: time.Now}
}

// UseCaller lets configuration sets publish events. Call it before serving
// requests.
func (s *Service) UseCaller(caller *catalog.Caller) { s.caller = caller }

func badRequest(format string, args ...any) error {
	return awserror.New("BadRequestException", fmt.Sprintf(format, args...))
}

func notFound(format string, args ...any) error {
	return awserror.New("NotFoundException", fmt.Sprintf(format, args...))
}

func alreadyExists(format string, args ...any) error {
	return awserror.New("AlreadyExistsException", fmt.Sprintf(format, args...))
}

// namePattern is what template and configuration set names may be.
var namePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func checkName(kind string, name *string) error {
	if !namePattern.MatchString(common.Value(name)) {
		return badRequest("The %s name must be 1 to 64 letters, digits, underscores, or dashes.", kind)
	}

	return nil
}

func arn(scope request.Scope, kind, name string) string {
	return "arn:aws:ses:" + scope.Region + ":" + scope.AccountID + ":" + kind + "/" + name
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
	m, err := tagging.Map(tags, func(t api.Tag) (key, value *string) { return t.Key, t.Value },
		badRequest("Every tag needs a Key and a Value."),
		func(key string) error { return badRequest("The tag key %s appears more than once.", key) })
	if err != nil {
		return nil, err
	}

	if err = tagging.CheckSet(m); err != nil {
		return nil, badRequest("%s", err.Error())
	}

	return m, nil
}

func tagList(tags map[string]string) []api.Tag {
	return tagging.List(tags, func(k, v string) api.Tag { return api.Tag{Key: new(k), Value: new(v)} })
}

// page returns the items after the one token names: size of them, def when
// size is unset, at most limit.
func page[T any](items []T, id func(T) string, token *string, size *int64, def, limit int) (
	selected []T, next *string, err error,
) {
	n := int(common.Value(size))
	switch {
	case size == nil:
		n = def
	case n < 1 || n > limit:
		return nil, nil, badRequest("PageSize must be between 1 and %d.", limit)
	}

	return common.Page(items, id, token, n, badRequest("Invalid NextToken."))
}

// identityKey is the key of an identity's record; identities are
// case-insensitive.
func identityKey(scope request.Scope, name string) string {
	return scope.Key(service, kindIdentity, strings.ToLower(name))
}
