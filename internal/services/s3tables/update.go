package s3tables

import (
	"context"
	"maps"
	"reflect"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3tables"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) RenameTable(ctx context.Context, in *api.RenameTableRequest) (*api.RenameTableOutput, error) {
	if in == nil {
		return nil, invalid("input is required")
	}
	_, _, e := s.rename(ctx, in, nil, nil)
	if e != nil {
		return nil, e
	}
	return &api.RenameTableOutput{}, nil
}

func (s *Service) rename(
	ctx context.Context,
	in *api.RenameTableRequest,
	owned, desired map[string]string,
) (before, after *Snapshot, err error) {
	scope, e := regionalScope(ctx)
	if e != nil {
		return nil, nil, e
	}
	if in == nil {
		return nil, nil, invalid("input is required")
	}
	// Capture every label explicitly: the generated label members use json:"-".
	arn, ns, name := common.Value(in.TableBucketARN), common.Value(in.Namespace), common.Value(in.Name)
	newNS, newName := ns, name
	if in.NewNamespaceName != nil {
		newNS = *in.NewNamespaceName
	}
	if in.NewName != nil {
		newName = *in.NewName
	}
	if checkErr := ValidateNamespaceName(newNS); checkErr != nil {
		return nil, nil, checkErr
	}
	if checkErr := ValidateTableName(newName); checkErr != nil {
		return nil, nil, checkErr
	}
	token := copyString(in.VersionToken)
	if checkErr := checkToken(token); checkErr != nil {
		return nil, nil, checkErr
	}
	owned, desired = maps.Clone(owned), maps.Clone(desired)
	e = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, err := readTable(tx, scope, arn, ns, name)
		if err != nil {
			return err
		}
		if token != nil && *token != *r.Table.VersionToken {
			return conflict("The table versionToken is stale")
		}
		dest, err := readNamespace(tx, scope, arn, newNS)
		if err != nil {
			return err
		}
		oldKey := recordKey(scope, r)
		before = common.Clone(r)
		if newNS != ns || newName != name {
			r.Table.Namespace = []string{newNS}
			r.Table.Name = new(newName)
			r.Table.NamespaceId = new(*dest.Namespace.NamespaceId)
			if _, ok := tx.Get(recordKey(scope, r)); ok {
				return conflict("The destination table already exists")
			}
			now := time.Now().UTC()
			if !now.After(*r.Table.ModifiedAt) {
				now = r.Table.ModifiedAt.Add(time.Nanosecond)
			}
			r.Table.ModifiedAt = &now
			r.Table.ModifiedBy = new(scope.AccountID)
			r.Table.VersionToken = new(fresh())
		}
		if desired != nil {
			tags := finalTags(r.Tags, owned, desired)
			if checkErr := ValidateTags(tags); checkErr != nil {
				return checkErr
			}
			r.Tags = tags
		}
		if !reflect.DeepEqual(before, r) {
			r.Revision = fresh()
			tx.Delete(oldKey)
			if checkErr := save(tx, scope, r); checkErr != nil {
				return checkErr
			}
		}
		after = common.Clone(r)
		return nil
	})
	if e != nil {
		return nil, nil, e
	}
	return before, after, nil
}

func finalTags(live, owned, desired map[string]string) map[string]string {
	tags := common.CloneTags(live)
	for k := range owned {
		if _, ok := desired[k]; !ok {
			delete(tags, k)
		}
	}
	maps.Copy(tags, desired)
	return tags
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResponse, error) {
	scope, e := regionalScope(ctx)
	if e != nil {
		return nil, e
	}
	if in == nil {
		return nil, invalid("input is required")
	}
	arn := common.Value(in.ResourceArn)
	var r *Snapshot
	e = s.store.View(ctx, func(tx storage.ReadTx) error { var err error; r, err = readARN(tx, scope, arn); return err })
	if e != nil {
		return nil, e
	}
	return &api.ListTagsForResourceResponse{Tags: common.CloneTags(r.Tags)}, nil
}

func (s *Service) tags(ctx context.Context, arn string, add map[string]string, remove []string) error {
	scope, e := regionalScope(ctx)
	if e != nil {
		return e
	}
	add = maps.Clone(add)
	remove = append([]string(nil), remove...)
	if checkErr := ValidateTags(add); checkErr != nil {
		return checkErr
	}
	for _, k := range remove {
		if checkErr := checkKey(k); checkErr != nil {
			return checkErr
		}
	}
	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, err := readARN(tx, scope, arn)
		if err != nil {
			return err
		}
		merged := common.CloneTags(r.Tags)
		maps.Copy(merged, add)
		for _, k := range remove {
			delete(merged, k)
		}
		if checkErr := ValidateTags(merged); checkErr != nil {
			return checkErr
		}
		if maps.Equal(r.Tags, merged) {
			return nil
		}
		r.Tags = merged
		r.Revision = fresh()
		return save(tx, scope, r)
	})
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	if in == nil || in.Tags == nil {
		return nil, invalid("tags is required")
	}
	if e := s.tags(ctx, common.Value(in.ResourceArn), in.Tags, nil); e != nil {
		return nil, e
	}
	return &api.TagResourceResponse{}, nil
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	if in == nil || len(in.TagKeys) < 1 || len(in.TagKeys) > 50 {
		return nil, invalid("tagKeys requires 1-50 keys")
	}
	if e := s.tags(ctx, common.Value(in.ResourceArn), nil, in.TagKeys); e != nil {
		return nil, e
	}
	return &api.UntagResourceResponse{}, nil
}

// CloudFormationUpdateTable applies rename and the final owned/external tag set
// in one transaction. Tags advance only the private revision, not table tokens.
func (s *Service) CloudFormationUpdateTable(
	ctx context.Context,
	in *api.RenameTableRequest,
	owned, desired map[string]string,
) (before, after *Snapshot, err error) {
	return s.rename(ctx, in, owned, desired)
}

// CloudFormationUpdateBucket replaces the owned tags without transient overflow.
func (s *Service) CloudFormationUpdateBucket(
	ctx context.Context,
	arn string,
	owned, desired map[string]string,
) (before, after *Snapshot, err error) {
	scope, e := regionalScope(ctx)
	if e != nil {
		return nil, nil, e
	}
	owned, desired = maps.Clone(owned), common.CloneTags(desired)
	e = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, err := readBucket(tx, scope, arn)
		if err != nil {
			return err
		}
		before = common.Clone(r)
		merged := finalTags(r.Tags, owned, desired)
		if checkErr := ValidateTags(merged); checkErr != nil {
			return checkErr
		}
		if !maps.Equal(r.Tags, merged) {
			r.Tags = merged
			r.Revision = fresh()
			if checkErr := save(tx, scope, r); checkErr != nil {
				return checkErr
			}
		}
		after = common.Clone(r)
		return nil
	})
	if e != nil {
		return nil, nil, e
	}
	return before, after, nil
}

// CloudFormationRestore compensates live configuration with a revision guard.
// Effective rename compensation uses a fresh public version token (no ABA).
func (s *Service) CloudFormationRestore(ctx context.Context, b *Snapshot, expected string) (*Snapshot, error) {
	scope, e := regionalScope(ctx)
	if e != nil {
		return nil, e
	}
	if b == nil || (b.Bucket == nil) == (b.Table == nil) {
		return nil, invalid("Bucket or table snapshot is required")
	}
	b = common.Clone(b)
	e = s.store.Update(ctx, func(tx storage.WriteTx) error {
		arn := b.BucketARN
		if b.Table != nil {
			arn = *b.Table.TableARN
		}
		r, err := readARN(tx, scope, arn)
		if err != nil {
			return err
		}
		if expected == "" || r.Revision != expected || r.Incarnation != b.Incarnation {
			return conflict("Compensation revision or incarnation changed")
		}
		if b.Table != nil {
			n, err := readNamespace(tx, scope, b.BucketARN, b.Table.Namespace[0])
			if err != nil {
				return err
			}
			if *n.Namespace.NamespaceId != *b.Table.NamespaceId {
				return conflict("Compensation namespace was recreated")
			}
			if recordKey(scope, b) != recordKey(scope, r) {
				if _, ok := tx.Get(recordKey(scope, b)); ok {
					return conflict("Compensation destination table already exists")
				}
			}
			// Preserve timestamps/config but never restore an old token after a rename.
			if *r.Table.VersionToken != *b.Table.VersionToken {
				b.Table.VersionToken = new(fresh())
			}
		} else if *r.Bucket.TableBucketId != *b.Bucket.TableBucketId {
			return conflict("Compensation bucket identity changed")
		}
		b.Revision = fresh()
		tx.Delete(recordKey(scope, r))
		return save(tx, scope, b)
	})
	if e != nil {
		return nil, e
	}
	return common.Clone(b), nil
}
