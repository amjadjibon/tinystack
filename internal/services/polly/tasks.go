package polly

import (
	"context"
	"net/url"
	"time"
	"unicode/utf8"
	"uuid"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/polly"
	s3api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	kindTask    = "synthesis-task"
	indexPrefix = "aws/polly/index/"
	scheduled   = "scheduled"
	inProgress  = "inProgress"
	completed   = "completed"
	failed      = "failed"
	oggExt      = "ogg"
)

type work struct {
	Input           *speechInput
	Scope           request.Scope
	ID, Bucket, Key string
}

func (w *work) taskKey() string { return w.Scope.Key(serviceName, kindTask, w.ID) }
func (w *work) indexKey() string {
	return indexPrefix + w.Scope.AccountID + "/" + w.Scope.Region + "/" + w.ID
}

func (s *Service) StartSpeechSynthesisTask(ctx context.Context, in *api.StartSpeechSynthesisTaskInput) (
	*api.StartSpeechSynthesisTaskOutput, error,
) {
	key := &speechInput{
		Text: common.Value(in.Text), VoiceID: common.Value(in.VoiceId),
		Engine: common.Value(in.Engine), LanguageCode: common.Value(in.LanguageCode),
		OutputFormat: common.Value(in.OutputFormat), SampleRate: common.Value(in.SampleRate),
		SpeechMarkTypes: in.SpeechMarkTypes, TextType: common.Value(in.TextType),
	}
	if err := key.normalize(taskCharacters); err != nil {
		return nil, err
	}

	if key.SampleRate == "44100" || key.SampleRate == "48000" {
		return nil, awserror.New("InvalidSampleRateException", "synthesis tasks support up to 24000 Hz")
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if _, err = s.buckets.HeadBucket(ctx, &s3api.HeadBucketRequest{Bucket: in.OutputS3BucketName}); err != nil {
		return nil, awserror.New("InvalidS3BucketException", "OutputS3BucketName must name an existing local bucket")
	}

	extension := key.OutputFormat
	if extension == oggVorbis {
		extension = oggExt
	}

	id := uuid.NewV4().String()
	w := work{
		Scope: scope, ID: id, Bucket: common.Value(in.OutputS3BucketName),
		Key: common.Value(in.OutputS3KeyPrefix) + id + "." + extension, Input: key,
	}
	task := api.SynthesisTask{
		TaskId: new(id), TaskStatus: new(scheduled), CreationTime: common.SecondsMilli(time.Now()),
		Engine: new(key.Engine), VoiceId: new(key.VoiceID), LanguageCode: new(key.LanguageCode),
		TextType: new(key.TextType), OutputFormat: new(key.OutputFormat), SpeechMarkTypes: key.SpeechMarkTypes,
		RequestCharacters: new(int64(utf8.RuneCountInString(key.Text))),
	}
	if key.SampleRate != "" {
		task.SampleRate = new(key.SampleRate)
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if e := storage.Write(tx, w.taskKey(), task); e != nil {
			return e
		}

		return storage.Write(tx, w.indexKey(), w)
	})
	if err != nil {
		return nil, err
	}

	return &api.StartSpeechSynthesisTaskOutput{SynthesisTask: &task}, nil
}

func (s *Service) GetSpeechSynthesisTask(ctx context.Context, in *api.GetSpeechSynthesisTaskInput) (
	*api.GetSpeechSynthesisTaskOutput, error,
) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var task *api.SynthesisTask
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		row, ok, e := storage.Read[api.SynthesisTask](tx, scope.Key(serviceName, kindTask, common.Value(in.TaskId)))
		if e != nil {
			return e
		}

		if !ok {
			return awserror.New("SynthesisTaskNotFoundException", "synthesis task does not exist")
		}

		task = row
		return nil
	})
	return &api.GetSpeechSynthesisTaskOutput{SynthesisTask: task}, err
}

func (s *Service) ListSpeechSynthesisTasks(ctx context.Context, in *api.ListSpeechSynthesisTasksInput) (
	*api.ListSpeechSynthesisTasksOutput, error,
) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var tasks []api.SynthesisTask
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		rows, e := storage.Records[api.SynthesisTask](tx, scope.Prefix(serviceName, kindTask))
		if e != nil {
			return e
		}

		for _, row := range rows {
			if in.Status == nil || *in.Status == common.Value(row.TaskStatus) {
				tasks = append(tasks, *row)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(
		tasks, func(row api.SynthesisTask) string { return common.Value(row.TaskId) },
		in.NextToken, int(*common.OrElse(in.MaxResults, new(int64(100)))), invalidToken(),
	)
	return &api.ListSpeechSynthesisTasksOutput{SynthesisTasks: page, NextToken: next}, err
}

func (s *Service) Start(ctx context.Context) error {
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		jobs, e := storage.Records[work](tx, indexPrefix)
		if e != nil {
			return e
		}

		for _, job := range jobs {
			task, ok, e := storage.Read[api.SynthesisTask](tx, job.taskKey())
			if e != nil {
				return e
			}

			if !ok {
				tx.Delete(job.indexKey())
				continue
			}

			if common.Value(task.TaskStatus) == inProgress {
				task.TaskStatus = new(failed)
				task.TaskStatusReason = new("interrupted by service restart")
				if e := storage.Write(tx, job.taskKey(), task); e != nil {
					return e
				}

				tx.Delete(job.indexKey())
			}
		}

		return nil
	})
	if err != nil {
		return err
	}

	s.workers.Every(time.Second, s.tick)
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

	for _, job := range jobs {
		if ctx.Err() != nil {
			return
		}

		var claimed bool
		err := s.store.Update(ctx, func(tx storage.WriteTx) error {
			row, ok, e := storage.Read[api.SynthesisTask](tx, job.taskKey())
			if e != nil || !ok || common.Value(row.TaskStatus) != scheduled {
				return e
			}

			row.TaskStatus = new(inProgress)
			claimed = true
			return storage.Write(tx, job.taskKey(), row)
		})
		if err == nil && claimed {
			s.execute(request.WithMetadata(ctx, request.Metadata{Scope: job.Scope}), job)
		}
	}
}

func (s *Service) execute(ctx context.Context, job *work) {
	data, contentType, outputErr := s.audio(ctx, job.Input)
	if outputErr == nil {
		_, outputErr = s.buckets.PutObject(ctx, &s3api.PutObjectRequest{
			Bucket: new(job.Bucket), Key: new(job.Key), Body: data, ContentType: new(contentType),
		})
	}

	_ = s.store.Update(ctx, func(tx storage.WriteTx) error {
		task, ok, e := storage.Read[api.SynthesisTask](tx, job.taskKey())
		if e != nil || !ok || common.Value(task.TaskStatus) != inProgress {
			return e
		}

		if outputErr != nil {
			task.TaskStatus = new(failed)
			task.TaskStatusReason = new(outputErr.Error())
		} else {
			task.TaskStatus = new(completed)
			task.OutputUri = new((&url.URL{Scheme: "s3", Host: job.Bucket, Path: "/" + job.Key}).String())
		}

		if e := storage.Write(tx, job.taskKey(), task); e != nil {
			return e
		}

		tx.Delete(job.indexKey())
		return nil
	})
}
