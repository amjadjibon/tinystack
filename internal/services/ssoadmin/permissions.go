package ssoadmin

import (
	"context"
	"strconv"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ssoadmin"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func checkPermission(p *api.PermissionSet) error {
	if err := checkString(p.Name, true, 32, namePattern); err != nil {
		return err
	}

	if err := checkString(p.Description, false, 700, descriptionPattern); err != nil {
		return err
	}

	if err := checkString(p.RelayState, false, 240, relayPattern); err != nil {
		return err
	}

	duration := common.Value(p.SessionDuration)
	parts := durationPattern.FindStringSubmatch(duration)
	if len(duration) > 100 || parts == nil {
		return awserror.Validation("Invalid ISO-8601 SessionDuration")
	}

	// A positive calendar component is always longer than the 12-hour maximum.
	for _, component := range parts[2:5] {
		if component != "" {
			n, e := strconv.ParseFloat(component, 64)
			if e != nil || n != 0 {
				return awserror.Validation("SessionDuration must be between PT1H and PT12H")
			}
		}
	}

	seconds := float64(0)
	for i, multiplier := range []float64{3600, 60, 1} {
		if parts[i+6] != "" {
			n, err := strconv.ParseFloat(parts[i+6], 64)
			if err != nil {
				return awserror.Validation("Invalid SessionDuration")
			}

			seconds += n * multiplier
		}
	}

	if parts[1] != "" || seconds < 3600 || seconds > 43200 {
		return awserror.Validation("SessionDuration must be between PT1H and PT12H")
	}

	return nil
}

func (s *Service) CreatePermissionSet(ctx context.Context, in *api.CreatePermissionSetRequest) (
	*api.CreatePermissionSetResponse, error,
) {
	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	scope, err := checkScope(ctx, in.InstanceArn)
	if err != nil {
		return nil, err
	}

	in = common.Clone(in)
	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	r := permissionRecord{PermissionSet: api.PermissionSet{
		Name: in.Name, Description: in.Description,
		SessionDuration: common.OrElse(in.SessionDuration, new("PT1H")), RelayState: in.RelayState,
		PermissionSetArn: new(permissionARN(scope.AccountID)), CreatedDate: common.Seconds(time.Now()),
	}, Tags: tags}
	if validationErr := checkPermission(&r.PermissionSet); validationErr != nil {
		return nil, validationErr
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		records, e := storage.Records[permissionRecord](tx, scope.Prefix(serviceName, permissionKind))
		if e != nil {
			return e
		}

		for _, other := range records {
			if *other.PermissionSet.Name == *r.PermissionSet.Name {
				return conflict()
			}
		}

		return storage.Write(tx, scope.Key(serviceName, permissionKind, *r.PermissionSet.PermissionSetArn), r)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreatePermissionSetResponse{PermissionSet: &r.PermissionSet}, nil
}

func (s *Service) DescribePermissionSet(ctx context.Context, in *api.DescribePermissionSetRequest) (
	*api.DescribePermissionSetResponse, error,
) {
	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	scope, err := checkScope(ctx, in.InstanceArn)
	if err != nil {
		return nil, err
	}

	var out *api.DescribePermissionSetResponse
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, _, e := loadPermission(tx, scope, in.PermissionSetArn)
		if e != nil {
			return e
		}

		out = &api.DescribePermissionSetResponse{PermissionSet: &r.PermissionSet}
		return nil
	})
	return out, err
}

func (s *Service) ListPermissionSets(ctx context.Context, in *api.ListPermissionSetsRequest) (
	*api.ListPermissionSetsResponse, error,
) {
	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	scope, err := checkScope(ctx, in.InstanceArn)
	if err != nil {
		return nil, err
	}

	limit, err := pageLimit(in.MaxResults)
	if err != nil {
		return nil, err
	}

	list := []string{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		records, e := storage.Records[permissionRecord](tx, scope.Prefix(serviceName, permissionKind))
		if e != nil {
			return e
		}

		for _, r := range records {
			list = append(list, *r.PermissionSet.PermissionSetArn)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(
		list,
		func(arn string) string { return arn },
		in.NextToken,
		limit,
		awserror.Validation("Invalid NextToken"),
	)
	return &api.ListPermissionSetsResponse{PermissionSets: page, NextToken: next}, err
}

func (s *Service) UpdatePermissionSet(ctx context.Context, in *api.UpdatePermissionSetRequest) (
	*api.UpdatePermissionSetResponse, error,
) {
	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	scope, err := checkScope(ctx, in.InstanceArn)
	if err != nil {
		return nil, err
	}

	in = common.Clone(in)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, key, e := loadPermission(tx, scope, in.PermissionSetArn)
		if e != nil {
			return e
		}

		if in.Description != nil {
			r.PermissionSet.Description = in.Description
		}

		if in.SessionDuration != nil {
			r.PermissionSet.SessionDuration = in.SessionDuration
		}

		if in.RelayState != nil {
			r.PermissionSet.RelayState = in.RelayState
		}

		if e := checkPermission(&r.PermissionSet); e != nil {
			return e
		}

		return storage.Write(tx, key, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdatePermissionSetResponse{}, nil
}

func (s *Service) DeletePermissionSet(ctx context.Context, in *api.DeletePermissionSetRequest) (
	*api.DeletePermissionSetResponse, error,
) {
	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	scope, err := checkScope(ctx, in.InstanceArn)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		_, key, e := loadPermission(tx, scope, in.PermissionSetArn)
		if e != nil {
			return e
		}

		if len(tx.Keys(scope.Key(serviceName, assignmentKind, *in.PermissionSetArn)+"/")) > 0 {
			return conflict()
		}

		for _, k := range tx.Keys(scope.Key(serviceName, managedKind, *in.PermissionSetArn) + "/") {
			tx.Delete(k)
		}

		tx.Delete(key)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeletePermissionSetResponse{}, nil
}
