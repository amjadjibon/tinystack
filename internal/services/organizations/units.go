package organizations

import (
	"context"
	"maps"
	"slices"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/organizations"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func selected(r *record, kind, parentID string) []*resource {
	out := []*resource{}
	for _, id := range slices.Sorted(maps.Keys(r.Resources)) {
		x := r.Resources[id]
		if (kind == "" || x.Kind == kind) && (parentID == "" || x.Parent == parentID) {
			out = append(out, x)
		}
	}

	return out
}

func (s *Service) accounts(
	ctx context.Context,
	parentID string,
	token *string,
	maxResults *int64,
) ([]api.Account, *string, error) {
	out := []api.Account{}
	var next *string
	err := s.view(ctx, true, func(r *record) error {
		if parentID != "" {
			if checkErr := parent(r, parentID); checkErr != nil {
				return checkErr
			}
		}

		items, n, txErr := page(
			selected(r, kindAccount, parentID),
			func(x *resource) string { return x.ID },
			token,
			maxResults,
		)
		if txErr != nil {
			return txErr
		}

		next = n
		for _, x := range items {
			out = append(out, *x.Account)
		}

		return nil
	})
	return out, next, err
}

func (s *Service) ListAccounts(ctx context.Context, in *api.ListAccountsRequest) (*api.ListAccountsResponse, error) {
	items, next, err := s.accounts(ctx, "", in.NextToken, in.MaxResults)
	return &api.ListAccountsResponse{Accounts: items, NextToken: next}, err
}

func (s *Service) ListAccountsForParent(
	ctx context.Context,
	in *api.ListAccountsForParentRequest,
) (*api.ListAccountsForParentResponse, error) {
	if common.Value(in.ParentId) == "" {
		return nil, invalid("ParentId required")
	}

	items, next, err := s.accounts(ctx, *in.ParentId, in.NextToken, in.MaxResults)
	return &api.ListAccountsForParentResponse{Accounts: items, NextToken: next}, err
}

func (s *Service) ListRoots(ctx context.Context, in *api.ListRootsRequest) (*api.ListRootsResponse, error) {
	out := &api.ListRootsResponse{}
	err := s.view(ctx, true, func(r *record) error {
		items, next, txErr := page(
			[]api.Root{r.Root},
			func(x api.Root) string { return common.Value(x.Id) },
			in.NextToken,
			in.MaxResults,
		)
		out.Roots = items
		out.NextToken = next
		return txErr
	})
	return out, err
}

func duplicateOU(r *record, parentID, name, except string) error {
	for _, x := range r.Resources {
		if x.OU != nil && x.Parent == parentID && common.Value(x.OU.Name) == name && x.ID != except {
			return awserror.New("DuplicateOrganizationalUnitException", "Name already exists under parent")
		}
	}

	return nil
}

func (s *Service) CreateOrganizationalUnit(
	ctx context.Context,
	in *api.CreateOrganizationalUnitRequest,
) (*api.CreateOrganizationalUnitResponse, error) {
	if checkErr := validName(common.Value(in.Name)); checkErr != nil {
		return nil, checkErr
	}

	ts, err := tags(in.Tags)
	if err != nil {
		return nil, err
	}

	out := &api.CreateOrganizationalUnitResponse{}
	err = s.change(ctx, func(r *record, _ storage.WriteTx) error {
		p := common.Value(in.ParentId)
		if checkErr := parent(r, p); checkErr != nil {
			return checkErr
		}

		if checkErr := duplicateOU(r, p, *in.Name, ""); checkErr != nil {
			return checkErr
		}

		depth := 0
		for x := r.Resources[p]; x != nil && x.Kind == kindOU; x = r.Resources[x.Parent] {
			depth++
		}

		if depth >= 5 {
			return awserror.New("ConstraintViolationException", "OU nesting limit is five")
		}

		id := "ou-" + (*r.Root.Id)[2:] + "-" + common.RandomID(8)
		ou := &api.OrganizationalUnit{Id: new(id), Arn: new(resourceARN(r, kindOU, id)), Name: in.Name}
		r.Resources[id] = &resource{ID: id, Kind: kindOU, Parent: p, Tags: ts, OU: ou}
		if enabled(r) {
			r.Resources[fullAccess].Targets[id] = true
		}
		out.OrganizationalUnit = ou
		return nil
	})
	return out, err
}

func (s *Service) DescribeOrganizationalUnit(
	ctx context.Context,
	in *api.DescribeOrganizationalUnitRequest,
) (*api.DescribeOrganizationalUnitResponse, error) {
	out := &api.DescribeOrganizationalUnitResponse{}
	err := s.view(ctx, true, func(r *record) error {
		x, txErr := lookup(r, common.Value(in.OrganizationalUnitId), kindOU)
		if txErr != nil {
			return txErr
		}

		out.OrganizationalUnit = x.OU
		return nil
	})
	return out, err
}

func (s *Service) UpdateOrganizationalUnit(
	ctx context.Context,
	in *api.UpdateOrganizationalUnitRequest,
) (*api.UpdateOrganizationalUnitResponse, error) {
	if checkErr := validName(common.Value(in.Name)); checkErr != nil {
		return nil, checkErr
	}

	out := &api.UpdateOrganizationalUnitResponse{}
	err := s.change(ctx, func(r *record, _ storage.WriteTx) error {
		x, txErr := lookup(r, common.Value(in.OrganizationalUnitId), kindOU)
		if txErr != nil {
			return txErr
		}

		if checkErr := duplicateOU(r, x.Parent, *in.Name, x.ID); checkErr != nil {
			return checkErr
		}

		x.OU.Name = in.Name
		out.OrganizationalUnit = x.OU
		return nil
	})
	return out, err
}

func (s *Service) DeleteOrganizationalUnit(
	ctx context.Context,
	in *api.DeleteOrganizationalUnitRequest,
) (*api.DeleteOrganizationalUnitOutput, error) {
	err := s.change(ctx, func(r *record, _ storage.WriteTx) error {
		x, txErr := lookup(r, common.Value(in.OrganizationalUnitId), kindOU)
		if txErr != nil {
			return txErr
		}

		if len(selected(r, "", x.ID)) > 0 {
			return awserror.New("OrganizationalUnitNotEmptyException", "OU has children")
		}

		for _, p := range r.Resources {
			delete(p.Targets, x.ID)
		}

		delete(r.Resources, x.ID)
		return nil
	})
	return &api.DeleteOrganizationalUnitOutput{}, err
}

func (s *Service) ListOrganizationalUnitsForParent(
	ctx context.Context,
	in *api.ListOrganizationalUnitsForParentRequest,
) (*api.ListOrganizationalUnitsForParentResponse, error) {
	out := &api.ListOrganizationalUnitsForParentResponse{}
	err := s.view(ctx, true, func(r *record) error {
		p := common.Value(in.ParentId)
		if checkErr := parent(r, p); checkErr != nil {
			return checkErr
		}

		items, next, txErr := page(
			selected(r, kindOU, p),
			func(x *resource) string { return x.ID },
			in.NextToken,
			in.MaxResults,
		)
		if txErr != nil {
			return txErr
		}

		out.NextToken = next
		out.OrganizationalUnits = []api.OrganizationalUnit{}
		for _, x := range items {
			out.OrganizationalUnits = append(out.OrganizationalUnits, *x.OU)
		}

		return nil
	})
	return out, err
}

func childType(kind string) string {
	switch kind {
	case kindAccount:
		return "ACCOUNT"
	case kindOU:
		return "ORGANIZATIONAL_UNIT"
	default:
		return "ROOT"
	}
}

func (s *Service) ListParents(ctx context.Context, in *api.ListParentsRequest) (*api.ListParentsResponse, error) {
	out := &api.ListParentsResponse{}
	err := s.view(ctx, true, func(r *record) error {
		x, txErr := lookup(r, common.Value(in.ChildId), "")
		if txErr != nil {
			return txErr
		}

		if x.Kind != kindAccount && x.Kind != kindOU {
			return invalid("Child must be an account or OU")
		}

		p := r.Resources[x.Parent]
		items, next, txErr := page(
			[]api.Parent{{Id: new(p.ID), Type: new(childType(p.Kind))}},
			func(p api.Parent) string { return *p.Id },
			in.NextToken,
			in.MaxResults,
		)
		out.Parents = items
		out.NextToken = next
		return txErr
	})
	return out, err
}

func (s *Service) ListChildren(ctx context.Context, in *api.ListChildrenRequest) (*api.ListChildrenResponse, error) {
	out := &api.ListChildrenResponse{}
	kind := map[string]string{"ACCOUNT": kindAccount, "ORGANIZATIONAL_UNIT": kindOU}[common.Value(in.ChildType)]
	if kind == "" {
		return nil, invalid("Invalid ChildType")
	}

	err := s.view(ctx, true, func(r *record) error {
		p := common.Value(in.ParentId)
		if checkErr := parent(r, p); checkErr != nil {
			return checkErr
		}

		items, next, txErr := page(
			selected(r, kind, p),
			func(x *resource) string { return x.ID },
			in.NextToken,
			in.MaxResults,
		)
		if txErr != nil {
			return txErr
		}

		out.NextToken = next
		out.Children = []api.Child{}
		for _, x := range items {
			out.Children = append(out.Children, api.Child{Id: new(x.ID), Type: in.ChildType})
		}

		return nil
	})
	return out, err
}
