package eks

import (
	"context"
	"uuid"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/eks"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// fargateProfile is a Fargate profile's record: its description and its create
// token.
type fargateProfile struct {
	Profile api.FargateProfile
	Token   string
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
	scope, err := identity(ctx)
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
		if value(sel.Namespace) == "" || len(sel.Labels) > 5 {
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
		c, loadErr := loadCluster(tx, scope, in.ClusterName)
		if loadErr != nil {
			return loadErr
		}

		if len(in.Subnets) > 0 && placed.VPC != value(c.Vpc.VpcId) {
			return invalidParameter("Subnets must be in the cluster's VPC %s.", value(c.Vpc.VpcId))
		}

		name := *in.FargateProfileName
		existing, ok, readErr := read[fargateProfile](tx, fargateKey(scope, c.Name, name))
		switch {
		case readErr != nil:
			return readErr
		case ok && value(in.ClientRequestToken) != "" && existing.Token == *in.ClientRequestToken:
			out = &existing.Profile
			return nil
		case ok:
			return inUse("A Fargate Profile already exists with this name in this cluster.")
		}

		p := &fargateProfile{Token: value(in.ClientRequestToken), Profile: api.FargateProfile{
			FargateProfileName: new(name),
			FargateProfileArn:  new(arn(scope, "fargateprofile/"+c.Name+"/"+name+"/"+uuid.NewV4().String())),
			ClusterName: new(
				c.Name,
			),
			CreatedAt:           seconds(s.now().UTC()),
			PodExecutionRoleArn: in.PodExecutionRoleArn,
			Subnets:             orEmpty(in.Subnets),
			Selectors:           in.Selectors,
			Status:              new(active),
			Tags:                cloneTags(in.Tags),
			Health:              &api.FargateProfileHealth{Issues: []api.FargateProfileIssue{}},
		}}
		out = &p.Profile
		return write(tx, fargateKey(scope, c.Name, name), p)
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
	scope, err := identity(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		c, loadErr := loadCluster(tx, scope, clusterName)
		if loadErr != nil {
			return loadErr
		}

		p, ok, readErr := read[fargateProfile](tx, fargateKey(scope, c.Name, value(name)))
		if readErr != nil || !ok {
			return errorsOr(readErr, profileNotFound(value(name)))
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
