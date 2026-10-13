package memorydb

import (
	"context"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/memorydb"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

type aclRecord struct {
	tagSet
	Name      string
	UserNames []string
}

// openAccess is the immutable ACL every account has. It is not stored, so it is identical on every read.
func openAccess() *aclRecord {
	return &aclRecord{Name: openAccessACL, UserNames: []string{defaultUserName}}
}

func aclNotFound(name string) error {
	return fault("ACLNotFoundFault", "ACL %s not found.", name)
}

func invalidACLState(format string, args ...any) error {
	return fault("InvalidACLStateFault", format, args...)
}

// listACLs returns the stored ACLs and the built-in one, in name order.
func listACLs(tx storage.ReadTx, scope request.Scope) ([]*aclRecord, error) {
	acls, err := storage.Records[aclRecord](tx, scope.Prefix(serviceName, kindACL))
	if err != nil {
		return nil, err
	}

	at, _ := slices.BinarySearchFunc(acls, openAccessACL, func(r *aclRecord, n string) int {
		return strings.Compare(r.Name, n)
	})

	return slices.Insert(acls, at, openAccess()), nil
}

// readACL returns an ACL, including the built-in one.
func readACL(tx storage.ReadTx, scope request.Scope, name string) (*aclRecord, error) {
	if name == openAccessACL {
		return openAccess(), nil
	}

	r, found, err := storage.Read[aclRecord](tx, key(scope, kindACL, name))
	if err != nil {
		return nil, err
	}

	if !found {
		return nil, aclNotFound(name)
	}

	return r, nil
}

// clustersOf lists the clusters that use an ACL.
func clustersOf(tx storage.ReadTx, scope request.Scope, acl string) ([]string, error) {
	clusters, err := storage.Records[clusterRecord](tx, scope.Prefix(serviceName, kindCluster))
	if err != nil {
		return nil, err
	}

	var names []string
	for _, c := range clusters {
		if c.ACLName == acl {
			names = append(names, c.Name)
		}
	}

	return names, nil
}

func (r *aclRecord) output(scope request.Scope, clusters []string) *api.ACL {
	return &api.ACL{
		Name:                 new(r.Name),
		Status:               new(statusActive),
		UserNames:            slices.Clone(r.UserNames),
		MinimumEngineVersion: new(minimumEngineVersion),
		Clusters:             clusters,
		ARN:                  new(arn(scope, kindACL, r.Name)),
	}
}

// checkMembers verifies that names are unique, existing users. The default user can only join the open-access ACL.
func checkMembers(tx storage.ReadTx, scope request.Scope, names []string) error {
	for i, name := range names {
		if slices.Contains(names[:i], name) {
			return fault("DuplicateUserNameFault", "User %s is listed more than once.", name)
		}

		if name == defaultUserName {
			return invalidValue("The default user can only belong to the %s ACL.", openAccessACL)
		}

		if _, exists := tx.Get(key(scope, kindUser, name)); !exists {
			return userNotFound(name)
		}
	}

	return nil
}

func lowerAll(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = strings.ToLower(n)
	}

	return out
}

func (s *Service) CreateACL(ctx context.Context, in *api.CreateACLRequest) (*api.CreateACLResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name := normalize(in.ACLName)
	err = checkName("ACLName", name)
	if err != nil {
		return nil, err
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	r := &aclRecord{Name: name, UserNames: lowerAll(in.UserNames), tagSet: tagSet{Tags: tags}}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, exists := tx.Get(key(scope, kindACL, name)); exists || name == openAccessACL {
			return fault("ACLAlreadyExistsFault", "ACL %s already exists.", name)
		}

		e := checkMembers(tx, scope, r.UserNames)
		if e != nil {
			return e
		}

		return storage.Write(tx, key(scope, kindACL, name), r)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateACLResponse{ACL: r.output(scope, nil)}, nil
}

func (s *Service) UpdateACL(ctx context.Context, in *api.UpdateACLRequest) (*api.UpdateACLResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if len(in.UserNamesToAdd) == 0 && len(in.UserNamesToRemove) == 0 {
		return nil, invalidCombination("UserNamesToAdd or UserNamesToRemove is required.")
	}

	name := normalize(in.ACLName)
	add, remove := lowerAll(in.UserNamesToAdd), lowerAll(in.UserNamesToRemove)
	var out *api.ACL
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := readACL(tx, scope, name)
		if e != nil {
			return e
		}

		if name == openAccessACL {
			return invalidACLState("The %s ACL cannot be modified.", openAccessACL)
		}

		for _, u := range remove {
			if !slices.Contains(r.UserNames, u) {
				return invalidValue("User %s is not in ACL %s.", u, name)
			}

			if slices.Contains(add, u) {
				return fault("DuplicateUserNameFault", "User %s is both added and removed.", u)
			}
		}

		e = checkMembers(tx, scope, add)
		if e != nil {
			return e
		}

		for _, u := range add {
			if slices.Contains(r.UserNames, u) {
				return invalidValue("User %s is already in ACL %s.", u, name)
			}
		}

		r.UserNames = append(
			slices.DeleteFunc(r.UserNames, func(u string) bool { return slices.Contains(remove, u) }),
			add...)
		clusters, e := clustersOf(tx, scope, name)
		if e != nil {
			return e
		}

		out = r.output(scope, clusters)
		return storage.Write(tx, key(scope, kindACL, name), r)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateACLResponse{ACL: out}, nil
}

func (s *Service) DeleteACL(ctx context.Context, in *api.DeleteACLRequest) (*api.DeleteACLResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name := normalize(in.ACLName)
	var out *api.ACL
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := readACL(tx, scope, name)
		if e != nil {
			return e
		}

		if name == openAccessACL {
			return invalidACLState("The %s ACL cannot be deleted.", openAccessACL)
		}

		clusters, e := clustersOf(tx, scope, name)
		if e != nil {
			return e
		}

		if len(clusters) > 0 {
			return invalidACLState("ACL %s is associated with a cluster.", name)
		}

		out = r.output(scope, nil)
		out.Status = new(statusDeleting)
		tx.Delete(key(scope, kindACL, name))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteACLResponse{ACL: out}, nil
}

func (s *Service) DescribeACLs(ctx context.Context, in *api.DescribeACLsRequest) (*api.DescribeACLsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	limit, err := pageSize(in.MaxResults)
	if err != nil {
		return nil, err
	}

	var items []*aclRecord
	clusters := map[string][]string{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		if in.ACLName == nil {
			items, e = listACLs(tx, scope)
		} else {
			var r *aclRecord
			if r, e = readACL(tx, scope, normalize(in.ACLName)); e == nil {
				items = []*aclRecord{r}
			}
		}

		if e != nil {
			return e
		}

		for _, r := range items {
			if clusters[r.Name], e = clustersOf(tx, scope, r.Name); e != nil {
				return e
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(
		items,
		func(r *aclRecord) string { return r.Name },
		in.NextToken,
		limit,
		invalidToken(),
	)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeACLsResponse{NextToken: next, ACLs: []api.ACL{}}
	for _, r := range page {
		out.ACLs = append(out.ACLs, *r.output(scope, clusters[r.Name]))
	}

	return out, nil
}
