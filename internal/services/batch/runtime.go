package batch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/batch"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/dockercli"
	"github.com/amjadjibon/tinystack/internal/services/ecs"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) UseCaller(c *catalog.Caller) { s.caller = c }

// UseRuntime recovers interrupted attempts before accepting work. Runtime ownership
// is independent of ECS tasks so resetting one service cannot remove the other's containers.
func (s *Service) UseRuntime(ctx context.Context, rt ecs.Runtime, logs ecs.LogWriter) error {
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		owner, ok, e := storage.Read[string](tx, "aws/batch/runtime-owner")
		if e != nil {
			return e
		}

		if ok {
			s.owner = *owner
		} else {
			s.owner = common.RandomID(32)
		}

		if e := storage.Write(tx, "aws/batch/runtime-owner", s.owner); e != nil {
			return e
		}

		for _, k := range tx.Keys(jobIndex) {
			key, ok, e := storage.Read[string](tx, k)
			if e != nil {
				return e
			}

			if !ok {
				continue
			}

			r, ok, e := storage.Read[record](tx, *key)
			if e != nil {
				return e
			}

			if !ok {
				tx.Delete(k)
				continue
			}

			st := common.Value(r.Job.Status)
			if st == statusStarting || st == statusRunning {
				finishAttempt(r, nil, "TinyStack restarted during job attempt", false)
				r.ContainerID = ""
				if e := saveJob(tx, r); e != nil {
					return e
				}
			}
		}

		return nil
	})
	if err != nil {
		return err
	}

	if cleaner, ok := rt.(interface {
		CleanOwned(context.Context, string, string, string) error
	}); ok {
		if err := cleaner.CleanOwned(ctx, "tinystack.batch.job", "tinystack.batch.store", s.owner); err != nil {
			return err
		}
	}

	s.runtime, s.writeLogs = rt, logs
	return nil
}

func (s *Service) Start() { s.bg.Every(time.Second, s.visitJobs) }

func (s *Service) Stop() {
	s.bg.Stop()
	if s.runtime != nil {
		s.runtime.Close()
	}
}

func (s *Service) Reset(ctx context.Context) {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	if s.runtime != nil {
		common.ResetRuntime(s.runtime)
	}

	// Keep the ownership identity across reset so the next startup can clean crashes during reset.
	_ = s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, k := range tx.Keys(jobIndex) {
			key, ok, err := storage.Read[string](tx, k)
			if err != nil {
				return err
			}

			if ok {
				tx.Delete(*key)
			}

			tx.Delete(k)
		}

		return nil
	})
}

func (s *Service) visitJobs(ctx context.Context) {
	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	var work []*record
	var events []*record
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		scopes := map[request.Scope]bool{}
		all := []*record{}
		for _, idx := range tx.Keys(jobIndex) {
			key, ok, e := storage.Read[string](tx, idx)
			if e != nil {
				return e
			}

			if !ok {
				continue
			}

			r, ok, e := storage.Read[record](tx, *key)
			if e != nil {
				return e
			}

			if !ok {
				tx.Delete(idx)
				continue
			}

			scopes[r.Scope] = true
		}

		for scope := range scopes {
			if e := settle(tx, scope); e != nil {
				return e
			}

			jobs, e := rows(tx, scope, kindJob)
			if e != nil {
				return e
			}

			all = append(all, jobs...)
		}

		for _, r := range all {
			if r.EventStatus != common.Value(r.Job.Status) {
				r.EventStatus = common.Value(r.Job.Status)
				if e := saveJob(tx, r); e != nil {
					return e
				}

				events = append(events, common.Clone(r))
			}
		}

		if s.runtime == nil {
			return nil
		}

		// Queue priority first, FIFO within each queue. Resource listing order is unaffected.
		priorities := map[string]int64{}
		queues := map[string]*record{}
		for scope := range scopes {
			qs, e := rows(tx, scope, kindQueue)
			if e != nil {
				return e
			}

			for _, q := range qs {
				queues[q.ARN] = q
				priorities[q.ARN] = common.Value(q.Queue.Priority)
			}
		}

		slices.SortStableFunc(all, func(a, b *record) int {
			if c := priorities[common.Value(b.Job.JobQueue)] - priorities[common.Value(a.Job.JobQueue)]; c != 0 {
				if c < 0 {
					return -1
				}

				return 1
			}

			if c := common.Value(a.Job.CreatedAt) - common.Value(b.Job.CreatedAt); c != 0 {
				if c < 0 {
					return -1
				}

				return 1
			}

			return strings.Compare(a.ID, b.ID)
		})
		used := map[string]int64{}
		for _, r := range all {
			if st := common.Value(
				r.Job.Status,
			); st == statusRunning || st == statusStarting ||
				(terminal(st) && r.ContainerID != "") {
				used[r.ComputeEnvironment] += jobCPU(r.Job.Container)
			}
		}

		for _, r := range all {
			st := common.Value(r.Job.Status)
			if r.Job.ArrayProperties != nil && r.Parent == "" {
				continue
			}

			if st == statusRunnable && !r.Cancelled {
				q := queues[common.Value(r.Job.JobQueue)]
				if q == nil || common.Value(q.Queue.State) != enabled || common.Value(q.Queue.Status) != valid {
					continue
				}

				for _, order := range q.Queue.ComputeEnvironmentOrder {
					ce, e := read(tx, r.Scope, kindCE, order.ComputeEnvironment)
					if e != nil {
						continue
					}

					if common.Value(ce.CE.State) != enabled || common.Value(ce.CE.Status) != valid ||
						common.Value(ce.CE.Type) != managed {
						continue
					}

					if ce.CE.ComputeResources == nil ||
						used[ce.ARN]+jobCPU(r.Job.Container) > common.Value(ce.CE.ComputeResources.MaxvCpus) {
						continue
					}

					r.ComputeEnvironment = ce.ARN
					used[ce.ARN] += jobCPU(r.Job.Container)
					r.Job.Status = new(statusStarting)
					r.EventStatus = statusStarting
					events = append(events, common.Clone(r))
					r.Job.Container.ExitCode = nil
					r.Job.Container.Reason = nil
					r.Job.Container.LogStreamName = nil
					r.Job.Container.TaskArn = nil
					if e := saveJob(tx, r); e != nil {
						return e
					}

					st = statusStarting
					break
				}
			}

			if st == statusStarting || st == statusRunning || terminal(st) && r.ContainerID != "" {
				work = append(work, common.Clone(r))
			}
		}

		return nil
	})
	if err != nil {
		return
	}

	for _, r := range events {
		s.emitJob(ctx, r)
	}

	for _, r := range work {
		key := r.Scope.Key(serviceName, kindJob, r.ID)
		s.mu.Lock()
		if s.busy[key] {
			s.mu.Unlock()
			continue
		}

		s.busy[key] = true
		s.mu.Unlock()
		s.bg.Go(func(ctx context.Context) {
			defer func() { s.mu.Lock(); delete(s.busy, key); s.mu.Unlock() }()
			s.lifecycle.RLock()
			defer s.lifecycle.RUnlock()
			_ = s.stepJob(ctx, r)
		})
	}
}

func jobCPU(c *api.ContainerDetail) int64 {
	for _, r := range c.ResourceRequirements {
		if common.Value(r.Type) == resourceVCPU {
			n, _ := strconv.ParseInt(common.Value(r.Value), 10, 64)
			return n
		}
	}

	return common.Value(c.Vcpus)
}

func jobMemory(c *api.ContainerDetail) int64 {
	for _, r := range c.ResourceRequirements {
		if common.Value(r.Type) == "MEMORY" {
			n, _ := strconv.ParseInt(common.Value(r.Value), 10, 64)
			return n
		}
	}

	return common.Value(c.Memory)
}

func (s *Service) stepJob(ctx context.Context, r *record) error {
	var live *record
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		v, ok, e := storage.Read[record](tx, r.Scope.Key(serviceName, kindJob, r.ID))
		if ok {
			live = v
		}

		return e
	})
	if err != nil || live == nil {
		return err
	}

	r = live
	st := common.Value(r.Job.Status)
	if terminal(st) {
		if r.ContainerID != "" {
			if e := s.stopContainer(ctx, r.ContainerID); e != nil {
				return e
			}

			if e := s.shipLogs(ctx, r); e != nil {
				return e
			}

			s.runtime.Remove(ctx, r.ContainerID)
			r.ContainerID = ""
			return s.replaceJob(ctx, r)
		}

		return nil
	}

	if st == statusStarting {
		id := common.RandomID(32)
		c := r.Job.Container
		env := map[string]string{
			"AWS_BATCH_JOB_ID":  r.ID,
			"AWS_BATCH_JQ_NAME": lastName(common.Value(r.Job.JobQueue)),
			"AWS_BATCH_CE_NAME": lastName(
				r.ComputeEnvironment,
			),
			"AWS_BATCH_JOB_ATTEMPT": strconv.Itoa(len(r.Job.Attempts) + 1),
			"AWS_REGION":            r.Scope.Region,
			"AWS_DEFAULT_REGION":    r.Scope.Region,
		}

		for _, kv := range c.Environment {
			env[common.Value(kv.Name)] = common.Value(kv.Value)
		}

		if r.Parent != "" {
			env["AWS_BATCH_JOB_ARRAY_INDEX"] = strconv.FormatInt(common.Value(r.Job.ArrayProperties.Index), 10)
			env["AWS_BATCH_JOB_ARRAY_SIZE"] = strconv.FormatInt(common.Value(r.Job.ArrayProperties.Size), 10)
		}

		spec := &ecs.ContainerSpec{
			Image: common.Value(c.Image), Name: "default", Command: c.Command,
			CPU: jobCPU(c) * 1024, Memory: jobMemory(c), Environment: env,
			Labels: map[string]string{"tinystack.batch.job": r.ARN, "tinystack.batch.store": s.owner},
		}

		started, e := s.runtime.Start(ctx, spec)
		if e != nil {
			finishAttempt(r, nil, "CannotStartContainerError: "+e.Error(), false)
			return s.replaceJob(ctx, r)
		}

		r.ContainerID = started.ID
		r.Job.Status = new(statusRunning)
		r.Job.StartedAt = new(time.Now().UnixMilli())
		r.Job.StoppedAt = nil
		r.LogCursor = time.Time{}
		c.TaskArn = new(fmt.Sprintf("arn:aws:ecs:%s:%s:task/batch/%s", r.Scope.Region, r.Scope.AccountID, id))
		definition := lastName(common.Value(r.Job.JobDefinition))
		definition, _, _ = strings.Cut(definition, ":")
		prefix := definition
		if l := c.LogConfiguration; l != nil && l.Options["awslogs-stream-prefix"] != "" {
			prefix = l.Options["awslogs-stream-prefix"]
		}

		c.LogStreamName = new(prefix + "/default/" + id)
		if e := s.replaceJob(ctx, r); e != nil {
			s.runtime.Remove(ctx, started.ID)
			return e
		}

		return nil
	}

	if st != statusRunning {
		return nil
	}

	reason := ""
	exited, code, err := s.runtime.State(ctx, r.ContainerID)
	if err != nil {
		if !errors.Is(err, ecs.ErrGone) {
			return err
		}

		reason = "Container disappeared during job attempt"
		exited = true
	}

	timeout := !exited && r.Job.Timeout != nil &&
		(time.Now().UnixMilli()-common.Value(r.Job.StartedAt))/1000 >= common.Value(
			r.Job.Timeout.AttemptDurationSeconds,
		)
	if timeout {
		if e := s.stopContainer(ctx, r.ContainerID); e != nil {
			return e
		}

		reason = "Job attempt duration exceeded timeout"
		exited = true
	}

	if e := s.shipLogs(ctx, r); e != nil {
		return e
	}

	if !exited {
		return s.replaceJob(ctx, r)
	}

	var exitCode *int64
	if err == nil && !timeout {
		exitCode = new(code)
	}

	if reason == "" && code != 0 {
		reason = "Essential container in task exited"
	}

	s.runtime.Remove(ctx, r.ContainerID)
	r.ContainerID = ""
	finishAttempt(r, exitCode, reason, timeout)
	return s.replaceJob(ctx, r)
}

func lastName(v string) string {
	i := strings.LastIndex(v, "/")
	return v[i+1:]
}

func finishAttempt(r *record, code *int64, reason string, noRetry bool) {
	j := r.Job
	stopped := time.Now().UnixMilli()
	j.Container.ExitCode = code
	j.Container.Reason = new(reason)
	j.Attempts = append(j.Attempts, api.AttemptDetail{
		StartedAt:    j.StartedAt,
		StoppedAt:    new(stopped),
		StatusReason: new(reason),
		Container: &api.AttemptContainerDetail{
			TaskArn:       j.Container.TaskArn,
			ExitCode:      code,
			Reason:        new(reason),
			LogStreamName: j.Container.LogStreamName,
		},
	})
	j.StoppedAt = new(stopped)
	j.StatusReason = new(reason)
	if code != nil && *code == 0 && reason == "" {
		j.Status = new(statusSucceeded)
		return
	}

	if !noRetry && retry(j.RetryStrategy, len(j.Attempts), code, reason, reason) {
		j.Status = new(statusRunnable)
		return
	}

	j.Status = new(statusFailed)
}

func wildcard(pattern, value string) bool {
	expression := "^" + strings.ReplaceAll(regexp.QuoteMeta(pattern), "\\*", ".*") + "$"
	match, _ := regexp.MatchString(expression, value)
	return match
}

func retry(strategy *api.RetryStrategy, attempts int, code *int64, reason, statusReason string) bool {
	if strategy == nil || int64(attempts) >= common.Value(common.OrElse(strategy.Attempts, new(int64(1)))) {
		return false
	}

	for _, rule := range strategy.EvaluateOnExit {
		match := true
		if rule.OnExitCode != nil {
			match = code != nil && wildcard(*rule.OnExitCode, strconv.FormatInt(common.Value(code), 10))
		}

		if rule.OnReason != nil {
			match = match && wildcard(*rule.OnReason, reason)
		}

		if rule.OnStatusReason != nil {
			match = match && wildcard(*rule.OnStatusReason, statusReason)
		}

		if match {
			return strings.EqualFold(common.Value(rule.Action), "RETRY")
		}
	}

	return true
}

// replaceJob commits only if no concurrent cancel/terminate changed the job.
func (s *Service) replaceJob(ctx context.Context, r *record) error {
	kept := false
	changed := false
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		cur, ok, e := storage.Read[record](tx, r.Scope.Key(serviceName, kindJob, r.ID))
		if e != nil {
			return e
		}

		if !ok {
			return nil
		}

		if terminal(common.Value(cur.Job.Status)) {
			if terminal(common.Value(r.Job.Status)) && r.ContainerID == "" {
				cur.ContainerID = ""
				return saveJob(tx, cur)
			}

			return nil
		}

		changed = common.Value(cur.Job.Status) != common.Value(r.Job.Status)
		r.EventStatus = common.Value(r.Job.Status)
		if e := saveJob(tx, r); e != nil {
			return e
		}

		kept = true
		return settle(tx, r.Scope)
	})
	if !kept && r.ContainerID != "" {
		s.runtime.Remove(ctx, r.ContainerID)
	}

	if kept && changed {
		s.emitJob(ctx, r)
	}

	return err
}

func (s *Service) emitJob(ctx context.Context, r *record) {
	if s.caller == nil {
		return
	}

	detail, err := json.Marshal(r.Job)
	if err != nil {
		return
	}

	ctx = request.WithMetadata(ctx, request.Metadata{Scope: r.Scope})
	_, _ = s.caller.Call(ctx, "eventbridge", "PutEvents", map[string]any{"Entries": []any{
		map[string]any{
			"Source":     "aws.batch",
			"DetailType": "Batch Job State Change",
			"Detail":     string(detail),
			"Resources":  []any{r.ARN},
		},
	}})
}

func (s *Service) shipLogs(ctx context.Context, r *record) error {
	lines, err := s.runtime.Logs(ctx, r.ContainerID, r.LogCursor)
	if err != nil {
		if errors.Is(err, ecs.ErrGone) || dockercli.Gone(err) {
			return nil
		}

		return err
	}

	if len(lines) == 0 || s.writeLogs == nil {
		return nil
	}

	messages := make([]string, 0, len(lines))
	for _, line := range lines {
		messages = append(messages, line.Message)
	}

	group := "/aws/batch/job"
	if l := r.Job.Container.LogConfiguration; l != nil && l.Options["awslogs-group"] != "" {
		group = l.Options["awslogs-group"]
	}

	if err := s.writeLogs(ctx, r.Scope, group, common.Value(r.Job.Container.LogStreamName), messages); err != nil {
		return err
	}

	r.LogCursor = lines[len(lines)-1].Time
	return nil
}

func (s *Service) stopContainer(ctx context.Context, id string) error {
	if rt, ok := s.runtime.(interface {
		StopContainer(context.Context, string) error
	}); ok {
		return rt.StopContainer(ctx, id)
	}

	return nil
}
