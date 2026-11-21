package batch

import (
	"context"
	"maps"
	"regexp"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/batch"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	kindPolicy = "scheduling-policy"
	maxShares  = 500
)

var sharePattern = regexp.MustCompile(`^[A-Za-z0-9]{1,255}\*?$`)

func sharesOverlap(a, b string) bool {
	return a == b ||
		strings.HasSuffix(a, "*") && strings.HasPrefix(b, strings.TrimSuffix(a, "*")) ||
		strings.HasSuffix(b, "*") && strings.HasPrefix(a, strings.TrimSuffix(b, "*"))
}

func fairshareOK(policy *api.FairsharePolicy) error {
	if policy == nil {
		return invalid("fairsharePolicy is required")
	}

	if d := common.Value(policy.ShareDecaySeconds); d < 0 || d > 604800 {
		return invalid("shareDecaySeconds must be 0-604800")
	}

	if r := common.Value(policy.ComputeReservation); r < 0 || r > 99 {
		return invalid("computeReservation must be 0-99")
	}

	if len(policy.ShareDistribution) > maxShares {
		return invalid("At most 500 share distribution entries are supported")
	}

	for i, share := range policy.ShareDistribution {
		id := common.Value(share.ShareIdentifier)
		if !sharePattern.MatchString(id) {
			return invalid("shareIdentifier requires 1-255 alphanumeric characters and an optional trailing asterisk")
		}

		weight := common.Value(common.OrElse(share.WeightFactor, new(1.0)))
		if !(weight >= 0.0001 && weight <= 999.9999) {
			return invalid("weightFactor must be 0.0001-999.9999")
		}

		for _, prior := range policy.ShareDistribution[:i] {
			if sharesOverlap(id, common.Value(prior.ShareIdentifier)) {
				return invalid("Share distribution identifiers must not overlap")
			}
		}
	}

	return nil
}

func readPolicy(tx storage.ReadTx, scope request.Scope, arn *string) (*record, error) {
	if !strings.HasPrefix(common.Value(arn), "arn:") {
		return nil, invalid("A full scheduling policy ARN is required")
	}

	return read(tx, scope, kindPolicy, arn)
}

func (s *Service) CreateSchedulingPolicy(
	ctx context.Context, in *api.CreateSchedulingPolicyRequest,
) (*api.CreateSchedulingPolicyResponse, error) {
	if err := nameOK(in.Name); err != nil {
		return nil, err
	}

	if err := fairshareOK(in.FairsharePolicy); err != nil {
		return nil, err
	}

	if err := tagsOK(in.Tags); err != nil {
		return nil, err
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	r := &record{Kind: kindPolicy, ID: *in.Name, ARN: arn(scope, kindPolicy, *in.Name), Tags: maps.Clone(in.Tags)}
	r.Fairshare = common.Clone(in.FairsharePolicy)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error { return unique(tx, scope, r) })
	return &api.CreateSchedulingPolicyResponse{Name: new(r.ID), Arn: new(r.ARN)}, err
}

func (s *Service) DescribeSchedulingPolicies(
	ctx context.Context, in *api.DescribeSchedulingPoliciesRequest,
) (*api.DescribeSchedulingPoliciesResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if len(in.Arns) == 0 || len(in.Arns) > 100 {
		return nil, invalid("DescribeSchedulingPolicies requires 1-100 ARNs")
	}

	out := &api.DescribeSchedulingPoliciesResponse{SchedulingPolicies: []api.SchedulingPolicyDetail{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, arn := range in.Arns {
			if !strings.HasPrefix(arn, "arn:") {
				return invalid("A full scheduling policy ARN is required")
			}
		}

		ids, e := filters(scope, kindPolicy, in.Arns)
		if e != nil {
			return e
		}

		for _, id := range ids {
			r, ok, e := storage.Read[record](tx, scope.Key(serviceName, kindPolicy, id))
			if e != nil {
				return e
			}

			if ok {
				out.SchedulingPolicies = append(out.SchedulingPolicies, policyOutput(r))
			}
		}

		return nil
	})
	return out, err
}

func policyOutput(r *record) api.SchedulingPolicyDetail {
	return api.SchedulingPolicyDetail{
		Name: new(r.ID), Arn: new(r.ARN), FairsharePolicy: common.Clone(r.Fairshare), Tags: maps.Clone(r.Tags),
	}
}

func (s *Service) ListSchedulingPolicies(
	ctx context.Context, in *api.ListSchedulingPoliciesRequest,
) (*api.ListSchedulingPoliciesResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.ListSchedulingPoliciesResponse{SchedulingPolicies: []api.SchedulingPolicyListingDetail{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		all, e := rows(tx, scope, kindPolicy)
		if e != nil {
			return e
		}

		all, out.NextToken, e = page(all, func(r *record) string { return r.ID }, in.NextToken, in.MaxResults,
			scope, kindPolicy)
		if e != nil {
			return e
		}

		for _, r := range all {
			out.SchedulingPolicies = append(out.SchedulingPolicies, api.SchedulingPolicyListingDetail{Arn: new(r.ARN)})
		}

		return nil
	})
	return out, err
}

func (s *Service) UpdateSchedulingPolicy(
	ctx context.Context, in *api.UpdateSchedulingPolicyRequest,
) (*api.UpdateSchedulingPolicyResponse, error) {
	if err := fairshareOK(in.FairsharePolicy); err != nil {
		return nil, err
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := readPolicy(tx, scope, in.Arn)
		if e != nil {
			return e
		}

		// Omitted attributes retain their current values.
		p := in.FairsharePolicy
		r.Fairshare.ShareDecaySeconds = common.OrElse(p.ShareDecaySeconds, r.Fairshare.ShareDecaySeconds)
		r.Fairshare.ComputeReservation = common.OrElse(p.ComputeReservation, r.Fairshare.ComputeReservation)
		if p.ShareDistribution != nil {
			r.Fairshare.ShareDistribution = p.ShareDistribution
		}

		return save(tx, scope, r)
	})
	return &api.UpdateSchedulingPolicyResponse{}, err
}

func (s *Service) DeleteSchedulingPolicy(
	ctx context.Context, in *api.DeleteSchedulingPolicyRequest,
) (*api.DeleteSchedulingPolicyResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := readPolicy(tx, scope, in.Arn)
		if e != nil {
			return e
		}

		queues, e := rows(tx, scope, kindQueue)
		if e != nil {
			return e
		}

		for _, q := range queues {
			if common.Value(q.Queue.SchedulingPolicyArn) == r.ARN {
				return invalid("Scheduling policy is associated with a job queue")
			}
		}

		tx.Delete(scope.Key(serviceName, kindPolicy, r.ID))
		return nil
	})
	return &api.DeleteSchedulingPolicyResponse{}, err
}

func submitShareOK(tx storage.ReadTx, scope request.Scope, q *api.JobQueueDetail, in *api.SubmitJobRequest) error {
	if q.SchedulingPolicyArn == nil {
		if in.ShareIdentifier != nil || in.SchedulingPriorityOverride != nil {
			return invalid("Share identifier and scheduling priority require a fair-share queue")
		}

		return nil
	}

	share := common.Value(in.ShareIdentifier)
	if !sharePattern.MatchString(share) {
		return invalid("Fair-share job queues require a valid shareIdentifier")
	}

	if p := common.Value(in.SchedulingPriorityOverride); p < 0 || p > 9999 {
		return invalid("schedulingPriorityOverride must be 0-9999")
	}

	jobs, err := rows(tx, scope, kindJob)
	if err != nil {
		return err
	}

	active := map[string]bool{}
	for _, r := range jobs {
		if !terminal(common.Value(r.Job.Status)) && common.Value(r.Job.JobQueue) == common.Value(q.JobQueueArn) {
			active[common.Value(r.Job.ShareIdentifier)] = true
		}
	}

	if len(active) >= maxShares && !active[share] {
		return invalid("Job queue has 500 active share identifiers")
	}

	return nil
}
