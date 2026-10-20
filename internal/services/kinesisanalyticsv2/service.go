// Package kinesisanalyticsv2 stores Managed Flink control-plane records. No jobs execute.
// DescribeApplication advances deterministic lifecycle transitions; snapshots contain metadata only.
package kinesisanalyticsv2

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	ec2api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/kinesisanalyticsv2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName          = "kinesisanalyticsv2"
	ready                = "READY"
	running              = "RUNNING"
	configurationDefault = "DEFAULT"
)

var (
	_                   api.API = (*Service)(nil)
	snapshotNamePattern         = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,256}$`)
	namePattern                 = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
	rolePattern                 = regexp.MustCompile(
		`^arn:aws(?:-us-gov|-cn)?:iam::[0-9]{12}:role/[A-Za-z0-9_+=,.@/-]+$`,
	)
)

// Network validates references against existing EC2 records in the same transaction.
type Network interface {
	MountSubnet(storage.ReadTx, request.Scope, string) (*ec2api.Subnet, error)
	MountGroups(storage.ReadTx, request.Scope, string, []string) ([]string, error)
}

// Source reads code objects through S3 outside storage transactions.
type Source interface {
	Object(context.Context, string, string, string) ([]byte, error)
}

// LogStreams validates log stream references within the caller's transaction.
type LogStreams interface {
	CheckStreamARN(storage.ReadTx, request.Scope, string) error
}

// Service implements the records-only Managed Flink control plane.
type Service struct {
	store   storage.Store
	network Network
	source  Source
	logs    LogStreams
}

type record struct {
	Detail        *api.ApplicationDetail
	Tags          map[string]string
	Code          *api.ApplicationCodeConfiguration
	PendingStatus string
	Versions      []api.ApplicationDetail
}

// New constructs a service with scoped EC2, S3 and Logs reference validation.
func New(store storage.Store, network Network, source Source, logs LogStreams) *Service {
	return &Service{store: store, network: network, source: source, logs: logs}
}

func invalid(format string, args ...any) error {
	return awserror.New("InvalidArgumentException", fmt.Sprintf(format, args...))
}

func missing() error {
	return awserror.New("ResourceNotFoundException", "The application or snapshot does not exist.")
}

func inUse() error {
	return awserror.New("ResourceInUseException", "The resource is not in a valid state for this operation.")
}

func snapshotPrefix(scope request.Scope, name string) string {
	return scope.Prefix(serviceName, "snapshot") + name + "/"
}

func read(tx storage.ReadTx, scope request.Scope, name *string) (*record, error) {
	if !namePattern.MatchString(common.Value(name)) {
		return nil, invalid("Invalid application name.")
	}

	r, ok, err := storage.Read[record](tx, scope.Key(serviceName, "application", *name))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, missing()
	}

	return r, nil
}

func save(tx storage.WriteTx, scope request.Scope, r *record) error {
	return storage.Write(tx, scope.Key(serviceName, "application", *r.Detail.ApplicationName), r)
}

func checkVersion(r *record, v *int64) error {
	if v == nil || *v != *r.Detail.ApplicationVersionId {
		return awserror.New("ConcurrentModificationException", "CurrentApplicationVersionId does not match.")
	}

	if r.PendingStatus != "" {
		return inUse()
	}

	return nil
}

func transition(r *record, status, target string) {
	r.Detail.ApplicationStatus = new(status)
	r.PendingStatus = target
}

func revise(r *record) {
	r.Detail.ApplicationVersionUpdatedFrom = new(*r.Detail.ApplicationVersionId)
	r.Detail.ApplicationVersionId = new(*r.Detail.ApplicationVersionId + 1)
	r.Detail.LastUpdateTimestamp = common.SecondsMilli(time.Now())
	r.Detail.ApplicationVersionCreateTimestamp = r.Detail.LastUpdateTimestamp
	r.Versions = append(r.Versions, *common.Clone(r.Detail))
}

func (s *Service) change(
	ctx context.Context,
	name *string,
	version *int64,
	fn func(storage.WriteTx, request.Scope, *record) error,
) (*record, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var r *record
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var e error
		r, e = read(tx, scope, name)
		if e != nil {
			return e
		}

		if version != nil {
			if e := checkVersion(r, version); e != nil {
				return e
			}
		}

		if e := fn(tx, scope, r); e != nil {
			return e
		}

		return save(tx, scope, r)
	})
	return r, err
}

func checkRuntime(value *string) error {
	// FLINK-2_x, SQL and Studio are deliberately outside this records-only service's scope.
	if !slices.Contains(
		[]string{
			"FLINK-1_6",
			"FLINK-1_8",
			"FLINK-1_11",
			"FLINK-1_13",
			"FLINK-1_15",
			"FLINK-1_18",
			"FLINK-1_19",
			"FLINK-1_20",
		},
		common.Value(value),
	) {
		return invalid("Only the pinned model's FLINK-1_x runtimes are supported; SQL and Zeppelin are unsupported.")
	}

	return nil
}

func checkRole(role *string) error {
	if !rolePattern.MatchString(common.Value(role)) {
		return invalid("ServiceExecutionRole must be an IAM role ARN.")
	}

	return nil
}

// CreateApplication stores a READY Flink application without executing code.
func (s *Service) CreateApplication(
	ctx context.Context,
	in *api.CreateApplicationRequest,
) (*api.CreateApplicationResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if !namePattern.MatchString(common.Value(in.ApplicationName)) {
		return nil, invalid("Invalid application name.")
	}

	if runtimeErr := checkRuntime(in.RuntimeEnvironment); runtimeErr != nil {
		return nil, runtimeErr
	}

	if roleErr := checkRole(in.ServiceExecutionRole); roleErr != nil {
		return nil, roleErr
	}

	if in.ApplicationMode != nil && *in.ApplicationMode != "STREAMING" {
		return nil, invalid("Only STREAMING application mode is supported.")
	}

	if len(common.Value(in.ApplicationDescription)) > 1024 {
		return nil, invalid("ApplicationDescription exceeds 1024 characters.")
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	var code *api.ApplicationCodeConfiguration
	if in.ApplicationConfiguration != nil {
		code = in.ApplicationConfiguration.ApplicationCodeConfiguration
	}

	codeDescription, err := s.codeDescription(ctx, code)
	if err != nil {
		return nil, err
	}

	// Creation timestamps round-trip through clients that serialize RFC3339 without fractions.
	now := common.Seconds(time.Now())
	r := &record{Tags: tags, Code: code, Detail: &api.ApplicationDetail{
		ApplicationName:        in.ApplicationName,
		ApplicationDescription: in.ApplicationDescription,
		RuntimeEnvironment:     in.RuntimeEnvironment,
		ServiceExecutionRole:   in.ServiceExecutionRole,
		ApplicationARN: new(
			fmt.Sprintf(
				"arn:aws:kinesisanalytics:%s:%s:application/%s",
				scope.Region,
				scope.AccountID,
				*in.ApplicationName,
			),
		),
		ApplicationStatus: new(
			ready,
		),
		ApplicationVersionId:                new(int64(1)),
		CreateTimestamp:                     now,
		LastUpdateTimestamp:                 now,
		ApplicationVersionCreateTimestamp:   now,
		ApplicationMode:                     new("STREAMING"),
		ApplicationConfigurationDescription: &api.ApplicationConfigurationDescription{},
	}}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, ok, e := storage.Read[record](tx, scope.Key(serviceName, "application", *in.ApplicationName)); e != nil {
			return e
		} else if ok {
			return inUse()
		}

		if e := s.configure(tx, scope, r, in.ApplicationConfiguration); e != nil {
			return e
		}

		r.Detail.ApplicationConfigurationDescription.ApplicationCodeConfigurationDescription = codeDescription
		for _, option := range in.CloudWatchLoggingOptions {
			if e := s.addLog(tx, scope, r, &option); e != nil {
				return e
			}
		}

		r.Versions = []api.ApplicationDetail{*common.Clone(r.Detail)}
		return save(tx, scope, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateApplicationResponse{ApplicationDetail: r.Detail}, nil
}

// DescribeApplication returns the current state and completes one pending lifecycle transition.
func (s *Service) DescribeApplication(
	ctx context.Context,
	in *api.DescribeApplicationRequest,
) (*api.DescribeApplicationResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var detail *api.ApplicationDetail
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := read(tx, scope, in.ApplicationName)
		if e != nil {
			return e
		}

		detail = common.Clone(r.Detail)
		if *r.Detail.ApplicationStatus == "DELETING" {
			for _, k := range tx.Keys(snapshotPrefix(scope, *in.ApplicationName)) {
				tx.Delete(k)
			}

			tx.Delete(scope.Key(serviceName, "application", *in.ApplicationName))
			return nil
		}

		if r.PendingStatus != "" {
			r.Detail.ApplicationStatus = new(r.PendingStatus)
			r.PendingStatus = ""
			return save(tx, scope, r)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DescribeApplicationResponse{ApplicationDetail: detail}, nil
}

func listLimit(limit *int64) (int, error) {
	if limit == nil {
		return 50, nil
	}

	if *limit < 1 || *limit > 50 {
		return 0, invalid("Limit must be between 1 and 50.")
	}

	return int(*limit), nil
}

// ListApplications lists applications in the caller's account and Region.
func (s *Service) ListApplications(
	ctx context.Context,
	in *api.ListApplicationsRequest,
) (*api.ListApplicationsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	limit, err := listLimit(in.Limit)
	if err != nil {
		return nil, err
	}

	rows := []api.ApplicationSummary{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		records, e := storage.Records[record](tx, scope.Prefix(serviceName, "application"))
		if e != nil {
			return e
		}

		for _, r := range records {
			d := r.Detail
			rows = append(
				rows,
				api.ApplicationSummary{
					ApplicationName:      d.ApplicationName,
					ApplicationARN:       d.ApplicationARN,
					ApplicationStatus:    d.ApplicationStatus,
					ApplicationVersionId: d.ApplicationVersionId,
					RuntimeEnvironment:   d.RuntimeEnvironment,
					ApplicationMode:      d.ApplicationMode,
				},
			)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(
		rows,
		func(a api.ApplicationSummary) string { return *a.ApplicationName },
		in.NextToken,
		limit,
		invalid("Invalid NextToken."),
	)
	return &api.ListApplicationsResponse{ApplicationSummaries: page, NextToken: next}, err
}

// DeleteApplication marks a stable application for deletion after checking its creation timestamp.
func (s *Service) DeleteApplication(
	ctx context.Context,
	in *api.DeleteApplicationRequest,
) (*api.DeleteApplicationResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := read(tx, scope, in.ApplicationName)
		if e != nil {
			return e
		}

		if in.CreateTimestamp == nil || *in.CreateTimestamp != *r.Detail.CreateTimestamp {
			return invalid("CreateTimestamp does not match.")
		}

		if *r.Detail.ApplicationStatus != ready && *r.Detail.ApplicationStatus != running {
			return inUse()
		}

		transition(r, "DELETING", "DELETED")
		return save(tx, scope, r)
	})
	return &api.DeleteApplicationResponse{}, err
}

// StartApplication enters STARTING without allocating compute or running code.
func (s *Service) StartApplication(
	ctx context.Context,
	in *api.StartApplicationRequest,
) (*api.StartApplicationResponse, error) {
	_, err := s.change(ctx, in.ApplicationName, nil, func(tx storage.WriteTx, scope request.Scope, r *record) error {
		if *r.Detail.ApplicationStatus != ready {
			return inUse()
		}

		if e := checkRun(tx, scope, r, in.RunConfiguration); e != nil {
			return e
		}

		transition(r, "STARTING", running)
		return nil
	})
	return &api.StartApplicationResponse{}, err
}

// StopApplication enters STOPPING; Force can interrupt a pending transition.
func (s *Service) StopApplication(
	ctx context.Context,
	in *api.StopApplicationRequest,
) (*api.StopApplicationResponse, error) {
	_, err := s.change(ctx, in.ApplicationName, nil, func(_ storage.WriteTx, _ request.Scope, r *record) error {
		if *r.Detail.ApplicationStatus == "DELETING" ||
			(*r.Detail.ApplicationStatus != running && !common.Value(in.Force)) {
			return inUse()
		}

		transition(r, "STOPPING", ready)
		return nil
	})
	return &api.StopApplicationResponse{}, err
}

func arnName(scope request.Scope, arn *string) (*string, error) {
	prefix := fmt.Sprintf("arn:aws:kinesisanalytics:%s:%s:application/", scope.Region, scope.AccountID)
	name, ok := strings.CutPrefix(common.Value(arn), prefix)
	if !ok || !namePattern.MatchString(name) {
		return nil, missing()
	}

	return new(name), nil
}
