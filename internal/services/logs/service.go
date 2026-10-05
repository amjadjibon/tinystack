// Package logs implements the CloudWatch Logs subset TinyStack serves, without
// HTTP dependencies. Log groups belong to an account and region; each group,
// stream, and event is its own record:
//
//	<scope>/logs/group/<group>
//	<scope>/logs/stream/<encoded group>/<stream>
//	<scope>/logs/event/<encoded group>/<encoded stream>/<position>
//
// Group names are stored as they are, so groups list in name order, while the
// stream and event keys encode the names that contain them, so a group's
// streams and a stream's events are each one key prefix. An event's position
// is its timestamp and the stream's ingestion sequence number, zero-padded, so
// a stream's events list in time order.
package logs

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/logs"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	classStandard = "STANDARD"
	maxDescribe   = 50
)

var _ api.API = (*Service)(nil)

type Service struct {
	store storage.Store
	// caller delivers to subscription filters' destinations; see UseCaller.
	caller *catalog.Caller
	now    func() time.Time
}

func New(store storage.Store) *Service {
	return &Service{store: store, now: time.Now}
}

// group is a log group record.
type group struct {
	Tags map[string]string `json:",omitempty"`
	// Retention is the retention policy in days; nil keeps events forever.
	Retention     *int64 `json:",omitempty"`
	Name          string
	Class         string
	Subscriptions []subscription `json:",omitempty"`
	MetricFilters []metricFilter `json:",omitempty"`
	Created       int64          // Unix milliseconds
	StoredBytes   int64
}

var (
	groupName  = regexp.MustCompile(`^[.\-_/#A-Za-z0-9]{1,512}$`)
	retentions = []int64{
		1, 3, 5, 7, 14, 30, 60, 90, 120, 150, 180, 365, 400, 545, 731, 1096, 1827, 2192, 2557, 2922, 3288, 3653,
	}
)

func value[T any](p *T) (v T) { return common.Value(p) }

func invalid(format string, args ...any) error {
	return awserror.New("InvalidParameterException", fmt.Sprintf(format, args...))
}

func notFound(message string) error {
	return awserror.New("ResourceNotFoundException", message)
}

func groupNotFound() error { return notFound("The specified log group does not exist.") }

func identity(ctx context.Context) (request.Scope, error) {
	m, ok := request.FromContext(ctx)
	if !ok || m.AccountID == "" || m.Region == "" {
		return request.Scope{}, fmt.Errorf("missing request identity")
	}

	return m.Scope, nil
}

func prefix(scope request.Scope, kind string) string {
	return scope.AccountID + "/" + scope.Region + "/logs/" + kind + "/"
}

// encode makes a name safe inside a storage key: it never contains '/'.
func encode(name string) string { return base64.RawURLEncoding.EncodeToString([]byte(name)) }

func groupKey(scope request.Scope, name string) string { return prefix(scope, "group") + name }

func groupARN(scope request.Scope, name string) string {
	return "arn:aws:logs:" + scope.Region + ":" + scope.AccountID + ":log-group:" + name
}

func checkGroupName(name *string) error {
	if !groupName.MatchString(value(name)) {
		return invalid("Log group name must be 1 to 512 characters: letters, digits, and . - _ / #")
	}

	return nil
}

func read[T any](tx storage.ReadTx, key string) (v *T, found bool, err error) {
	return storage.Read[T](tx, key)
}

func write(tx storage.WriteTx, key string, v any) error { return storage.Write(tx, key, v) }

func loadGroup(tx storage.ReadTx, scope request.Scope, name string) (*group, error) {
	g, found, err := read[group](tx, groupKey(scope, name))
	if err != nil {
		return nil, err
	}

	if !found {
		return nil, groupNotFound()
	}

	return g, nil
}

// resolveGroup returns the group a request names by logGroupName or by
// logGroupIdentifier (a name or ARN), which are mutually exclusive.
func resolveGroup(scope request.Scope, name, identifier *string) (string, error) {
	switch {
	case name != nil && identifier != nil:
		return "", invalid("LogGroup name and LogGroup identifier are mutually exclusive parameters.")
	case name != nil:
		if err := checkGroupName(name); err != nil {
			return "", err
		}

		return *name, nil
	case identifier == nil:
		return "", invalid("Either logGroupName or logGroupIdentifier must be specified.")
	}

	id := strings.TrimSuffix(*identifier, ":*")
	if rest, ok := strings.CutPrefix(id, "arn:"); ok {
		parts := strings.SplitN(rest, ":", 6)
		if len(parts) != 6 || parts[1] != "logs" || parts[4] != "log-group" {
			return "", invalid("Invalid log group ARN: %s", *identifier)
		}

		if parts[2] != scope.Region || parts[3] != scope.AccountID {
			return "", groupNotFound()
		}

		id = parts[5]
	}

	if err := checkGroupName(&id); err != nil {
		return "", err
	}

	return id, nil
}

// view loads the group named by name or identifier and passes it to fn.
func (s *Service) view(
	ctx context.Context,
	name, identifier *string,
	fn func(tx storage.ReadTx, scope request.Scope, g *group) error,
) error {
	scope, err := identity(ctx)
	if err != nil {
		return err
	}

	resolved, err := resolveGroup(scope, name, identifier)
	if err != nil {
		return err
	}

	return s.store.View(ctx, func(tx storage.ReadTx) error {
		g, loadErr := loadGroup(tx, scope, resolved)
		if loadErr != nil {
			return loadErr
		}

		return fn(tx, scope, g)
	})
}

// change loads the group named name, applies fn, and saves it.
func (s *Service) change(ctx context.Context, name *string, fn func(g *group) error) error {
	scope, err := identity(ctx)
	if err != nil {
		return err
	}

	if guardErr := checkGroupName(name); guardErr != nil {
		return guardErr
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		g, loadErr := loadGroup(tx, scope, *name)
		if loadErr != nil {
			return loadErr
		}

		if fnErr := fn(g); fnErr != nil {
			return fnErr
		}

		return write(tx, groupKey(scope, g.Name), g)
	})
}

// tagError reports a tagging.Check failure with the Logs error code.
func tagError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, tagging.ErrLimit):
		return awserror.New("TooManyTagsException", err.Error())
	default:
		return invalid("Invalid tags: %v", err)
	}
}

func (s *Service) CreateLogGroup(
	ctx context.Context,
	in *api.CreateLogGroupRequest,
) (*api.CreateLogGroupOutput, error) {
	scope, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	if guardErr := checkGroupName(in.LogGroupName); guardErr != nil {
		return nil, guardErr
	}

	class := value(in.LogGroupClass)
	switch class {
	case "":
		class = classStandard
	case classStandard, "INFREQUENT_ACCESS":
	default:
		return nil, invalid("logGroupClass must be STANDARD or INFREQUENT_ACCESS")
	}

	if guardErr := tagError(tagging.Check(in.Tags)); guardErr != nil {
		return nil, guardErr
	}

	g := &group{Name: *in.LogGroupName, Class: class, Created: s.now().UnixMilli(), Tags: in.Tags}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		return create(tx, scope, g)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateLogGroupOutput{}, nil
}

func create(tx storage.WriteTx, scope request.Scope, g *group) error {
	if _, exists := tx.Get(groupKey(scope, g.Name)); exists {
		return awserror.New("ResourceAlreadyExistsException", "The specified log group already exists")
	}

	return write(tx, groupKey(scope, g.Name), g)
}

// DeleteLogGroup deletes the group with its streams and events.
func (s *Service) DeleteLogGroup(
	ctx context.Context,
	in *api.DeleteLogGroupRequest,
) (*api.DeleteLogGroupOutput, error) {
	scope, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	if guardErr := checkGroupName(in.LogGroupName); guardErr != nil {
		return nil, guardErr
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, loadErr := loadGroup(tx, scope, *in.LogGroupName); loadErr != nil {
			return loadErr
		}

		tx.Delete(groupKey(scope, *in.LogGroupName))
		for _, kind := range []string{"stream", "event"} {
			for _, key := range tx.Keys(prefix(scope, kind) + encode(*in.LogGroupName) + "/") {
				tx.Delete(key)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteLogGroupOutput{}, nil
}

func (g *group) describe(scope request.Scope) api.LogGroup {
	arn := groupARN(scope, g.Name)
	return api.LogGroup{
		LogGroupName:      new(g.Name),
		CreationTime:      new(g.Created),
		RetentionInDays:   g.Retention,
		MetricFilterCount: new(int64(0)),
		Arn:               new(arn + ":*"),
		StoredBytes:       new(g.StoredBytes),
		LogGroupClass:     new(g.Class),
		LogGroupArn:       &arn,
	}
}

// DescribeLogGroups lists groups in name order, filtered by name prefix or by
// a case-insensitive substring pattern.
func (s *Service) DescribeLogGroups(
	ctx context.Context,
	in *api.DescribeLogGroupsRequest,
) (*api.DescribeLogGroupsResponse, error) {
	scope, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	if in.LogGroupNamePrefix != nil && in.LogGroupNamePattern != nil {
		return nil, invalid("LogGroup name prefix and LogGroup name pattern are mutually exclusive parameters.")
	}

	limit, err := describeLimit(in.Limit)
	if err != nil {
		return nil, err
	}

	after, err := decodeToken(in.NextToken)
	if err != nil {
		return nil, err
	}

	pattern := strings.ToLower(value(in.LogGroupNamePattern))
	out := &api.DescribeLogGroupsResponse{LogGroups: []api.LogGroup{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys(groupKey(scope, value(in.LogGroupNamePrefix))) {
			name := strings.TrimPrefix(key, prefix(scope, "group"))
			if name <= after || !strings.Contains(strings.ToLower(name), pattern) {
				continue
			}

			if len(out.LogGroups) == limit {
				out.NextToken = encodeToken(value(out.LogGroups[limit-1].LogGroupName))
				break
			}

			g, _, readErr := read[group](tx, key)
			if readErr != nil {
				return readErr
			}

			out.LogGroups = append(out.LogGroups, g.describe(scope))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func describeLimit(limit *int64) (int, error) {
	if limit == nil {
		return maxDescribe, nil
	}

	if *limit < 1 || *limit > maxDescribe {
		return 0, invalid("limit must be between 1 and %d", maxDescribe)
	}

	return int(*limit), nil
}

// encodeToken and decodeToken make a pagination token of the last key a page
// returned.
func encodeToken(last string) *string { return new(encode(last)) }

func decodeToken(token *string) (string, error) {
	if token == nil {
		return "", nil
	}

	last, err := base64.RawURLEncoding.DecodeString(*token)
	if err != nil || len(last) == 0 {
		return "", invalid("The specified nextToken is invalid.")
	}

	return string(last), nil
}

func (s *Service) PutRetentionPolicy(
	ctx context.Context,
	in *api.PutRetentionPolicyRequest,
) (*api.PutRetentionPolicyOutput, error) {
	if !slices.Contains(retentions, value(in.RetentionInDays)) {
		return nil, invalid("retentionInDays must be one of %v", retentions)
	}

	err := s.change(ctx, in.LogGroupName, func(g *group) error {
		g.Retention = in.RetentionInDays
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.PutRetentionPolicyOutput{}, nil
}

func (s *Service) DeleteRetentionPolicy(
	ctx context.Context,
	in *api.DeleteRetentionPolicyRequest,
) (*api.DeleteRetentionPolicyOutput, error) {
	err := s.change(ctx, in.LogGroupName, func(g *group) error {
		g.Retention = nil
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteRetentionPolicyOutput{}, nil
}

// taggedGroup returns the name of the log group a tagging request's ARN
// names, with or without its ":*" suffix.
func taggedGroup(scope request.Scope, arn *string) (*string, error) {
	name, err := resolveGroup(scope, nil, arn)
	if err != nil {
		return nil, err
	}

	if !strings.HasPrefix(value(arn), "arn:") {
		return nil, invalid("resourceArn must be a log group ARN")
	}

	return &name, nil
}

func (s *Service) retag(ctx context.Context, arn *string, fn func(map[string]string) (map[string]string, error)) error {
	scope, err := identity(ctx)
	if err != nil {
		return err
	}

	name, err := taggedGroup(scope, arn)
	if err != nil {
		return err
	}

	return s.change(ctx, name, func(g *group) error {
		tags, tagErr := fn(g.Tags)
		g.Tags = tags
		return tagErr
	})
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceOutput, error) {
	err := s.retag(ctx, in.ResourceArn, func(tags map[string]string) (map[string]string, error) {
		merged, mergeErr := tagging.Merge(tags, in.Tags)
		return merged, tagError(mergeErr)
	})
	if err != nil {
		return nil, err
	}

	return &api.TagResourceOutput{}, nil
}

func (s *Service) UntagResource(
	ctx context.Context,
	in *api.UntagResourceRequest,
) (*api.UntagResourceOutput, error) {
	err := s.retag(ctx, in.ResourceArn, func(tags map[string]string) (map[string]string, error) {
		return tagging.Remove(tags, in.TagKeys), nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagResourceOutput{}, nil
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResponse, error) {
	scope, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	name, err := taggedGroup(scope, in.ResourceArn)
	if err != nil {
		return nil, err
	}

	out := &api.ListTagsForResourceResponse{Tags: map[string]string{}}
	err = s.view(ctx, name, nil, func(_ storage.ReadTx, _ request.Scope, g *group) error {
		maps.Copy(out.Tags, g.Tags)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
