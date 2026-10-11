package mwaa

import (
	"context"
	"maps"
	"reflect"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/mwaa"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func apply(env *api.Environment, in *api.UpdateEnvironmentInput) {
	if in.ExecutionRoleArn != nil {
		env.ExecutionRoleArn = in.ExecutionRoleArn
	}
	if in.SourceBucketArn != nil {
		env.SourceBucketArn = in.SourceBucketArn
	}
	if in.DagS3Path != nil {
		env.DagS3Path = in.DagS3Path
	}
	if in.EnvironmentClass != nil {
		env.EnvironmentClass = in.EnvironmentClass
	}
	if in.MinWorkers != nil {
		env.MinWorkers = in.MinWorkers
	}
	if in.MaxWorkers != nil {
		env.MaxWorkers = in.MaxWorkers
	}
	if in.Schedulers != nil {
		env.Schedulers = in.Schedulers
	}
}

// update merges inside the transaction. No-op requests preserve the record and
// its internal compensation revision. There is no simulated lifecycle status.
func (s *Service) update(ctx context.Context, in *api.UpdateEnvironmentInput,
	owned, desired map[string]string, restore *api.Environment, expected string,
) (before, after *api.Environment, committed string, err error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, nil, "", e
	}
	if in == nil {
		return nil, nil, "", invalid("UpdateEnvironment input is required")
	}
	name := common.Value(in.Name)
	in = common.Clone(in)
	in.Name = new(name)
	owned = maps.Clone(owned)
	desired = maps.Clone(desired)
	restore = common.Clone(restore)
	e = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, readErr := read(tx, scope, common.Value(in.Name))
		if readErr != nil {
			return readErr
		}
		before = common.Clone(r.Environment)
		if restore != nil {
			if r.Revision != expected || !reflect.DeepEqual(r.Environment.CreatedAt, restore.CreatedAt) ||
				!reflect.DeepEqual(r.Environment.Arn, restore.Arn) {
				return invalid("Rollback revision or environment incarnation changed")
			}
			r.Environment = restore
		} else {
			apply(r.Environment, in)
			if desired != nil {
				tags := common.CloneTags(r.Environment.Tags)
				for k := range owned {
					if _, ok := desired[k]; !ok {
						delete(tags, k)
					}
				}
				maps.Copy(tags, desired)
				if !maps.Equal(r.Environment.Tags, tags) {
					r.Environment.Tags = tags
				}
			}
			if checkErr := validateConfig(r.Environment, scope); checkErr != nil {
				return checkErr
			}
			// Retained network references do not need to be re-resolved for scalar changes.
			// A changed bucket is checked in the same write as the candidate configuration.
			if !reflect.DeepEqual(before.SourceBucketArn, r.Environment.SourceBucketArn) {
				if s.source == nil {
					return invalid("Scoped S3 metadata lookup is unavailable")
				}
				if refErr := s.source.CheckMWAASource(tx, scope, *r.Environment.SourceBucketArn); refErr != nil {
					return invalid(refErr.Error())
				}
			}
		}
		if !reflect.DeepEqual(before, r.Environment) {
			r.Revision = revision()
			if saveErr := save(tx, scope, r); saveErr != nil {
				return saveErr
			}
		}
		after = common.Clone(r.Environment)
		committed = r.Revision
		return nil
	})
	if e != nil {
		return nil, nil, "", e
	}
	return before, after, committed, nil
}

func (s *Service) UpdateEnvironment(
	ctx context.Context,
	in *api.UpdateEnvironmentInput,
) (*api.UpdateEnvironmentOutput, error) {
	_, r, _, e := s.update(ctx, in, nil, nil, nil, "")
	if e != nil {
		return nil, e
	}
	return &api.UpdateEnvironmentOutput{Arn: r.Arn}, nil
}

// CloudFormationUpdate commits configuration and the final owned/external tag
// set atomically and returns the actual live pre-write configuration for rollback.
func (s *Service) CloudFormationUpdate(ctx context.Context, in *api.UpdateEnvironmentInput,
	owned, desired map[string]string,
) (before, after *api.Environment, committed string, err error) {
	if e := checkTags(desired); e != nil {
		return nil, nil, "", e
	}
	return s.update(ctx, in, owned, desired, nil, "")
}

// CloudFormationRestore checks a specific committed revision and compensates
// with a fresh revision, preventing an old compensation token from being reused.
func (s *Service) CloudFormationRestore(
	ctx context.Context,
	b *api.Environment,
	expected string,
) (*api.Environment, error) {
	_, r, _, e := s.update(ctx, &api.UpdateEnvironmentInput{Name: b.Name}, nil, nil, b, expected)
	return r, e
}
