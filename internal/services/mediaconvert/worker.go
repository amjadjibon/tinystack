package mediaconvert

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/mediaconvert"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// finishAfter is how long, in seconds, a job stays PROGRESSING.
const finishAfter = 0.6

func active(j *api.Job) bool { return *j.Status == statusSubmitted || *j.Status == statusProgressing }

func (s *Service) advance(ctx context.Context) {
	var indices []*jobIndex
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		var err error
		indices, err = storage.Records[jobIndex](tx, indexPrefix)
		return err
	})
	if err != nil {
		return
	}

	for _, idx := range indices {
		s.advanceJob(request.WithMetadata(ctx, request.Metadata{Scope: idx.Scope}), idx)
	}
}

func (s *Service) advanceJob(ctx context.Context, idx *jobIndex) {
	r, readErr := s.get(ctx, kindJob, new(idx.ID))
	if readErr != nil || !active(r.Job) {
		return
	}

	// Inputs are probed only when the job is due to finish.
	failure := ""
	if *r.Job.Status == statusProgressing && common.Unix(time.Now())-*r.Job.Timing.StartTime >= finishAfter {
		for _, input := range r.Job.Settings.Inputs {
			bucket, key, _ := strings.Cut(strings.TrimPrefix(*input.FileInput, "s3://"), "/")
			_, err := s.caller.Call(ctx, "s3", "HeadObject", map[string]any{"Bucket": bucket, "Key": key})
			if err != nil {
				failure = fmt.Sprintf(
					"Unable to open input file [%s]: [Failed probe/open: [Can't read input stream: "+
						"[Failed to read data: HeadObject failed]]]",
					*input.FileInput,
				)
				break
			}
		}
	}

	var changed *api.Job
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		current, err := read(tx, idx.Scope, kindJob, idx.ID)
		if err != nil {
			return err
		}

		j := current.Job
		if !active(j) || *j.Status != *r.Job.Status {
			return nil
		}

		now := common.Unix(time.Now())
		// Each persisted step has a minimum dwell time so cancellation and restart are observable.
		if *j.Status == statusSubmitted {
			q, err := read(tx, idx.Scope, kindQueue, *j.Queue)
			if err != nil {
				return err
			}

			if *q.Queue.Status == "PAUSED" || now-*j.Timing.SubmitTime < 0.4 {
				return nil
			}

			if countsErr := queueCounts(tx, idx.Scope, q.Queue); countsErr != nil {
				return countsErr
			}

			if q.Queue.ConcurrentJobs != nil && *q.Queue.ProgressingJobsCount >= *q.Queue.ConcurrentJobs {
				return nil
			}

			waiting, err := storage.Records[record](tx, idx.Scope.Prefix(serviceName, kindJob))
			if err != nil {
				return err
			}

			for _, other := range waiting {
				if other.Name == current.Name || *other.Job.Status != statusSubmitted || *other.Job.Queue != *j.Queue {
					continue
				}

				higher := common.Value(other.Job.Priority) > common.Value(j.Priority)
				earlier := common.Value(other.Job.Priority) == common.Value(j.Priority) &&
					other.Sequence < current.Sequence
				if higher || earlier {
					return nil
				}
			}

			j.Status = new(statusProgressing)
			j.CurrentPhase = new("PROBING")
			j.JobPercentComplete = new(int64(50))
			j.Timing.StartTime = new(now)
		} else {
			if now-*j.Timing.StartTime < finishAfter {
				return nil
			}

			j.Status = new("COMPLETE")
			j.CurrentPhase = nil
			j.JobPercentComplete = new(int64(100))
			j.Timing.FinishTime = new(now)
			if failure != "" {
				j.Status = new("ERROR")
				j.ErrorCode = new(int64(1404))
				j.ErrorMessage = new(failure)
				j.JobPercentComplete = nil
			}

			tx.Delete(indexKey(idx.Scope, idx.ID))
		}

		if err := write(tx, idx.Scope, kindJob, current); err != nil {
			return err
		}

		changed = j
		return nil
	})

	if err == nil && changed != nil {
		s.emit(ctx, changed)
	}
}

func (s *Service) emit(ctx context.Context, j *api.Job) {
	if s.caller == nil {
		return
	}

	m, _ := request.FromContext(ctx)
	detail := map[string]any{
		"timestamp":    time.Now().UnixMilli(),
		"accountId":    m.AccountID,
		"queue":        *j.Queue,
		"jobId":        *j.Id,
		"status":       *j.Status,
		"userMetadata": j.UserMetadata,
	}

	if j.ErrorCode != nil {
		detail["errorCode"] = *j.ErrorCode
		detail["errorMessage"] = *j.ErrorMessage
	}

	if j.Timing != nil {
		timing := map[string]any{}
		if j.Timing.SubmitTime != nil {
			timing["submitTime"] = int64(*j.Timing.SubmitTime * 1000)
		}

		if j.Timing.StartTime != nil {
			timing["startTime"] = int64(*j.Timing.StartTime * 1000)
		}

		if j.Timing.FinishTime != nil {
			timing["finishTime"] = int64(*j.Timing.FinishTime * 1000)
		}

		detail["timing"] = timing
	}

	data, err := json.Marshal(detail)
	if err != nil {
		return
	}

	_, _ = s.caller.Call(ctx, "eventbridge", "PutEvents", map[string]any{"Entries": []any{
		map[string]any{
			"Source":     "aws.mediaconvert",
			"DetailType": "MediaConvert Job State Change",
			"Detail":     string(data),
			"Resources":  []any{*j.Arn},
		},
	}})
}
