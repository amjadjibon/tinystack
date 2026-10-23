package ec2

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const kindPrefixList = "prefix-list"

type customerPrefixList struct {
	Tags        map[string]string `json:",omitempty"`
	ID          string
	Name        string
	Family      string
	State       string
	ClientToken string
	Versions    map[int64][]api.PrefixListEntry
	Entries     []api.PrefixListEntry
	MaxEntries  int64
	Version     int64
}

func prefixNotFound(id string) error {
	return notFound("InvalidPrefixListID.NotFound", "prefix list", id)
}

func loadPrefixList(tx storage.ReadTx, scope request.Scope, id string) (*customerPrefixList, error) {
	pl, ok, err := storage.Read[customerPrefixList](tx, scope.Key("ec2", kindPrefixList, id))
	if err == nil && !ok {
		err = prefixNotFound(id)
	}

	return pl, err
}

func prefixListExists(tx storage.ReadTx, scope request.Scope, id string) error {
	if _, ok := tx.Get(scope.Key("ec2", kindPrefixList, id)); ok {
		return nil
	}

	for name := range gatewayServices {
		if managedPrefixList(scope.Region, name).ID == id {
			return nil
		}
	}

	return prefixNotFound(id)
}

func prefixOutput(scope request.Scope, pl *customerPrefixList) api.ManagedPrefixList {
	return api.ManagedPrefixList{
		PrefixListId: new(pl.ID), PrefixListName: new(pl.Name), AddressFamily: new(pl.Family),
		State: new(pl.State), Version: new(pl.Version), MaxEntries: new(pl.MaxEntries), Tags: tagList(pl.Tags),
		PrefixListArn: new(fmt.Sprintf("arn:aws:ec2:%s:%s:prefix-list/%s", scope.Region, scope.AccountID, pl.ID)),
		OwnerId:       new(scope.AccountID),
	}
}

func prefixValues(scope request.Scope, pl *customerPrefixList) values {
	return values{
		"prefix-list-id": {pl.ID}, "prefix-list-name": {pl.Name}, "address-family": {pl.Family},
		filterState: {pl.State}, filterOwnerID: {scope.AccountID},
	}.withTags(pl.Tags)
}

func checkPrefixList(pl *customerPrefixList) error {
	if pl.Name == "" || len(pl.Name) > 255 || strings.HasPrefix(pl.Name, "com.amazonaws") {
		return invalidParameter("Prefix list name must be 1..255 characters and cannot start with com.amazonaws")
	}

	if pl.Family != ec2IPv4 && pl.Family != "IPv6" {
		return invalidParameter("AddressFamily must be IPv4 or IPv6")
	}

	if pl.MaxEntries < 1 || int64(len(pl.Entries)) > pl.MaxEntries {
		return invalidParameter("Invalid MaxEntries")
	}

	seen := map[string]bool{}
	for i := range pl.Entries {
		entry := &pl.Entries[i]
		p, err := netip.ParsePrefix(common.Value(entry.Cidr))
		if err != nil || p.Addr().Is4() != (pl.Family == ec2IPv4) {
			return invalidParameter("Invalid prefix list CIDR")
		}

		key := p.Masked().String()
		if seen[key] {
			return invalidParameter("Duplicate prefix list CIDR")
		}

		seen[key] = true
		entry.Cidr = new(key)
		if len(common.Value(entry.Description)) > 255 {
			return invalidParameter("Prefix list description exceeds 255 characters")
		}
	}

	slices.SortFunc(
		pl.Entries,
		func(a, b api.PrefixListEntry) int { return strings.Compare(common.Value(a.Cidr), common.Value(b.Cidr)) },
	)
	return nil
}

func (s *Service) CreateManagedPrefixList(ctx context.Context, in *api.CreateManagedPrefixListRequest) (
	*api.CreateManagedPrefixListResult, error,
) {
	tags, err := specTags(in.TagSpecifications, kindPrefixList)
	if err != nil {
		return nil, err
	}

	pl := &customerPrefixList{
		ID: newID("pl"), Name: common.Value(in.PrefixListName), Family: common.Value(in.AddressFamily),
		MaxEntries: common.Value(in.MaxEntries), Version: 1, State: "create-complete", Tags: tags,
		ClientToken: common.Value(in.ClientToken),
	}

	for _, entry := range in.Entries {
		pl.Entries = append(pl.Entries, api.PrefixListEntry(entry))
	}

	if checkPrefixListErr := checkPrefixList(pl); checkPrefixListErr != nil {
		return nil, checkPrefixListErr
	}

	pl.Versions = map[int64][]api.PrefixListEntry{pl.Version: slices.Clone(pl.Entries)}
	var out api.ManagedPrefixList
	err = s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		all, recordsErr := storage.Records[customerPrefixList](tx, scope.Key("ec2", kindPrefixList, ""))
		if recordsErr != nil {
			return recordsErr
		}

		for _, existing := range all {
			if pl.ClientToken != "" && pl.ClientToken == existing.ClientToken {
				if pl.Name != existing.Name || pl.Family != existing.Family || pl.MaxEntries != existing.MaxEntries {
					return awserror.New("IdempotentParameterMismatch", "ClientToken parameters differ")
				}

				out = prefixOutput(scope, existing)
				return nil
			}
		}

		out = prefixOutput(scope, pl)
		return storage.Write(tx, scope.Key("ec2", kindPrefixList, pl.ID), pl)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateManagedPrefixListResult{PrefixList: &out}, nil
}

func prefixAssociations(tx storage.ReadTx, scope request.Scope, id string) ([]api.PrefixListAssociation, error) {
	tables, err := storage.Records[routeTable](tx, scope.Key("ec2", kindRouteTable, ""))
	if err != nil {
		return nil, err
	}

	groups, err := storage.Records[securityGroup](tx, scope.Key("ec2", kindSecurityGroup, ""))
	if err != nil {
		return nil, err
	}

	out := []api.PrefixListAssociation{}
	for _, rt := range tables {
		if slices.ContainsFunc(rt.Routes, func(r route) bool { return r.PrefixList == id }) {
			out = append(out, api.PrefixListAssociation{ResourceId: new(rt.ID), ResourceOwner: new(scope.AccountID)})
		}
	}

	for _, g := range groups {
		if slices.ContainsFunc(
			append(slices.Clone(g.Ingress), g.Egress...),
			func(r rule) bool { return r.PrefixList == id },
		) {
			out = append(out, api.PrefixListAssociation{ResourceId: new(g.ID), ResourceOwner: new(scope.AccountID)})
		}
	}

	slices.SortFunc(out, func(a, b api.PrefixListAssociation) int {
		return strings.Compare(common.Value(a.ResourceId), common.Value(b.ResourceId))
	})
	return out, nil
}

func (s *Service) DeleteManagedPrefixList(ctx context.Context, in *api.DeleteManagedPrefixListRequest) (
	*api.DeleteManagedPrefixListResult, error,
) {
	var out api.ManagedPrefixList
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		pl, err := loadPrefixList(tx, scope, common.Value(in.PrefixListId))
		if err != nil {
			return err
		}

		uses, err := prefixAssociations(tx, scope, pl.ID)
		if err != nil {
			return err
		}

		if len(uses) > 0 {
			return awserror.New("DependencyViolation", "Prefix list is in use")
		}

		pl.State = "delete-complete"
		out = prefixOutput(scope, pl)
		tx.Delete(scope.Key("ec2", kindPrefixList, pl.ID))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteManagedPrefixListResult{PrefixList: &out}, nil
}

func (s *Service) ModifyManagedPrefixList(ctx context.Context, in *api.ModifyManagedPrefixListRequest) (
	*api.ModifyManagedPrefixListResult, error,
) {
	if in.MaxEntries != nil && (len(in.AddEntries) > 0 || len(in.RemoveEntries) > 0) {
		return nil, invalidParameter("MaxEntries cannot be changed together with entries")
	}

	var out api.ManagedPrefixList
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		pl, err := loadPrefixList(tx, scope, common.Value(in.PrefixListId))
		if err != nil {
			return err
		}

		entriesChanged := len(in.AddEntries) > 0 || len(in.RemoveEntries) > 0
		if (entriesChanged && in.CurrentVersion == nil) ||
			(in.CurrentVersion != nil && *in.CurrentVersion != pl.Version) {
			return awserror.New("PrefixListVersionMismatch", "CurrentVersion does not match")
		}

		if in.PrefixListName != nil {
			pl.Name = *in.PrefixListName
		}

		if in.MaxEntries != nil {
			pl.MaxEntries = *in.MaxEntries
		}

		for _, entry := range in.RemoveEntries {
			p, err := netip.ParsePrefix(common.Value(entry.Cidr))
			if err != nil {
				return invalidParameter("Invalid removed CIDR")
			}

			pl.Entries = slices.DeleteFunc(
				pl.Entries,
				func(e api.PrefixListEntry) bool { return common.Value(e.Cidr) == p.Masked().String() },
			)
		}

		for _, entry := range in.AddEntries {
			pl.Entries = append(
				pl.Entries,
				api.PrefixListEntry{Cidr: common.Clone(entry.Cidr), Description: common.Clone(entry.Description)},
			)
		}

		if err := checkPrefixList(pl); err != nil {
			return err
		}

		if entriesChanged {
			pl.Version++
			pl.Versions[pl.Version] = slices.Clone(pl.Entries)
		}

		pl.State = "modify-complete"
		out = prefixOutput(scope, pl)
		return storage.Write(tx, scope.Key("ec2", kindPrefixList, pl.ID), pl)
	})
	if err != nil {
		return nil, err
	}

	return &api.ModifyManagedPrefixListResult{PrefixList: &out}, nil
}

func (s *Service) DescribeManagedPrefixLists(ctx context.Context, in *api.DescribeManagedPrefixListsRequest) (
	*api.DescribeManagedPrefixListsResult, error,
) {
	match, err := compileFilters(in.Filters, prefixValues(request.Scope{}, &customerPrefixList{}).names()...)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeManagedPrefixListsResult{PrefixLists: []api.ManagedPrefixList{}}
	err = s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		records, recordsErr2 := storage.Records[customerPrefixList](tx, scope.Key("ec2", kindPrefixList, ""))
		if recordsErr2 != nil {
			return recordsErr2
		}

		var all []api.ManagedPrefixList
		for _, pl := range records {
			if match.match(prefixValues(scope, pl)) {
				all = append(all, prefixOutput(scope, pl))
			}
		}

		for service := range gatewayServices {
			managed := managedPrefixList(scope.Region, service)
			pl := &customerPrefixList{
				ID: managed.ID, Name: managed.Name, Family: ec2IPv4, State: "create-complete",
				Version: 1, MaxEntries: int64(len(managed.CIDRs)),
			}

			if match.match(prefixValues(request.Scope{AccountID: "AWS"}, pl)) {
				item := prefixOutput(scope, pl)
				item.OwnerId = new("AWS")
				all = append(all, item)
			}
		}

		id := func(pl api.ManagedPrefixList) string { return common.Value(pl.PrefixListId) }
		slices.SortFunc(all, func(a, b api.ManagedPrefixList) int { return strings.Compare(id(a), id(b)) })
		for _, want := range in.PrefixListIds {
			if prefixListExistsErr := prefixListExists(tx, scope, want); prefixListExistsErr != nil {
				return prefixListExistsErr
			}
		}

		all = slices.DeleteFunc(all, func(pl api.ManagedPrefixList) bool {
			return len(in.PrefixListIds) > 0 && !slices.Contains(in.PrefixListIds, id(pl))
		})
		limit := len(all) + 1
		if in.MaxResults != nil {
			if *in.MaxResults < 1 {
				return invalidParameter("MaxResults must be positive")
			}

			limit = int(*in.MaxResults)
		}

		out.PrefixLists, out.NextToken, recordsErr2 = common.Page(
			all,
			id,
			in.NextToken,
			limit,
			invalidParameter("Invalid NextToken"),
		)
		return recordsErr2
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) GetManagedPrefixListEntries(ctx context.Context, in *api.GetManagedPrefixListEntriesRequest) (
	*api.GetManagedPrefixListEntriesResult, error,
) {
	out := &api.GetManagedPrefixListEntriesResult{Entries: []api.PrefixListEntry{}}
	err := s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		var entries []api.PrefixListEntry
		pl, ok, err := storage.Read[customerPrefixList](
			tx,
			scope.Key("ec2", kindPrefixList, common.Value(in.PrefixListId)),
		)
		if err != nil {
			return err
		}

		if ok {
			version := pl.Version
			if in.TargetVersion != nil {
				version = *in.TargetVersion
			}

			var found bool
			entries, found = pl.Versions[version]
			if !found {
				return awserror.New("InvalidPrefixListVersion", "Unknown prefix list version")
			}
		} else {
			if prefixListExistsErr2 := prefixListExists(
				tx,
				scope,
				common.Value(in.PrefixListId),
			); prefixListExistsErr2 != nil {
				return prefixListExistsErr2
			}

			if in.TargetVersion != nil && *in.TargetVersion != 1 {
				return awserror.New("InvalidPrefixListVersion", "Unknown version")
			}

			for service := range gatewayServices {
				managed := managedPrefixList(scope.Region, service)
				if managed.ID == common.Value(in.PrefixListId) {
					for _, text := range managed.CIDRs {
						entries = append(entries, api.PrefixListEntry{Cidr: new(text)})
					}
				}
			}
		}

		limit := len(entries) + 1
		if in.MaxResults != nil {
			if *in.MaxResults < 1 {
				return invalidParameter("MaxResults must be positive")
			}

			limit = int(*in.MaxResults)
		}

		out.Entries, out.NextToken, err = common.Page(
			entries,
			func(e api.PrefixListEntry) string { return common.Value(e.Cidr) },
			in.NextToken,
			limit,
			invalidParameter("Invalid NextToken"),
		)
		return err
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) GetManagedPrefixListAssociations(
	ctx context.Context,
	in *api.GetManagedPrefixListAssociationsRequest,
) (
	*api.GetManagedPrefixListAssociationsResult, error,
) {
	out := &api.GetManagedPrefixListAssociationsResult{PrefixListAssociations: []api.PrefixListAssociation{}}
	err := s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		if err := prefixListExists(tx, scope, common.Value(in.PrefixListId)); err != nil {
			return err
		}

		all, err := prefixAssociations(tx, scope, common.Value(in.PrefixListId))
		if err != nil {
			return err
		}

		limit := len(all) + 1
		if in.MaxResults != nil {
			if *in.MaxResults < 1 {
				return invalidParameter("MaxResults must be positive")
			}

			limit = int(*in.MaxResults)
		}

		out.PrefixListAssociations, out.NextToken, err = common.Page(all,
			func(a api.PrefixListAssociation) string { return common.Value(a.ResourceId) }, in.NextToken, limit,
			invalidParameter("Invalid NextToken"))
		return err
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
