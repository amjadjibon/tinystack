package s3

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const restoreStandard = "Standard"

func (o *object) archived() bool {
	return o.StorageClass == storageGlacier || o.StorageClass == storageDeepArchive
}

func (o *object) readable(now time.Time) bool {
	return !o.archived() || (o.RestoreReady <= now.UnixMilli() && o.RestoreUntil > now.UnixMilli())
}

func (o *object) restoreHeader(now time.Time) *string {
	if !o.archived() || o.RestoreUntil <= now.UnixMilli() {
		return nil
	}

	if o.RestoreReady > now.UnixMilli() {
		return new(`ongoing-request="true"`)
	}

	date := time.UnixMilli(o.RestoreUntil).UTC().Format(http.TimeFormat)
	return new(fmt.Sprintf(`ongoing-request="false", expiry-date=%q`, date))
}

func invalidObjectState() error {
	return fail(http.StatusForbidden, "InvalidObjectState", "The operation is not valid for the object's storage class")
}

func restoreDelay(r *api.RestoreRequest) (time.Duration, error) {
	if r == nil || common.Value(r.Days) < 1 || common.Value(r.Days) > 2147483647 {
		return 0, invalidArgument("Restore requires a positive number of days")
	}

	tier := common.Value(r.Tier)
	if r.GlacierJobParameters != nil {
		if r.Tier != nil || r.GlacierJobParameters.Tier == nil {
			return 0, malformedXML()
		}

		tier = *r.GlacierJobParameters.Tier
	}

	switch tier {
	case "", restoreStandard:
		return time.Second, nil
	case "Expedited":
		return 250 * time.Millisecond, nil
	case "Bulk":
		return 2 * time.Second, nil
	default:
		return 0, invalidArgument("Invalid archive retrieval tier")
	}
}

func (s *Service) RestoreObject(ctx context.Context, in *api.RestoreObjectRequest) (*api.RestoreObjectOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if keyErr := validKey(in.Key); keyErr != nil {
		return nil, keyErr
	}

	delay, err := restoreDelay(in.RestoreRequest)
	if err != nil {
		return nil, err
	}

	status := http.StatusAccepted
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		base, resolveErr := resolve(tx, m, in.Bucket, in.ExpectedBucketOwner)
		if resolveErr != nil {
			return resolveErr
		}

		key, version := *in.Key, common.Value(in.VersionId)
		obj, _, readErr := locate(tx, base, key, version)
		if readErr != nil {
			return readErr
		}

		if !obj.archived() {
			return fail(http.StatusForbidden, "ObjectAlreadyInActiveTierError", "Object is in an active storage tier")
		}

		if obj.StorageClass == storageDeepArchive && delay < time.Second {
			return invalidArgument("Expedited retrieval is unavailable for DEEP_ARCHIVE")
		}

		now := s.now()
		if obj.RestoreReady > now.UnixMilli() {
			return fail(http.StatusConflict, "RestoreAlreadyInProgress", "Object restore is already in progress")
		}

		ready := now.Add(delay)
		if obj.readable(now) {
			status = http.StatusOK
			ready = now
		}

		day := cmp.Or(s.day, lifecycleDay)
		until := ready.UnixMilli() + *in.RestoreRequest.Days*day.Milliseconds()
		if day == lifecycleDay {
			until = time.UnixMilli(until).UTC().Truncate(day).Add(day).UnixMilli()
		}

		_, writeErr := rewrite(tx, base, key, version, func(stored *object) {
			stored.RestoreReady, stored.RestoreUntil = ready.UnixMilli(), until
		})
		return writeErr
	})
	if err != nil {
		return nil, err
	}

	if response := request.ResponsePayload(ctx); response != nil {
		response.Status = status
	}

	return &api.RestoreObjectOutput{}, nil
}
