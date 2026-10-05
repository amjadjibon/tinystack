package ec2

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/iam"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	associated     = "associated"
	disassociating = "disassociating"
)

// InstanceProfiles finds an IAM instance profile in an account by name or ARN.
type InstanceProfiles func(ctx context.Context, account, ref string) (iam.InstanceProfile, bool, error)

// UseInstanceProfiles lets instances be launched with, and associated with,
// IAM instance profiles. Call it before serving requests.
func (s *Service) UseInstanceProfiles(profiles InstanceProfiles) { s.profiles = profiles }

// profileAssociation is an instance's association with an instance profile.
type profileAssociation struct {
	Since     time.Time
	ID        string
	ARN       string
	ProfileID string
}

func (a *profileAssociation) output(instanceID, state string) *api.IamInstanceProfileAssociation {
	return &api.IamInstanceProfileAssociation{
		AssociationId: new(a.ID), InstanceId: new(instanceID), State: new(state), Timestamp: new(a.Since),
		IamInstanceProfile: &api.IamInstanceProfile{Arn: new(a.ARN), Id: new(a.ProfileID)},
	}
}

// resolveProfile finds the instance profile a request names, outside any
// transaction, and returns a new association with it.
func (s *Service) resolveProfile(ctx context.Context, spec *api.IamInstanceProfileSpecification) (
	*profileAssociation, error,
) {
	if spec == nil {
		return nil, awserror.New("MissingParameter", "The request must contain the parameter IamInstanceProfile")
	}

	ref, field := common.Value(spec.Name), "name"
	switch {
	case spec.Arn != nil && spec.Name != nil:
		return nil, awserror.New("InvalidParameterCombination", "The parameter 'iamInstanceProfile.name' "+
			"may not be used in combination with 'iamInstanceProfile.arn'")
	case spec.Arn != nil:
		ref, field = *spec.Arn, "arn"
	case ref == "":
		return nil, awserror.New("MissingParameter", "The request must contain the parameter "+
			"iamInstanceProfile.name or iamInstanceProfile.arn")
	}

	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	found := false
	var p iam.InstanceProfile
	if s.profiles != nil {
		if p, found, err = s.profiles(ctx, m.AccountID, ref); err != nil {
			return nil, err
		}
	}

	if !found {
		return nil, invalidParameter("Value (" + ref + ") for parameter iamInstanceProfile." + field +
			" is invalid. Invalid IAM Instance Profile " + field)
	}

	return &profileAssociation{Since: s.now().UTC(), ID: newID("iip-assoc"), ARN: p.ARN, ProfileID: p.ID}, nil
}

// findAssociation returns the active instance that holds an association.
func findAssociation(tx storage.ReadTx, scope request.Scope, id string) (*instance, error) {
	all, err := list[instance](tx, scope, kindInstance)
	if err != nil {
		return nil, err
	}

	for _, i := range all {
		if i.active() && i.Profile != nil && i.Profile.ID == id {
			return i, nil
		}
	}

	return nil, awserror.New("InvalidAssociationID.NotFound", "An association with the ID "+id+
		" does not exist.")
}

func (s *Service) AssociateIamInstanceProfile(
	ctx context.Context,
	in *api.AssociateIamInstanceProfileRequest,
) (*api.AssociateIamInstanceProfileResult, error) {
	a, err := s.resolveProfile(ctx, in.IamInstanceProfile)
	if err != nil {
		return nil, err
	}

	id := common.Value(in.InstanceId)
	err = s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		i, ok, readErr := storage.Read[instance](tx, scope.Key("ec2", kindInstance, id))
		switch {
		case readErr != nil:
			return readErr
		case !ok || !i.visible(s.now()):
			return instanceNotFound(id)
		case i.State != stateRunning && i.State != stateStopped:
			return awserror.New("IncorrectState", "The instance '"+id+"' is not in a valid state for this "+
				"operation.")
		case i.Profile != nil:
			return awserror.New("IncorrectState", "There is an existing association for instance "+id)
		}

		i.Profile = a
		return storage.Write(tx, scope.Key("ec2", kindInstance, id), i)
	})
	if err != nil {
		return nil, err
	}

	return &api.AssociateIamInstanceProfileResult{IamInstanceProfileAssociation: a.output(id, associated)}, nil
}

func (s *Service) DescribeIamInstanceProfileAssociations(
	ctx context.Context,
	in *api.DescribeIamInstanceProfileAssociationsRequest,
) (*api.DescribeIamInstanceProfileAssociationsResult, error) {
	match, err := compileFilters(in.Filters, "instance-id", "state")
	if err != nil {
		return nil, err
	}

	type held struct {
		profile  *profileAssociation
		instance string
	}

	out := &api.DescribeIamInstanceProfileAssociationsResult{
		IamInstanceProfileAssociations: []api.IamInstanceProfileAssociation{},
	}
	err = s.view(ctx, nil, func(tx storage.ReadTx, scope request.Scope) error {
		all, listErr := list[instance](tx, scope, kindInstance)
		if listErr != nil {
			return listErr
		}

		var associations []*held
		for _, i := range all {
			if i.active() && i.Profile != nil &&
				match.match(values{filterInstanceID: {i.ID}, "state": {associated}}) {
				associations = append(associations, &held{profile: i.Profile, instance: i.ID})
			}
		}

		slices.SortFunc(associations, func(a, b *held) int {
			return strings.Compare(a.profile.ID, b.profile.ID)
		})
		associations, selectErr := selectIDs(associations, func(h *held) string { return h.profile.ID },
			in.AssociationIds, func(id string) error {
				return awserror.New("InvalidAssociationID.NotFound", "An association with the ID "+id+
					" does not exist.")
			})
		if selectErr != nil {
			return selectErr
		}

		associations, out.NextToken, selectErr = page(associations, func(h *held) string { return h.profile.ID },
			in.NextToken, in.MaxResults, maxDescribe)
		for _, h := range associations {
			out.IamInstanceProfileAssociations = append(out.IamInstanceProfileAssociations,
				*h.profile.output(h.instance, associated))
		}

		return selectErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DisassociateIamInstanceProfile(
	ctx context.Context,
	in *api.DisassociateIamInstanceProfileRequest,
) (*api.DisassociateIamInstanceProfileResult, error) {
	var out *api.IamInstanceProfileAssociation
	err := s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		i, findErr := findAssociation(tx, scope, common.Value(in.AssociationId))
		if findErr != nil {
			return findErr
		}

		out = i.Profile.output(i.ID, disassociating)
		i.Profile = nil
		return storage.Write(tx, scope.Key("ec2", kindInstance, i.ID), i)
	})
	if err != nil {
		return nil, err
	}

	return &api.DisassociateIamInstanceProfileResult{IamInstanceProfileAssociation: out}, nil
}

// ReplaceIamInstanceProfileAssociation swaps an instance's instance profile in
// one step, under a new association ID.
func (s *Service) ReplaceIamInstanceProfileAssociation(
	ctx context.Context,
	in *api.ReplaceIamInstanceProfileAssociationRequest,
) (*api.ReplaceIamInstanceProfileAssociationResult, error) {
	a, err := s.resolveProfile(ctx, in.IamInstanceProfile)
	if err != nil {
		return nil, err
	}

	var instanceID string
	err = s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		i, findErr := findAssociation(tx, scope, common.Value(in.AssociationId))
		if findErr != nil {
			return findErr
		}

		instanceID, i.Profile = i.ID, a
		return storage.Write(tx, scope.Key("ec2", kindInstance, i.ID), i)
	})
	if err != nil {
		return nil, err
	}

	return &api.ReplaceIamInstanceProfileAssociationResult{
		IamInstanceProfileAssociation: a.output(instanceID, associated),
	}, nil
}
