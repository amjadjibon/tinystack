package transcribe

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	s3api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/transcribe"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) StartTranscriptionJob(
	ctx context.Context,
	in *api.StartTranscriptionJobRequest,
) (*api.StartTranscriptionJobResponse, error) {
	if in.OutputBucketName == nil {
		return nil, invalid("OutputBucketName is required; service-managed output buckets are not emulated")
	}

	if in.LanguageCode == nil {
		return nil, invalid("LanguageCode is required; automatic language identification is not emulated")
	}

	if in.Media.RedactedMediaFileUri != nil {
		return nil, invalid("redacted media is not emulated")
	}

	if err := s.file(ctx, common.Value(in.Media.MediaFileUri)); err != nil {
		return nil, err
	}

	if _, err := s.buckets.HeadBucket(ctx, &s3api.HeadBucketRequest{Bucket: in.OutputBucketName}); err != nil {
		return nil, invalid("OutputBucketName must name an existing TinyStack bucket: %s", err)
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name := common.Value(in.TranscriptionJobName)
	key := common.Value(in.OutputKey)
	// https://docs.aws.amazon.com/transcribe/latest/APIReference/API_StartTranscriptionJob.html
	if !strings.HasSuffix(key, ".json") {
		if key != "" {
			key = strings.TrimSuffix(key, "/") + "/"
		}

		key += name + ".json"
	}

	job := &api.TranscriptionJob{
		TranscriptionJobName:   in.TranscriptionJobName,
		TranscriptionJobStatus: new(statusQueued),
		LanguageCode:           in.LanguageCode,
		Media:                  in.Media,
		MediaFormat:            in.MediaFormat,
		MediaSampleRateHertz:   in.MediaSampleRateHertz,
		CreationTime:           new(now()),
		Settings:               in.Settings,
		Tags:                   in.Tags,
	}

	r := record{
		Job:      job,
		Name:     name,
		Language: common.Value(in.LanguageCode),
		State:    statusQueued,
		Bucket:   common.Value(in.OutputBucketName),
		Key:      key,
		Tags:     tags,
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, ok := tx.Get(scope.Key(serviceName, kindJob, name)); ok {
			return conflict("transcription job already exists")
		}

		if in.Settings != nil {
			settings := in.Settings
			if (settings.VocabularyFilterName == nil) != (settings.VocabularyFilterMethod == nil) {
				return invalid("VocabularyFilterName and VocabularyFilterMethod must be supplied together")
			}

			references := []struct {
				name *string
				kind string
			}{
				{settings.VocabularyName, kindVocabulary},
				{settings.VocabularyFilterName, kindFilter},
			}

			for _, ref := range references {
				kind, n := ref.kind, ref.name
				if n == nil {
					continue
				}

				vocabulary, e := read(tx, scope, kind, *n)
				if e != nil {
					return e
				}

				if vocabulary.State != statusReady || vocabulary.Language != r.Language {
					return invalid("vocabulary or filter must be READY and match LanguageCode")
				}
			}
		}

		if e := storage.Write(tx, scope.Key(serviceName, kindJob, name), r); e != nil {
			return e
		}

		return queue(tx, scope, kindJob, name)
	})
	return &api.StartTranscriptionJobResponse{TranscriptionJob: job}, err
}

func (s *Service) GetTranscriptionJob(
	ctx context.Context,
	in *api.GetTranscriptionJobRequest,
) (*api.GetTranscriptionJobResponse, error) {
	r, err := s.get(ctx, kindJob, common.Value(in.TranscriptionJobName))
	if err != nil {
		return nil, err
	}

	return &api.GetTranscriptionJobResponse{TranscriptionJob: r.Job}, nil
}

func (s *Service) DeleteTranscriptionJob(
	ctx context.Context,
	in *api.DeleteTranscriptionJobRequest,
) (*api.DeleteTranscriptionJobOutput, error) {
	return &api.DeleteTranscriptionJobOutput{}, s.remove(
		ctx,
		kindJob,
		common.Value(in.TranscriptionJobName),
	)
}

func (s *Service) ListTranscriptionJobs(
	ctx context.Context,
	in *api.ListTranscriptionJobsRequest,
) (*api.ListTranscriptionJobsResponse, error) {
	rows, err := s.list(ctx, kindJob, common.Value(in.JobNameContains), common.Value(in.Status))
	if err != nil {
		return nil, err
	}

	items := []api.TranscriptionJobSummary{}
	for _, r := range rows {
		p := r.Job
		items = append(
			items,
			api.TranscriptionJobSummary{
				TranscriptionJobName:   p.TranscriptionJobName,
				TranscriptionJobStatus: p.TranscriptionJobStatus,
				CreationTime:           p.CreationTime,
				StartTime:              p.StartTime,
				CompletionTime:         p.CompletionTime,
				LanguageCode:           p.LanguageCode,
				FailureReason:          p.FailureReason,
				OutputLocationType:     new("CUSTOMER_BUCKET"),
			},
		)
	}

	page, next, err := common.Page(
		items,
		func(v api.TranscriptionJobSummary) string { return common.Value(v.TranscriptionJobName) },
		in.NextToken,
		limit(in.MaxResults),
		invalid("invalid NextToken"),
	)
	return &api.ListTranscriptionJobsResponse{TranscriptionJobSummaries: page, NextToken: next, Status: in.Status}, err
}

func (s *Service) Start(ctx context.Context) error {
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, key := range tx.Keys(indexPrefix) {
			w, _, e := storage.Read[work](tx, key)
			if e != nil {
				return e
			}

			if w.Kind != kindJob {
				continue
			}

			r, e := read(tx, w.Scope, w.Kind, w.Name)
			if e != nil {
				return e
			}

			r.State = statusFailed
			r.Job.TranscriptionJobStatus = new(r.State)
			r.Job.FailureReason = new("interrupted by service restart")
			r.Job.CompletionTime = new(now())
			if e := storage.Write(tx, w.Scope.Key(serviceName, w.Kind, w.Name), r); e != nil {
				return e
			}

			tx.Delete(key)
		}

		return nil
	})
	if err != nil {
		return err
	}

	s.workers.Every(100*time.Millisecond, s.tick)
	return nil
}

func (s *Service) Stop() { s.workers.Stop() }

func (s *Service) tick(ctx context.Context) {
	var jobs []*work
	if err := s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		jobs, e = storage.Records[work](tx, indexPrefix)
		return e
	}); err != nil {
		return
	}

	for _, w := range jobs {
		if ctx.Err() != nil {
			return
		}

		var job *record
		_ = s.store.Update(ctx, func(tx storage.WriteTx) error {
			r, e := read(tx, w.Scope, w.Kind, w.Name)
			if e != nil {
				return e
			}

			if w.Kind != kindJob {
				r.State = statusReady
				tx.Delete(indexPrefix + w.Scope.AccountID + "/" + w.Scope.Region + "/" + w.Kind + "/" + w.Name)
			} else {
				if r.State != statusQueued {
					return nil
				}

				r.State = statusInProgress
				r.Job.TranscriptionJobStatus = new(r.State)
				r.Job.StartTime = new(now())
				job = r
			}

			return storage.Write(tx, w.Scope.Key(serviceName, w.Kind, w.Name), r)
		})
		if job == nil {
			continue
		}

		jobCtx := request.WithMetadata(ctx, request.Metadata{Scope: w.Scope})
		s.execute(jobCtx, *w, job)
	}
}

func (s *Service) execute(ctx context.Context, w work, r *record) {
	text, err := s.match(ctx, w.Scope, common.Value(r.Job.Media.MediaFileUri), r.Language)
	if err == nil {
		// No word items, timestamps or confidence values can be inferred from a text fixture.
		data, e := json.Marshal(
			map[string]any{
				"jobName":   r.Name,
				"accountId": w.Scope.AccountID,
				"status":    statusCompleted,
				"results":   map[string]any{"transcripts": []map[string]string{{"transcript": text}}},
			},
		)
		err = e
		if err == nil {
			_, err = s.buckets.PutObject(
				ctx,
				&s3api.PutObjectRequest{
					Bucket:      new(r.Bucket),
					Key:         new(r.Key),
					Body:        data,
					ContentType: new("application/json"),
				},
			)
		}
	}

	r.State = statusCompleted
	if err != nil {
		r.State = statusFailed
		r.Job.FailureReason = new(err.Error())
	} else {
		r.Job.Transcript = &api.Transcript{TranscriptFileUri: new("s3://" + r.Bucket + "/" + r.Key)}
	}

	r.Job.TranscriptionJobStatus = new(r.State)
	r.Job.CompletionTime = new(now())
	_ = s.store.Update(ctx, func(tx storage.WriteTx) error {
		current, e := read(tx, w.Scope, w.Kind, w.Name)
		if e != nil {
			return e
		}

		if current.State != statusInProgress {
			return nil
		}

		r.Tags = current.Tags
		r.Job.Tags = current.Job.Tags
		tx.Delete(indexPrefix + w.Scope.AccountID + "/" + w.Scope.Region + "/" + w.Kind + "/" + w.Name)
		return storage.Write(tx, w.Scope.Key(serviceName, w.Kind, w.Name), r)
	})
}
