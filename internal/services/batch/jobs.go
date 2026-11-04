package batch

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/batch"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	dependencyNToN = "N_TO_N"
	resourceVCPU   = "VCPU"
)

const (
	jobIndex        = "aws/batch/index/"
	statusRunnable  = "RUNNABLE"
	statusPending   = "PENDING"
	statusRunning   = "RUNNING"
	statusStarting  = "STARTING"
	statusSucceeded = "SUCCEEDED"
	statusFailed    = "FAILED"
)

func terminal(status string) bool { return status == statusSucceeded || status == statusFailed }

func resolveDefinition(tx storage.ReadTx, scope request.Scope, ref *string) (*record, error) {
	if ref == nil {
		return nil, missing()
	}

	name := *ref
	if strings.HasPrefix(name, "arn:") {
		prefix := fmt.Sprintf("arn:aws:batch:%s:%s:job-definition/", scope.Region, scope.AccountID)
		if !strings.HasPrefix(name, prefix) {
			return nil, missing()
		}

		name = strings.TrimPrefix(name, prefix)
	}

	if strings.Contains(name, ":") {
		return read(tx, scope, kindDefinition, ref)
	}

	all, err := rows(tx, scope, kindDefinition)
	if err != nil {
		return nil, err
	}

	var latest *record
	for _, r := range all {
		if common.Value(r.Definition.JobDefinitionName) == name && common.Value(r.Definition.Status) == active &&
			(latest == nil || *r.Definition.Revision > *latest.Definition.Revision) {
			latest = r
		}
	}

	if latest == nil {
		return nil, missing()
	}

	return latest, nil
}

func substitute(command []string, parameters map[string]string) ([]string, error) {
	out := slices.Clone(command)
	for i, word := range out {
		if strings.HasPrefix(word, "Ref::") {
			v, ok := parameters[strings.TrimPrefix(word, "Ref::")]
			if !ok {
				return nil, invalid("Missing command parameter: " + word)
			}

			out[i] = v
		}
	}

	return out, nil
}

func mergedContainer(scope request.Scope, d *api.ContainerProperties, o *api.ContainerOverrides,
	params map[string]string, platform string,
) (*api.ContainerDetail, error) {
	c := common.Clone(d)
	if o != nil {
		if platform == fargatePlatform && (o.Vcpus != nil || o.Memory != nil) {
			return nil, invalid("Fargate overrides require resourceRequirements instead of vcpus/memory")
		}

		if o.Command != nil {
			c.Command = slices.Clone(o.Command)
		}

		if o.Vcpus != nil {
			c.Vcpus = common.Clone(o.Vcpus)
		}

		if o.Memory != nil {
			c.Memory = common.Clone(o.Memory)
		}

		if o.ResourceRequirements != nil {
			for _, req := range o.ResourceRequirements {
				c.ResourceRequirements = slices.DeleteFunc(
					c.ResourceRequirements,
					func(v api.ResourceRequirement) bool { return common.Value(v.Type) == common.Value(req.Type) },
				)
				c.ResourceRequirements = append(c.ResourceRequirements, req)
			}

			c.Vcpus = nil
			c.Memory = nil
		}

		for _, kv := range o.Environment {
			c.Environment = slices.DeleteFunc(
				c.Environment,
				func(v api.KeyValuePair) bool { return common.Value(v.Name) == common.Value(kv.Name) },
			)
			c.Environment = append(c.Environment, kv)
		}
	}

	if err := containerOK(scope, c, platform); err != nil {
		return nil, err
	}

	command, err := substitute(c.Command, params)
	if err != nil {
		return nil, err
	}

	return &api.ContainerDetail{
		Image: c.Image, Vcpus: c.Vcpus, Memory: c.Memory, Command: command,
		JobRoleArn: c.JobRoleArn, ExecutionRoleArn: c.ExecutionRoleArn, Environment: c.Environment,
		ResourceRequirements: c.ResourceRequirements, LogConfiguration: c.LogConfiguration,
		NetworkConfiguration: c.NetworkConfiguration, FargatePlatformConfiguration: c.FargatePlatformConfiguration,
		RuntimePlatform: c.RuntimePlatform,
	}, nil
}

func (s *Service) SubmitJob(ctx context.Context, in *api.SubmitJobRequest) (*api.SubmitJobResponse, error) {
	if err := executionOptionsOK(in.RetryStrategy, in.Timeout); err != nil {
		return nil, err
	}

	if err := nameOK(in.JobName); err != nil {
		return nil, err
	}

	if err := tagsOK(in.Tags); err != nil {
		return nil, err
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.SubmitJobResponse
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if s.runtime != nil {
			if e := storage.Write(tx, "aws/batch/runtime-owner", s.owner); e != nil {
				return e
			}
		}

		q, e := read(tx, scope, kindQueue, in.JobQueue)
		if e != nil {
			return e
		}

		if common.Value(q.Queue.State) != enabled || common.Value(q.Queue.Status) != valid {
			return invalid("Job queue must be ENABLED and VALID")
		}

		d, e := resolveDefinition(tx, scope, in.JobDefinition)
		if e != nil {
			return e
		}

		if common.Value(d.Definition.Status) != active {
			return invalid("Job definition must be ACTIVE")
		}

		platform := d.Definition.PlatformCapabilities[0]
		if platform == fargatePlatform && s.runtime != nil {
			return invalid("Fargate execution is unsupported in Docker mode; use records mode")
		}

		for _, order := range q.Queue.ComputeEnvironmentOrder {
			ce, readErr := read(tx, scope, kindCE, order.ComputeEnvironment)
			if readErr != nil {
				return readErr
			}

			fargate := ce.CE.ComputeResources != nil && isFargate(common.Value(ce.CE.ComputeResources.Type))
			if fargate != (platform == fargatePlatform) {
				return invalid("Job platform must match the queue compute environments")
			}
		}

		params := maps.Clone(d.Definition.Parameters)
		if params == nil {
			params = map[string]string{}
		}

		maps.Copy(params, in.Parameters)
		c, e := mergedContainer(scope, d.Definition.ContainerProperties, in.ContainerOverrides, params, platform)
		if e != nil {
			return e
		}

		size := int64(0)
		if in.ArrayProperties != nil {
			size = common.Value(in.ArrayProperties.Size)
			if size < 2 || size > 10000 {
				return invalid("Array size must be 2-10000")
			}
		}

		if len(in.DependsOn) > 20 {
			return invalid("At most 20 dependencies")
		}

		deps := slices.Clone(in.DependsOn)
		for i := range deps {
			dep := &deps[i]
			typ := common.Value(dep.Type)
			if typ == "SEQUENTIAL" {
				if size == 0 || dep.JobId != nil {
					return invalid("SEQUENTIAL requires an array and no jobId")
				}

				continue
			}

			if typ != "" && typ != dependencyNToN {
				return invalid("Invalid dependency type")
			}

			r, e := read(tx, scope, kindJob, dep.JobId)
			if e != nil {
				return e
			}

			if typ == dependencyNToN &&
				(size == 0 || r.Job.ArrayProperties == nil || common.Value(r.Job.ArrayProperties.Size) != size) {
				return invalid("N_TO_N requires arrays of equal size")
			}

			dep.JobId = new(r.ID)
		}

		id := common.RandomID(32)
		job := &api.JobDetail{
			JobArn:  new(arn(scope, kindJob, id)),
			JobId:   new(id),
			JobName: common.Clone(in.JobName),
			JobQueue: new(
				q.ARN,
			),
			JobDefinition:        new(d.ARN),
			Status:               new("SUBMITTED"),
			CreatedAt:            new(time.Now().UnixMilli()),
			Parameters:           params,
			Container:            c,
			DependsOn:            deps,
			Tags:                 maps.Clone(in.Tags),
			PlatformCapabilities: slices.Clone(d.Definition.PlatformCapabilities),
			RetryStrategy: common.Clone(
				common.OrElse(in.RetryStrategy, d.Definition.RetryStrategy),
			),
			Timeout: common.Clone(common.OrElse(in.Timeout, d.Definition.Timeout)),
		}

		if job.RetryStrategy == nil {
			job.RetryStrategy = &api.RetryStrategy{}
		}

		job.RetryStrategy.Attempts = common.OrElse(job.RetryStrategy.Attempts, new(int64(1)))

		if size > 0 {
			job.ArrayProperties = &api.ArrayPropertiesDetail{Size: new(size)}
		}

		r := &record{ID: id, ARN: *job.JobArn, Kind: kindJob, Job: job, Scope: scope, Tags: maps.Clone(in.Tags)}
		if e := saveJob(tx, r); e != nil {
			return e
		}

		for i := range size {
			child := common.Clone(r)
			child.ID = fmt.Sprintf("%s:%d", id, i)
			child.ARN = arn(scope, kindJob, child.ID)
			child.Parent = id
			child.Job.JobId = new(child.ID)
			child.Job.JobArn = new(child.ARN)
			child.Job.ArrayProperties = &api.ArrayPropertiesDetail{Size: new(size), Index: new(i)}
			child.Job.DependsOn = nil
			for _, dep := range deps {
				switch common.Value(dep.Type) {
				case "SEQUENTIAL":
					if i > 0 {
						child.Job.DependsOn = append(
							child.Job.DependsOn,
							api.JobDependency{JobId: new(fmt.Sprintf("%s:%d", id, i-1))},
						)
					}

				case dependencyNToN:
					child.Job.DependsOn = append(
						child.Job.DependsOn,
						api.JobDependency{JobId: new(fmt.Sprintf("%s:%d", *dep.JobId, i))},
					)
				default:
					child.Job.DependsOn = append(child.Job.DependsOn, dep)
				}
			}

			if e := saveJob(tx, child); e != nil {
				return e
			}
		}

		out = &api.SubmitJobResponse{JobArn: job.JobArn, JobId: job.JobId, JobName: job.JobName}
		return settle(tx, scope)
	})
	return out, err
}

func saveJob(tx storage.WriteTx, r *record) error {
	if err := save(tx, r.Scope, r); err != nil {
		return err
	}

	return storage.Write(
		tx,
		jobIndex+r.Scope.AccountID+"/"+r.Scope.Region+"/"+r.ID,
		r.Scope.Key(serviceName, kindJob, r.ID),
	)
}

func (s *Service) DescribeJobs(ctx context.Context, in *api.DescribeJobsRequest) (*api.DescribeJobsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if len(in.Jobs) < 1 || len(in.Jobs) > 100 {
		return nil, invalid("DescribeJobs requires 1-100 IDs")
	}

	out := &api.DescribeJobsResponse{Jobs: []api.JobDetail{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, ref := range in.Jobs {
			id, e := identifier(scope, new(ref), kindJob)
			if e != nil {
				return e
			}

			r, ok, e := storage.Read[record](tx, scope.Key(serviceName, kindJob, id))
			if e != nil {
				return e
			}

			if ok {
				out.Jobs = append(out.Jobs, *common.Clone(r.Job))
			}
		}

		return nil
	})
	return out, err
}

func (s *Service) CancelJob(ctx context.Context, in *api.CancelJobRequest) (*api.CancelJobResponse, error) {
	return &api.CancelJobResponse{}, s.endJob(ctx, in.JobId, in.Reason, false)
}

func (s *Service) TerminateJob(ctx context.Context, in *api.TerminateJobRequest) (*api.TerminateJobResponse, error) {
	return &api.TerminateJobResponse{}, s.endJob(ctx, in.JobId, in.Reason, true)
}

func (s *Service) endJob(ctx context.Context, id, reason *string, terminate bool) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := read(tx, scope, kindJob, id)
		if e != nil {
			return e
		}

		all, e := rows(tx, scope, kindJob)
		if e != nil {
			return e
		}

		for _, j := range all {
			if j.ID != r.ID && j.Parent != r.ID {
				continue
			}

			st := common.Value(j.Job.Status)
			if terminal(st) || !terminate && (st == statusStarting || st == statusRunning) {
				continue
			}

			j.CancelReason = common.Value(reason)
			j.Cancelled = true
			if st != statusPending && st != statusRunning && st != statusStarting &&
				(j.Job.ArrayProperties == nil || j.Parent != "") {
				failJob(j, common.Value(reason))
			}

			if terminate && (st == statusRunning || st == statusStarting) {
				if st == statusRunning {
					finishAttempt(j, nil, common.Value(reason), true)
				}

				failJob(j, common.Value(reason))
			}

			if e := saveJob(tx, j); e != nil {
				return e
			}
		}

		return settle(tx, scope)
	})
}

func failJob(r *record, reason string) {
	r.Job.Status = new(statusFailed)
	r.Job.StatusReason = new(reason)
	r.Job.StoppedAt = new(time.Now().UnixMilli())
}

func settle(tx storage.WriteTx, scope request.Scope) error {
	all, err := rows(tx, scope, kindJob)
	if err != nil {
		return err
	}

	jobs := map[string]*record{}
	for _, r := range all {
		jobs[r.ID] = r
	}

	// Iterate until transitive failures and array summaries reach a fixed point.
	for changed := true; changed; {
		changed = false
		for _, r := range all {
			before := common.Value(r.Job.Status)
			if r.Job.ArrayProperties != nil && r.Parent == "" {
				counts := map[string]int64{}
				for _, child := range all {
					if child.Parent == r.ID {
						counts[common.Value(child.Job.Status)]++
					}
				}

				if !maps.Equal(r.Job.ArrayProperties.StatusSummary, counts) {
					r.Job.ArrayProperties.StatusSummaryLastUpdatedAt = new(time.Now().UnixMilli())
				}

				r.Job.ArrayProperties.StatusSummary = counts
				size := common.Value(r.Job.ArrayProperties.Size)
				switch {
				case counts[statusSucceeded] == size:
					r.Job.Status = new(statusSucceeded)
					if before != statusSucceeded {
						r.Job.StoppedAt = new(time.Now().UnixMilli())
					}

				case counts[statusFailed]+counts[statusSucceeded] == size:
					reason := "One or more child jobs failed"
					if r.Cancelled {
						reason = r.CancelReason
					}

					if before != statusFailed {
						failJob(r, reason)
					}

				default:
					r.Job.Status = new(statusPending)
				}
			} else if before == "SUBMITTED" || before == statusPending || before == statusRunnable {
				pending, failed := false, false
				for _, dep := range r.Job.DependsOn {
					d := jobs[common.Value(dep.JobId)]
					if d == nil || common.Value(d.Job.Status) == statusFailed {
						failed = true
						continue
					}

					pending = pending || common.Value(d.Job.Status) != statusSucceeded
				}

				switch {
				case failed && !r.Cancelled:
					failJob(r, "Dependency job failed")
				case pending:
					r.Job.Status = new(statusPending)
				case r.Cancelled:
					failJob(r, r.CancelReason)
				case failed:
					failJob(r, "Dependency job failed")
				default:
					r.Job.Status = new(statusRunnable)
				}
			}

			changed = changed || before != common.Value(r.Job.Status)
		}
	}

	for _, r := range all {
		if err := saveJob(tx, r); err != nil {
			return err
		}
	}

	return nil
}

func (s *Service) ListJobs(ctx context.Context, in *api.ListJobsRequest) (*api.ListJobsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if (in.JobQueue == nil) == (in.ArrayJobId == nil) {
		return nil, invalid("Specify exactly one jobQueue or arrayJobId")
	}

	// AWS ignores filters when listing array children.
	filters := in.Filters
	if in.ArrayJobId != nil {
		filters = nil
	}

	if len(filters) > 1 {
		return nil, invalid("Only one filter is allowed")
	}

	for _, f := range filters {
		switch common.Value(f.Name) {
		case "JOB_NAME", "JOB_DEFINITION", "BEFORE_CREATED_AT", "AFTER_CREATED_AT":
		default:
			return nil, invalid("Unsupported ListJobs filter")
		}

		if len(f.Values) != 1 {
			return nil, invalid("Filter requires one value")
		}

		if strings.HasSuffix(common.Value(f.Name), "CREATED_AT") {
			if _, e := strconv.ParseInt(f.Values[0], 10, 64); e != nil {
				return nil, invalid("Invalid timestamp filter")
			}
		}
	}

	out := &api.ListJobsResponse{JobSummaryList: []api.JobSummary{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		queue := ""
		if in.JobQueue != nil {
			q, e := read(tx, scope, kindQueue, in.JobQueue)
			if e != nil {
				return e
			}

			queue = q.ARN
		}

		all, e := rows(tx, scope, kindJob)
		if e != nil {
			return e
		}

		selected := []*record{}
		status := common.Value(common.OrElse(in.JobStatus, new(statusRunning)))
		for _, r := range all {
			if in.ArrayJobId != nil {
				if r.Parent != *in.ArrayJobId {
					continue
				}
			} else if r.Parent != "" || common.Value(r.Job.JobQueue) != queue {
				continue
			}

			if len(filters) == 0 && (in.ArrayJobId == nil || in.JobStatus != nil) &&
				common.Value(r.Job.Status) != status {
				continue
			}

			match := true
			for _, f := range filters {
				v := f.Values[0]
				switch common.Value(f.Name) {
				case "JOB_NAME":
					match = wildcard(strings.ToLower(v), strings.ToLower(common.Value(r.Job.JobName)))
				case "JOB_DEFINITION":
					if strings.HasPrefix(v, "arn:") {
						match = v == common.Value(r.Job.JobDefinition)
					} else {
						definition, _, _ := strings.Cut(lastName(common.Value(r.Job.JobDefinition)), ":")
						match = wildcard(v, definition)
					}

				case "BEFORE_CREATED_AT":
					n, _ := strconv.ParseInt(v, 10, 64)
					match = common.Value(r.Job.CreatedAt) < n
				case "AFTER_CREATED_AT":
					n, _ := strconv.ParseInt(v, 10, 64)
					match = common.Value(r.Job.CreatedAt) > n
				}
			}

			if match {
				selected = append(selected, r)
			}
		}

		// Lists retain storage key order; AWS permits up to 1000 results.
		limit := common.Value(common.OrElse(in.MaxResults, new(int64(100))))
		if len(filters) > 0 && limit > 100 {
			return invalid("Filters permit at most 100 results")
		}

		if limit < 1 || limit > 1000 {
			return invalid("maxResults must be 1-1000")
		}

		binding := query(kindJob, nil, queue, common.Value(in.ArrayJobId), status, filterJSON(in.Filters))
		selected, out.NextToken, e = jobPage(selected, in.NextToken, int(limit), scope, binding)
		if e != nil {
			return e
		}

		for _, r := range selected {
			j := r.Job
			summary := api.JobSummary{
				JobArn:        j.JobArn,
				JobId:         j.JobId,
				JobName:       j.JobName,
				CreatedAt:     j.CreatedAt,
				Status:        j.Status,
				StatusReason:  j.StatusReason,
				StartedAt:     j.StartedAt,
				StoppedAt:     j.StoppedAt,
				JobDefinition: j.JobDefinition,
				Container:     &api.ContainerSummary{ExitCode: j.Container.ExitCode, Reason: j.Container.Reason},
			}

			if a := j.ArrayProperties; a != nil {
				summary.ArrayProperties = &api.ArrayPropertiesSummary{
					Size:          a.Size,
					Index:         a.Index,
					StatusSummary: a.StatusSummary,
				}
			}

			out.JobSummaryList = append(out.JobSummaryList, summary)
		}

		return nil
	})
	return out, err
}

func filterJSON(filters []api.KeyValuesPair) string {
	b, _ := json.Marshal(filters)
	return string(b)
}
