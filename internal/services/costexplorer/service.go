// Package costexplorer stores account-wide anomaly and cost category definitions.
// Definitions are records only: no cost data, evaluation or notification delivery.
package costexplorer

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/costexplorer"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName      = "costexplorer"
	monitorKind      = "anomalymonitor"
	subscriptionKind = "anomalysubscription"
	categoryKind     = "costcategory"
)

var _ api.API = (*Service)(nil)

type Service struct{ store storage.Store }

func New(store storage.Store) *Service { return &Service{store: store} }

type record struct {
	Tags         map[string]string
	Monitor      *api.AnomalyMonitor
	Subscription *api.AnomalySubscription
	Category     *api.CostCategory
	ARN          string
}

func (r *record) name() string {
	switch {
	case r.Monitor != nil:
		return common.Value(r.Monitor.MonitorName)
	case r.Subscription != nil:
		return common.Value(r.Subscription.SubscriptionName)
	default:
		return common.Value(r.Category.Name)
	}
}

var arnPattern = regexp.MustCompile(`^arn:aws[-a-z0-9]*:[a-z0-9]+:[-a-z0-9]*:[0-9]{12}:[-a-zA-Z0-9/:_]+$`)

func resourceKey(scope request.Scope, value *string, kind string) (string, error) {
	if value == nil || utf8.RuneCountInString(*value) < 20 || utf8.RuneCountInString(*value) > 2048 ||
		!arnPattern.MatchString(*value) {
		return "", awserror.Validation("Resource ARN is invalid.")
	}

	parts := strings.SplitN(*value, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[1] != "aws" || parts[2] != "ce" || parts[3] != "" {
		return "", awserror.Validation("Resource ARN is not a Cost Explorer ARN.")
	}

	got, id, ok := strings.Cut(parts[5], "/")
	if !ok || id == "" || strings.Contains(id, "/") ||
		!slices.Contains([]string{monitorKind, subscriptionKind, categoryKind}, got) ||
		kind != "" && got != kind {
		return "", awserror.Validation("Resource ARN has an invalid resource type or identifier.")
	}

	if parts[4] != scope.AccountID {
		return "", notFound(kind)
	}

	return scope.Key(serviceName, got, id), nil
}

func notFound(kind string) error {
	code := "ResourceNotFoundException"
	switch kind {
	case monitorKind:
		code = "UnknownMonitorException"
	case subscriptionKind:
		code = "UnknownSubscriptionException"
	}

	return awserror.New(code, "The requested Cost Explorer resource does not exist.")
}

func (s *Service) create(ctx context.Context, kind string, r *record, tags []api.ResourceTag) error {
	scope, err := request.GlobalScope(ctx)
	if err != nil {
		return err
	}

	r.Tags, err = tagMap(tags)
	if err != nil {
		return err
	}

	id := common.RandomID(32, "0123456789abcdef")
	id = id[:8] + "-" + id[8:12] + "-" + id[12:16] + "-" + id[16:20] + "-" + id[20:]
	r.ARN = fmt.Sprintf("arn:aws:ce::%s:%s/%s", scope.AccountID, kind, id)
	switch kind {
	case monitorKind:
		r.Monitor.MonitorArn = new(r.ARN)
	case subscriptionKind:
		r.Subscription.SubscriptionArn = new(r.ARN)
		r.Subscription.AccountId = new(scope.AccountID)
	case categoryKind:
		r.Category.CostCategoryArn = new(r.ARN)
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		if kind == subscriptionKind {
			if e := validateSubscription(r.Subscription, scope, tx); e != nil {
				return e
			}
		}

		if kind == categoryKind {
			rs, e := storage.Records[record](tx, scope.Prefix(serviceName, kind))
			if e != nil {
				return e
			}

			for _, existing := range rs {
				if existing.name() == r.name() {
					return awserror.Validation("A cost category with this name already exists.")
				}
			}
		}

		return storage.Write(tx, scope.Key(serviceName, kind, id), r)
	})
}

func (s *Service) edit(
	ctx context.Context,
	value *string,
	kind string,
	change func(*record, storage.WriteTx) error,
) error {
	scope, err := request.GlobalScope(ctx)
	if err != nil {
		return err
	}

	k, err := resourceKey(scope, value, kind)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, found, e := storage.Read[record](tx, k)
		if e != nil {
			return e
		}

		if !found {
			return notFound(kind)
		}

		if e := change(r, tx); e != nil {
			return e
		}

		return storage.Write(tx, k, r)
	})
}

func (s *Service) remove(ctx context.Context, value *string, kind string) error {
	scope, err := request.GlobalScope(ctx)
	if err != nil {
		return err
	}

	k, err := resourceKey(scope, value, kind)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, found := tx.Get(k); !found {
			return notFound(kind)
		}

		tx.Delete(k)
		return nil
	})
}

func (s *Service) list(
	ctx context.Context,
	kind string,
	token *string,
	size *int64,
	filter func(*record) bool,
	references []string,
) ([]*record, *string, error) {
	scope, err := request.GlobalScope(ctx)
	if err != nil {
		return nil, nil, err
	}

	limit := 100
	if size != nil {
		if *size < 0 || kind == categoryKind && (*size < 1 || *size > 100) {
			return nil, nil, awserror.Validation("Invalid MaxResults.")
		}

		if *size > 0 {
			limit = int(*size)
		}
	}

	var records []*record
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, ref := range references {
			k, e := resourceKey(scope, new(ref), kind)
			if e != nil {
				return e
			}

			if _, found := tx.Get(k); !found {
				return notFound(kind)
			}
		}

		all, e := storage.Records[record](tx, scope.Prefix(serviceName, kind))
		if e != nil {
			return e
		}

		for _, r := range all {
			if filter(r) {
				records = append(records, r)
			}
		}

		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	return common.Page(
		records,
		func(r *record) string { return r.ARN },
		token,
		limit,
		awserror.New("InvalidNextTokenException", "Invalid pagination token."),
	)
}

func today() string { return time.Now().UTC().Format("2006-01-02") }

// read resolves an ARN within the caller's account without writing state.
func (s *Service) read(ctx context.Context, value *string, kind string) (*record, error) {
	scope, err := request.GlobalScope(ctx)
	if err != nil {
		return nil, err
	}

	k, err := resourceKey(scope, value, kind)
	if err != nil {
		return nil, err
	}

	var r *record
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var found bool
		var e error
		r, found, e = storage.Read[record](tx, k)
		if e != nil {
			return e
		}

		if !found {
			return notFound(kind)
		}

		return nil
	})
	return r, err
}
