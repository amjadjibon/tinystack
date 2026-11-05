package s3control

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3control"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/idempotency"
	"github.com/amjadjibon/tinystack/internal/services/s3"
	"github.com/amjadjibon/tinystack/internal/storage"
)

type mrapRecord struct {
	api.MultiRegionAccessPointReport
	Policy   string
	Proposed string
	Routes   []api.MultiRegionAccessPointRoute
}

type operationRecord struct {
	api.AsyncOperation
	Scope request.Scope
	Due   time.Time
	Name  string
}

func mrapScope(ctx context.Context, account *string) (request.Scope, error) {
	scope, err := scopeFor(ctx, account)
	scope.Region = ""
	return scope, err
}

func readMRAP(tx storage.ReadTx, scope request.Scope, name string) (*mrapRecord, error) {
	r, ok, err := storage.Read[mrapRecord](tx, scope.Key(serviceName, "mrap", name))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, controlError(
			404,
			"NoSuchMultiRegionAccessPoint",
			"The specified Multi-Region Access Point does not exist",
		)
	}

	return r, nil
}

func startOperation(
	tx storage.WriteTx,
	scope request.Scope,
	token, name, op string,
	params *api.AsyncRequestParameters,
) (*string, error) {
	tokenKey := idempotency.Key(scope, serviceName, "MRAP", new(token))
	input := struct {
		Parameters *api.AsyncRequestParameters
		Operation  string
	}{params, op}
	previous, found, replayErr := idempotency.Replay[*string](
		tx,
		tokenKey,
		input,
		controlError(400, "InvalidRequest", "ClientToken was used for a different request"),
	)
	if replayErr != nil || found {
		return previous, replayErr
	}

	key := scope.Key(serviceName, "operation", token)
	arn := fmt.Sprintf("arn:aws:s3:us-west-2:%s:async-request/mrap/%s", scope.AccountID, token)
	now := time.Now().UTC()
	r := operationRecord{
		AsyncOperation: api.AsyncOperation{
			CreationTime:      new(now),
			Operation:         new(op),
			RequestTokenARN:   new(arn),
			RequestParameters: params,
			RequestStatus:     new("INPROGRESS"),
		},
		Scope: scope,
		Name:  name,
		Due:   now.Add(200 * time.Millisecond),
	}

	if writeErr := storage.Write(tx, key, r); writeErr != nil {
		return nil, writeErr
	}

	if writeErr := storage.Write(tx, "aws/s3control/operations/"+scope.AccountID+"/"+token, key); writeErr != nil {
		return nil, writeErr
	}

	return new(arn), idempotency.Remember(tx, tokenKey, input, new(arn))
}

func (s *Service) CreateMultiRegionAccessPoint(
	ctx context.Context,
	in *api.CreateMultiRegionAccessPointRequest,
) (*api.CreateMultiRegionAccessPointResult, error) {
	scope, err := mrapScope(ctx, in.AccountId)
	if err != nil {
		return nil, err
	}

	details := in.Details
	if len(details.Regions) == 0 {
		return nil, awserror.New("InvalidRequest", "At least one bucket Region is required")
	}

	regions := make([]api.RegionReport, 0, len(details.Regions))
	routes := make([]api.MultiRegionAccessPointRoute, 0, len(details.Regions))
	seen := map[string]bool{}
	for _, r := range details.Regions {
		owner := common.Value(common.OrElse(r.BucketAccountId, in.AccountId))
		if owner != scope.AccountID {
			return nil, controlError(403, "AccessDenied", "Cross-account buckets are unsupported")
		}

		bucketScope, bucketErr := s.buckets.BucketScope(ctx, common.Value(r.Bucket), owner)
		if bucketErr != nil {
			return nil, bucketErr
		}

		if seen[bucketScope.Region] {
			return nil, awserror.New("InvalidRequest", "Only one bucket per Region is permitted")
		}

		seen[bucketScope.Region] = true
		regions = append(
			regions,
			api.RegionReport{Bucket: r.Bucket, BucketAccountId: new(owner), Region: new(bucketScope.Region)},
		)
		routes = append(
			routes,
			api.MultiRegionAccessPointRoute{
				Bucket:                r.Bucket,
				Region:                new(bucketScope.Region),
				TrafficDialPercentage: new(int64(100)),
			},
		)
	}

	config := common.Clone(details.PublicAccessBlock)
	if config == nil {
		config = &api.PublicAccessBlockConfiguration{
			BlockPublicAcls:       new(true),
			IgnorePublicAcls:      new(true),
			BlockPublicPolicy:     new(true),
			RestrictPublicBuckets: new(true),
		}
	}

	var arn *string
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		tokenKey := scope.Key(serviceName, "operation", common.Value(in.ClientToken))
		if _, ok := tx.Get(tokenKey); ok {
			var txErr error
			arn, txErr = startOperation(
				tx,
				scope,
				common.Value(in.ClientToken),
				common.Value(details.Name),
				"CreateMultiRegionAccessPoint",
				&api.AsyncRequestParameters{CreateMultiRegionAccessPointRequest: details},
			)
			return txErr
		}

		key := scope.Key(serviceName, "mrap", common.Value(details.Name))
		if _, ok := tx.Get(key); ok {
			return controlError(
				409,
				"MultiRegionAccessPointAlreadyOwnedByYou",
				"Multi-Region Access Point already exists",
			)
		}

		r := mrapRecord{
			MultiRegionAccessPointReport: api.MultiRegionAccessPointReport{
				Name:              details.Name,
				Alias:             new(common.RandomID(13, "abcdefghijklmnopqrstuvwxyz0123456789") + ".mrap"),
				CreatedAt:         new(time.Now().UTC()),
				PublicAccessBlock: config,
				Status:            new("CREATING"),
				Regions:           regions,
			},
			Routes: routes,
		}

		if writeErr := storage.Write(tx, key, r); writeErr != nil {
			return writeErr
		}

		var txErr error
		arn, txErr = startOperation(
			tx,
			scope,
			common.Value(in.ClientToken),
			common.Value(details.Name),
			"CreateMultiRegionAccessPoint",
			&api.AsyncRequestParameters{CreateMultiRegionAccessPointRequest: details},
		)
		return txErr
	})
	return &api.CreateMultiRegionAccessPointResult{RequestTokenARN: arn}, err
}

func (s *Service) advanceMRAP(ctx context.Context) {
	_ = s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, indexKey := range tx.Keys("aws/s3control/operations/") {
			key, _, txErr := storage.Read[string](tx, indexKey)
			if txErr != nil {
				return txErr
			}

			op, _, txErr := storage.Read[operationRecord](tx, *key)
			if txErr != nil {
				return txErr
			}

			if common.Value(op.RequestStatus) != "INPROGRESS" || time.Now().Before(op.Due) {
				continue
			}

			r, txErr := readMRAP(tx, op.Scope, op.Name)
			if txErr != nil {
				return txErr
			}

			resourceKey := op.Scope.Key(serviceName, "mrap", op.Name)
			switch common.Value(op.Operation) {
			case "DeleteMultiRegionAccessPoint":
				tx.Delete(resourceKey)
			default:
				r.Status = new("READY")
				if common.Value(op.Operation) == "PutMultiRegionAccessPointPolicy" {
					r.Policy = r.Proposed
					r.Proposed = ""
				}

				if writeErr := storage.Write(tx, resourceKey, r); writeErr != nil {
					return writeErr
				}
			}

			responses := []api.MultiRegionAccessPointRegionalResponse{}
			for _, region := range r.Regions {
				responses = append(
					responses,
					api.MultiRegionAccessPointRegionalResponse{Name: region.Region, RequestStatus: new("SUCCEEDED")},
				)
			}

			op.RequestStatus = new("SUCCEEDED")
			op.ResponseDetails = &api.AsyncResponseDetails{
				MultiRegionAccessPointDetails: &api.MultiRegionAccessPointsAsyncResponse{Regions: responses},
			}

			if writeErr := storage.Write(tx, *key, op); writeErr != nil {
				return writeErr
			}

			tx.Delete(indexKey)
		}

		return nil
	})
}

func (s *Service) DescribeMultiRegionAccessPointOperation(
	ctx context.Context,
	in *api.DescribeMultiRegionAccessPointOperationRequest,
) (*api.DescribeMultiRegionAccessPointOperationResult, error) {
	scope, err := mrapScope(ctx, in.AccountId)
	if err != nil {
		return nil, err
	}

	prefix := fmt.Sprintf("arn:aws:s3:us-west-2:%s:async-request/mrap/", scope.AccountID)
	token, ok := strings.CutPrefix(common.Value(in.RequestTokenARN), prefix)
	if !ok {
		return nil, controlError(404, "NoSuchAsyncRequest", "The asynchronous request does not exist")
	}

	var out *api.AsyncOperation
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, found, txErr := storage.Read[operationRecord](tx, scope.Key(serviceName, "operation", token))
		if txErr != nil {
			return txErr
		}

		if !found {
			return controlError(404, "NoSuchAsyncRequest", "The asynchronous request does not exist")
		}

		out = new(r.AsyncOperation)
		return nil
	})
	return &api.DescribeMultiRegionAccessPointOperationResult{AsyncOperation: out}, err
}

func (s *Service) GetMultiRegionAccessPoint(
	ctx context.Context,
	in *api.GetMultiRegionAccessPointRequest,
) (*api.GetMultiRegionAccessPointResult, error) {
	scope, err := mrapScope(ctx, in.AccountId)
	if err != nil {
		return nil, err
	}

	var out *api.MultiRegionAccessPointReport
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, txErr := readMRAP(tx, scope, common.Value(in.Name))
		if txErr != nil {
			return txErr
		}

		out = new(r.MultiRegionAccessPointReport)
		return nil
	})
	return &api.GetMultiRegionAccessPointResult{AccessPoint: out}, err
}

func (s *Service) ListMultiRegionAccessPoints(
	ctx context.Context,
	in *api.ListMultiRegionAccessPointsRequest,
) (*api.ListMultiRegionAccessPointsResult, error) {
	scope, err := mrapScope(ctx, in.AccountId)
	if err != nil {
		return nil, err
	}

	var items []api.MultiRegionAccessPointReport
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		records, txErr := storage.Records[mrapRecord](tx, scope.Prefix(serviceName, "mrap"))
		if txErr != nil {
			return txErr
		}

		for _, r := range records {
			items = append(items, r.MultiRegionAccessPointReport)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	page, token, err := common.Page(
		items,
		func(r api.MultiRegionAccessPointReport) string { return common.Value(r.Name) },
		in.NextToken,
		pageLimit(in.MaxResults),
		awserror.New("InvalidToken", "Invalid next token"),
	)
	return &api.ListMultiRegionAccessPointsResult{AccessPoints: page, NextToken: token}, err
}

func (s *Service) changeMRAP(ctx context.Context, account, token, name, policy *string) (*string, error) {
	scope, err := mrapScope(ctx, account)
	if err != nil {
		return nil, err
	}

	public := false
	if policy != nil {
		public, err = s3.PolicyIsPublic(*policy)
		if err != nil {
			return nil, err
		}
	}

	blocked, err := s.AccountBlocksPublicPolicy(ctx)
	if err != nil {
		return nil, err
	}

	var arn *string
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		params := &api.AsyncRequestParameters{
			DeleteMultiRegionAccessPointRequest: &api.DeleteMultiRegionAccessPointInput{Name: name},
		}

		op := "DeleteMultiRegionAccessPoint"
		if policy != nil {
			op = "PutMultiRegionAccessPointPolicy"
			params = &api.AsyncRequestParameters{
				PutMultiRegionAccessPointPolicyRequest: &api.PutMultiRegionAccessPointPolicyInput{
					Name:   name,
					Policy: policy,
				},
			}
		}

		if _, ok := tx.Get(scope.Key(serviceName, "operation", common.Value(token))); ok {
			var txErr error
			arn, txErr = startOperation(tx, scope, common.Value(token), common.Value(name), op, params)
			return txErr
		}

		r, txErr := readMRAP(tx, scope, common.Value(name))
		if txErr != nil {
			return txErr
		}

		if common.Value(r.Status) != "READY" || r.Proposed != "" {
			return controlError(409, "InvalidRequest", "A request for this Multi-Region Access Point is in progress")
		}

		if public && (blocked || common.Value(r.PublicAccessBlock.BlockPublicPolicy)) {
			return controlError(403, "AccessDenied", "Public policies are blocked")
		}

		if policy == nil {
			r.Status = new("DELETING")
		} else {
			r.Proposed = *policy
		}

		if writeErr := storage.Write(tx, scope.Key(serviceName, "mrap", common.Value(name)), r); writeErr != nil {
			return writeErr
		}

		arn, txErr = startOperation(tx, scope, common.Value(token), common.Value(name), op, params)
		return txErr
	})
	return arn, err
}

func (s *Service) DeleteMultiRegionAccessPoint(
	ctx context.Context,
	in *api.DeleteMultiRegionAccessPointRequest,
) (*api.DeleteMultiRegionAccessPointResult, error) {
	arn, err := s.changeMRAP(ctx, in.AccountId, in.ClientToken, in.Details.Name, nil)
	return &api.DeleteMultiRegionAccessPointResult{RequestTokenARN: arn}, err
}

func (s *Service) PutMultiRegionAccessPointPolicy(
	ctx context.Context,
	in *api.PutMultiRegionAccessPointPolicyRequest,
) (*api.PutMultiRegionAccessPointPolicyResult, error) {
	arn, err := s.changeMRAP(ctx, in.AccountId, in.ClientToken, in.Details.Name, in.Details.Policy)
	return &api.PutMultiRegionAccessPointPolicyResult{RequestTokenARN: arn}, err
}

func (s *Service) GetMultiRegionAccessPointPolicy(
	ctx context.Context,
	in *api.GetMultiRegionAccessPointPolicyRequest,
) (*api.GetMultiRegionAccessPointPolicyResult, error) {
	scope, err := mrapScope(ctx, in.AccountId)
	if err != nil {
		return nil, err
	}

	out := &api.MultiRegionAccessPointPolicyDocument{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, txErr := readMRAP(tx, scope, common.Value(in.Name))
		if txErr != nil {
			return txErr
		}

		if r.Policy != "" {
			out.Established = &api.EstablishedMultiRegionAccessPointPolicy{Policy: new(r.Policy)}
		}

		if r.Proposed != "" {
			out.Proposed = &api.ProposedMultiRegionAccessPointPolicy{Policy: new(r.Proposed)}
		}

		return nil
	})
	return &api.GetMultiRegionAccessPointPolicyResult{Policy: out}, err
}

func (s *Service) GetMultiRegionAccessPointPolicyStatus(
	ctx context.Context,
	in *api.GetMultiRegionAccessPointPolicyStatusRequest,
) (*api.GetMultiRegionAccessPointPolicyStatusResult, error) {
	out, err := s.GetMultiRegionAccessPointPolicy(
		ctx,
		&api.GetMultiRegionAccessPointPolicyRequest{AccountId: in.AccountId, Name: in.Name},
	)
	if err != nil {
		return nil, err
	}

	public := false
	if out.Policy.Established != nil {
		public, err = s3.PolicyIsPublic(common.Value(out.Policy.Established.Policy))
		if err != nil {
			return nil, err
		}
	}

	return &api.GetMultiRegionAccessPointPolicyStatusResult{Established: &api.PolicyStatus{IsPublic: new(public)}}, nil
}
