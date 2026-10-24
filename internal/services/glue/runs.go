package glue

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/glue"
	s3api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/services/hostprocess"
	"github.com/amjadjibon/tinystack/internal/services/iam"
	"github.com/amjadjibon/tinystack/internal/services/logs"
	"github.com/amjadjibon/tinystack/internal/services/s3"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const runIndex = "aws/glue/index/run/"

type runRecord struct {
	Run         api.JobRun
	Job         api.Job
	Scope       request.Scope
	Endpoint    string
	AccessKey   string
	Owner       string
	Workflow    string
	WorkflowRun string
}

type runReference struct {
	Scope request.Scope
	Job   string
	ID    string
}

// UseExecution starts durable job orchestration. Host scripts use the same executor selection as local Lambda.
func (s *Service) UseExecution(roles *iam.Service, buckets *s3.Service, logGroups *logs.Service,
	enabled bool, python string, secrets map[string]string,
) {
	s.roles, s.buckets, s.logs = roles, buckets, logGroups
	s.enabled, s.python, s.secrets = enabled, python, secrets
	s.session = common.RandomID(32)
	s.active = map[string]context.CancelFunc{}
	s.workers = background.New()
	s.workers.Every(100*time.Millisecond, s.tick)
}

// Stop cancels process groups and joins all workers before the store closes.
func (s *Service) Stop() {
	if s.workers != nil {
		s.workers.Stop()
	}
}

func runKey(scope request.Scope, job, id string) string {
	return scope.Key(service, "run", url.PathEscape(job)+"/"+id)
}

func indexRun(ref runReference) string {
	return runIndex + ref.Scope.AccountID + "/" + ref.Scope.Region + "/" + url.PathEscape(ref.Job) + "/" + ref.ID
}

func activeRun(state string) bool {
	return state == stateStarting || state == stateRunning || state == stateStopping
}

func loadRun(tx storage.ReadTx, scope request.Scope, job, id string) (*runRecord, error) {
	if err := orchestrationName(new(job)); err != nil {
		return nil, err
	}

	if err := orchestrationName(new(id)); err != nil {
		return nil, err
	}

	r, ok, err := storage.Read[runRecord](tx, runKey(scope, job, id))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, notFound("Job run %s not found", id)
	}

	return r, nil
}

func (s *Service) StartJobRun(ctx context.Context, in *api.StartJobRunRequest) (*api.StartJobRunResponse, error) {
	if err := validateTimeout(in.Timeout); err != nil {
		return nil, err
	}

	if err := validateArguments(in.Arguments); err != nil {
		return nil, err
	}

	out := &api.StartJobRunResponse{}
	metadata, _ := request.FromContext(ctx)
	err := s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := s.startRun(tx, scope, in, new(metadata), "", "", "")
		if err != nil {
			return err
		}

		out.JobRunId = r.Run.Id
		return nil
	})
	return out, err
}

func (s *Service) startRun(tx storage.WriteTx, scope request.Scope, in *api.StartJobRunRequest,
	metadata *request.Metadata, trigger, workflow, workflowRun string,
) (*runRecord, error) {
	j, err := loadJob(tx, scope, common.Value(in.JobName))
	if err != nil {
		return nil, err
	}

	if common.Value(j.Job.Command.Name) != commandPython {
		return nil, invalid("TinyStack has no Spark or Ray runtime; only pythonshell jobs can run")
	}

	rows, err := storage.Records[runRecord](tx, scope.Key(service, "run", url.PathEscape(*in.JobName)+"/"))
	if err != nil {
		return nil, err
	}

	count := int64(0)
	for _, r := range rows {
		if activeRun(common.Value(r.Run.JobRunState)) {
			count++
		}
	}

	if count >= common.Value(j.Job.ExecutionProperty.MaxConcurrentRuns) {
		return nil, awserror.New("ConcurrentRunsExceededException", "Concurrent runs exceeded for job")
	}

	args := common.CloneTags(j.Job.DefaultArguments)
	if args == nil {
		args = map[string]string{}
	}

	maps.Copy(args, in.Arguments)
	maps.Copy(args, j.Job.NonOverridableArguments)
	if workflow != "" {
		args["--WORKFLOW_NAME"], args["--WORKFLOW_RUN_ID"] = workflow, workflowRun
	}
	if err := validateArguments(args); err != nil {
		return nil, err
	}

	id := "jr_" + common.RandomID(64, "0123456789abcdef")
	attempt := int64(0)
	if in.JobRunId != nil {
		previous, err := loadRun(tx, scope, *in.JobName, *in.JobRunId)
		if err != nil {
			return nil, err
		}

		if activeRun(common.Value(previous.Run.JobRunState)) {
			return nil, invalid("Cannot retry an active run")
		}

		attempt = common.Value(previous.Run.Attempt) + 1
	}

	now := common.Unix(s.now())
	r := &runRecord{
		Job: j.Job, Scope: scope, Endpoint: metadata.Endpoint, AccessKey: metadata.AccessKey,
		Workflow: workflow, WorkflowRun: workflowRun, Run: api.JobRun{
			Id: new(id), JobName: in.JobName, Attempt: new(attempt), PreviousRunId: in.JobRunId,
			StartedOn: new(now), LastModifiedOn: new(now), JobRunState: new(stateStarting), Arguments: args,
			Timeout: common.OrElse(in.Timeout, j.Job.Timeout), MaxCapacity: j.Job.MaxCapacity,
			GlueVersion: j.Job.GlueVersion, WorkerType: j.Job.WorkerType, NumberOfWorkers: j.Job.NumberOfWorkers,
			LogGroupName: new("/aws-glue/python-jobs"),
		},
	}

	if trigger != "" {
		r.Run.TriggerName = new(trigger)
	}

	if err := storage.Write(tx, runKey(scope, *in.JobName, id), r); err != nil {
		return nil, err
	}

	ref := runReference{Scope: scope, Job: *in.JobName, ID: id}
	if err := storage.Write(tx, indexRun(ref), ref); err != nil {
		return nil, err
	}

	return r, nil
}

func (s *Service) GetJobRun(ctx context.Context, in *api.GetJobRunRequest) (*api.GetJobRunResponse, error) {
	out := &api.GetJobRunResponse{}
	err := s.view(ctx, nil, func(tx storage.ReadTx, scope request.Scope) error {
		r, err := loadRun(tx, scope, common.Value(in.JobName), common.Value(in.RunId))
		if err != nil {
			return err
		}

		out.JobRun = &r.Run
		return nil
	})
	return out, err
}

func (s *Service) GetJobRuns(ctx context.Context, in *api.GetJobRunsRequest) (*api.GetJobRunsResponse, error) {
	var all []*runRecord
	err := s.view(ctx, nil, func(tx storage.ReadTx, scope request.Scope) error {
		if _, err := loadJob(tx, scope, common.Value(in.JobName)); err != nil {
			return err
		}

		var err error
		all, err = storage.Records[runRecord](tx, scope.Key(service, "run", url.PathEscape(*in.JobName)+"/"))
		return err
	})
	if err != nil {
		return nil, err
	}

	rows, next, err := page(all, func(r *runRecord) string { return *r.Run.Id }, in.NextToken, in.MaxResults)
	out := &api.GetJobRunsResponse{JobRuns: []api.JobRun{}, NextToken: next}
	for _, r := range rows {
		out.JobRuns = append(out.JobRuns, r.Run)
	}

	return out, err
}

func (s *Service) BatchStopJobRun(
	ctx context.Context,
	in *api.BatchStopJobRunRequest,
) (*api.BatchStopJobRunResponse, error) {
	if len(in.JobRunIds) < 1 || len(in.JobRunIds) > 25 {
		return nil, invalid("JobRunIds must contain 1 to 25 IDs")
	}

	out := &api.BatchStopJobRunResponse{}
	err := s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		for _, id := range in.JobRunIds {
			r, ok, err := storage.Read[runRecord](tx, runKey(scope, common.Value(in.JobName), id))
			if err != nil {
				return err
			}

			if !ok {
				out.Errors = append(out.Errors, api.BatchStopJobRunError{
					JobName:  in.JobName,
					JobRunId: new(id),
					ErrorDetail: &api.ErrorDetail{
						ErrorCode:    new("EntityNotFoundException"),
						ErrorMessage: new("Job run not found"),
					},
				})
				continue
			}

			if activeRun(common.Value(r.Run.JobRunState)) {
				r.Run.JobRunState = new(stateStopping)
				if err := storage.Write(tx, runKey(scope, *in.JobName, id), r); err != nil {
					return err
				}
			}

			out.SuccessfulSubmissions = append(out.SuccessfulSubmissions,
				api.BatchStopJobRunSuccessfulSubmission{JobName: in.JobName, JobRunId: new(id)})
		}

		return nil
	})
	return out, err
}

func (s *Service) tick(ctx context.Context) {
	var refs []*runReference
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		var err error
		refs, err = storage.Records[runReference](tx, runIndex)
		return err
	})
	if err != nil {
		return
	}

	for _, ref := range refs {
		s.advanceRun(ctx, *ref)
	}

	s.tickTriggers(ctx)
}

func (s *Service) advanceRun(ctx context.Context, ref runReference) {
	key := runKey(ref.Scope, ref.Job, ref.ID)
	var launch *runRecord
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, err := loadRun(tx, ref.Scope, ref.Job, ref.ID)
		if err != nil {
			return err
		}

		state := common.Value(r.Run.JobRunState)
		if !activeRun(state) {
			tx.Delete(indexRun(ref))
			return nil
		}

		s.mu.Lock()
		cancel := s.active[key]
		s.mu.Unlock()
		switch {
		case state == stateStopping:
			if cancel != nil {
				cancel()
			} else {
				s.finishRecord(r, stateStopped, "")
				tx.Delete(indexRun(ref))
			}
		case state == stateRunning && r.Owner != s.session:
			s.finishRecord(
				r,
				stateFailed,
				"TinyStack stopped while the Python process was running; execution was interrupted",
			)
			tx.Delete(indexRun(ref))
		case state == stateStarting:
			r.Owner = s.session
			r.Run.JobRunState = new(stateRunning)
			r.Run.LastModifiedOn = new(common.Unix(s.now()))
			launch = r
		}

		return storage.Write(tx, key, r)
	})
	if err != nil {
		return
	}

	if launch != nil {
		s.workers.Go(func(ctx context.Context) { s.execute(ctx, launch) })
	}
}

func (s *Service) finishRecord(r *runRecord, state, message string) {
	now := common.Unix(s.now())
	r.Run.JobRunState, r.Run.CompletedOn, r.Run.LastModifiedOn = new(state), new(now), new(now)
	r.Run.ExecutionTime = new(int64(max(0, now-common.Value(r.Run.StartedOn))))
	if message != "" {
		r.Run.ErrorMessage = new(message)
	}
}

func (s *Service) execute(parent context.Context, r *runRecord) {
	ctx, cancel := context.WithTimeout(parent, time.Duration(*r.Run.Timeout)*time.Minute)
	defer cancel()
	key := runKey(r.Scope, *r.Run.JobName, *r.Run.Id)
	s.mu.Lock()
	s.active[key] = cancel
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.active, key); s.mu.Unlock() }()
	state, message := stateSucceeded, ""
	// A stop may win between claiming the record and starting this goroutine.
	var runnable bool
	readErr := s.store.View(ctx, func(tx storage.ReadTx) error {
		current, err := loadRun(tx, r.Scope, *r.Run.JobName, *r.Run.Id)
		if err != nil {
			return err
		}

		runnable = common.Value(current.Run.JobRunState) == stateRunning
		return nil
	})
	if readErr != nil || !runnable {
		return
	}
	if !s.enabled {
		state, message = stateFailed, "Local execution is disabled: select the Lambda local executor "+
			"(lambda.executor: local) to run Glue Python shell jobs"
	} else {
		_, _, message = s.runPython(ctx, r)
		if message != "" {
			state = stateFailed
		}
	}

	// Shutdown leaves the claimed RUNNING record for explicit recovery on restart.
	if parent.Err() != nil {
		return
	}

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		state, message = stateTimeout, "Job exceeded its Timeout in minutes"
	}

	_ = s.store.Update(context.WithoutCancel(parent), func(tx storage.WriteTx) error {
		current, err := loadRun(tx, r.Scope, *r.Run.JobName, *r.Run.Id)
		if err != nil {
			return err
		}

		if common.Value(current.Run.JobRunState) == stateStopping {
			state, message = stateStopped, ""
		}

		s.finishRecord(current, state, message)
		ref := runReference{Scope: r.Scope, Job: *r.Run.JobName, ID: *r.Run.Id}
		tx.Delete(indexRun(ref))
		if state == stateFailed && common.Value(current.Run.Attempt) < common.Value(r.Job.MaxRetries) {
			next := common.Clone(current)
			next.Run.Id = new("jr_" + common.RandomID(64, "0123456789abcdef"))
			next.Run.PreviousRunId = current.Run.Id
			next.Run.Attempt = new(common.Value(current.Run.Attempt) + 1)
			next.Run.JobRunState = new(stateStarting)
			next.Run.StartedOn = new(common.Unix(s.now()))
			next.Run.CompletedOn, next.Run.ExecutionTime, next.Run.ErrorMessage = nil, nil, nil
			next.Owner = ""
			if err := storage.Write(tx, runKey(r.Scope, *r.Run.JobName, *next.Run.Id), next); err != nil {
				return err
			}

			ref.ID = *next.Run.Id
			if err := storage.Write(tx, indexRun(ref), ref); err != nil {
				return err
			}
		}

		return storage.Write(tx, key, current)
	})
}

func (s *Service) runPython(ctx context.Context, r *runRecord) (stdout, stderr, message string) {
	metadata := request.Metadata{Scope: r.Scope, Endpoint: r.Endpoint, AccessKey: r.AccessKey, Service: service}
	ctx = request.WithMetadata(ctx, metadata)
	u, err := url.Parse(*r.Job.Command.ScriptLocation)
	if err != nil {
		return "", "", err.Error()
	}

	object, err := s.buckets.GetObject(
		ctx,
		&s3api.GetObjectRequest{Bucket: new(u.Host), Key: new(strings.TrimPrefix(u.Path, "/"))},
	)
	if err != nil {
		return "", "", "Failed to download script from S3: " + err.Error()
	}

	script := object.Body

	root, err := os.MkdirTemp("", "tinystack-glue-")
	if err != nil {
		return "", "", err.Error()
	}

	defer func() { _ = os.RemoveAll(root) }()
	path := filepath.Join(root, "job.py")
	if writeErr := os.WriteFile(path, script, 0o600); writeErr != nil {
		return "", "", writeErr.Error()
	}

	python := s.python
	if python == "" {
		python = "python3"
	}

	executable, err := exec.LookPath(python)
	if err != nil {
		return "", "", err.Error()
	}

	args := []string{path, "--JOB_NAME", *r.Run.JobName, "--JOB_RUN_ID", *r.Run.Id}
	keys := slices.Sorted(maps.Keys(r.Run.Arguments))
	for _, k := range keys {
		if k == "--JOB_NAME" || k == "--JOB_RUN_ID" {
			continue
		}

		args = append(args, "--"+strings.TrimPrefix(k, "--"), r.Run.Arguments[k])
	}

	// #nosec G204 -- executable is the configured local interpreter; scripts are explicitly enabled host code.
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Dir = root
	cmd.Env = []string{}
	for _, k := range []string{"PATH", "HOME", "LANG", "SYSTEMROOT"} {
		if v, ok := os.LookupEnv(k); ok {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}

	access := r.AccessKey
	if access == "" {
		access = r.Scope.AccountID
	}

	secret := s.secrets[access]
	if secret == "" {
		secret = "test"
	}

	cmd.Env = append(cmd.Env, "AWS_ACCESS_KEY_ID="+access, "AWS_SECRET_ACCESS_KEY="+secret,
		"AWS_REGION="+r.Scope.Region, "AWS_DEFAULT_REGION="+r.Scope.Region, "AWS_ENDPOINT_URL="+r.Endpoint,
		"AWS_EC2_METADATA_DISABLED=true", "PYTHONUNBUFFERED=1", "PYTHONDONTWRITEBYTECODE=1", "PYTHONNOUSERSITE=1")
	hostprocess.Configure(cmd)
	cmd.Cancel = func() error { hostprocess.Kill(cmd); return nil }
	cmd.WaitDelay = time.Second
	out, err := os.CreateTemp(root, "stdout-")
	if err != nil {
		return "", "", err.Error()
	}

	defer func() { _ = out.Close() }()
	errout, err := os.CreateTemp(root, "stderr-")
	if err != nil {
		return "", "", err.Error()
	}

	defer func() { _ = errout.Close() }()
	cmd.Stdout, cmd.Stderr = out, errout
	err = cmd.Run()
	if cmd.Process != nil {
		hostprocess.Kill(cmd)
	}

	stdout, stderr = "", fileTail(errout, 8192)
	for kind, file := range map[string]*os.File{"output": out, "error": errout} {
		if logErr := s.publishOutput(ctx, r, file, kind); logErr != nil {
			return stdout, stderr, "Failed to publish job logs: " + logErr.Error()
		}
	}

	if err != nil {
		message = strings.TrimSpace(stderr)
		if len(message) > 8192 {
			message = message[len(message)-8192:]
		}

		if message == "" {
			message = fmt.Sprintf("Python process failed: %v", err)
		}
	}

	return stdout, stderr, message
}

func fileTail(f *os.File, limit int64) string {
	info, err := f.Stat()
	if err != nil {
		return ""
	}

	data := make([]byte, min(info.Size(), limit))
	_, _ = f.ReadAt(data, max(0, info.Size()-limit))
	return string(data)
}

func (s *Service) publishOutput(ctx context.Context, r *runRecord, file *os.File, kind string) error {
	if _, err := file.Seek(0, 0); err != nil {
		return err
	}

	reader := bufio.NewReaderSize(file, 256*1024)
	for {
		data, err := reader.ReadSlice('\n')
		if len(data) > 0 {
			if writeErr := s.logs.Append(context.WithoutCancel(ctx), r.Scope, "/aws-glue/python-jobs/"+kind,
				*r.Run.Id, []string{strings.TrimSuffix(string(data), "\n")}); writeErr != nil {
				return writeErr
			}
		}

		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			return nil
		}
	}
}
