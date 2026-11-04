package batch

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/batch"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) CreateComputeEnvironment(
	ctx context.Context,
	in *api.CreateComputeEnvironmentRequest,
) (*api.CreateComputeEnvironmentResponse, error) {
	if guardErr := nameOK(in.ComputeEnvironmentName); guardErr != nil {
		return nil, guardErr
	}

	if in.State != nil && *in.State != enabled {
		return nil, invalid("Compute environments must be created in the ENABLED state")
	}

	if guardErr := tagsOK(in.Tags); guardErr != nil {
		return nil, guardErr
	}

	typ := common.Value(in.Type)
	if typ != managed && typ != unmanaged {
		return nil, invalid("Type must be MANAGED or UNMANAGED")
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if guardErr := roleOK(scope, in.ServiceRole, "role", true); guardErr != nil {
		return nil, guardErr
	}

	role := common.Clone(in.ServiceRole)
	if role == nil {
		if typ == unmanaged {
			return nil, invalid("UNMANAGED environments require serviceRole")
		}

		role = new(
			"arn:aws:iam::" + scope.AccountID + ":role/aws-service-role/batch.amazonaws.com/AWSServiceRoleForBatch",
		)
	} else if len(*role) < 4 || (*role)[:4] != "arn:" {
		*role = "arn:aws:iam::" + scope.AccountID + ":role/" + strings.TrimPrefix(*role, "/")
	}

	r := &record{Kind: kindCE, ID: *in.ComputeEnvironmentName, Tags: maps.Clone(in.Tags)}
	r.ARN = arn(scope, r.Kind, r.ID)
	r.CE = &api.ComputeEnvironmentDetail{
		ComputeEnvironmentName: new(r.ID),
		ComputeEnvironmentArn:  new(r.ARN),
		Type:                   new(typ),
		State:                  common.OrElse(common.Clone(in.State), new(enabled)),
		Status:                 new(valid),
		StatusReason:           new(metadataReason),
		ServiceRole:            role,
		ComputeResources:       common.Clone(in.ComputeResources),
	}

	if c := r.CE.ComputeResources; c != nil && !isFargate(common.Value(c.Type)) {
		c.MinvCpus = common.OrElse(c.MinvCpus, new(int64(0)))
		c.DesiredvCpus = common.OrElse(c.DesiredvCpus, common.Clone(c.MinvCpus))
		c.AllocationStrategy = common.OrElse(c.AllocationStrategy, new("BEST_FIT"))
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if typ == unmanaged {
			if r.CE.ComputeResources != nil {
				return invalid("UNMANAGED environments cannot specify computeResources")
			}
		} else if guardErr := s.computeOK(tx, scope, r.CE.ComputeResources); guardErr != nil {
			return guardErr
		}

		return unique(tx, scope, r)
	})
	return &api.CreateComputeEnvironmentResponse{
		ComputeEnvironmentName: new(r.ID),
		ComputeEnvironmentArn:  new(r.ARN),
	}, err
}

func (s *Service) UpdateComputeEnvironment(
	ctx context.Context,
	in *api.UpdateComputeEnvironmentRequest,
) (*api.UpdateComputeEnvironmentResponse, error) {
	if guardErr := stateOK(in.State); guardErr != nil {
		return nil, guardErr
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if guardErr := roleOK(scope, in.ServiceRole, "role", true); guardErr != nil {
		return nil, guardErr
	}

	var out *api.UpdateComputeEnvironmentResponse
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := read(tx, scope, kindCE, in.ComputeEnvironment)
		if e != nil {
			return e
		}

		if in.ServiceRole != nil {
			role := common.Clone(in.ServiceRole)
			if !strings.HasPrefix(*role, "arn:") {
				*role = "arn:aws:iam::" + scope.AccountID + ":role/" + strings.TrimPrefix(*role, "/")
			}

			if linkedRole(role) != linkedRole(r.CE.ServiceRole) {
				return invalid("Cannot switch between service-linked and regular service roles")
			}

			r.CE.ServiceRole = role
		}

		if in.State != nil {
			r.CE.State = common.Clone(in.State)
		}

		if u := in.ComputeResources; u != nil {
			c := r.CE.ComputeResources
			if c == nil {
				return invalid("UNMANAGED environments cannot update computeResources")
			}

			if u.MinvCpus == nil && u.MaxvCpus == nil && u.DesiredvCpus == nil {
				return invalid("Specify a capacity update")
			}

			if isFargate(common.Value(c.Type)) && (u.MinvCpus != nil || u.DesiredvCpus != nil) {
				return invalid("Fargate only supports maxvCpus updates")
			}

			if u.DesiredvCpus != nil && *u.DesiredvCpus < common.Value(c.DesiredvCpus) {
				return invalid("desiredvCpus cannot decrease")
			}

			if u.MinvCpus != nil {
				c.MinvCpus = common.Clone(u.MinvCpus)
			}

			if u.MaxvCpus != nil {
				c.MaxvCpus = common.Clone(u.MaxvCpus)
			}

			if u.DesiredvCpus != nil {
				c.DesiredvCpus = common.Clone(u.DesiredvCpus)
			}

			// No scheduler runs; changes to bounds alone retain the existing desired capacity.
			if isFargate(common.Value(c.Type)) {
				if guardErr := integerOK(c.MaxvCpus, 1, "maxvCpus"); guardErr != nil {
					return guardErr
				}
			} else if guardErr := capacityOK(c); guardErr != nil {
				return guardErr
			}
		}

		out = &api.UpdateComputeEnvironmentResponse{
			ComputeEnvironmentName: new(r.ID),
			ComputeEnvironmentArn:  new(r.ARN),
		}

		return save(tx, scope, r)
	})
	return out, err
}

func (s *Service) DeleteComputeEnvironment(
	ctx context.Context,
	in *api.DeleteComputeEnvironmentRequest,
) (*api.DeleteComputeEnvironmentResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := read(tx, scope, kindCE, in.ComputeEnvironment)
		if e != nil {
			return e
		}

		if common.Value(r.CE.State) != disabled {
			return invalid("Disable the compute environment before deleting it")
		}

		queues, e := rows(tx, scope, kindQueue)
		if e != nil {
			return e
		}

		for _, q := range queues {
			for _, order := range q.Queue.ComputeEnvironmentOrder {
				if common.Value(order.ComputeEnvironment) == r.ARN {
					return invalid("Disassociate all job queues before deleting the compute environment")
				}
			}
		}

		tx.Delete(scope.Key(serviceName, kindCE, r.ID))
		return nil
	})
	return &api.DeleteComputeEnvironmentResponse{}, err
}

// Normalize reference lists for stable filter-bound cursors. Unknown references
// are omitted by describe operations; mutations require actual records.
func filters(scope request.Scope, kind string, list []string, maximum int) ([]string, error) {
	if guardErr := stringsUnique(list, maximum); guardErr != nil {
		return nil, guardErr
	}

	out := []string{}
	for _, v := range list {
		id, err := identifier(scope, new(v), kind)
		if err != nil {
			return nil, err
		}

		if slices.Contains(out, id) {
			return nil, invalid("Duplicate normalized reference")
		}

		out = append(out, id)
	}

	slices.Sort(out)
	return out, nil
}

func query(kind string, refs []string, extra ...string) string {
	b, _ := json.Marshal(struct {
		Kind        string
		Refs, Extra []string
	}{kind, refs, extra})
	return string(b)
}

func (s *Service) DescribeComputeEnvironments(
	ctx context.Context,
	in *api.DescribeComputeEnvironmentsRequest,
) (*api.DescribeComputeEnvironmentsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	refs, err := filters(scope, kindCE, in.ComputeEnvironments, 100)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeComputeEnvironmentsResponse{ComputeEnvironments: []api.ComputeEnvironmentDetail{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		all, e := rows(tx, scope, kindCE)
		if e != nil {
			return e
		}

		if len(refs) > 0 {
			all = slices.DeleteFunc(all, func(r *record) bool { return !slices.Contains(refs, r.ID) })
		}

		all, out.NextToken, e = page(
			all,
			func(r *record) string { return r.ID },
			in.NextToken,
			in.MaxResults,
			scope,
			query(kindCE, refs),
		)
		if e != nil {
			return e
		}

		for _, r := range all {
			out.ComputeEnvironments = append(out.ComputeEnvironments, ceOutput(r))
		}

		return nil
	})
	return out, err
}
