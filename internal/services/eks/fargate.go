package eks

import (
	"context"
	"uuid"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/eks"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/idempotency"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// fargateProfile is a Fargate profile's record: its description and its create
// token.
type fargateProfile struct {
	Profile api.FargateProfile
}

func fargateKey(scope request.Scope, clusterName, name string) string {
	return owned(scope, kindFargate, clusterName, name)
}

func profileNotFound(name string) error {
	return notFound("No Fargate Profile found with name: %s.", name)
}

func (s *Service) CreateFargateProfile(
	ctx context.Context,
	in *api.CreateFargateProfileRequest,
) (*api.CreateFargateProfileResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if checkErr := checkName(in.FargateProfileName, "fargateProfileName", 63); checkErr != nil {
		return nil, checkErr
	}

	if checkErr := checkRole(in.PodExecutionRoleArn, "podExecutionRoleArn"); checkErr != nil {
		return nil, checkErr
	}

	if len(in.Selectors) < 1 || len(in.Selectors) > 5 {
		return nil, invalidParameter("A Fargate profile needs 1 to 5 selectors.")
	}

	for _, sel := range in.Selectors {
		if common.Value(sel.Namespace) == "" || len(sel.Labels) > 5 {
			return nil, invalidParameter("Each selector needs a namespace and can have up to 5 labels.")
		}
	}

	if checkErr := checkTags(in.Tags); checkErr != nil {
		return nil, checkErr
	}

	placed, err := s.place(ctx, scope, in.Subnets, nil)
	if err != nil {
		return nil, err
	}

	var out *api.FargateProfile
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		token := idempotency.Key(scope, service, "CreateFargateProfile", in.ClientRequestToken)
		previous, found, replayErr := idempotency.Replay[*api.FargateProfile](tx, token, in,
			invalidParameter("ClientRequestToken was used with different parameters."))
		if replayErr != nil || found {
			out = previous
			return replayErr
		}

		c, loadErr := loadCluster(tx, scope, in.ClusterName)
		if loadErr != nil {
			return loadErr
		}

		if len(in.Subnets) > 0 && placed.VPC != common.Value(c.Vpc.VpcId) {
			return invalidParameter("Subnets must be in the cluster's VPC %s.", common.Value(c.Vpc.VpcId))
		}

		name := *in.FargateProfileName
		_, ok, readErr := storage.Read[fargateProfile](tx, fargateKey(scope, c.Name, name))
		switch {
		case readErr != nil:
			return readErr
		case ok:
			return inUse("A Fargate Profile already exists with this name in this cluster.")
		}

		p := &fargateProfile{Profile: api.FargateProfile{
			FargateProfileName: new(name),
			FargateProfileArn:  new(arn(scope, "fargateprofile/"+c.Name+"/"+name+"/"+uuid.NewV4().String())),
			ClusterName: new(
				c.Name,
			),
			CreatedAt:           common.SecondsMilli(s.now().UTC()),
			PodExecutionRoleArn: in.PodExecutionRoleArn,
			Subnets:             orEmpty(in.Subnets),
			Selectors:           in.Selectors,
			Status:              new(active),
			Tags:                common.CloneTags(in.Tags),
			Health:              &api.FargateProfileHealth{Issues: []api.FargateProfileIssue{}},
		}}
		out = &p.Profile
		if e := storage.Write(tx, fargateKey(scope, c.Name, name), p); e != nil {
			return e
		}

		return idempotency.Remember(tx, token, in, out)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateFargateProfileResponse{FargateProfile: out}, nil
}

// fargate runs fn on a cluster's Fargate profile.
func (s *Service) fargate(
	ctx context.Context,
	clusterName, name *string,
	fn func(storage.WriteTx, request.Scope, *fargateProfile) error,
) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		c, loadErr := loadCluster(tx, scope, clusterName)
		if loadErr != nil {
			return loadErr
		}

		p, ok, readErr := storage.Read[fargateProfile](tx, fargateKey(scope, c.Name, common.Value(name)))
		if readErr != nil || !ok {
			return common.ErrorsOr(readErr, profileNotFound(common.Value(name)))
		}

		return fn(tx, scope, p)
	})
}

func (s *Service) DescribeFargateProfile(
	ctx context.Context,
	in *api.DescribeFargateProfileRequest,
) (*api.DescribeFargateProfileResponse, error) {
	var out *api.FargateProfile
	err := s.fargate(ctx, in.ClusterName, in.FargateProfileName,
		func(_ storage.WriteTx, _ request.Scope, p *fargateProfile) error {
			out = &p.Profile
			return nil
		})
	if err != nil {
		return nil, err
	}

	return &api.DescribeFargateProfileResponse{FargateProfile: out}, nil
}

func (s *Service) ListFargateProfiles(
	ctx context.Context,
	in *api.ListFargateProfilesRequest,
) (*api.ListFargateProfilesResponse, error) {
	names, next, err := s.names(ctx, in.ClusterName, kindFargate, in.NextToken, in.MaxResults)
	if err != nil {
		return nil, err
	}

	return &api.ListFargateProfilesResponse{FargateProfileNames: names, NextToken: next}, nil
}

func (s *Service) DeleteFargateProfile(
	ctx context.Context,
	in *api.DeleteFargateProfileRequest,
) (*api.DeleteFargateProfileResponse, error) {
	var out *api.FargateProfile
	err := s.fargate(ctx, in.ClusterName, in.FargateProfileName,
		func(tx storage.WriteTx, scope request.Scope, p *fargateProfile) error {
			tx.Delete(fargateKey(scope, *p.Profile.ClusterName, *p.Profile.FargateProfileName))
			p.Profile.Status = new(deleting)
			out = &p.Profile
			return nil
		})
	if err != nil {
		return nil, err
	}

	return &api.DeleteFargateProfileResponse{FargateProfile: out}, nil
}
