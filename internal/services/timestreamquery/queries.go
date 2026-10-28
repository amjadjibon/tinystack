package timestreamquery

import (
	"context"
	"encoding/json"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/timestreamquery"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/schedule"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const clientTokenTTL = 8 * time.Hour

type scheduledQueryRecord struct {
	tagSet
	ScheduleConfiguration          *api.ScheduleConfiguration
	NotificationConfiguration      *api.NotificationConfiguration
	TargetConfiguration            *api.TargetConfiguration
	KmsKeyId                       *string
	ErrorReportConfiguration       *api.ErrorReportConfiguration
	LastRun                        *api.ScheduledQueryRunSummary
	Previous                       *float64
	Created                        time.Time
	Enabled                        time.Time
	Claimed                        time.Time
	NextDue                        time.Time
	Name                           string
	QueryString                    string
	State                          string
	ScheduledQueryExecutionRoleArn string
	ClientToken                    string
	Fingerprint                    string
	FailedRuns                     []api.ScheduledQueryRunSummary
	Running                        bool
}

// nextInvocation is the first fire time after now of an ENABLED query's schedule, counted from when it was
// enabled; a DISABLED query has none.
func (r *scheduledQueryRecord) nextInvocation(now time.Time) *float64 {
	if r.State != stateEnabled {
		return nil
	}

	sched, err := schedule.Parse(*r.ScheduleConfiguration.ScheduleExpression)
	if err != nil {
		return nil
	}

	next, ok := sched.First(r.Enabled)
	if ok && !next.After(now) {
		next, ok = sched.After(next, now)
	}

	if !ok {
		return nil
	}

	return common.Seconds(next)
}

func (r *scheduledQueryRecord) description(scope request.Scope, now time.Time) *api.ScheduledQueryDescription {
	return &api.ScheduledQueryDescription{
		Arn:                            new(scheduledQueryARN(scope, r.Name)),
		LastRunSummary:                 common.Clone(r.LastRun),
		RecentlyFailedRuns:             *common.Clone(new(r.FailedRuns)),
		PreviousInvocationTime:         common.Clone(r.Previous),
		Name:                           new(r.Name),
		QueryString:                    new(r.QueryString),
		CreationTime:                   common.Seconds(r.Created),
		State:                          new(r.State),
		NextInvocationTime:             r.nextInvocation(now),
		ScheduleConfiguration:          common.Clone(r.ScheduleConfiguration),
		NotificationConfiguration:      common.Clone(r.NotificationConfiguration),
		TargetConfiguration:            common.Clone(r.TargetConfiguration),
		ScheduledQueryExecutionRoleArn: new(r.ScheduledQueryExecutionRoleArn),
		KmsKeyId:                       common.Clone(r.KmsKeyId),
		ErrorReportConfiguration:       common.Clone(r.ErrorReportConfiguration),
	}
}

func (r *scheduledQueryRecord) summary(scope request.Scope, now time.Time) api.ScheduledQuery {
	var dest *api.TargetDestination
	if r.TargetConfiguration != nil && r.TargetConfiguration.TimestreamConfiguration != nil {
		tc := r.TargetConfiguration.TimestreamConfiguration
		dest = &api.TargetDestination{
			TimestreamDestination: &api.TimestreamDestination{
				DatabaseName: new(*tc.DatabaseName),
				TableName:    new(*tc.TableName),
			},
		}
	}

	return api.ScheduledQuery{
		Arn:                      new(scheduledQueryARN(scope, r.Name)),
		Name:                     new(r.Name),
		CreationTime:             common.Seconds(r.Created),
		State:                    new(r.State),
		NextInvocationTime:       r.nextInvocation(now),
		ErrorReportConfiguration: common.Clone(r.ErrorReportConfiguration),
		TargetDestination:        dest,
	}
}

func requestFingerprint(in *api.CreateScheduledQueryRequest, tags map[string]string) (string, error) {
	copyReq := *in
	copyReq.ClientToken = nil
	copyReq.Tags = tagList(tags)
	// #nosec G117 -- ClientToken is an idempotency identifier cleared above, not a credential.
	raw, err := json.Marshal(copyReq)
	return string(raw), err
}

func (s *Service) validateCreateReferences(
	ctx context.Context,
	scope request.Scope,
	in *api.CreateScheduledQueryRequest,
) error {
	if in.ErrorReportConfiguration != nil && in.ErrorReportConfiguration.S3Configuration != nil &&
		common.Value(in.ErrorReportConfiguration.S3Configuration.EncryptionOption) == "SSE_KMS" {
		return invalid("Unsupported error-report encryption: SSE_KMS; only SSE_S3 is implemented")
	}

	if err := s.checkTopic(ctx, *in.NotificationConfiguration.SnsConfiguration.TopicArn); err != nil {
		return err
	}

	if err := s.checkRole(ctx, scope, *in.ScheduledQueryExecutionRoleArn); err != nil {
		return err
	}

	if in.TargetConfiguration != nil && in.TargetConfiguration.TimestreamConfiguration != nil {
		tc := in.TargetConfiguration.TimestreamConfiguration
		if err := s.checkTable(ctx, scope, *tc.DatabaseName, *tc.TableName); err != nil {
			return err
		}
	}

	if err := s.checkBucket(ctx, scope, *in.ErrorReportConfiguration.S3Configuration.BucketName); err != nil {
		return err
	}

	if in.KmsKeyId != nil {
		if err := s.checkKMSKey(ctx, scope, *in.KmsKeyId); err != nil {
			return err
		}
	}

	return nil
}

func (s *Service) CreateScheduledQuery(
	ctx context.Context,
	in *api.CreateScheduledQueryRequest,
) (*api.CreateScheduledQueryResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	sched, err := schedule.Parse(*in.ScheduleConfiguration.ScheduleExpression)
	if err != nil {
		return nil, invalid("ScheduleExpression is not valid: %s", err.Error())
	}

	if _, ok := sched.First(now); !ok {
		return nil, invalid("ScheduleExpression has no future invocation time.")
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	err = s.validateCreateReferences(ctx, scope, in)
	if err != nil {
		return nil, err
	}

	name := *in.Name
	token := common.Value(in.ClientToken)
	fp, err := requestFingerprint(in, tags)
	if err != nil {
		return nil, err
	}

	var arn string
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if token != "" {
			prefix := key(scope, kindScheduledQuery, "")
			for _, k := range tx.Keys(prefix) {
				existing, _, e := storage.Read[scheduledQueryRecord](tx, k)
				if e != nil {
					return e
				}

				if existing.ClientToken == token && now.Sub(existing.Created) < clientTokenTTL {
					if existing.Fingerprint != fp {
						return fault("ConflictException", "ClientToken already used with different parameters.")
					}

					arn = scheduledQueryARN(scope, existing.Name)
					return nil
				}
			}
		}

		k := key(scope, kindScheduledQuery, name)
		_, found, e := storage.Read[scheduledQueryRecord](tx, k)
		if e != nil {
			return e
		}

		if found {
			return fault("ConflictException", "ScheduledQuery %s already exists.", name)
		}

		record := &scheduledQueryRecord{
			tagSet:                         tagSet{Tags: tags},
			Name:                           name,
			QueryString:                    *in.QueryString,
			State:                          stateEnabled,
			ScheduleConfiguration:          common.Clone(in.ScheduleConfiguration),
			NotificationConfiguration:      common.Clone(in.NotificationConfiguration),
			TargetConfiguration:            common.Clone(in.TargetConfiguration),
			ScheduledQueryExecutionRoleArn: *in.ScheduledQueryExecutionRoleArn,
			KmsKeyId:                       common.Clone(in.KmsKeyId),
			ErrorReportConfiguration:       common.Clone(in.ErrorReportConfiguration),
			ClientToken:                    token,
			Fingerprint:                    fp,
			Created:                        now,
			Enabled:                        now,
		}

		arn = scheduledQueryARN(scope, name)
		if writeErr := storage.Write(tx, k, record); writeErr != nil {
			return writeErr
		}

		return storage.Write(tx, scheduledIndex(scope, name), scheduledIndexRecord{Scope: scope, Name: name})
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateScheduledQueryResponse{Arn: new(arn)}, nil
}

func (s *Service) DescribeScheduledQuery(
	ctx context.Context,
	in *api.DescribeScheduledQueryRequest,
) (*api.DescribeScheduledQueryResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name, err := parseARN(scope, in.ScheduledQueryArn)
	if err != nil {
		return nil, err
	}

	var out *api.ScheduledQueryDescription
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, found, e := storage.Read[scheduledQueryRecord](tx, key(scope, kindScheduledQuery, name))
		if e != nil {
			return e
		}

		if !found {
			return scheduledQueryNotFound(*in.ScheduledQueryArn)
		}

		out = r.description(scope, time.Now().UTC())
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DescribeScheduledQueryResponse{ScheduledQuery: out}, nil
}

func pageLimit(in *int64, defaultLimit int) int {
	if in == nil {
		return defaultLimit
	}

	return int(*in)
}

func (s *Service) ListScheduledQueries(
	ctx context.Context,
	in *api.ListScheduledQueriesRequest,
) (*api.ListScheduledQueriesResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	items := []api.ScheduledQuery{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys(key(scope, kindScheduledQuery, "")) {
			r, _, e := storage.Read[scheduledQueryRecord](tx, k)
			if e != nil {
				return e
			}

			items = append(items, r.summary(scope, now))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	page, nextToken, err := common.Page(
		items,
		func(q api.ScheduledQuery) string { return *q.Name },
		in.NextToken,
		pageLimit(in.MaxResults, defaultMaxResults),
		invalid("NextToken is not valid."),
	)
	if err != nil {
		return nil, err
	}

	return &api.ListScheduledQueriesResponse{
		ScheduledQueries: page,
		NextToken:        nextToken,
	}, nil
}

func (s *Service) UpdateScheduledQuery(
	ctx context.Context,
	in *api.UpdateScheduledQueryRequest,
) (*api.UpdateScheduledQueryOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name, err := parseARN(scope, in.ScheduledQueryArn)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		k := key(scope, kindScheduledQuery, name)
		r, found, e := storage.Read[scheduledQueryRecord](tx, k)
		if e != nil {
			return e
		}

		if !found {
			return scheduledQueryNotFound(*in.ScheduledQueryArn)
		}

		if *in.State == stateEnabled && r.State != stateEnabled {
			r.Enabled = time.Now().UTC()
			r.NextDue = time.Time{}
		}

		r.State = *in.State

		return storage.Write(tx, k, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateScheduledQueryOutput{}, nil
}

func (s *Service) DeleteScheduledQuery(
	ctx context.Context,
	in *api.DeleteScheduledQueryRequest,
) (*api.DeleteScheduledQueryOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name, err := parseARN(scope, in.ScheduledQueryArn)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		k := key(scope, kindScheduledQuery, name)
		_, found, e := storage.Read[scheduledQueryRecord](tx, k)
		if e != nil {
			return e
		}

		if !found {
			return scheduledQueryNotFound(*in.ScheduledQueryArn)
		}

		tx.Delete(k)
		tx.Delete(scheduledIndex(scope, name))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteScheduledQueryOutput{}, nil
}
