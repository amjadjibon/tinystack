package s3

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const accessLogQueueBytes = 4 << 20

// AccessRecord contains request and response metadata for an S3 server access log line.
type AccessRecord struct {
	Time                                      time.Time
	Bucket, Key, Region                       string
	RemoteIP, Requester, RequestID, Operation string
	URI, ErrorCode, Referer, UserAgent        string
	Version, HostID, Signature, Cipher        string
	Authentication, Host, TLS                 string
	Status                                    int
	Bytes, ObjectSize                         int64
	TotalMS, TurnaroundMS                     int64
}

type accessLogBatch struct {
	Day     time.Time
	Content string
	Source  string
	Target  string
	Prefix  string
	Mode    string
	Scope   request.Scope
}

func (b *accessLogBatch) indexKey() string {
	settings := b.Target + "\x00" + b.Prefix + "\x00" + b.Mode + "\x00" + b.Day.Format(time.DateOnly)
	digest := sha256.Sum256([]byte(settings))
	return fmt.Sprintf("%s%s/%s/access-log/%s/%x", workIndex, b.Scope.AccountID, b.Scope.Region, b.Source, digest)
}

func logField(value string) string {
	if value == "" {
		return "-"
	}

	return url.PathEscape(value)
}

func logString(value string) string {
	if len(value) > 2048 {
		value = value[:2048]
	}

	if value == "" {
		return "-"
	}

	return strconv.Quote(value)
}

func logNumber(value int64) string {
	if value < 0 {
		return "-"
	}

	return strconv.FormatInt(value, 10)
}

func (r *AccessRecord) line(owner string) string {
	return strings.Join([]string{
		canonicalID(owner), r.Bucket, r.Time.UTC().Format("[02/Jan/2006:15:04:05 -0700]"),
		logField(r.RemoteIP), logField(r.Requester), logField(r.RequestID), r.Operation,
		logField(r.Key), logString(r.URI), strconv.Itoa(r.Status), logField(r.ErrorCode),
		logNumber(r.Bytes), logNumber(r.ObjectSize), logNumber(r.TotalMS), logNumber(r.TurnaroundMS),
		logString(r.Referer), logString(r.UserAgent), logField(r.Version), logField(r.HostID),
		logField(r.Signature), logField(r.Cipher), logField(r.Authentication), logField(r.Host), logField(r.TLS),
		"-", "-", "-",
	}, " ") + "\n"
}

// RecordAccess queues one request for best-effort log delivery in the bucket owner's account.
func (s *Service) RecordAccess(ctx context.Context, r *AccessRecord) error {
	var batch accessLogBatch
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		index, found, err := storage.Read[bucketIndex](tx, indexPrefix+r.Bucket)
		if err != nil || !found || index.Region != r.Region {
			return err
		}

		base := index.Account + "/" + index.Region + "/s3/" + r.Bucket
		meta, found, err := storage.Read[bucketMeta](tx, base)
		if err != nil || !found || meta.Logging == nil {
			return err
		}

		cfg := meta.Logging
		batch = accessLogBatch{
			Scope:  request.Scope{AccountID: index.Account, Region: index.Region},
			Source: r.Bucket, Target: common.Value(cfg.TargetBucket), Prefix: common.Value(cfg.TargetPrefix),
		}

		if f := cfg.TargetObjectKeyFormat; f != nil && f.PartitionedPrefix != nil {
			batch.Mode = common.Value(f.PartitionedPrefix.PartitionDateSource)
			if batch.Mode == "EventTime" {
				batch.Day = r.Time.UTC().Truncate(lifecycleDay)
			}
		}

		return nil
	})
	if err != nil || batch.Target == "" {
		return err
	}

	line := r.line(batch.Scope.AccountID)
	key := batch.indexKey()
	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		pending, found, readErr := storage.Read[accessLogBatch](tx, key)
		if readErr != nil {
			return readErr
		}

		if found {
			batch.Content = pending.Content
		}

		if int64(len(batch.Content)+len(line)) > min(s.maxObjectBytes, accessLogQueueBytes) {
			return nil
		}

		batch.Content += line
		return storage.Write(tx, key, &batch)
	})
}

func (batch *accessLogBatch) key(now time.Time) string {
	prefix := batch.Prefix
	if batch.Mode != "" {
		if !batch.Day.IsZero() {
			now = batch.Day
		}

		prefix += batch.Scope.AccountID + "/" + batch.Scope.Region + "/" + batch.Source + "/" +
			now.Format("2006/01/02/")
	}

	return prefix + now.Format("2006-01-02-15-04-05") + "-" + rand.Text()
}

// deliverAccessLog claims a batch before writing it directly to S3. The internal
// write never enters the request recorder, even when the destination also collects logs.
func (s *Service) deliverAccessLog(ctx context.Context, indexKey string) error {
	var batch *accessLogBatch
	if err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		var readErr error
		batch, _, readErr = storage.Read[accessLogBatch](tx, indexKey)
		if readErr != nil {
			return readErr
		}

		tx.Delete(indexKey)
		return nil
	}); err != nil {
		return err
	}

	if batch == nil {
		return nil
	}

	m := request.Metadata{Scope: batch.Scope, RequestID: rand.Text(), Service: "s3", Operation: "PutObject"}
	logCtx := request.WithMetadata(ctx, m)
	key := batch.key(s.now().UTC())
	_, err := s.PutObject(logCtx, &api.PutObjectRequest{
		Bucket: new(batch.Target), ExpectedBucketOwner: new(batch.Scope.AccountID), Key: new(key),
		Body: []byte(batch.Content), ContentType: new("text/plain"),
	})
	return err
}
