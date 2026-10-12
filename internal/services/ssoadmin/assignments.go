package ssoadmin

import (
	"context"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ssoadmin"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/identitystore"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func assignmentID(a *api.AccountAssignment) string {
	return *a.PermissionSetArn + "/" + *a.AccountId + "/" + *a.PrincipalType + "/" + *a.PrincipalId
}

func (s *Service) assignment(
	ctx context.Context,
	instance, target, targetType, permission, principalType, principal *string,
	create bool,
) (
	*api.AccountAssignmentOperationStatus, error,
) {
	scope, err := checkScope(ctx, instance)
	if err != nil {
		return nil, err
	}

	if common.Value(targetType) != "AWS_ACCOUNT" || !accountPattern.MatchString(common.Value(target)) {
		return nil, awserror.Validation("TargetType must be AWS_ACCOUNT and TargetId must be a 12-digit account")
	}

	if common.Value(principalType) != "USER" && common.Value(principalType) != "GROUP" {
		return nil, awserror.Validation("PrincipalType must be USER or GROUP")
	}

	if !principalPattern.MatchString(common.Value(principal)) {
		return nil, awserror.Validation("Invalid PrincipalId")
	}

	a := api.AccountAssignment{
		AccountId: new(*target), PermissionSetArn: new(common.Value(permission)),
		PrincipalType: new(*principalType), PrincipalId: new(*principal),
	}

	id := common.RandomID(32, "0123456789abcdef")
	requestID := id[:8] + "-" + id[8:12] + "-" + id[12:16] + "-" + id[16:20] + "-" + id[20:]
	status := &api.AccountAssignmentOperationStatus{
		Status: new("SUCCEEDED"), RequestId: new(requestID), TargetId: a.AccountId,
		TargetType: new("AWS_ACCOUNT"), PermissionSetArn: a.PermissionSetArn, PrincipalType: a.PrincipalType,
		PrincipalId: a.PrincipalId, CreatedDate: common.Seconds(time.Now()),
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, _, e := loadPermission(tx, scope, permission); e != nil {
			return e
		}

		key := scope.Key(serviceName, assignmentKind, assignmentID(&a))
		_, exists := tx.Get(key)
		kind := "deletion-status"
		if create {
			if !identitystore.PrincipalExists(tx, scope.AccountID, *principalType, *principal) {
				return missing()
			}

			if exists {
				return conflict()
			}

			if e := storage.Write(tx, key, a); e != nil {
				return e
			}

			kind = "creation-status"
		} else {
			if !exists {
				return missing()
			}

			tx.Delete(key)
		}

		return storage.Write(tx, scope.Key(serviceName, kind, requestID), status)
	})
	return status, err
}

func (s *Service) CreateAccountAssignment(ctx context.Context, in *api.CreateAccountAssignmentRequest) (
	*api.CreateAccountAssignmentResponse, error,
) {
	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	status, err := s.assignment(
		ctx,
		in.InstanceArn,
		in.TargetId,
		in.TargetType,
		in.PermissionSetArn,
		in.PrincipalType,
		in.PrincipalId,
		true,
	)
	if err != nil {
		return nil, err
	}

	return &api.CreateAccountAssignmentResponse{AccountAssignmentCreationStatus: status}, nil
}

func (s *Service) DeleteAccountAssignment(ctx context.Context, in *api.DeleteAccountAssignmentRequest) (
	*api.DeleteAccountAssignmentResponse, error,
) {
	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	status, err := s.assignment(
		ctx,
		in.InstanceArn,
		in.TargetId,
		in.TargetType,
		in.PermissionSetArn,
		in.PrincipalType,
		in.PrincipalId,
		false,
	)
	if err != nil {
		return nil, err
	}

	return &api.DeleteAccountAssignmentResponse{AccountAssignmentDeletionStatus: status}, nil
}

func (s *Service) assignmentStatus(
	ctx context.Context,
	instance, id *string,
	kind string,
) (*api.AccountAssignmentOperationStatus, error) {
	scope, err := checkScope(ctx, instance)
	if err != nil {
		return nil, err
	}

	if !requestPattern.MatchString(common.Value(id)) {
		return nil, awserror.Validation("Invalid assignment request ID")
	}

	var out *api.AccountAssignmentOperationStatus
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, found, e := storage.Read[api.AccountAssignmentOperationStatus](tx, scope.Key(serviceName, kind, *id))
		if e != nil {
			return e
		}

		if !found {
			return missing()
		}

		out = r
		return nil
	})
	return out, err
}

func (s *Service) DescribeAccountAssignmentCreationStatus(
	ctx context.Context,
	in *api.DescribeAccountAssignmentCreationStatusRequest,
) (
	*api.DescribeAccountAssignmentCreationStatusResponse, error,
) {
	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	status, err := s.assignmentStatus(ctx, in.InstanceArn, in.AccountAssignmentCreationRequestId, "creation-status")
	if err != nil {
		return nil, err
	}

	return &api.DescribeAccountAssignmentCreationStatusResponse{AccountAssignmentCreationStatus: status}, nil
}

func (s *Service) DescribeAccountAssignmentDeletionStatus(
	ctx context.Context,
	in *api.DescribeAccountAssignmentDeletionStatusRequest,
) (
	*api.DescribeAccountAssignmentDeletionStatusResponse, error,
) {
	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	status, err := s.assignmentStatus(ctx, in.InstanceArn, in.AccountAssignmentDeletionRequestId, "deletion-status")
	if err != nil {
		return nil, err
	}

	return &api.DescribeAccountAssignmentDeletionStatusResponse{AccountAssignmentDeletionStatus: status}, nil
}

func accountAssignments(
	tx storage.ReadTx,
	scope request.Scope,
	permission, account *string,
) ([]api.AccountAssignment, error) {
	if _, _, err := loadPermission(tx, scope, permission); err != nil {
		return nil, err
	}

	prefix := scope.Key(serviceName, assignmentKind, *permission) + "/"
	if account != nil {
		prefix += *account + "/"
	}

	records, err := storage.Records[api.AccountAssignment](tx, prefix)
	if err != nil {
		return nil, err
	}

	list := []api.AccountAssignment{}
	for _, r := range records {
		list = append(list, *r)
	}

	return list, nil
}

func (s *Service) ListAccountAssignments(ctx context.Context, in *api.ListAccountAssignmentsRequest) (
	*api.ListAccountAssignmentsResponse, error,
) {
	if in == nil || !accountPattern.MatchString(common.Value(in.AccountId)) {
		return nil, awserror.Validation("Invalid AccountId")
	}

	scope, err := checkScope(ctx, in.InstanceArn)
	if err != nil {
		return nil, err
	}

	limit, err := pageLimit(in.MaxResults)
	if err != nil {
		return nil, err
	}

	var list []api.AccountAssignment
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		list, e = accountAssignments(tx, scope, in.PermissionSetArn, in.AccountId)
		return e
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(
		list,
		func(a api.AccountAssignment) string { return assignmentID(&a) },
		in.NextToken,
		limit,
		awserror.Validation("Invalid NextToken"),
	)
	return &api.ListAccountAssignmentsResponse{AccountAssignments: page, NextToken: next}, err
}

func (s *Service) ListAccountsForProvisionedPermissionSet(
	ctx context.Context,
	in *api.ListAccountsForProvisionedPermissionSetRequest,
) (
	*api.ListAccountsForProvisionedPermissionSetResponse, error,
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

	accounts := []string{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		list, e := accountAssignments(tx, scope, in.PermissionSetArn, nil)
		if e != nil {
			return e
		}

		last := ""
		for _, a := range list {
			if *a.AccountId != last {
				accounts = append(accounts, *a.AccountId)
				last = *a.AccountId
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(
		accounts,
		func(a string) string { return a },
		in.NextToken,
		limit,
		awserror.Validation("Invalid NextToken"),
	)
	return &api.ListAccountsForProvisionedPermissionSetResponse{AccountIds: page, NextToken: next}, err
}

func assignmentName(a *api.AccountAssignment) string {
	return strings.Join([]string{*a.AccountId, *a.PrincipalType, *a.PrincipalId}, "|")
}
