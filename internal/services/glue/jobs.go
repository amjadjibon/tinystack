package glue

import (
	"context"
	"math"
	"net/url"
	"strings"
	"unicode/utf8"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/glue"
	iamapi "github.com/amjadjibon/tinystack/internal/aws/generated/iam"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const kindJob = "job"

type jobRecord struct {
	Job  api.Job
	Tags map[string]string
}

func orchestrationName(n *string) error {
	if n == nil || len(*n) < 1 || len(*n) > 255 || !utf8.ValidString(*n) {
		return invalid("Name must contain 1 to 255 UTF-8 bytes")
	}

	for _, r := range *n {
		if r < 32 && r != '\t' {
			return invalid("Name must be a single-line string")
		}
	}

	return nil
}

func loadJob(tx storage.ReadTx, scope request.Scope, n string) (*jobRecord, error) {
	if err := orchestrationName(new(n)); err != nil {
		return nil, err
	}

	r, ok, err := storage.Read[jobRecord](tx, orchestrationKey(scope, kindJob, n))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, notFound("Job %s not found.", n)
	}

	return r, nil
}

func (s *Service) validateJob(ctx context.Context, j *api.Job) error {
	if err := orchestrationName(j.Name); err != nil {
		return err
	}

	if j.Description != nil && len(*j.Description) > 2048 {
		return invalid("Description exceeds 2048 bytes")
	}

	if j.Role == nil || *j.Role == "" {
		return invalid("Role is required")
	}

	if strings.HasPrefix(*j.Role, "arn:") {
		scope, err := request.RegionalScope(ctx)
		if err != nil {
			return err
		}

		role, found, err := s.roles.RoleByARN(ctx, *j.Role)
		if err != nil {
			return err
		}

		if !found || role.Account != scope.AccountID {
			return invalid("IAM role %s does not exist", *j.Role)
		}
	} else {
		if _, err := s.roles.GetRole(ctx, &iamapi.GetRoleRequest{RoleName: j.Role}); err != nil {
			return invalid("IAM role %s does not exist", *j.Role)
		}
	}

	if j.Command == nil {
		return invalid("Command is required")
	}

	command := common.Value(j.Command.Name)
	switch command {
	case commandPython, commandSpark, commandStreaming, commandRay:
	default:
		return invalid("Command.Name must be glueetl, gluestreaming, pythonshell or glueray")
	}

	u, err := url.Parse(common.Value(j.Command.ScriptLocation))
	if err != nil || u.Scheme != "s3" || u.Host == "" || len(u.Path) < 2 || u.RawQuery != "" || u.Fragment != "" {
		return invalid("Command.ScriptLocation must be an s3://bucket/key URI")
	}

	if err := validateTimeout(j.Timeout); err != nil {
		return err
	}

	if j.Command.Runtime != nil && (command != commandRay || *j.Command.Runtime != "Ray2.4") {
		return invalid("Command.Runtime is supported only as Ray2.4 for glueray jobs")
	}

	if j.MaxRetries != nil && *j.MaxRetries < 0 {
		return invalid("MaxRetries must be nonnegative")
	}

	if j.ExecutionProperty != nil && (j.ExecutionProperty.MaxConcurrentRuns != nil &&
		*j.ExecutionProperty.MaxConcurrentRuns < 1) {
		return invalid("MaxConcurrentRuns must be at least 1")
	}

	for _, args := range []map[string]string{j.DefaultArguments, j.NonOverridableArguments} {
		if err := validateArguments(args); err != nil {
			return err
		}
	}

	if j.GlueVersion == nil && command != commandPython {
		j.GlueVersion = new("5.1")
	}

	version := common.Value(j.GlueVersion)
	if version != "" && version != version09 && version != version10 && version != "2.0" && version != version30 &&
		version != version40 && version != version50 && version != "5.1" {
		return invalid("Unsupported GlueVersion")
	}

	python := common.Value(j.Command.PythonVersion)
	if command == commandPython {
		if python != "" && python != "3" && python != "3.9" {
			return invalid("Python shell requires PythonVersion 3 or 3.9")
		}

		if j.WorkerType != nil || j.NumberOfWorkers != nil {
			return invalid("Python shell uses MaxCapacity, not workers")
		}

		if j.MaxCapacity != nil && *j.MaxCapacity != 0.0625 && *j.MaxCapacity != 1 {
			return invalid("Python shell MaxCapacity must be 0.0625 or 1")
		}
	} else {
		if python != "" && python != "2" && python != "3" && (command != commandRay || python != "3.9") {
			return invalid("Invalid PythonVersion for command")
		}

		if python == "2" && version != "" && version != version09 && version != version10 {
			return invalid("Python 2 requires GlueVersion 0.9 or 1.0")
		}

		if j.MaxCapacity != nil && (j.WorkerType != nil || j.NumberOfWorkers != nil ||
			version != "" && version != version09 && version != version10 || command == commandRay) {
			return invalid("MaxCapacity cannot be combined with workers or GlueVersion 2.0 and later")
		}

		if j.MaxCapacity != nil &&
			(*j.MaxCapacity < 2 || *j.MaxCapacity > 100 || math.Trunc(*j.MaxCapacity) != *j.MaxCapacity) {
			return invalid("Spark MaxCapacity must be an integer from 2 to 100")
		}

		if (j.WorkerType == nil) != (j.NumberOfWorkers == nil) {
			return invalid("WorkerType and NumberOfWorkers are required together")
		}

		if j.NumberOfWorkers != nil && *j.NumberOfWorkers < 2 {
			return invalid("NumberOfWorkers must be at least 2")
		}

		worker := common.Value(j.WorkerType)
		switch worker {
		case "", "Standard", "G.1X", "G.2X", "G.025X", worker4X, worker8X, workerRay:
		default:
			return invalid("Invalid WorkerType")
		}

		if worker == workerRay && command != commandRay || command == commandRay && worker != workerRay {
			return invalid("Ray jobs require WorkerType Z.2X")
		}

		if command == commandRay && version < version40 {
			return invalid("Ray jobs require GlueVersion 4.0 or later")
		}

		if worker == "G.025X" && (command != commandStreaming || version < version30) {
			return invalid("G.025X requires a streaming job with GlueVersion 3.0 or later")
		}

		if worker == "Standard" && version > version10 {
			return invalid("Standard workers require GlueVersion 0.9 or 1.0")
		}

		if command == commandStreaming && version == version09 {
			return invalid("Streaming requires GlueVersion 1.0 or later")
		}

		if (worker == worker4X || worker == worker8X) && command != commandSpark {
			return invalid("G.4X and G.8X workers require a Spark ETL job")
		}

		if (worker == worker4X || worker == worker8X) && version < version30 {
			return invalid("G.4X and G.8X require GlueVersion 3.0 or later")
		}
	}

	if j.Timeout == nil {
		j.Timeout = new(int64(2880))
		if version >= version50 {
			j.Timeout = new(int64(480))
		}
	}

	if j.MaxRetries == nil {
		j.MaxRetries = new(int64(0))
	}

	if j.ExecutionProperty == nil || j.ExecutionProperty.MaxConcurrentRuns == nil {
		j.ExecutionProperty = &api.ExecutionProperty{MaxConcurrentRuns: new(int64(1))}
	}

	if command == commandPython && j.MaxCapacity == nil {
		j.MaxCapacity = new(0.0625)
	}

	return nil
}

func validateTimeout(timeout *int64) error {
	if timeout != nil && (*timeout < 1 || *timeout > 10080) {
		return invalid("Timeout must be between 1 and 10080 minutes")
	}

	return nil
}

func validateArguments(args map[string]string) error {
	size := 0
	for k, v := range args {
		if k == "" || strings.ContainsRune(k, 0) || strings.ContainsRune(v, 0) {
			return invalid("Invalid job argument")
		}

		size += len(k) + len(v)
	}

	if size > 260*1024 {
		return invalid("Job arguments exceed 260 KB")
	}

	return nil
}

func (s *Service) CreateJob(ctx context.Context, in *api.CreateJobRequest) (*api.CreateJobResponse, error) {
	j := &api.Job{
		Name:                    in.Name,
		Description:             in.Description,
		Role:                    in.Role,
		ExecutionProperty:       in.ExecutionProperty,
		Command:                 in.Command,
		DefaultArguments:        in.DefaultArguments,
		NonOverridableArguments: in.NonOverridableArguments,
		MaxRetries:              in.MaxRetries,
		Timeout:                 in.Timeout,
		MaxCapacity:             in.MaxCapacity,
		GlueVersion:             in.GlueVersion,
		NumberOfWorkers:         in.NumberOfWorkers,
		WorkerType:              in.WorkerType,
	}

	if err := s.validateJob(ctx, j); err != nil {
		return nil, err
	}

	if err := checkTags(in.Tags); err != nil {
		return nil, err
	}

	j.CreatedOn, j.LastModifiedOn = new(common.Unix(s.now())), new(common.Unix(s.now()))
	err := s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		key := orchestrationKey(scope, kindJob, *j.Name)
		if _, ok := tx.Get(key); ok {
			return alreadyExists("Job %s already exists", *j.Name)
		}

		return storage.Write(tx, key, &jobRecord{Job: *j, Tags: in.Tags})
	})
	return &api.CreateJobResponse{Name: j.Name}, err
}

func (s *Service) UpdateJob(ctx context.Context, in *api.UpdateJobRequest) (*api.UpdateJobResponse, error) {
	if in.JobUpdate == nil {
		return nil, invalid("JobUpdate is required")
	}

	u := in.JobUpdate
	j := &api.Job{
		Name:                    in.JobName,
		Description:             u.Description,
		Role:                    u.Role,
		ExecutionProperty:       u.ExecutionProperty,
		Command:                 u.Command,
		DefaultArguments:        u.DefaultArguments,
		NonOverridableArguments: u.NonOverridableArguments,
		MaxRetries:              u.MaxRetries,
		Timeout:                 u.Timeout,
		MaxCapacity:             u.MaxCapacity,
		GlueVersion:             u.GlueVersion,
		NumberOfWorkers:         u.NumberOfWorkers,
		WorkerType:              u.WorkerType,
	}

	// AWS retains GlueVersion when this one field is omitted from JobUpdate.
	if j.GlueVersion == nil {
		current, err := s.GetJob(ctx, &api.GetJobRequest{JobName: in.JobName})
		if err != nil {
			return nil, err
		}

		j.GlueVersion = current.Job.GlueVersion
	}

	if err := s.validateJob(ctx, j); err != nil {
		return nil, err
	}

	err := s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := loadJob(tx, scope, common.Value(in.JobName))
		if err != nil {
			return err
		}

		j.CreatedOn, j.LastModifiedOn = r.Job.CreatedOn, new(common.Unix(s.now()))
		r.Job = *j
		return storage.Write(tx, orchestrationKey(scope, kindJob, *j.Name), r)
	})
	return &api.UpdateJobResponse{JobName: in.JobName}, err
}

func (s *Service) GetJob(ctx context.Context, in *api.GetJobRequest) (*api.GetJobResponse, error) {
	out := &api.GetJobResponse{}
	err := s.view(ctx, nil, func(tx storage.ReadTx, scope request.Scope) error {
		r, err := loadJob(tx, scope, common.Value(in.JobName))
		if err != nil {
			return err
		}

		out.Job = &r.Job
		return nil
	})
	return out, err
}

func (s *Service) DeleteJob(ctx context.Context, in *api.DeleteJobRequest) (*api.DeleteJobResponse, error) {
	if err := orchestrationName(in.JobName); err != nil {
		return nil, err
	}

	err := s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		tx.Delete(orchestrationKey(scope, kindJob, *in.JobName))
		return nil
	})
	return &api.DeleteJobResponse{JobName: in.JobName}, err
}

func (s *Service) jobPage(
	ctx context.Context,
	tags map[string]string,
	token *string,
	limit *int64,
) ([]*jobRecord, *string, error) {
	var all []*jobRecord
	err := s.view(ctx, nil, func(tx storage.ReadTx, scope request.Scope) error {
		records, err := storage.Records[jobRecord](tx, scope.Prefix(service, kindJob))
		if err != nil {
			return err
		}

		for _, r := range records {
			match := true
			for k, v := range tags {
				if value, ok := r.Tags[k]; !ok || value != v {
					match = false
					break
				}
			}

			if match {
				all = append(all, r)
			}
		}

		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	return orchestrationPage(all, func(r *jobRecord) string { return *r.Job.Name }, token, limit)
}

func (s *Service) GetJobs(ctx context.Context, in *api.GetJobsRequest) (*api.GetJobsResponse, error) {
	rows, next, err := s.jobPage(ctx, nil, in.NextToken, in.MaxResults)
	out := &api.GetJobsResponse{Jobs: []api.Job{}, NextToken: next}
	for _, r := range rows {
		out.Jobs = append(out.Jobs, r.Job)
	}

	return out, err
}

func (s *Service) ListJobs(ctx context.Context, in *api.ListJobsRequest) (*api.ListJobsResponse, error) {
	rows, next, err := s.jobPage(ctx, in.Tags, in.NextToken, in.MaxResults)
	out := &api.ListJobsResponse{JobNames: []string{}, NextToken: next}
	for _, r := range rows {
		out.JobNames = append(out.JobNames, *r.Job.Name)
	}

	return out, err
}

func (s *Service) BatchGetJobs(ctx context.Context, in *api.BatchGetJobsRequest) (*api.BatchGetJobsResponse, error) {
	if len(in.JobNames) < 1 {
		return nil, invalid("JobNames must not be empty")
	}

	out := &api.BatchGetJobsResponse{Jobs: []api.Job{}, JobsNotFound: []string{}}
	err := s.view(ctx, nil, func(tx storage.ReadTx, scope request.Scope) error {
		for _, n := range in.JobNames {
			if err := orchestrationName(new(n)); err != nil {
				return err
			}

			r, ok, err := storage.Read[jobRecord](tx, orchestrationKey(scope, kindJob, n))
			if err != nil {
				return err
			}

			if !ok {
				out.JobsNotFound = append(out.JobsNotFound, n)
			} else {
				out.Jobs = append(out.Jobs, r.Job)
			}
		}

		return nil
	})
	return out, err
}

// Resource names are path-escaped so valid names containing slashes cannot alias nested storage keys.
func orchestrationKey(scope request.Scope, kind, n string) string {
	return scope.Key(service, kind, url.PathEscape(n))
}

func orchestrationPage[T any](
	items []T,
	id func(T) string,
	token *string,
	limit *int64,
) (selected []T, next *string, err error) {
	return page(items, func(item T) string { return url.PathEscape(id(item)) }, token, limit)
}
