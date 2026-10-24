package translate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"strings"
	"time"

	s3api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/translate"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const indexPrefix = "aws/translate/index/"

type jobRecord struct {
	Properties api.TextTranslationJobProperties
	Digest     string
	Token      string
	Input      []string
}

type work struct {
	Scope request.Scope
	Kind  string
	Name  string
}

func s3URI(v string) (bucket, key string, err error) {
	u, err := url.Parse(v)
	if err != nil || u.Scheme != "s3" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return "", "", invalid("expected s3://bucket/key")
	}

	return u.Host, strings.TrimPrefix(u.Path, "/"), nil
}

func queue(tx storage.WriteTx, scope request.Scope, kind, name string) error {
	return storage.Write(
		tx,
		indexPrefix+scope.AccountID+"/"+scope.Region+"/"+kind+"/"+name,
		work{Scope: scope, Kind: kind, Name: name},
	)
}

func (s *Service) StartTextTranslationJob(
	ctx context.Context,
	in *api.StartTextTranslationJobRequest,
) (*api.StartTextTranslationJobResponse, error) {
	if err := s.options(ctx, in.TerminologyNames, in.Settings); err != nil {
		return nil, err
	}

	if err := contentType(common.Value(in.InputDataConfig.ContentType)); err != nil {
		return nil, err
	}

	_, found, err := s.roles.RoleByARN(ctx, common.Value(in.DataAccessRoleArn))
	if err != nil {
		return nil, err
	}

	if !found {
		return nil, invalid("DataAccessRoleArn must name an existing IAM role")
	}

	bucket, prefix, err := s3URI(common.Value(in.InputDataConfig.S3Uri))
	if err != nil {
		return nil, err
	}

	outputBucket, _, err := s3URI(common.Value(in.OutputDataConfig.S3Uri))
	if err != nil {
		return nil, err
	}

	if _, err = s.buckets.HeadBucket(ctx, &s3api.HeadBucketRequest{Bucket: new(outputBucket)}); err != nil {
		return nil, invalid("output S3 bucket must exist: %s", err)
	}

	var objects []string
	var token *string
	for {
		listing, e := s.buckets.ListObjectsV2(
			ctx,
			&s3api.ListObjectsV2Request{Bucket: new(bucket), Prefix: new(prefix), ContinuationToken: token},
		)
		if e != nil {
			return nil, invalid("input S3 prefix must exist: %s", e)
		}

		for _, object := range listing.Contents {
			objects = append(objects, common.Value(object.Key))
		}

		token = listing.NextContinuationToken
		if token == nil {
			break
		}
	}

	if len(objects) == 0 {
		return nil, invalid("InputDataConfig S3 prefix has no objects")
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	id := common.RandomID(32, "0123456789abcdef")
	r := jobRecord{
		Digest: fingerprint(in),
		Token:  common.Value(in.ClientToken),
		Input:  objects,
		Properties: api.TextTranslationJobProperties{
			JobId:               new(id),
			JobName:             in.JobName,
			JobStatus:           new(statusSubmitted),
			InputDataConfig:     in.InputDataConfig,
			OutputDataConfig:    in.OutputDataConfig,
			DataAccessRoleArn:   in.DataAccessRoleArn,
			SourceLanguageCode:  in.SourceLanguageCode,
			TargetLanguageCodes: in.TargetLanguageCodes,
			TerminologyNames:    in.TerminologyNames,
			ParallelDataNames:   in.ParallelDataNames,
			SubmittedTime:       new(now()),
		},
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, name := range in.ParallelDataNames {
			p, e := read[parallelRecord](tx, scope, kindParallel, name)
			if e != nil {
				return e
			}

			if common.Value(p.Properties.Status) != statusActive {
				return invalid("parallel data must be ACTIVE")
			}
		}

		rows, e := storage.Records[jobRecord](tx, scope.Prefix(serviceName, "job"))
		if e != nil {
			return e
		}

		for _, old := range rows {
			if r.Token != "" && old.Token == r.Token {
				if old.Digest != r.Digest {
					return invalid("ClientToken parameters changed")
				}

				r = *old
				return nil
			}
		}

		if e := storage.Write(tx, scope.Key(serviceName, "job", id), r); e != nil {
			return e
		}

		return queue(tx, scope, "job", id)
	})
	return &api.StartTextTranslationJobResponse{JobId: r.Properties.JobId, JobStatus: r.Properties.JobStatus}, err
}

func (s *Service) DescribeTextTranslationJob(
	ctx context.Context,
	in *api.DescribeTextTranslationJobRequest,
) (*api.DescribeTextTranslationJobResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeTextTranslationJobResponse{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, e := read[jobRecord](tx, scope, "job", common.Value(in.JobId))
		if e != nil {
			return e
		}

		out.TextTranslationJobProperties = new(r.Properties)
		return nil
	})
	return out, err
}

func (s *Service) ListTextTranslationJobs(
	ctx context.Context,
	in *api.ListTextTranslationJobsRequest,
) (*api.ListTextTranslationJobsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	items := []api.TextTranslationJobProperties{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		rows, e := storage.Records[jobRecord](tx, scope.Prefix(serviceName, "job"))
		if e != nil {
			return e
		}

		for _, r := range rows {
			p := r.Properties
			if f := in.Filter; f != nil {
				if f.JobName != nil && common.Value(p.JobName) != *f.JobName ||
					f.JobStatus != nil && common.Value(p.JobStatus) != *f.JobStatus ||
					f.SubmittedBeforeTime != nil && common.Value(p.SubmittedTime) >= *f.SubmittedBeforeTime ||
					f.SubmittedAfterTime != nil && common.Value(p.SubmittedTime) <= *f.SubmittedAfterTime {
					continue
				}
			}

			items = append(items, p)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(
		items,
		func(v api.TextTranslationJobProperties) string { return common.Value(v.JobId) },
		in.NextToken,
		limit(in.MaxResults),
		invalid("invalid NextToken"),
	)
	return &api.ListTextTranslationJobsResponse{TextTranslationJobPropertiesList: page, NextToken: next}, err
}

func (s *Service) StopTextTranslationJob(
	ctx context.Context,
	in *api.StopTextTranslationJobRequest,
) (*api.StopTextTranslationJobResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	status := statusStopped
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := read[jobRecord](tx, scope, "job", common.Value(in.JobId))
		if e != nil {
			return e
		}

		if v := common.Value(r.Properties.JobStatus); v != statusSubmitted && v != statusInProgress {
			return invalid("job cannot be stopped in state %s", v)
		}

		r.Properties.JobStatus = new(status)
		r.Properties.EndTime = new(now())
		tx.Delete(indexPrefix + scope.AccountID + "/" + scope.Region + "/job/" + common.Value(in.JobId))
		return storage.Write(tx, scope.Key(serviceName, "job", common.Value(in.JobId)), r)
	})
	return &api.StopTextTranslationJobResponse{JobId: in.JobId, JobStatus: new(status)}, err
}

// Start recovers interrupted jobs before starting the persistent queue.
func (s *Service) Start(ctx context.Context) error {
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, key := range tx.Keys(indexPrefix) {
			w, _, e := storage.Read[work](tx, key)
			if e != nil {
				return e
			}

			if w.Kind != "job" {
				continue
			}

			r, e := read[jobRecord](tx, w.Scope, "job", w.Name)
			if e != nil {
				return e
			}

			r.Properties.JobStatus = new(statusFailed)
			r.Properties.Message = new("interrupted by service restart")
			r.Properties.EndTime = new(now())
			if e := storage.Write(tx, w.Scope.Key(serviceName, "job", w.Name), r); e != nil {
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

		if w.Kind == kindParallel {
			_ = s.store.Update(ctx, func(tx storage.WriteTx) error {
				r, e := read[parallelRecord](tx, w.Scope, w.Kind, w.Name)
				if e != nil {
					return e
				}

				r.Properties.Status = new(statusActive)
				r.Properties.LastUpdatedAt = new(now())
				if r.Properties.LatestUpdateAttemptStatus != nil {
					r.Properties.LatestUpdateAttemptStatus = new(statusActive)
				}

				tx.Delete(indexPrefix + w.Scope.AccountID + "/" + w.Scope.Region + "/" + w.Kind + "/" + w.Name)
				return storage.Write(tx, w.Scope.Key(serviceName, w.Kind, w.Name), r)
			})
			continue
		}

		var job *jobRecord
		_ = s.store.Update(ctx, func(tx storage.WriteTx) error {
			r, e := read[jobRecord](tx, w.Scope, "job", w.Name)
			if e != nil {
				return e
			}

			if common.Value(r.Properties.JobStatus) != statusSubmitted {
				return nil
			}

			r.Properties.JobStatus = new(statusInProgress)
			job = r
			return storage.Write(tx, w.Scope.Key(serviceName, "job", w.Name), r)
		})
		if job == nil {
			continue
		}

		jobCtx := request.WithMetadata(ctx, request.Metadata{Scope: w.Scope})
		s.execute(jobCtx, *w, job)
	}
}

func (s *Service) execute(ctx context.Context, w work, r *jobRecord) {
	p := &r.Properties
	inBucket, _, _ := s3URI(common.Value(p.InputDataConfig.S3Uri))
	outBucket, prefix, _ := s3URI(common.Value(p.OutputDataConfig.S3Uri))
	prefix = strings.TrimSuffix(prefix, "/") + "/" + w.Scope.AccountID + "-TranslateText-" + w.Name + "/"
	prefix = strings.TrimPrefix(prefix, "/")
	var good, bad, outputs int64
	failures := map[string]int64{}
	details := map[string][]map[string]string{}
	message := ""
	for _, key := range r.Input {
		data, readErr := s.buckets.Object(ctx, inBucket, key, "")
		documentFailed := false
		for _, target := range p.TargetLanguageCodes {
			var translated *Fixture
			err := readErr
			if err == nil {
				translated, err = s.match(ctx, common.Value(p.SourceLanguageCode), target, string(data))
			}

			if err == nil {
				output := prefix + target + "." + path.Base(key)
				_, err = s.buckets.PutObject(
					ctx,
					&s3api.PutObjectRequest{
						Bucket:      new(outBucket),
						Key:         new(output),
						Body:        []byte(common.Value(translated.TranslatedText)),
						ContentType: p.InputDataConfig.ContentType,
					},
				)
			}

			if err == nil {
				outputs++
				details[target] = append(details[target], map[string]string{
					"sourceFile": path.Base(key), "targetFile": target + "." + path.Base(key),
				})
			}

			if err != nil {
				failures[target]++
				documentFailed = true
				message = err.Error()
			}
		}

		if documentFailed {
			bad++
		} else {
			good++
		}
	}

	p.JobStatus = new(statusCompleted)
	if bad > 0 {
		p.JobStatus = new(statusCompletedWithError)
	}

	if outputs == 0 {
		p.JobStatus = new(statusFailed)
	}

	p.Message = new(message)
	p.EndTime = new(now())
	p.JobDetails = &api.JobDetails{
		InputDocumentsCount:      new(int64(len(r.Input))),
		TranslatedDocumentsCount: new(good),
		DocumentsWithErrorsCount: new(bad),
	}

	p.OutputDataConfig.S3Uri = new("s3://" + outBucket + "/" + prefix)
	for _, target := range p.TargetLanguageCodes {
		details, _ := json.Marshal(
			map[string]any{
				"sourceLanguageCode":             common.Value(p.SourceLanguageCode),
				"targetLanguageCode":             target,
				"documentCountWithCustomerError": fmt.Sprint(failures[target]),
				"details":                        details[target],
				"inputDataPrefix":                common.Value(p.InputDataConfig.S3Uri),
				"outputDataPrefix":               common.Value(p.OutputDataConfig.S3Uri),
			},
		)
		if _, err := s.buckets.PutObject(
			ctx,
			&s3api.PutObjectRequest{
				Bucket:      new(outBucket),
				Key:         new(prefix + "details/" + target + ".auxiliary-translation-details.json"),
				Body:        details,
				ContentType: new("application/json"),
			},
		); err != nil {
			p.JobStatus = new(statusFailed)
			p.Message = new(err.Error())
		}
	}

	_ = s.store.Update(ctx, func(tx storage.WriteTx) error {
		current, e := read[jobRecord](tx, w.Scope, "job", w.Name)
		if e != nil {
			return e
		}

		if common.Value(current.Properties.JobStatus) != statusInProgress {
			return nil
		}

		tx.Delete(indexPrefix + w.Scope.AccountID + "/" + w.Scope.Region + "/job/" + w.Name)
		return storage.Write(tx, w.Scope.Key(serviceName, "job", w.Name), r)
	})
}
