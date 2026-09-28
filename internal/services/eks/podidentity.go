package eks

import (
	"context"
	"regexp"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/eks"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// association is a pod identity association's record: its description and
// its create token.
type association struct {
	Association api.PodIdentityAssociation
	Token       string
}

func associationKey(scope request.Scope, clusterName, id string) string {
	return owned(scope, "pod-identity", clusterName, id)
}

var kubeName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)

// externalID returns the external ID a target role's association assumes it
// with, if it has one.
func externalID(targetRole *string) *string {
	if value(targetRole) == "" {
		return nil
	}

	return new(newUUID())
}

func (s *Service) CreatePodIdentityAssociation(
	ctx context.Context,
	in *api.CreatePodIdentityAssociationRequest,
) (*api.CreatePodIdentityAssociationResponse, error) {
	scope, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	switch {
	case !kubeName.MatchString(value(in.Namespace)) || !kubeName.MatchString(value(in.ServiceAccount)):
		return nil, invalidParameter("namespace and serviceAccount must be Kubernetes names.")
	case checkRole(in.RoleArn, "roleArn") != nil:
		return nil, checkRole(in.RoleArn, "roleArn")
	case in.TargetRoleArn != nil && checkRole(in.TargetRoleArn, "targetRoleArn") != nil:
		return nil, checkRole(in.TargetRoleArn, "targetRoleArn")
	}

	if checkErr := checkTags(in.Tags); checkErr != nil {
		return nil, checkErr
	}

	var out *api.PodIdentityAssociation
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		c, loadErr := loadCluster(tx, scope, in.ClusterName)
		if loadErr != nil {
			return loadErr
		}

		existing, readErr := records[association](tx, prefix(scope, "pod-identity")+c.Name+"/")
		if readErr != nil {
			return readErr
		}

		// A service account has one association.
		for _, a := range existing {
			if *a.Association.Namespace != *in.Namespace || *a.Association.ServiceAccount != *in.ServiceAccount {
				continue
			}

			if value(in.ClientRequestToken) != "" && a.Token == *in.ClientRequestToken {
				out = &a.Association
				return nil
			}

			return inUse("Association already exists: %s", *a.Association.AssociationId)
		}

		now := s.now().UTC()
		id := "a-" + hexID(17)
		a := &association{Token: value(in.ClientRequestToken), Association: api.PodIdentityAssociation{
			ClusterName: new(c.Name), Namespace: in.Namespace, ServiceAccount: in.ServiceAccount, RoleArn: in.RoleArn,
			AssociationArn: new(arn(scope, "podidentityassociation/"+c.Name+"/"+id)), AssociationId: new(id),
			Tags: cloneTags(in.Tags), CreatedAt: seconds(now), ModifiedAt: seconds(now),
			DisableSessionTags: new(value(in.DisableSessionTags)), TargetRoleArn: in.TargetRoleArn,
			ExternalId: externalID(in.TargetRoleArn),
		}}
		out = &a.Association
		return write(tx, associationKey(scope, c.Name, id), a)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreatePodIdentityAssociationResponse{Association: out}, nil
}

// changeAssociation runs fn on a pod identity association.
func (s *Service) changeAssociation(
	ctx context.Context,
	clusterName, id *string,
	fn func(storage.WriteTx, request.Scope, *association) error,
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

		a, ok, readErr := read[association](tx, associationKey(scope, c.Name, value(id)))
		if readErr != nil || !ok {
			return errorsOr(readErr, notFound("Association does not exist: %s", value(id)))
		}

		return fn(tx, scope, a)
	})
}

func (s *Service) DescribePodIdentityAssociation(
	ctx context.Context,
	in *api.DescribePodIdentityAssociationRequest,
) (*api.DescribePodIdentityAssociationResponse, error) {
	var out *api.PodIdentityAssociation
	err := s.changeAssociation(ctx, in.ClusterName, in.AssociationId,
		func(_ storage.WriteTx, _ request.Scope, a *association) error {
			out = &a.Association
			return nil
		})
	if err != nil {
		return nil, err
	}

	return &api.DescribePodIdentityAssociationResponse{Association: out}, nil
}

func (s *Service) ListPodIdentityAssociations(
	ctx context.Context,
	in *api.ListPodIdentityAssociationsRequest,
) (*api.ListPodIdentityAssociationsResponse, error) {
	scope, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.ListPodIdentityAssociationsResponse{Associations: []api.PodIdentityAssociationSummary{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		c, loadErr := loadCluster(tx, scope, in.ClusterName)
		if loadErr != nil {
			return loadErr
		}

		all, readErr := records[association](tx, prefix(scope, "pod-identity")+c.Name+"/")
		if readErr != nil {
			return readErr
		}

		var summaries []api.PodIdentityAssociationSummary
		for _, a := range all {
			p := &a.Association
			if in.Namespace != nil && *p.Namespace != *in.Namespace ||
				in.ServiceAccount != nil && *p.ServiceAccount != *in.ServiceAccount {
				continue
			}

			summaries = append(summaries, api.PodIdentityAssociationSummary{
				ClusterName: p.ClusterName, Namespace: p.Namespace, ServiceAccount: p.ServiceAccount,
				AssociationArn: p.AssociationArn, AssociationId: p.AssociationId,
			})
		}

		summaries, out.NextToken, readErr = page(summaries,
			func(a api.PodIdentityAssociationSummary) string { return *a.AssociationId }, in.NextToken, in.MaxResults)
		out.Associations = append(out.Associations, summaries...)
		return readErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) UpdatePodIdentityAssociation(
	ctx context.Context,
	in *api.UpdatePodIdentityAssociationRequest,
) (*api.UpdatePodIdentityAssociationResponse, error) {
	for member, role := range map[string]*string{"roleArn": in.RoleArn, "targetRoleArn": in.TargetRoleArn} {
		if value(role) != "" {
			if err := checkRole(role, member); err != nil {
				return nil, err
			}
		}
	}

	var out *api.PodIdentityAssociation
	err := s.changeAssociation(ctx, in.ClusterName, in.AssociationId,
		func(tx storage.WriteTx, scope request.Scope, a *association) error {
			p := &a.Association
			p.RoleArn = orElse(in.RoleArn, p.RoleArn)
			p.DisableSessionTags = orElse(in.DisableSessionTags, p.DisableSessionTags)
			// An empty target role removes it.
			if in.TargetRoleArn != nil && value(in.TargetRoleArn) != value(p.TargetRoleArn) {
				p.TargetRoleArn, p.ExternalId = nonEmpty(*in.TargetRoleArn), externalID(in.TargetRoleArn)
			}

			p.ModifiedAt = seconds(s.now().UTC())
			out = p
			return write(tx, associationKey(scope, *p.ClusterName, *p.AssociationId), a)
		})
	if err != nil {
		return nil, err
	}

	return &api.UpdatePodIdentityAssociationResponse{Association: out}, nil
}

func (s *Service) DeletePodIdentityAssociation(
	ctx context.Context,
	in *api.DeletePodIdentityAssociationRequest,
) (*api.DeletePodIdentityAssociationResponse, error) {
	var out *api.PodIdentityAssociation
	err := s.changeAssociation(ctx, in.ClusterName, in.AssociationId,
		func(tx storage.WriteTx, scope request.Scope, a *association) error {
			tx.Delete(associationKey(scope, *a.Association.ClusterName, *a.Association.AssociationId))
			out = &a.Association
			return nil
		})
	if err != nil {
		return nil, err
	}

	return &api.DeletePodIdentityAssociationResponse{Association: out}, nil
}
