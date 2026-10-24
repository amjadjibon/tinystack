// Package mediaconnect stores MediaConnect records. No media is transported.
package mediaconnect

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/mediaconnect"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/services/ec2"
	"github.com/amjadjibon/tinystack/internal/services/iam"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName       = "mediaconnect"
	indexPrefix       = "aws/mediaconnect/index/"
	entitlementPrefix = "aws/mediaconnect/entitlement/"
)

// Service owns flow records and the durable lifecycle queue.
type Service struct {
	store   storage.Store
	network *ec2.Service
	roles   *iam.Service
	caller  *catalog.Caller
	workers *background.Group
}

// New constructs the records service; UseCaller starts the worker.
func New(store storage.Store, network *ec2.Service, roles *iam.Service) *Service {
	return &Service{store: store, network: network, roles: roles, workers: background.New()}
}

// UseCaller connects Secrets Manager validation and starts lifecycle processing.
func (s *Service) UseCaller(c *catalog.Caller) {
	s.caller = c
	s.workers.Every(100*time.Millisecond, s.advance)
}

// Stop joins the worker before storage closes.
func (s *Service) Stop() { s.workers.Stop() }

type record struct {
	Tags       map[string]map[string]string
	Resume     string
	Flow       api.Flow
	InlineVpcs []string
}

type index struct {
	Scope request.Scope
	ARN   string
}

func invalid(format string, args ...any) error {
	return awserror.New("BadRequestException", fmt.Sprintf(format, args...))
}

func missing() error {
	return awserror.New("NotFoundException", "MediaConnect resource does not exist")
}

func resourceARN(scope request.Scope, kind, name string) string {
	return fmt.Sprintf("arn:aws:mediaconnect:%s:%s:%s:%s:%s", scope.Region, scope.AccountID,
		kind, common.RandomID(12, "0123456789abcdef"), name)
}

func recordKey(scope request.Scope, arn string) (string, error) {
	prefix := fmt.Sprintf("arn:aws:mediaconnect:%s:%s:flow:", scope.Region, scope.AccountID)
	if !strings.HasPrefix(arn, prefix) {
		return "", missing()
	}

	return scope.Key(serviceName, "flow", arn), nil
}

func indexKey(scope request.Scope, arn string) string {
	return fmt.Sprintf("%s%s/%s/%s", indexPrefix, scope.AccountID, scope.Region, arn)
}

func read(tx storage.ReadTx, scope request.Scope, arn string) (*record, error) {
	key, err := recordKey(scope, arn)
	if err != nil {
		return nil, err
	}

	r, ok, err := storage.Read[record](tx, key)
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, missing()
	}

	return r, nil
}

func save(tx storage.WriteTx, scope request.Scope, r *record) error {
	arn := common.Value(r.Flow.FlowArn)
	key, err := recordKey(scope, arn)
	if err != nil {
		return err
	}

	// Entitlements remain partition-wide for cross-account source lookup.
	old, found, err := storage.Read[record](tx, key)
	if err != nil {
		return err
	}

	if found {
		for _, e := range old.Flow.Entitlements {
			tx.Delete(entitlementPrefix + common.Value(e.EntitlementArn))
		}
	}

	for _, e := range r.Flow.Entitlements {
		if checkErr := storage.Write(tx, entitlementPrefix+common.Value(e.EntitlementArn),
			entitlementIndex{Scope: scope, FlowARN: arn, Entitlement: e}); checkErr != nil {
			return checkErr
		}
	}

	return storage.Write(tx, key, r)
}

type entitlementIndex struct {
	Scope       request.Scope
	FlowARN     string
	Entitlement api.Entitlement
}

func (s *Service) mutate(
	ctx context.Context,
	arn *string,
	fn func(storage.WriteTx, request.Scope, *record) error,
) (*record, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var r *record
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var txErr error
		r, txErr = read(tx, scope, common.Value(arn))
		if txErr != nil {
			return txErr
		}

		switch common.Value(r.Flow.Status) {
		case "STARTING", "STOPPING", "UPDATING", "DELETING":
			return invalid("Flow is transitioning")
		}

		if checkErr := fn(tx, scope, r); checkErr != nil {
			return checkErr
		}

		return save(tx, scope, r)
	})
	return r, err
}

func queue(tx storage.WriteTx, scope request.Scope, r *record, state string) error {
	r.Flow.Status = new(state)
	arn := common.Value(r.Flow.FlowArn)
	return storage.Write(tx, indexKey(scope, arn), index{Scope: scope, ARN: arn})
}

func (s *Service) advance(ctx context.Context) {
	var entries []*index
	if checkErr := s.store.View(ctx, func(tx storage.ReadTx) error {
		var checkErr error
		entries, checkErr = storage.Records[index](tx, indexPrefix)
		return checkErr
	}); checkErr != nil {
		return
	}

	for _, e := range entries {
		_ = s.store.Update(ctx, func(tx storage.WriteTx) error {
			key, txErr := recordKey(e.Scope, e.ARN)
			if txErr != nil {
				return txErr
			}

			r, ok, txErr := storage.Read[record](tx, key)
			if txErr != nil {
				return txErr
			}

			if !ok {
				tx.Delete(indexKey(e.Scope, e.ARN))
				return nil
			}

			switch common.Value(r.Flow.Status) {
			case "STARTING":
				r.Flow.Status = new("ACTIVE")
			case "STOPPING":
				r.Flow.Status = new("STANDBY")
			case "UPDATING":
				r.Flow.Status = new(r.Resume)
			case "DELETING":
				for _, ent := range r.Flow.Entitlements {
					tx.Delete(entitlementPrefix + common.Value(ent.EntitlementArn))
				}

				tx.Delete(key)
				tx.Delete(indexKey(e.Scope, e.ARN))
				return nil
			}

			if checkErr := save(tx, e.Scope, r); checkErr != nil {
				return checkErr
			}

			tx.Delete(indexKey(e.Scope, e.ARN))
			return nil
		})
	}
}

var _ api.API = (*Service)(nil)
