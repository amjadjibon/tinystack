package s3

import (
	"cmp"
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const lifecycleDay = 24 * time.Hour

type lifecycleTarget struct {
	bucket, key string
	upload      bool
}

type lifecycleChange struct {
	name, version, class string
}

// StartLifecycle runs lifecycle actions using the same partition-wide index that reserves bucket names.
func (s *Service) StartLifecycle(day time.Duration, logger *slog.Logger) {
	s.day = cmp.Or(day, lifecycleDay)
	s.workers.Every(time.Second, func(ctx context.Context) {
		if err := s.sweepLifecycle(ctx); err != nil && ctx.Err() == nil {
			logger.Error("S3 lifecycle sweep failed", "error", err)
		}
	})
}

func (s *Service) sweepLifecycle(ctx context.Context) error {
	var names []string
	if err := s.store.View(ctx, func(tx storage.ReadTx) error {
		names = tx.Keys(indexPrefix)
		return nil
	}); err != nil {
		return err
	}

	for _, name := range names {
		if err := s.sweepLifecycleBucket(ctx, strings.TrimPrefix(name, indexPrefix)); err != nil {
			return err
		}
	}

	return nil
}

func (s *Service) sweepLifecycleBucket(ctx context.Context, name string) error {
	var index bucketIndex
	var targets []lifecycleTarget
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		found, err := readJSON(tx, indexPrefix+name, &index)
		if err != nil || !found {
			return err
		}

		base := index.Account + "/" + index.Region + "/s3/" + name
		meta, found, err := storage.Read[bucketMeta](tx, base)
		if err != nil || !found || meta.Lifecycle == nil {
			return err
		}

		var keys []string
		for _, kind := range []string{"/o/", "/v/"} {
			for _, key := range tx.Keys(base + kind) {
				keys = append(keys, strings.TrimPrefix(key, base+kind))
			}
		}

		slices.Sort(keys)
		for _, key := range slices.Compact(keys) {
			targets = append(targets, lifecycleTarget{bucket: base, key: key})
		}

		prefix := base + "/u/"
		for _, key := range tx.Keys(prefix) {
			targets = append(targets, lifecycleTarget{bucket: base, key: strings.TrimPrefix(key, prefix), upload: true})
		}

		return nil
	})
	if err != nil {
		return err
	}

	scope := request.Scope{AccountID: index.Account, Region: index.Region}
	for _, target := range targets {
		var cfg *api.NotificationConfiguration
		var changes []lifecycleChange
		err := s.store.Update(ctx, func(tx storage.WriteTx) error {
			meta, found, err := storage.Read[bucketMeta](tx, target.bucket)
			if err != nil || !found || meta.Lifecycle == nil || meta.CreatedAt != index.CreatedAt {
				return err
			}

			cfg = meta.Notifications
			now := s.now()
			if target.upload {
				return s.lifecycleAbort(tx, meta.Lifecycle, target, now)
			}

			changes, err = s.lifecycleObject(tx, meta, target, now)
			return err
		})
		if err != nil {
			return err
		}

		for _, change := range changes {
			m := request.Metadata{Scope: scope, RequestID: rand.Text()}
			s.notify(request.WithMetadata(ctx, m), m, cfg, objectEvent{
				name: change.name, version: change.version, storageClass: change.class,
				reason: "Lifecycle Expiration", bucket: name, key: target.key,
			})
		}
	}

	return nil
}

func (s *Service) lifecycleAbort(tx storage.WriteTx, cfg *lifecycle, target lifecycleTarget, now time.Time) error {
	u, found, err := storage.Read[upload](tx, uploadKey(target.bucket, target.key))
	if err != nil || !found {
		return err
	}

	for i := range cfg.Rules {
		rule := &cfg.Rules[i]
		a := rule.AbortIncompleteMultipartUpload
		if a != nil && ruleMatches(rule, u.Key, nil) &&
			!now.Before(s.lifecycleDue(u.Initiated, a.DaysAfterInitiation, nil)) {
			removeUpload(tx, target.bucket, target.key)
			break
		}
	}

	return nil
}

func (s *Service) lifecycleObject(
	tx storage.WriteTx, meta *bucketMeta, t lifecycleTarget, now time.Time,
) ([]lifecycleChange, error) {
	current, found, err := storage.Read[object](tx, t.bucket+"/o/"+t.key)
	if err != nil {
		return nil, err
	}

	h, err := history(tx, t.bucket, t.key)
	if err != nil {
		return nil, err
	}

	var changes []lifecycleChange
	if found && allowsLifecycle(current) {
		expires, _ := s.expiration(meta.Lifecycle, t.key, current)
		canExpire := checkLock(current, false, now) == nil || meta.Versioning == versioningEnabled ||
			(meta.Versioning == versioningSuspended && current.VersionID != "")
		if canExpire && !expires.IsZero() && !now.Before(expires) {
			removed, deleteErr := deleteCurrent(tx, t.bucket, t.key, now.UnixMilli(), false)
			if deleteErr != nil {
				return nil, deleteErr
			}

			name := eventLifecycleDelete
			if removed.marker {
				name = eventLifecycleMarker
			}

			return []lifecycleChange{{name: name, version: removed.version}}, nil
		}

		class := s.lifecycleTransition(meta.Lifecycle, t.key, current, 0, 0, now)
		if class != "" {
			current.StorageClass = class
			if writeErr := storage.Write(tx, t.bucket+"/o/"+t.key, current); writeErr != nil {
				return nil, writeErr
			}

			changes = append(changes, lifecycleChange{
				name: eventLifecycleTransition, class: class, version: common.Value(reported(meta.Versioning, current)),
			})
		}
	} else if !found && len(h) == 1 && h[0].DeleteMarker && allowsLifecycle(&h[0]) &&
		s.expireMarker(meta.Lifecycle, t.key, h[0].LastModified, now) {
		tx.Delete(t.bucket + "/v/" + t.key)
		return []lifecycleChange{{name: eventLifecycleDelete, version: versionOf(&h[0])}}, nil
	}

	more, err := s.lifecycleHistory(tx, meta.Lifecycle, t, h, current, now)
	return append(changes, more...), err
}

func (s *Service) lifecycleHistory(
	tx storage.WriteTx, cfg *lifecycle, t lifecycleTarget, h []object, current *object, now time.Time,
) ([]lifecycleChange, error) {
	var changes []lifecycleChange
	repaired := false
	kept := h[:0]
	newer := int64(0)
	var previous int64
	if current != nil {
		previous = current.LastModified
	}

	for i := range h {
		o := h[i]
		since := cmp.Or(o.NoncurrentSince, previous)
		previous = o.LastModified
		if o.DeleteMarker {
			kept = append(kept, o)
			continue
		}

		if o.NoncurrentSince == 0 && since != 0 {
			o.NoncurrentSince = since
			repaired = true
		}

		if !allowsLifecycle(&o) {
			kept = append(kept, o)
			newer++
			continue
		}

		if s.expireNoncurrent(cfg, t.key, &o, since, newer, now) && checkLock(&o, false, now) == nil {
			tx.Delete(bodyKey(t.bucket, t.key, &o))
			changes = append(changes, lifecycleChange{name: eventLifecycleDelete, version: versionOf(&o)})
		} else {
			class := s.lifecycleTransition(cfg, t.key, &o, since, newer, now)
			if class != "" {
				o.StorageClass = class
				changes = append(changes, lifecycleChange{
					name: eventLifecycleTransition, class: class, version: versionOf(&o),
				})
			}

			kept = append(kept, o)
		}

		newer++
	}

	if len(changes) > 0 || repaired {
		return changes, saveHistory(tx, t.bucket, t.key, kept)
	}

	return nil, nil
}

func allowsLifecycle(o *object) bool {
	return o.ReplicationStatus != replicationPending && o.ReplicationStatus != replicationFailed
}

func ruleMatches(rule *api.LifecycleRule, key string, o *object) bool {
	if common.Value(rule.Status) != configurationEnabled || !strings.HasPrefix(key, common.Value(rule.Prefix)) {
		return false
	}

	f := rule.Filter
	if f == nil {
		return true
	}

	if !strings.HasPrefix(key, common.Value(f.Prefix)) ||
		!sizeMatches(o, f.ObjectSizeGreaterThan, f.ObjectSizeLessThan) {
		return false
	}

	if f.Tag != nil && !tagMatches(o, f.Tag) {
		return false
	}

	if a := f.And; a != nil {
		if !strings.HasPrefix(key, common.Value(a.Prefix)) ||
			!sizeMatches(o, a.ObjectSizeGreaterThan, a.ObjectSizeLessThan) {
			return false
		}

		for i := range a.Tags {
			if !tagMatches(o, &a.Tags[i]) {
				return false
			}
		}
	}

	return true
}

func sizeMatches(o *object, lower, upper *int64) bool {
	return (lower == nil || (o != nil && o.Size > *lower)) && (upper == nil || (o != nil && o.Size < *upper))
}

func tagMatches(o *object, tag *api.Tag) bool {
	if o == nil {
		return false
	}

	value, found := o.Tags[common.Value(tag.Key)]
	return found && value == common.Value(tag.Value)
}

func (s *Service) lifecycleDue(base int64, days *int64, date *time.Time) time.Time {
	if date != nil {
		return *date
	}

	t := time.UnixMilli(base).UTC()
	day := cmp.Or(s.day, lifecycleDay)
	if day == lifecycleDay {
		return t.Truncate(lifecycleDay).AddDate(0, 0, int(common.Value(days))+1)
	}

	return time.UnixMilli(base + common.Value(days)*day.Milliseconds()).UTC()
}

func (s *Service) expiration(cfg *lifecycle, key string, o *object) (earliest time.Time, id string) {
	if cfg == nil {
		return earliest, id
	}

	for i := range cfg.Rules {
		rule := &cfg.Rules[i]
		e := rule.Expiration
		if e != nil && (e.Days != nil || e.Date != nil) && ruleMatches(rule, key, o) {
			due := s.lifecycleDue(o.LastModified, e.Days, e.Date)
			if earliest.IsZero() || due.Before(earliest) {
				earliest, id = due, common.Value(rule.ID)
			}
		}
	}

	return earliest, id
}

func (s *Service) expirationHeader(cfg *lifecycle, key string, o *object) *string {
	expires, id := s.expiration(cfg, key, o)
	if expires.IsZero() {
		return nil
	}

	escaped := strings.ReplaceAll(url.QueryEscape(id), "+", "%20")
	return new(fmt.Sprintf(`expiry-date=%q, rule-id=%q`, expires.UTC().Format(http.TimeFormat), escaped))
}

func (s *Service) expireMarker(cfg *lifecycle, key string, modified int64, now time.Time) bool {
	for i := range cfg.Rules {
		r := &cfg.Rules[i]
		e := r.Expiration
		if e != nil && ruleMatches(r, key, nil) && (common.Value(e.ExpiredObjectDeleteMarker) ||
			((e.Days != nil || e.Date != nil) && !now.Before(s.lifecycleDue(modified, e.Days, e.Date)))) {
			return true
		}
	}

	return false
}

func (s *Service) expireNoncurrent(cfg *lifecycle, key string, o *object, since, newer int64, now time.Time) bool {
	for i := range cfg.Rules {
		rule := &cfg.Rules[i]
		e := rule.NoncurrentVersionExpiration
		if e != nil && newer >= common.Value(e.NewerNoncurrentVersions) && ruleMatches(rule, key, o) &&
			!now.Before(s.lifecycleDue(since, e.NoncurrentDays, nil)) {
			return true
		}
	}

	return false
}

func (s *Service) lifecycleTransition(cfg *lifecycle, key string, o *object, since, newer int64, now time.Time) string {
	var best string
	for i := range cfg.Rules {
		r := &cfg.Rules[i]
		if !ruleMatches(r, key, o) {
			continue
		}

		if since == 0 {
			for _, t := range r.Transitions {
				class := common.Value(t.StorageClass)
				if !now.Before(s.lifecycleDue(o.LastModified, t.Days, t.Date)) {
					best = transitionTarget(cfg, r, o, class, best)
				}
			}
		} else {
			for _, t := range r.NoncurrentVersionTransitions {
				class := common.Value(t.StorageClass)
				if newer >= common.Value(t.NewerNoncurrentVersions) &&
					!now.Before(s.lifecycleDue(since, t.NoncurrentDays, nil)) {
					best = transitionTarget(cfg, r, o, class, best)
				}
			}
		}
	}

	return best
}

// A transition goes toward colder storage. Rank also selects a single target when several transitions are due.
var transitionRank = map[string]int{
	"STANDARD_IA": 1, "INTELLIGENT_TIERING": 2, storageOneZoneIA: 3,
	"GLACIER_IR": 4, storageGlacier: 5, storageDeepArchive: 6,
}

func transitionTarget(cfg *lifecycle, r *api.LifecycleRule, o *object, class, best string) string {
	source := o.StorageClass
	if transitionRank[class] <= max(transitionRank[source], transitionRank[best]) ||
		(source == storageOneZoneIA && class != storageGlacier && class != storageDeepArchive) {
		return best
	}

	f := r.Filter
	explicitSize := f != nil && (f.ObjectSizeGreaterThan != nil || f.ObjectSizeLessThan != nil ||
		(f.And != nil && (f.And.ObjectSizeGreaterThan != nil || f.And.ObjectSizeLessThan != nil)))
	allowsSmall := cfg.MinimumSize == "varies_by_storage_class" &&
		(class == storageGlacier || class == storageDeepArchive)
	if o.Size < 128<<10 && !explicitSize && !allowsSmall {
		return best
	}

	return class
}
