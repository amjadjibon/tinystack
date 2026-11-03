package kafka

import (
	"context"
	"maps"
	"reflect"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/kafka"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// update uses an expected revision for configuration changes, including CF's
// final owned/external tag set. Successful effective changes generate a fresh
// opaque revision; explicit no-ops retain it, after checking the expected version.
func (s *Service) update(
	ctx context.Context,
	in *api.UpdateMonitoringRequest,
	owned, desired map[string]string,
	restore *api.ClusterInfo,
) (before, after *api.ClusterInfo, err error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, nil, e
	}

	arn, expected := common.Value(in.ClusterArn), common.Value(in.CurrentVersion)
	monitor := common.Clone(in.EnhancedMonitoring)
	owned = maps.Clone(owned)
	desired = maps.Clone(desired)
	restore = common.Clone(restore)
	if expected == "" {
		return nil, nil, invalid("CurrentVersion is required")
	}

	if restore == nil {
		if monitorErr := checkMonitoring(monitor); monitorErr != nil {
			return nil, nil, monitorErr
		}

		if tagErr := checkTags(desired); tagErr != nil {
			return nil, nil, tagErr
		}
	}

	e = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, readErr := read(tx, scope, arn)
		if readErr != nil {
			return readErr
		}

		if *r.CurrentVersion != expected {
			return invalid(staleRevisionMessage)
		}

		if common.Value(r.State) == "UPDATING" {
			return invalid("Cluster must be ACTIVE before updating")
		}

		before = common.Clone(r)
		if restore != nil {
			if common.Value(restore.ClusterArn) != arn || !reflect.DeepEqual(r.CreationTime, restore.CreationTime) {
				return invalid("Rollback snapshot belongs to another cluster incarnation")
			}

			r.EnhancedMonitoring = restore.EnhancedMonitoring
			r.Tags = restore.Tags
		} else {
			r.EnhancedMonitoring = monitor
			if desired != nil {
				tags := common.CloneTags(r.Tags)
				for k := range owned {
					if _, ok := desired[k]; !ok {
						delete(tags, k)
					}
				}

				maps.Copy(tags, desired)
				r.Tags = tags
			}
		}

		if checkErr := checkTags(r.Tags); checkErr != nil {
			return checkErr
		}

		if checkErr := checkMonitoring(r.EnhancedMonitoring); checkErr != nil {
			return checkErr
		}

		if !reflect.DeepEqual(before.EnhancedMonitoring, r.EnhancedMonitoring) ||
			!maps.Equal(before.Tags, r.Tags) {
			r.CurrentVersion = revision()
			if saveErr := save(tx, scope, r); saveErr != nil {
				return saveErr
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

func (s *Service) UpdateMonitoring(
	ctx context.Context,
	in *api.UpdateMonitoringRequest,
) (*api.UpdateMonitoringResponse, error) {
	_, r, e := s.update(ctx, in, nil, nil, nil)
	if e != nil {
		return nil, e
	}

	return &api.UpdateMonitoringResponse{ClusterArn: r.ClusterArn}, nil
}

// CloudFormationUpdate atomically checks the live revision, changes monitoring
// and replaces only template-owned tags, and captures actual pre-update state.
func (s *Service) CloudFormationUpdate(
	ctx context.Context,
	in *api.UpdateMonitoringRequest,
	owned, desired map[string]string,
) (before, after *api.ClusterInfo, err error) {
	return s.update(ctx, in, owned, desired, nil)
}

// CloudFormationRestore compensates a specific committed revision. Restoring
// configuration advances the revision rather than resurrecting an old CAS token.
func (s *Service) CloudFormationRestore(
	ctx context.Context,
	b *api.ClusterInfo,
	expected string,
) (*api.ClusterInfo, error) {
	_, r, e := s.update(
		ctx,
		&api.UpdateMonitoringRequest{ClusterArn: b.ClusterArn, CurrentVersion: new(expected)},
		nil,
		nil,
		b,
	)
	return r, e
}
