// Package budgets stores account-wide budget, notification and subscriber definitions.
// No spend is calculated, notifications delivered, or actions executed.
package budgets

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/budgets"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const serviceName = "budgets"

var _ api.API = (*Service)(nil)

type Service struct{ store storage.Store }

func New(store storage.Store) *Service { return &Service{store: store} }

type record struct {
	Budget        api.Budget
	Tags          map[string]string
	Notifications []api.NotificationWithSubscribers
}

func invalid(message string) error { return awserror.New("InvalidParameterException", message) }
func missing() error {
	return awserror.New("NotFoundException", "The budget, notification or subscriber does not exist.")
}

func duplicate() error { return awserror.New("DuplicateRecordException", "The record already exists.") }

func scopeFor(ctx context.Context, account *string) (request.Scope, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return request.Scope{}, err
	}

	if account == nil || !accountPattern.MatchString(*account) {
		return request.Scope{}, invalid("AccountId must contain 12 digits.")
	}

	if *account != m.AccountID {
		return request.Scope{}, awserror.New("AccessDeniedException", "AccountId must match the request account.")
	}

	return request.Scope{AccountID: m.AccountID}, nil
}

func key(scope request.Scope, name string) string {
	return scope.Key(serviceName, "budget", base64.RawURLEncoding.EncodeToString([]byte(name)))
}

func arn(account, name string) string {
	return fmt.Sprintf("arn:aws:budgets::%s:budget/%s", account, name)
}

func read(tx storage.ReadTx, k string) (record, error) {
	r, ok, err := storage.Read[record](tx, k)
	if err != nil {
		return record{}, err
	}

	if !ok {
		return record{}, missing()
	}

	return *r, nil
}

func (s *Service) change(ctx context.Context, account, name *string, change func(*record) error) error {
	scope, err := scopeFor(ctx, account)
	if err != nil {
		return err
	}

	if checkErr := checkName(name); checkErr != nil {
		return checkErr
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		k := key(scope, *name)
		r, err := read(tx, k)
		if err != nil {
			return err
		}

		if checkErr := change(&r); checkErr != nil {
			return checkErr
		}

		return storage.Write(tx, k, r)
	})
}

func (s *Service) get(ctx context.Context, account, name *string) (record, error) {
	scope, err := scopeFor(ctx, account)
	if err != nil {
		return record{}, err
	}

	if nameErr := checkName(name); nameErr != nil {
		return record{}, nameErr
	}

	var r record
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var readErr error
		r, readErr = read(tx, key(scope, *name))
		return readErr
	})
	return r, err
}

func resource(ctx context.Context, value *string) (account, name *string, err error) {
	m, e := request.Global(ctx)
	if e != nil {
		return nil, nil, e
	}

	prefix := "arn:aws:budgets::" + m.AccountID + ":budget/"
	if value == nil || !strings.HasPrefix(*value, prefix) {
		return nil, nil, invalid("ResourceARN must name a budget in the request account.")
	}

	n := strings.TrimPrefix(*value, prefix)
	return new(m.AccountID), new(n), checkName(new(n))
}

func page[T any](
	items []T,
	id func(T) string,
	token *string,
	requested *int64,
	limit int,
) (selected []T, next *string, err error) {
	size := limit
	if requested != nil {
		if *requested < 1 || *requested > int64(limit) {
			return nil, nil, invalid("MaxResults is outside its modeled range.")
		}

		size = int(*requested)
	}

	return common.Page(items, id, token, size, awserror.New("InvalidNextTokenException", "NextToken is invalid."))
}
