package codebuild

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/codebuild"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/services/dockercli"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	statusFailed    = "FAILED"
	statusSucceeded = "SUCCEEDED"
)

// Runtime runs a build in one container until its script finishes or ctx is canceled.
// Run removes its container before returning, including on cancellation.
type Runtime interface {
	Run(context.Context, *RunSpec, func(string) error) error
	Clean(context.Context, string) error
}

type RunSpec struct {
	Environment                                map[string]string
	ID, Owner, Image, Directory, Shell, Script string
	Privileged                                 bool
}

func (s *Service) UseCaller(c *catalog.Caller) { s.caller = c }

// UseRuntime recovers interrupted records before starting the queue worker.
func (s *Service) UseRuntime(
	ctx context.Context,
	rt Runtime,
	endpoint string,
	logs func(context.Context, request.Scope, string, string, []string) error,
) error {
	owner, err := dockercli.RuntimeOwner(ctx, s.store, "aws/codebuild/runtime-owner")
	if err != nil {
		return err
	}

	s.owner = owner
	if cleanErr := rt.Clean(ctx, owner); cleanErr != nil {
		return cleanErr
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, index := range tx.Keys(buildIndex) {
			key, ok, e := storage.Read[string](tx, index)
			if e != nil {
				return e
			}

			if !ok {
				continue
			}

			r, found, e := storage.Read[buildRecord](tx, *key)
			if e != nil {
				return e
			}

			if !found {
				tx.Delete(index)
				continue
			}

			if common.Value(r.Build.CurrentPhase) != "QUEUED" && !common.Value(r.Build.BuildComplete) {
				complete(&r.Build, statusFailed, "TinyStack restarted while build was executing")
				if e := storage.Write(tx, *key, r); e != nil {
					return e
				}
			}
		}

		return nil
	})
	if err != nil {
		return err
	}

	s.runtime, s.endpoint, s.writeLogs = rt, endpoint, logs
	s.active = map[string]context.CancelFunc{}
	s.bg = background.New()
	s.bg.Every(time.Second, s.visit)
	return nil
}

func (s *Service) Stop() {
	if s.bg != nil {
		s.bg.Stop()
	}
}

func (s *Service) Reset(context.Context) {
	s.mu.Lock()
	for _, cancel := range s.active {
		cancel()
	}

	s.mu.Unlock()
	// Workers are joined before reset clears the storage, then a fresh group serves new builds.
	if s.bg != nil {
		s.bg.Stop()
		s.bg = background.New()
		s.bg.Every(time.Second, s.visit)
	}
}

func (s *Service) visit(ctx context.Context) {
	var keys []string
	if err := s.store.View(ctx, func(tx storage.ReadTx) error {
		keys = tx.Keys(buildIndex)
		return nil
	}); err != nil {
		return
	}

	for _, index := range keys {
		s.mu.Lock()
		if s.active[index] != nil {
			s.mu.Unlock()
			continue
		}

		worker, cancel := context.WithCancel(ctx)
		s.active[index] = cancel
		s.mu.Unlock()
		s.bg.Go(func(context.Context) {
			defer func() {
				cancel()
				s.mu.Lock()
				delete(s.active, index)
				s.mu.Unlock()
			}()

			s.claim(worker, index)
		})
	}
}

func (s *Service) claim(ctx context.Context, index string) {
	var rec *buildRecord
	var key string
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		k, ok, e := storage.Read[string](tx, index)
		if e != nil {
			return e
		}

		if !ok {
			return nil
		}

		key = *k
		r, ok, e := storage.Read[buildRecord](tx, key)
		if e != nil {
			return e
		}

		if !ok || common.Value(r.Build.BuildComplete) {
			tx.Delete(index)
			return nil
		}

		if common.Value(r.Build.CurrentPhase) != "QUEUED" {
			return nil
		}

		if stamp()-common.Value(r.Build.StartTime) > float64(common.Value(r.Build.QueuedTimeoutInMinutes)*60) {
			complete(&r.Build, "TIMED_OUT", "Build exceeded queued timeout")
			return storage.Write(tx, key, r)
		}

		transition(&r.Build, "PROVISIONING")
		rec = r
		return storage.Write(tx, key, r)
	})
	if err != nil || rec == nil {
		return
	}

	event := common.Clone(new(rec.Build))
	event.Phases = event.Phases[:len(event.Phases)-1]
	s.emit(ctx, rec.Scope, event, true)
	s.execute(ctx, key, rec)
}

func finishPhase(b *api.Build, status, message string) {
	if len(b.Phases) == 0 {
		return
	}

	p := &b.Phases[len(b.Phases)-1]
	if p.EndTime != nil {
		return
	}

	now := stamp()
	p.EndTime = new(now)
	p.DurationInSeconds = new(int64(now - common.Value(p.StartTime)))
	p.PhaseStatus = new(status)
	if message != "" {
		p.Contexts = []api.PhaseContext{{StatusCode: new(status), Message: new(message)}}
	}
}

func transition(b *api.Build, next string) {
	finishPhase(b, statusSucceeded, "")
	b.CurrentPhase = new(next)
	b.Phases = append(b.Phases, api.BuildPhase{PhaseType: new(next), StartTime: new(stamp())})
}

func complete(b *api.Build, status, message string) {
	finishPhase(b, status, message)
	b.BuildStatus = new(status)
	b.BuildComplete = new(true)
	b.EndTime = new(stamp())
	b.CurrentPhase = new("COMPLETED")
	b.Phases = append(b.Phases, api.BuildPhase{PhaseType: new("COMPLETED"), StartTime: b.EndTime})
}

// mutate reloads the record so a worker cannot overwrite StopBuild or recreate a reset/deleted record.
func (s *Service) mutate(ctx context.Context, key string, fn func(*api.Build)) error {
	var before, after *api.Build
	var scope request.Scope
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, ok, err := storage.Read[buildRecord](tx, key)
		if err != nil {
			return err
		}

		if !ok || common.Value(r.Build.BuildComplete) {
			return context.Canceled
		}

		before = common.Clone(new(r.Build))
		scope = r.Scope
		fn(&r.Build)
		after = common.Clone(new(r.Build))
		return storage.Write(tx, key, r)
	})
	if err != nil {
		return err
	}

	if len(before.Phases) > 0 {
		old := before.Phases[len(before.Phases)-1]
		if old.EndTime == nil &&
			(len(after.Phases) > len(before.Phases) || after.Phases[len(after.Phases)-1].EndTime != nil) {
			event := common.Clone(after)
			event.Phases = event.Phases[:len(before.Phases)]
			s.emit(ctx, scope, event, true)
		}
	}

	if common.Value(before.BuildStatus) != common.Value(after.BuildStatus) {
		s.emit(ctx, scope, after, false)
	}

	return nil
}

func (s *Service) execute(parent context.Context, key string, r *buildRecord) {
	ctx, cancel := context.WithTimeout(parent, time.Duration(common.Value(r.Build.TimeoutInMinutes))*time.Minute)
	defer cancel()
	ctx = request.WithMetadata(ctx, request.Metadata{Scope: r.Scope})

	finalStatus := statusFailed
	var resultErr error
	defer func() {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			finalStatus = "TIMED_OUT"
			resultErr = ctx.Err()
		}

		if parent.Err() != nil {
			finalStatus = statusFailed
			resultErr = fmt.Errorf("TinyStack stopped or reset during build")
		}

		message := ""
		if resultErr != nil {
			message = resultErr.Error()
		}

		// Completion must survive service shutdown's canceled worker context.
		finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer finishCancel()
		_ = s.mutate(finishCtx, key, func(b *api.Build) {
			if resultErr == nil {
				finishPhase(b, statusSucceeded, "")
			}

			complete(b, finalStatus, message)
		})
	}()
	if r.Build.Logs != nil && s.writeLogs != nil {
		if err := s.writeLogs(ctx, r.Scope, common.Value(r.Build.Logs.GroupName), common.Value(r.Build.Logs.StreamName),
			[]string{"[Container] Starting CodeBuild worker"}); err != nil {
			resultErr = err
			return
		}
	}

	dir, err := os.MkdirTemp("", "tinystack-codebuild-")
	if err != nil {
		resultErr = err
		return
	}

	defer func() { _ = os.RemoveAll(dir) }()
	if err = s.mutate(ctx, key, func(b *api.Build) { transition(b, "DOWNLOAD_SOURCE") }); err != nil {
		resultErr = err
		return
	}

	spec, err := s.download(ctx, r, dir)
	if err != nil {
		resultErr = err
		return
	}

	env, err := s.environment(ctx, r, spec)
	if err != nil {
		resultErr = err
		return
	}

	marker := "TINYSTACK_" + common.RandomID(32)
	failed := false
	skipArtifacts := false
	run := &RunSpec{
		ID:          common.Value(r.Build.Id),
		Owner:       s.owner,
		Image:       common.Value(r.Build.Environment.Image),
		Directory:   dir,
		Shell:       spec.Env.Shell,
		Script:      spec.script(marker),
		Privileged:  common.Value(r.Build.Environment.PrivilegedMode),
		Environment: env,
	}

	err = s.runtime.Run(ctx, run, func(line string) error {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == marker {
			switch fields[1] {
			case "START":
				return s.mutate(ctx, key, func(b *api.Build) { transition(b, fields[2]) })
			case "END":
				status, message := statusSucceeded, ""
				if len(fields) != 4 || fields[3] != "0" {
					status = statusFailed
					message = "Buildspec command failed"
					failed = true
					if fields[2] == "INSTALL" && spec.Phases[installPhase].OnFailure != continueOnFailure ||
						fields[2] == "PRE_BUILD" && spec.Phases[preBuildPhase].OnFailure != continueOnFailure {
						skipArtifacts = true
					}
				}

				return s.mutate(ctx, key, func(b *api.Build) { finishPhase(b, status, message) })
			}
		}

		if r.Build.Logs != nil && s.writeLogs != nil {
			return s.writeLogs(
				ctx,
				r.Scope,
				common.Value(r.Build.Logs.GroupName),
				common.Value(r.Build.Logs.StreamName),
				[]string{line},
			)
		}

		return nil
	})
	if ctx.Err() != nil {
		resultErr = ctx.Err()
		return
	}

	if err != nil && !failed {
		resultErr = err
		return
	}

	if reportErr := s.collectReports(ctx, key, r, spec, dir); reportErr != nil {
		resultErr = reportErr
		return
	}
	if skipArtifacts {
		resultErr = s.mutate(ctx, key, func(b *api.Build) { transition(b, "FINALIZING") })
		return
	}

	if err = s.mutate(ctx, key, func(b *api.Build) { transition(b, "UPLOAD_ARTIFACTS") }); err != nil {
		resultErr = err
		return
	}

	location, err := s.upload(ctx, r, spec, dir)
	if err != nil {
		resultErr = err
		return
	}

	if err = s.mutate(ctx, key, func(b *api.Build) {
		if location != "" {
			b.Artifacts = &api.BuildArtifacts{
				Location:           new(location),
				EncryptionDisabled: r.Project.Artifacts.EncryptionDisabled,
			}
		}

		transition(b, "FINALIZING")
	}); err != nil {
		resultErr = err
		return
	}

	if !failed {
		finalStatus = statusSucceeded
	}
}
