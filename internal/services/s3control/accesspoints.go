package s3control

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	s3api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3control"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/s3"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const serviceName = "s3control"

type accessPointRecord struct {
	api.GetAccessPointResult
	Policy string
	Tags   map[string]string
}

func scopeFor(ctx context.Context, account *string) (request.Scope, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return scope, err
	}

	if common.Value(account) != scope.AccountID {
		return scope, controlError(403, "AccessDenied", "Account does not own the resource")
	}

	return scope, nil
}

func controlError(status int, code, message string) error {
	return &awserror.Error{StatusCode: status, Code: code, Message: message}
}

func missingPoint() error {
	return controlError(404, "NoSuchAccessPoint", "The specified access point does not exist")
}
func missingPolicy() error {
	return controlError(404, "NoSuchAccessPointPolicy", "The access point policy does not exist")
}

func pointName(scope request.Scope, name *string) string {
	return strings.TrimPrefix(common.Value(name), fmt.Sprintf("arn:aws:s3:%s:%s:accesspoint/", scope.Region, scope.AccountID))
}

func readPoint(tx storage.ReadTx, scope request.Scope, name *string) (*accessPointRecord, error) {
	r, ok, err := storage.Read[accessPointRecord](tx, scope.Key(serviceName, "accesspoint", pointName(scope, name)))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, missingPoint()
	}

	return r, nil
}

var pointPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,48}[a-z0-9]$`)

func (s *Service) CreateAccessPoint(ctx context.Context, in *api.CreateAccessPointRequest) (*api.CreateAccessPointResult, error) {
	scope, err := scopeFor(ctx, in.AccountId)
	if err != nil {
		return nil, err
	}

	name := common.Value(in.Name)
	if !pointPattern.MatchString(name) || strings.HasPrefix(name, "xn--") || strings.HasSuffix(name, "-s3alias") || strings.HasSuffix(name, "--ol-s3") {
		return nil, controlError(400, "InvalidAccessPointName", "Invalid general purpose access point name")
	}

	if in.BucketAccountId != nil && *in.BucketAccountId != scope.AccountID {
		return nil, controlError(403, "AccessDenied", "Bucket account does not match")
	}

	if _, err = s.buckets.HeadBucket(ctx, &s3api.HeadBucketRequest{Bucket: in.Bucket, ExpectedBucketOwner: in.AccountId}); err != nil {
		return nil, err
	}

	tags := make(map[string]string, len(in.Tags))
	for _, t := range in.Tags {
		k := common.Value(t.Key)
		if _, ok := tags[k]; ok {
			return nil, awserror.New("InvalidTag", "Duplicate tag key")
		}

		tags[k] = common.Value(t.Value)
	}

	if err = tagging.CheckSet(tags); err != nil {
		return nil, awserror.New("InvalidTag", err.Error())
	}

	config := common.Clone(in.PublicAccessBlockConfiguration)
	if config == nil {
		config = &api.PublicAccessBlockConfiguration{BlockPublicAcls: new(true), IgnorePublicAcls: new(true), BlockPublicPolicy: new(true), RestrictPublicBuckets: new(true)}
	}

	origin := "Internet"
	if in.VpcConfiguration != nil {
		origin = "VPC"
	}

	arn := fmt.Sprintf("arn:aws:s3:%s:%s:accesspoint/%s", scope.Region, scope.AccountID, name)
	alias := name + "-" + common.RandomID(24, "abcdefghijklmnopqrstuvwxyz0123456789") + "-s3alias"
	r := accessPointRecord{GetAccessPointResult: api.GetAccessPointResult{
		Name: in.Name, Bucket: in.Bucket, NetworkOrigin: new(origin), VpcConfiguration: in.VpcConfiguration,
		PublicAccessBlockConfiguration: config, CreationDate: new(time.Now().UTC()), Alias: new(alias), AccessPointArn: new(arn),
		BucketAccountId: in.AccountId, Endpoints: map[string]string{"ipv4": fmt.Sprintf("%s-%s.s3-accesspoint.%s.amazonaws.com", name, scope.AccountID, scope.Region)},
	}, Tags: tags}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		key := scope.Key(serviceName, "accesspoint", name)
		if _, ok := tx.Get(key); ok {
			return controlError(http.StatusConflict, "AccessPointAlreadyOwnedByYou", "Access point already exists")
		}

		if in.VpcConfiguration != nil {
			if _, err := s.network.MountVPC(tx, scope, common.Value(in.VpcConfiguration.VpcId)); err != nil {
				return err
			}
		}

		if err := storage.Write(tx, key, r); err != nil {
			return err
		}

		return storage.Write(tx, "aws/s3control/aliases/"+alias, aliasRecord{Bucket: common.Value(in.Bucket), Account: scope.AccountID, Region: scope.Region, Name: name})
	})
	return &api.CreateAccessPointResult{AccessPointArn: new(arn), Alias: new(alias)}, err
}

type aliasRecord struct{ Bucket, Account, Region, Name string }

func (s *Service) GetAccessPoint(ctx context.Context, in *api.GetAccessPointRequest) (*api.GetAccessPointResult, error) {
	scope, err := scopeFor(ctx, in.AccountId)
	if err != nil {
		return nil, err
	}

	var r *accessPointRecord
	err = s.store.View(ctx, func(tx storage.ReadTx) error { var e error; r, e = readPoint(tx, scope, in.Name); return e })
	if err != nil {
		return nil, err
	}

	return &r.GetAccessPointResult, nil
}

func (s *Service) DeleteAccessPoint(ctx context.Context, in *api.DeleteAccessPointRequest) (*api.DeleteAccessPointOutput, error) {
	scope, err := scopeFor(ctx, in.AccountId)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, err := readPoint(tx, scope, in.Name)
		if err != nil {
			if e, ok := err.(*awserror.Error); ok && e.Code == "NoSuchAccessPoint" {
				return nil
			}
			return err
		}

		tx.Delete("aws/s3control/aliases/" + common.Value(r.Alias))
		tx.Delete(scope.Key(serviceName, "accesspoint", pointName(scope, in.Name)))
		return nil
	})
	return &api.DeleteAccessPointOutput{}, err
}

func (s *Service) ListAccessPoints(ctx context.Context, in *api.ListAccessPointsRequest) (*api.ListAccessPointsResult, error) {
	scope, err := scopeFor(ctx, in.AccountId)
	if err != nil {
		return nil, err
	}

	var items []api.AccessPoint
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		records, err := storage.Records[accessPointRecord](tx, scope.Prefix(serviceName, "accesspoint"))
		if err != nil {
			return err
		}

		for _, r := range records {
			if in.Bucket != nil && *in.Bucket != common.Value(r.Bucket) {
				continue
			}

			items = append(items, api.AccessPoint{Name: r.Name, Bucket: r.Bucket, NetworkOrigin: r.NetworkOrigin, VpcConfiguration: r.VpcConfiguration, AccessPointArn: r.AccessPointArn, Alias: r.Alias, BucketAccountId: r.BucketAccountId})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	page, token, err := common.Page(items, func(r api.AccessPoint) string { return common.Value(r.Name) }, in.NextToken, int(common.Value(common.OrElse(in.MaxResults, new(int64(1000))))), awserror.New("InvalidToken", "Invalid next token"))
	return &api.ListAccessPointsResult{AccessPointList: page, NextToken: token}, err
}

func (s *Service) editPointPolicy(ctx context.Context, account, name, policy *string) error {
	scope, err := scopeFor(ctx, account)
	if err != nil {
		return err
	}

	isPublic := false
	if policy != nil {
		isPublic, err = s3.PolicyIsPublic(*policy)
		if err != nil {
			return err
		}
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, err := readPoint(tx, scope, name)
		if err != nil {
			return err
		}

		if policy != nil && isPublic && common.Value(r.PublicAccessBlockConfiguration.BlockPublicPolicy) {
			return controlError(403, "AccessDenied", "Public access point policies are blocked")
		}

		r.Policy = common.Value(policy)
		return storage.Write(tx, scope.Key(serviceName, "accesspoint", common.Value(r.Name)), r)
	})
}

func (s *Service) PutAccessPointPolicy(ctx context.Context, in *api.PutAccessPointPolicyRequest) (*api.PutAccessPointPolicyOutput, error) {
	err := s.editPointPolicy(ctx, in.AccountId, in.Name, in.Policy)
	return &api.PutAccessPointPolicyOutput{}, err
}

func (s *Service) DeleteAccessPointPolicy(ctx context.Context, in *api.DeleteAccessPointPolicyRequest) (*api.DeleteAccessPointPolicyOutput, error) {
	err := s.editPointPolicy(ctx, in.AccountId, in.Name, nil)
	return &api.DeleteAccessPointPolicyOutput{}, err
}

func (s *Service) GetAccessPointPolicy(ctx context.Context, in *api.GetAccessPointPolicyRequest) (*api.GetAccessPointPolicyResult, error) {
	scope, err := scopeFor(ctx, in.AccountId)
	if err != nil {
		return nil, err
	}

	var policy string
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, err := readPoint(tx, scope, in.Name)
		if err != nil {
			return err
		}

		if r.Policy == "" {
			return missingPolicy()
		}

		policy = r.Policy
		return nil
	})
	return &api.GetAccessPointPolicyResult{Policy: new(policy)}, err
}

func (s *Service) GetAccessPointPolicyStatus(ctx context.Context, in *api.GetAccessPointPolicyStatusRequest) (*api.GetAccessPointPolicyStatusResult, error) {
	out, err := s.GetAccessPointPolicy(ctx, &api.GetAccessPointPolicyRequest{AccountId: in.AccountId, Name: in.Name})
	if err != nil {
		return nil, err
	}

	public, err := s3.PolicyIsPublic(common.Value(out.Policy))
	return &api.GetAccessPointPolicyStatusResult{PolicyStatus: &api.PolicyStatus{IsPublic: new(public)}}, err
}
