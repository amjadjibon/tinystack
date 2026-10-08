// Package cloudcontrol manages live S3 buckets and SQS queues through the
// CloudFormation resource handlers. It stores only request history, scoped to
// the caller's account and Region:
//
//	<scope>/cloudcontrol/request/<token>  (progress, fingerprint and creation time)
//	<scope>/cloudcontrol/client/<hash>    (client token to request token)
//	<scope>/cloudcontrol/lock/<type>/<name> (a write in progress)
//
// Writes execute synchronously outside storage transactions. History is visible
// for seven days and client tokens are idempotent for 36 hours. On restart,
// interrupted writes become FAILED: a downstream change may already have been
// applied, so callers must read the live resource before submitting a new write.
package cloudcontrol

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/cloudcontrol"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/cloudformation"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	service               = "cloudcontrol"
	errorResourceNotFound = "ResourceNotFoundException"
	kindRequest           = "request"
	kindClient            = "client"
	kindLock              = "lock"
	actionCreate          = "CREATE"
	actionUpdate          = "UPDATE"
	actionDelete          = "DELETE"
	statusPending         = "IN_PROGRESS"
	statusSuccess         = "SUCCESS"
	statusFailed          = "FAILED"
	historyTTL            = 7 * 24 * time.Hour
	clientTTL             = 36 * time.Hour
	maxDocument           = 262144
)

var (
	_            api.API = (*Service)(nil)
	typePattern          = regexp.MustCompile(`^[A-Za-z0-9]{2,64}::[A-Za-z0-9]{2,64}::[A-Za-z0-9]{2,64}$`)
	tokenPattern         = regexp.MustCompile(`^[-A-Za-z0-9+/=]{1,128}$`)
)

type Service struct {
	store    storage.Store
	provider *cloudformation.ResourceProvider
	now      func() time.Time
}

type operation struct {
	Created     time.Time
	Event       api.ProgressEvent
	Fingerprint string
	Lock        string
}

// New recovers the request journal before any requests are served.
func New(store storage.Store) (*Service, error) {
	s := &Service{store: store, now: time.Now}
	err := store.Update(context.Background(), func(tx storage.WriteTx) error {
		for _, key := range tx.Keys("") {
			_, _, rest, ok := storage.Scoped(key, service)
			if !ok {
				continue
			}

			if !strings.HasPrefix(rest, kindRequest+"/") {
				continue
			}

			op, _, err := storage.Read[operation](tx, key)
			if err != nil {
				return err
			}

			if s.now().Sub(op.Created) >= historyTTL {
				tx.Delete(key)
				tx.Delete(op.Lock)
				continue
			}

			if common.Value(op.Event.OperationStatus) == statusPending {
				op.Event.OperationStatus = new(statusFailed)
				op.Event.ErrorCode = new("GeneralServiceException")
				op.Event.StatusMessage = new("TinyStack restarted during this write; read the resource before retrying")
				op.Event.EventTime = new(float64(s.now().UnixMilli()) / 1000)
				tx.Delete(op.Lock)
				if err := storage.Write(tx, key, op); err != nil {
					return err
				}
			}
		}

		// Remove client indexes whose history has already expired.
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, service)
			if !ok || !strings.HasPrefix(rest, kindClient+"/") {
				continue
			}

			token, _, err := storage.Read[string](tx, key)
			if err != nil {
				return err
			}

			scope := request.Scope{AccountID: account, Region: region}
			if _, found := tx.Get(scope.Key(service, kindRequest, *token)); !found {
				tx.Delete(key)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return s, nil
}

func (s *Service) UseCaller(c *catalog.Caller) { s.provider = cloudformation.NewResourceProvider(c) }

func invalid(format string, args ...any) error {
	return awserror.New("InvalidRequestException", fmt.Sprintf(format, args...))
}

func (s *Service) checkType(name *string) error {
	if !typePattern.MatchString(common.Value(name)) {
		return invalid("TypeName must be a resource type such as AWS::S3::Bucket")
	}

	return s.provider.CheckType(*name)
}

func identifier(value *string) error {
	if n := len(common.Value(value)); n == 0 || n > 1024 {
		return invalid("Identifier must contain between 1 and 1024 bytes")
	}

	return nil
}

func document(raw *string) (map[string]any, error) {
	if n := len(common.Value(raw)); n == 0 || n > maxDocument {
		return nil, invalid("DesiredState must contain between 1 and %d bytes", maxDocument)
	}

	var model map[string]any
	if err := json.Unmarshal([]byte(*raw), &model); err != nil || model == nil {
		return nil, invalid("DesiredState must be a JSON object")
	}

	return model, nil
}

func hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// write claims the client token and resource before invoking any service. A
// retry sees the saved event even when the original caller has disconnected.
func (s *Service) write(
	ctx context.Context, action, name, id, payload string, token *string, model map[string]any,
	apply func() (string, map[string]any, error),
) (*api.ProgressEvent, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if token != nil && !tokenPattern.MatchString(*token) {
		return nil, invalid("ClientToken must contain 1 to 128 token characters")
	}

	n, err := s.provider.Name(ctx, name, id, model)
	if err != nil {
		return nil, err
	}

	data, err := json.Marshal([]string{action, name, id, payload})
	if err != nil {
		return nil, err
	}

	op := &operation{
		Created: s.now(), Fingerprint: hash(data),
		Lock: m.Key(service, kindLock, name+"/"+base64.RawURLEncoding.EncodeToString([]byte(n))),
		Event: api.ProgressEvent{
			TypeName: new(name), Operation: new(action), OperationStatus: new(statusPending),
			RequestToken: new(uuid.NewV4().String()), EventTime: new(float64(s.now().UnixMilli()) / 1000),
		},
	}
	if id != "" {
		op.Event.Identifier = new(id)
	}

	var replay *operation
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if token != nil {
			key := m.Key(service, kindClient, hash([]byte(*token)))
			previous, found, readErr := storage.Read[string](tx, key)
			if readErr != nil {
				return readErr
			}

			if found {
				old, exists, readErr := storage.Read[operation](tx, m.Key(service, kindRequest, *previous))
				if readErr != nil {
					return readErr
				}

				if exists && s.now().Sub(old.Created) < clientTTL {
					if old.Fingerprint != op.Fingerprint {
						return awserror.New(
							"ClientTokenConflictException",
							"ClientToken was used for a different request",
						)
					}

					replay = old
					return nil
				}
			}
		}

		if _, found := tx.Get(op.Lock); found {
			return awserror.New("ConcurrentOperationException", "A write is already in progress for this resource")
		}

		if token != nil {
			if writeErr := storage.Write(
				tx,
				m.Key(service, kindClient, hash([]byte(*token))),
				op.Event.RequestToken,
			); writeErr != nil {
				return writeErr
			}
		}

		if writeErr := storage.Write(tx, op.Lock, op.Event.RequestToken); writeErr != nil {
			return writeErr
		}

		return storage.Write(tx, m.Key(service, kindRequest, *op.Event.RequestToken), op)
	})
	if err != nil {
		return nil, err
	}

	if replay != nil {
		return &replay.Event, nil
	}

	id, result, applyErr := apply()
	op.Event.EventTime = new(float64(s.now().UnixMilli()) / 1000)
	if id != "" {
		op.Event.Identifier = new(id)
	}

	if applyErr != nil {
		e := mappedError(applyErr)
		op.Event.OperationStatus, op.Event.ErrorCode, op.Event.StatusMessage = new(
			statusFailed,
		), new(
			handlerCode(e.Code),
		), new(
			e.Message,
		)
	} else {
		op.Event.OperationStatus = new(statusSuccess)
		if result != nil {
			data, marshalErr := json.Marshal(result)
			if marshalErr != nil {
				return nil, marshalErr
			}

			op.Event.ResourceModel = new(string(data))
		}
	}

	err = s.store.Update(context.WithoutCancel(ctx), func(tx storage.WriteTx) error {
		tx.Delete(op.Lock)
		return storage.Write(tx, m.Key(service, kindRequest, *op.Event.RequestToken), op)
	})
	return &op.Event, err
}

// mappedError translates owning-service failures into resource handler codes.
func mappedError(err error) *awserror.Error {
	e := awserror.From(err)
	switch e.Code {
	case errorResourceNotFound, "AlreadyExistsException", "InvalidRequestException", "NotUpdatableException",
		"TypeNotFoundException":
	case "NoSuchBucket", "QueueDoesNotExist":
		e.Code = errorResourceNotFound
	case "BucketAlreadyExists", "BucketAlreadyOwnedByYou", "QueueNameExists":
		e.Code = "AlreadyExistsException"
	case "BucketNotEmpty":
		e.Code = "ResourceConflictException"
	case "ValidationException", "InvalidParameterValue", "InvalidAttributeValue", "InvalidAttributeName",
		"InvalidBucketName", "InvalidArgument", "InvalidAddress", "InvalidTag", "TooManyTags":
		e.Code = "InvalidRequestException"
	default:
		e.Code = "GeneralServiceException"
	}

	return e
}

func handlerCode(code string) string {
	switch code {
	case errorResourceNotFound:
		return "NotFound"
	case "GeneralServiceException":
		return code
	default:
		return strings.TrimSuffix(code, "Exception")
	}
}

func pageSize(value *int64) (int, error) {
	n := common.Value(common.OrElse(value, new(int64(20))))
	if n < 1 || n > 100 {
		return 0, invalid("MaxResults must be between 1 and 100")
	}

	return int(n), nil
}

func (s *Service) GetResourceRequestStatus(
	ctx context.Context, in *api.GetResourceRequestStatusInput,
) (*api.GetResourceRequestStatusOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if !tokenPattern.MatchString(common.Value(in.RequestToken)) {
		return nil, invalid("RequestToken must contain 1 to 128 token characters")
	}

	var op *operation
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var found bool
		var readErr error
		op, found, readErr = storage.Read[operation](tx, m.Key(service, kindRequest, *in.RequestToken))
		if readErr != nil {
			return readErr
		}

		if !found || s.now().Sub(op.Created) >= historyTTL {
			return awserror.New("RequestTokenNotFoundException", "RequestToken was not found in the caller's scope")
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.GetResourceRequestStatusOutput{ProgressEvent: &op.Event}, nil
}

func (s *Service) ListResourceRequests(
	ctx context.Context, in *api.ListResourceRequestsInput,
) (*api.ListResourceRequestsOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	limit, err := pageSize(in.MaxResults)
	if err != nil {
		return nil, err
	}

	f := in.ResourceRequestStatusFilter
	if f != nil {
		for _, action := range f.Operations {
			if !slices.Contains([]string{actionCreate, actionUpdate, actionDelete}, action) {
				return nil, invalid("Unknown operation filter %s", action)
			}
		}

		for _, status := range f.OperationStatuses {
			if !slices.Contains([]string{
				"PENDING", statusPending, statusSuccess, statusFailed,
				"CANCEL_IN_PROGRESS", "CANCEL_COMPLETE",
			}, status) {
				return nil, invalid("Unknown status filter %s", status)
			}
		}
	}

	var events []api.ProgressEvent
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		ops, readErr := storage.Records[operation](tx, m.Prefix(service, kindRequest))
		if readErr != nil {
			return readErr
		}

		for _, op := range ops {
			if s.now().Sub(op.Created) >= historyTTL {
				continue
			}

			if f != nil && (len(f.Operations) > 0 && !slices.Contains(f.Operations, common.Value(op.Event.Operation)) ||
				len(f.OperationStatuses) > 0 && !slices.Contains(f.OperationStatuses, common.Value(op.Event.OperationStatus))) {
				continue
			}

			events = append(events, op.Event)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	items, next, err := common.Page(events, func(e api.ProgressEvent) string { return common.Value(e.RequestToken) },
		in.NextToken, limit, invalid("Invalid NextToken"))
	return &api.ListResourceRequestsOutput{ResourceRequestStatusSummaries: items, NextToken: next}, err
}
