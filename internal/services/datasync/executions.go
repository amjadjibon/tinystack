package datasync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/datasync"
	s3api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const queuePrefix = "aws/datasync/index/"

// batchSize bounds the objects one worker step lists and copies before it commits progress.
const batchSize = 100

type queueEntry struct {
	Scope    request.Scope
	ARN      string
	Sequence int64
}

// Execution ARNs nest under task ARNs; storage keeps them in their own kind.
func executionID(arn string) string { return strings.Replace(arn, ":task/", ":execution/", 1) }

func queueKey(scope request.Scope, arn string, sequence int64) string {
	task, _, _ := strings.Cut(arn, "/execution/")
	return fmt.Sprintf("%s%s/%s/%s/%020d", queuePrefix, scope.AccountID, scope.Region, task, sequence)
}

func terminal(status *string) bool {
	return common.Value(status) == statusSuccess || common.Value(status) == statusError
}

func (s *Service) StartTaskExecution(
	ctx context.Context,
	in *api.StartTaskExecutionRequest,
) (*api.StartTaskExecutionResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	task := common.Value(in.TaskArn)
	arn := task + "/execution/exec-" + common.RandomID(17, "0123456789abcdef")
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		t, e := readRecord[taskRecord](tx, scope, kindTask, task)
		if e != nil {
			return e
		}

		source, e := readRecord[locationRecord](tx, scope, kindLocation, common.Value(t.SourceLocationArn))
		if e != nil {
			return e
		}

		dest, e := readRecord[locationRecord](tx, scope, kindLocation, common.Value(t.DestinationLocationArn))
		if e != nil {
			return e
		}

		if source.Kind != locationS3 || dest.Kind != locationS3 {
			return invalid("Only S3-to-S3 transfers run locally")
		}

		if source.Bucket == dest.Bucket &&
			(strings.HasPrefix(source.Prefix, dest.Prefix) || strings.HasPrefix(dest.Prefix, source.Prefix)) {
			return invalid("Source and destination S3 prefixes must not overlap")
		}

		options := mergeOptions(t.Options, in.OverrideOptions)
		includes, excludes := t.Includes, t.Excludes
		if in.Includes != nil {
			includes = in.Includes
		}

		if in.Excludes != nil {
			excludes = in.Excludes
		}

		if optionsErr := validateTaskOptions(options, includes, excludes); optionsErr != nil {
			return optionsErr
		}

		executions, e := storage.Records[executionRecord](tx, scope.Prefix(serviceName, kindExecution))
		if e != nil {
			return e
		}

		for _, x := range executions {
			if x.Task == task && !terminal(x.Status) && common.Value(options.TaskQueueing) == "DISABLED" {
				return invalid("Task already has an active execution and queueing is disabled")
			}
		}

		t.Sequence++
		if writeErr := writeRecord(tx, scope, kindTask, task, t); writeErr != nil {
			return writeErr
		}

		x := executionRecord{
			Sequence: t.Sequence,
			DescribeTaskExecutionResponse: api.DescribeTaskExecutionResponse{
				TaskExecutionArn:         new(arn),
				Status:                   new("QUEUED"),
				TaskMode:                 new(modeBasic),
				Options:                  options,
				Includes:                 includes,
				Excludes:                 excludes,
				StartTime:                new(timestamp()),
				EstimatedFilesToTransfer: new(int64(0)),
				EstimatedBytesToTransfer: new(int64(0)),
				FilesTransferred:         new(int64(0)),
				BytesWritten:             new(int64(0)),
				BytesTransferred:         new(int64(0)),
				FilesSkipped:             new(int64(0)),
				FilesDeleted:             new(int64(0)),
				FilesVerified:            new(int64(0)),
				FilesPrepared:            new(int64(0)),
				EstimatedFilesToDelete:   new(int64(0)),
				Result: &api.TaskExecutionResultDetail{
					PrepareStatus:  new(phasePending),
					TransferStatus: new(phasePending),
					VerifyStatus:   new(phasePending),
				},
			},
			Task: task, Source: source, Destination: dest, Tags: tags,
		}

		if writeErr := writeRecord(tx, scope, kindExecution, executionID(arn), x); writeErr != nil {
			return writeErr
		}

		return storage.Write(
			tx,
			queueKey(scope, arn, x.Sequence),
			queueEntry{Scope: scope, ARN: arn, Sequence: x.Sequence},
		)
	})
	return &api.StartTaskExecutionResponse{TaskExecutionArn: new(arn)}, err
}

func (s *Service) DescribeTaskExecution(
	ctx context.Context,
	in *api.DescribeTaskExecutionRequest,
) (*api.DescribeTaskExecutionResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var r executionRecord
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		r, e = readRecord[executionRecord](tx, scope, kindExecution, executionID(common.Value(in.TaskExecutionArn)))
		return e
	})
	return new(r.DescribeTaskExecutionResponse), err
}

func (s *Service) CancelTaskExecution(
	ctx context.Context,
	in *api.CancelTaskExecutionRequest,
) (*api.CancelTaskExecutionResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	arn := common.Value(in.TaskExecutionArn)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := readRecord[executionRecord](tx, scope, kindExecution, executionID(arn))
		if e != nil {
			return e
		}

		if terminal(r.Status) {
			return invalid("Execution has already finished")
		}

		updated := new(r)
		failExecution(updated, invalid("Task execution cancelled"))
		return s.saveExecution(tx, scope, updated)
	})
	return &api.CancelTaskExecutionResponse{}, err
}

func failExecution(r *executionRecord, err error) {
	switch common.Value(r.Status) {
	case statusLaunching, statusPreparing:
		r.Result.PrepareStatus = new(statusError)
	case statusTransferring:
		r.Result.TransferStatus = new(statusError)
	case statusVerifying:
		r.Result.VerifyStatus = new(statusError)
	}

	r.Status = new(statusError)
	r.EndTime = new(timestamp())
	r.Result.ErrorCode = new("InvalidRequestException")
	r.Result.ErrorDetail = new(err.Error())
}

func (s *Service) saveExecution(tx storage.WriteTx, scope request.Scope, r *executionRecord) error {
	arn := common.Value(r.TaskExecutionArn)
	if terminal(r.Status) {
		tx.Delete(queueKey(scope, arn, r.Sequence))
		t, e := readRecord[taskRecord](tx, scope, kindTask, r.Task)
		if e != nil {
			return e
		}

		if common.Value(t.CurrentTaskExecutionArn) == arn {
			t.Status = new("AVAILABLE")
			t.CurrentTaskExecutionArn = nil
			if writeErr := writeRecord(tx, scope, kindTask, r.Task, t); writeErr != nil {
				return writeErr
			}
		}
	}

	return writeRecord(tx, scope, kindExecution, executionID(arn), r)
}

func (s *Service) tick(ctx context.Context) {
	var entries []*queueEntry
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		entries, e = storage.Records[queueEntry](tx, queuePrefix)
		return e
	})
	if err != nil {
		return
	}

	for _, entry := range entries {
		if ctx.Err() != nil {
			return
		}

		workctx := request.WithMetadata(ctx, request.Metadata{Scope: entry.Scope, Service: serviceName})
		s.advance(workctx, *entry)
	}
}

func (s *Service) advance(ctx context.Context, q queueEntry) {
	r := new(executionRecord)
	claimed := false
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		loaded, e := readRecord[executionRecord](tx, q.Scope, kindExecution, executionID(q.ARN))
		if e != nil {
			return e
		}

		*r = loaded

		if terminal(r.Status) {
			tx.Delete(queueKey(q.Scope, q.ARN, q.Sequence))
			return nil
		}

		t, e := readRecord[taskRecord](tx, q.Scope, kindTask, r.Task)
		if e != nil {
			return e
		}

		if common.Value(r.Status) == "QUEUED" {
			if t.CurrentTaskExecutionArn != nil {
				return nil
			}

			t.CurrentTaskExecutionArn = new(q.ARN)
			t.Status = new(statusRunning)
			r.Status = new(statusLaunching)
			r.LaunchTime = new(timestamp())
			if writeErr := writeRecord(tx, q.Scope, kindTask, r.Task, t); writeErr != nil {
				return writeErr
			}

			return s.saveExecution(tx, q.Scope, r)
		}

		if common.Value(r.Status) == statusLaunching {
			r.Status = new(statusPreparing)
			r.Result.PrepareStatus = new(phasePending)
			return s.saveExecution(tx, q.Scope, r)
		}

		r.Revision++
		claimed = true
		return s.saveExecution(tx, q.Scope, r)
	})
	if err != nil || !claimed {
		return
	}

	expected := common.Value(r.Status)
	err = s.step(ctx, r)
	if ctx.Err() != nil {
		return
	}

	commitErr := s.store.Update(ctx, func(tx storage.WriteTx) error {
		current, e := readRecord[executionRecord](tx, q.Scope, kindExecution, executionID(q.ARN))
		if e != nil {
			return e
		}

		if common.Value(current.Status) != expected || current.Revision != r.Revision {
			return nil
		}

		// Tags may have changed while the object I/O ran.
		r.Tags = current.Tags
		if err != nil {
			failExecution(r, err)
		}

		return s.saveExecution(tx, q.Scope, r)
	})
	if commitErr != nil {
		return
	}
}

func (s *Service) listObjects(
	ctx context.Context,
	loc *locationRecord,
	cursor *string,
) (*s3api.ListObjectsV2Output, error) {
	return s.buckets.ListObjectsV2(ctx, &s3api.ListObjectsV2Request{
		Bucket: new(loc.Bucket), Prefix: new(loc.Prefix), ContinuationToken: cursor, MaxKeys: new(int64(batchSize)),
	})
}

func (s *Service) step(ctx context.Context, r *executionRecord) error {
	switch common.Value(r.Status) {
	case statusPreparing:
		return s.prepare(ctx, r)
	case statusTransferring:
		return s.transfer(ctx, r)
	case statusVerifying:
		return s.verify(ctx, r)
	}

	return invalid("Unsupported execution state")
}

func (s *Service) prepare(ctx context.Context, r *executionRecord) error {
	out, err := s.listObjects(ctx, new(r.Source), r.Cursor)
	if err != nil {
		return err
	}

	for _, obj := range out.Contents {
		key := strings.TrimPrefix(common.Value(obj.Key), r.Source.Prefix)
		if selected(r, key) {
			*r.FilesPrepared++
			skip, e := s.skipObject(ctx, r, key, common.Value(obj.Size), common.Value(obj.ETag))
			if e != nil {
				return e
			}

			if !skip {
				*r.EstimatedFilesToTransfer++
				*r.EstimatedBytesToTransfer += common.Value(obj.Size)
			}
		}
	}

	r.Cursor = out.NextContinuationToken
	if !common.Value(out.IsTruncated) {
		r.Cursor = nil
		r.Status = new(statusTransferring)
		r.Result.PrepareStatus = new(statusSuccess)
	}

	return nil
}

func (s *Service) transfer(ctx context.Context, r *executionRecord) error {
	if r.Deleting {
		return s.deleteAbsent(ctx, r)
	}

	out, err := s.listObjects(ctx, new(r.Source), r.Cursor)
	if err != nil {
		return err
	}

	for _, obj := range out.Contents {
		key := strings.TrimPrefix(common.Value(obj.Key), r.Source.Prefix)
		if !selected(r, key) {
			continue
		}

		destKey := r.Destination.Prefix + key
		skip, e := s.skipObject(ctx, r, key, common.Value(obj.Size), common.Value(obj.ETag))
		if e != nil {
			return e
		}

		if skip {
			*r.FilesSkipped++
			continue
		}

		taggingDirective, metadataDirective := "COPY", "COPY"
		if common.Value(r.Options.ObjectTags) == optionNone {
			taggingDirective = "REPLACE"
		}

		if common.Value(r.Options.ObjectMetadata) == optionNone {
			metadataDirective = "REPLACE"
		}

		_, err = s.buckets.CopyObject(ctx, &s3api.CopyObjectRequest{
			Bucket:            new(r.Destination.Bucket),
			Key:               new(destKey),
			CopySource:        new(url.PathEscape(r.Source.Bucket + "/" + common.Value(obj.Key))),
			StorageClass:      r.Destination.StorageClass,
			TaggingDirective:  new(taggingDirective),
			MetadataDirective: new(metadataDirective),
		})
		if err != nil {
			return err
		}

		*r.FilesTransferred++
		*r.BytesTransferred += common.Value(obj.Size)
		*r.BytesWritten += common.Value(obj.Size)
		if common.Value(r.Options.VerifyMode) == "ONLY_FILES_TRANSFERRED" {
			if verifyErr := s.verifyObject(ctx, r, key); verifyErr != nil {
				return verifyErr
			}

			*r.FilesVerified++
		}
	}

	r.Cursor = out.NextContinuationToken
	if !common.Value(out.IsTruncated) {
		r.Cursor = nil
		if common.Value(r.Options.PreserveDeletedFiles) == "REMOVE" {
			r.Deleting = true
		} else {
			r.Status = new(statusVerifying)
			r.Result.TransferStatus = new(statusSuccess)
		}
	}

	return nil
}

func (s *Service) deleteAbsent(ctx context.Context, r *executionRecord) error {
	out, err := s.listObjects(ctx, new(r.Destination), r.Cursor)
	if err != nil {
		return err
	}

	for _, obj := range out.Contents {
		key := strings.TrimPrefix(common.Value(obj.Key), r.Destination.Prefix)
		if !selected(r, key) {
			continue
		}

		_, e := s.buckets.HeadObject(
			ctx,
			&s3api.HeadObjectRequest{Bucket: new(r.Source.Bucket), Key: new(r.Source.Prefix + key)},
		)
		if e == nil {
			continue
		}

		if !missingObject(e) {
			return e
		}

		_, e = s.buckets.DeleteObject(ctx, &s3api.DeleteObjectRequest{Bucket: new(r.Destination.Bucket), Key: obj.Key})
		if e != nil {
			return e
		}

		*r.FilesDeleted++
		*r.EstimatedFilesToDelete++
	}

	r.Cursor = out.NextContinuationToken
	if !common.Value(out.IsTruncated) {
		r.Cursor = nil
		r.Status = new(statusVerifying)
		r.Result.TransferStatus = new(statusSuccess)
	}

	return nil
}

func (s *Service) verifyObject(ctx context.Context, r *executionRecord, key string) error {
	src, err := s.buckets.Object(ctx, r.Source.Bucket, r.Source.Prefix+key, "")
	if err != nil {
		return err
	}

	dst, err := s.buckets.Object(ctx, r.Destination.Bucket, r.Destination.Prefix+key, "")
	if err != nil {
		return err
	}

	if !bytes.Equal(src, dst) {
		return invalid("Verification failed for %s", key)
	}

	return nil
}

func (s *Service) verify(ctx context.Context, r *executionRecord) error {
	if common.Value(r.Options.VerifyMode) == verifyPointInTime {
		out, err := s.listObjects(ctx, new(r.Source), r.Cursor)
		if err != nil {
			return err
		}

		for _, obj := range out.Contents {
			key := strings.TrimPrefix(common.Value(obj.Key), r.Source.Prefix)
			if selected(r, key) {
				if verifyErr := s.verifyObject(ctx, r, key); verifyErr != nil {
					return verifyErr
				}

				*r.FilesVerified++
			}
		}

		r.Cursor = out.NextContinuationToken
		if common.Value(out.IsTruncated) {
			return nil
		}
	}

	r.Status = new(statusSuccess)
	r.EndTime = new(timestamp())
	r.Result.VerifyStatus = new(statusSuccess)

	r.Result.TotalDuration = new(int64((common.Value(r.EndTime) - common.Value(r.StartTime)) * 1000))
	return nil
}

func missingObject(err error) bool {
	awsErr, ok := errors.AsType[*awserror.Error](err)
	return ok && (awsErr.Code == "NoSuchKey" || awsErr.Code == "NotFound")
}

func (s *Service) skipObject(
	ctx context.Context,
	r *executionRecord,
	key string,
	size int64,
	etag string,
) (bool, error) {
	dest, err := s.buckets.HeadObject(
		ctx,
		&s3api.HeadObjectRequest{Bucket: new(r.Destination.Bucket), Key: new(r.Destination.Prefix + key)},
	)
	if err != nil {
		if missingObject(err) {
			return false, nil
		}

		return false, err
	}

	return common.Value(r.Options.OverwriteMode) == "NEVER" ||
		common.Value(r.Options.TransferMode) == "CHANGED" && common.Value(dest.ContentLength) == size &&
			common.Value(dest.ETag) == etag, nil
}
