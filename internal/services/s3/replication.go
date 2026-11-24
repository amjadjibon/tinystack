package s3

import (
	"context"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	iampolicy "github.com/amjadjibon/tinystack/internal/services/iam/policy"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	workIndex            = "aws/s3/index/"
	replicationPending   = "PENDING"
	replicationCompleted = "COMPLETED"
	replicationFailed    = "FAILED"
	replicationReplica   = "REPLICA"
	bucketARNPrefix      = "arn:aws:s3:::"
)

var (
	replicationRole = regexp.MustCompile(`^arn:aws:iam::(\d{12}):role/[[:alnum:]_+=,.@/-]+$`)
	accountNumber   = regexp.MustCompile(`^\d{12}$`)
)

func bucketARNName(arn string) (string, bool) {
	name, ok := strings.CutPrefix(arn, bucketARNPrefix)
	return name, ok && validBucketName(name)
}

func replicationRuleValid(r *api.ReplicationRule) (string, error) {
	if !enabledDisabled(r.Status) || r.Destination == nil || (r.Filter != nil && r.Prefix != nil) ||
		common.Value(r.Priority) < 0 || len(common.Value(r.ID)) > 255 {
		return "", malformedXML()
	}

	dest, ok := bucketARNName(common.Value(r.Destination.Bucket))
	if !ok {
		return "", invalidArgument("The replication destination must be an S3 bucket ARN")
	}

	account := common.Value(r.Destination.Account)
	if r.Destination.Account != nil && !accountNumber.MatchString(account) {
		return "", invalidArgument("The replication destination account must have 12 digits")
	}

	class := common.Value(r.Destination.StorageClass)
	if class != "" && class != storageStandard && !transitionClasses[class] {
		return "", invalidArgument("Unsupported replication destination StorageClass")
	}

	markers := r.DeleteMarkerReplication
	if markers != nil && !enabledDisabled(markers.Status) {
		return "", malformedXML()
	}

	f := r.Filter
	if f == nil {
		return dest, nil
	}

	if r.Priority == nil || markers == nil || count(f.Prefix != nil, f.Tag != nil, f.And != nil) > 1 {
		return "", malformedXML()
	}

	tagFilter := f.Tag != nil || (f.And != nil && len(f.And.Tags) > 0)
	if tagFilter && common.Value(markers.Status) == versioningEnabled {
		return "", invalidArgument("Tag-based replication rules cannot replicate delete markers")
	}

	if f.Tag != nil {
		return dest, configurationTags([]api.Tag{*f.Tag})
	}

	if f.And != nil {
		return dest, configurationTags(f.And.Tags)
	}

	return dest, nil
}

func enabledDisabled(status *string) bool {
	return common.Value(status) == configurationEnabled || common.Value(status) == configurationDisabled
}

func (s *Service) PutBucketReplication(
	ctx context.Context, in *api.PutBucketReplicationRequest,
) (*api.PutBucketReplicationOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	cfg := in.ReplicationConfiguration
	if cfg == nil || len(cfg.Rules) == 0 || len(cfg.Rules) > 1000 {
		return nil, malformedXML()
	}

	match := replicationRole.FindStringSubmatch(common.Value(cfg.Role))
	if len(match) != 2 {
		return nil, invalidArgument("The replication role must be an IAM role ARN in the source account")
	}

	ids, priorities := map[string]bool{}, map[int64]bool{}
	for i := range cfg.Rules {
		rule := &cfg.Rules[i]
		dest, ruleErr := replicationRuleValid(rule)
		if ruleErr != nil {
			return nil, ruleErr
		}

		id, priority := common.Value(rule.ID), common.Value(rule.Priority)
		if dest == common.Value(in.Bucket) || (id != "" && ids[id]) || (rule.Priority != nil && priorities[priority]) {
			return nil, invalidArgument("Replication requires unique rule IDs and priorities and a different bucket")
		}

		ids[id], priorities[priority] = true, true
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		base, resolveErr := resolve(tx, m, in.Bucket, in.ExpectedBucketOwner)
		if resolveErr != nil {
			return resolveErr
		}

		owner, _, _ := strings.Cut(base, "/")
		if match[1] != owner {
			return invalidArgument("The replication role must be an IAM role ARN in the source account")
		}

		meta, _, readErr := storage.Read[bucketMeta](tx, base)
		if readErr != nil {
			return readErr
		}

		if meta.Versioning != versioningEnabled {
			return replicationVersioningError()
		}

		for i := range cfg.Rules {
			dest := cfg.Rules[i].Destination
			index, target, destErr := replicationDestination(tx, dest)
			if destErr != nil {
				return destErr
			}

			if target == nil || target.Versioning != versioningEnabled {
				return replicationVersioningError()
			}

			if dest.Account != nil && *dest.Account != index.Account {
				return invalidArgument("The replication destination owner does not match")
			}
		}

		meta.Replication = cfg
		return storage.Write(tx, base, meta)
	})
	if err != nil {
		return nil, err
	}

	return &api.PutBucketReplicationOutput{}, nil
}

func replicationVersioningError() error {
	return fail(http.StatusBadRequest, "InvalidRequest", "Source and destination buckets must have versioning enabled")
}

func replicationDestination(tx storage.ReadTx, dest *api.Destination) (*bucketIndex, *bucketMeta, error) {
	name, _ := bucketARNName(common.Value(dest.Bucket))
	index, found, err := storage.Read[bucketIndex](tx, indexPrefix+name)
	if err != nil || !found {
		return nil, nil, err
	}

	meta, _, err := storage.Read[bucketMeta](tx, index.Account+"/"+index.Region+"/s3/"+name)
	return index, meta, err
}

func (s *Service) GetBucketReplication(
	ctx context.Context, in *api.GetBucketReplicationRequest,
) (*api.GetBucketReplicationOutput, error) {
	meta, err := s.bucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	if meta.Replication == nil {
		return nil, fail(http.StatusNotFound, "ReplicationConfigurationNotFoundError",
			"The replication configuration was not found")
	}

	return &api.GetBucketReplicationOutput{ReplicationConfiguration: meta.Replication}, nil
}

func (s *Service) DeleteBucketReplication(
	ctx context.Context, in *api.DeleteBucketReplicationRequest,
) (*api.DeleteBucketReplicationOutput, error) {
	err := s.updateBucket(ctx, in.Bucket, in.ExpectedBucketOwner, func(meta *bucketMeta) {
		meta.Replication = nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteBucketReplicationOutput{}, nil
}

type replicationJob struct {
	ID           string
	Role         string
	Destinations []*api.Destination
}

type replicationQueue struct {
	Bucket string
	Key    string
	Jobs   []replicationJob
}

func replicationQueueKey(base, key string) string {
	account, rest, _ := strings.Cut(base, "/")
	region, bucket, _ := strings.Cut(rest, "/s3/")
	return workIndex + account + "/" + region + "/replication/" + bucket + "/" + key
}

func replicates(rule *api.ReplicationRule, key string, obj *object) bool {
	if common.Value(rule.Status) != versioningEnabled || !strings.HasPrefix(key, common.Value(rule.Prefix)) {
		return false
	}

	f := rule.Filter
	if f == nil {
		return true
	}

	if !strings.HasPrefix(key, common.Value(f.Prefix)) || (f.Tag != nil && !tagMatches(obj, f.Tag)) {
		return false
	}

	if f.And != nil {
		if !strings.HasPrefix(key, common.Value(f.And.Prefix)) {
			return false
		}

		for i := range f.And.Tags {
			if !tagMatches(obj, &f.And.Tags[i]) {
				return false
			}
		}
	}

	return true
}

func enqueueReplication(tx storage.WriteTx, cfg *api.ReplicationConfiguration, base, key string, obj *object) error {
	if cfg == nil {
		return nil
	}

	var rules []*api.ReplicationRule
	for i := range cfg.Rules {
		if r := &cfg.Rules[i]; replicates(r, key, obj) {
			rules = append(rules, r)
		}
	}

	if len(rules) == 0 {
		return nil
	}

	slices.SortStableFunc(rules, func(a, b *api.ReplicationRule) int {
		// Priorities are nonnegative Smithy integers.
		return int(common.Value(b.Priority) - common.Value(a.Priority))
	})

	job := replicationJob{ID: obj.VersionID, Role: common.Value(cfg.Role)}
	seen := map[string]bool{}
	for _, rule := range rules {
		dest := common.Value(rule.Destination.Bucket)
		if seen[dest] {
			continue
		}

		seen[dest] = true
		markers := rule.DeleteMarkerReplication
		if !obj.DeleteMarker || (markers != nil && common.Value(markers.Status) == versioningEnabled) {
			job.Destinations = append(job.Destinations, rule.Destination)
		}
	}

	if len(job.Destinations) == 0 {
		return nil
	}

	at := replicationQueueKey(base, key)
	queue, found, err := storage.Read[replicationQueue](tx, at)
	if err != nil {
		return err
	}

	if !found {
		queue = &replicationQueue{Bucket: base, Key: key}
	}

	queue.Jobs = append(queue.Jobs, job)
	obj.ReplicationStatus = replicationPending
	return storage.Write(tx, at, queue)
}

// StartDeliveries runs committed replication and access log work.
func (s *Service) StartDeliveries(logger *slog.Logger) {
	s.workers.Every(time.Second, func(ctx context.Context) {
		var keys []string
		if err := s.store.View(ctx, func(tx storage.ReadTx) error {
			keys = tx.Keys(workIndex)
			return nil
		}); err != nil {
			return
		}

		for _, key := range keys {
			parts := strings.SplitN(strings.TrimPrefix(key, workIndex), "/", 4)
			if len(parts) != 4 {
				continue
			}

			var err error
			switch parts[2] {
			case "replication":
				err = s.replicateKey(ctx, key)
			case "access-log":
				err = s.deliverAccessLog(ctx, key)
			}

			if err != nil && ctx.Err() == nil {
				logger.Error("S3 delivery failed", "kind", parts[2], "error", err)
			}
		}
	})
}

func (s *Service) replicateKey(ctx context.Context, key string) error {
	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		queue, found, err := storage.Read[replicationQueue](tx, key)
		if err != nil || !found {
			return err
		}

		for i := range queue.Jobs {
			if err := replicateVersion(tx, queue.Bucket, queue.Key, &queue.Jobs[i]); err != nil {
				return err
			}
		}

		tx.Delete(key)
		return nil
	})
}

func replicateVersion(tx storage.WriteTx, base, key string, job *replicationJob) error {
	currentKey := base + "/o/" + key
	current, found, err := storage.Read[object](tx, currentKey)
	if err != nil {
		return err
	}

	var obj *object
	var h []object
	if found && current.VersionID == job.ID {
		obj = current
	} else {
		h, err = history(tx, base, key)
		if err != nil {
			return err
		}

		for i := range h {
			if h[i].VersionID == job.ID {
				obj = &h[i]
				break
			}
		}
	}

	if obj == nil || obj.ReplicationStatus != replicationPending {
		return nil
	}

	ref := base + "/db/" + key
	if obj != current {
		ref = bodyKey(base, key, obj)
	}

	body, found := tx.Get(ref)
	obj.ReplicationStatus = replicationCompleted
	if !obj.DeleteMarker && !found {
		obj.ReplicationStatus = replicationFailed
	} else {
		for _, dest := range job.Destinations {
			ok, err := replicateTo(tx, job.Role, dest, key, obj, body)
			if err != nil {
				return err
			}

			if !ok {
				obj.ReplicationStatus = replicationFailed
			}
		}
	}

	if obj == current {
		return storage.Write(tx, currentKey, obj)
	}

	return saveHistory(tx, base, key, h)
}

func replicateTo(
	tx storage.WriteTx, role string, dest *api.Destination, key string, obj *object, body []byte,
) (bool, error) {
	index, meta, err := replicationDestination(tx, dest)
	if err != nil || meta == nil || meta.Versioning != versioningEnabled {
		return false, err
	}

	if dest.Account != nil && *dest.Account != index.Account {
		return false, nil
	}

	parts := strings.SplitN(role, ":", 6)
	principal := &request.Principal{Kind: request.PrincipalRole, Account: parts[4], ARN: role, PolicyARN: role}
	allowed, err := replicaAllowed(tx, principal, index, meta, common.Value(dest.Bucket)+"/"+key, obj.DeleteMarker)
	if err != nil || !allowed {
		return false, err
	}

	base := index.Account + "/" + index.Region + "/s3/" + strings.TrimPrefix(common.Value(dest.Bucket), bucketARNPrefix)
	replica := *obj
	replica.ReplicationStatus, replica.NoncurrentSince = replicationReplica, 0
	replica.RestoreReady, replica.RestoreUntil = 0, 0
	if dest.StorageClass != nil {
		replica.StorageClass = common.Value(dest.StorageClass)
		if replica.StorageClass == storageStandard {
			replica.StorageClass = ""
		}
	}

	return true, insertReplica(tx, base, key, &replica, body)
}

func replicaAllowed(
	tx storage.ReadTx, principal *request.Principal, index *bucketIndex, meta *bucketMeta, arn string, marker bool,
) (bool, error) {
	scope := request.Scope{AccountID: index.Account, Region: index.Region}
	policy, err := restrictPublicPolicy(tx, scope, principal, meta.Policy, meta.PublicAccessBlock)
	if err != nil {
		return false, err
	}

	action := "s3:ReplicateObject"
	if marker {
		action = "s3:ReplicateDelete"
	}

	values := map[string][]string{"aws:PrincipalArn": {principal.ARN}, "aws:PrincipalAccount": {principal.Account}}
	decision := iampolicy.EvaluateResource(iampolicy.Document{JSON: policy}, action, arn,
		iampolicy.Context{Values: values}, principal)
	return !decision.ExplicitDeny && len(decision.Unsupported) == 0 &&
		(principal.Account == index.Account || decision.Allowed), nil
}

// insertReplica preserves the source version ID and modification date. An older
// replica is inserted into history without replacing a newer destination write.
func insertReplica(tx storage.WriteTx, base, key string, obj *object, body []byte) error {
	h, err := history(tx, base, key)
	if err != nil {
		return err
	}

	current, found, err := storage.Read[object](tx, base+"/o/"+key)
	if err != nil {
		return err
	}

	position := slices.IndexFunc(h, func(other object) bool { return other.LastModified <= obj.LastModified })
	if position < 0 {
		position = len(h)
	}

	newerCurrent := found && current.LastModified > obj.LastModified
	if newerCurrent || position > 0 {
		if position > 0 {
			obj.NoncurrentSince = h[position-1].LastModified
		} else {
			obj.NoncurrentSince = current.LastModified
		}

		if !obj.DeleteMarker {
			tx.Put(bodyKey(base, key, obj), body)
		}

		return saveHistory(tx, base, key, slices.Insert(h, position, *obj))
	}

	h, err = archive(tx, base, key, h, obj.LastModified)
	if err != nil {
		return err
	}

	if obj.DeleteMarker {
		return saveHistory(tx, base, key, append([]object{*obj}, h...))
	}

	if err := saveHistory(tx, base, key, h); err != nil {
		return err
	}

	tx.Put(base+"/db/"+key, body)
	return storage.Write(tx, base+"/o/"+key, obj)
}
