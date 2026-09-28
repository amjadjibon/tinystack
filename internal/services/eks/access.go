package eks

import (
	"context"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/eks"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	standard     = "STANDARD"
	scopeCluster = "cluster"
	policyPrefix = "arn:aws:eks::aws:cluster-access-policy/"
)

var (
	entryTypes = []string{
		standard,
		"EC2_LINUX",
		"EC2_WINDOWS",
		"FARGATE_LINUX",
		"EC2",
		"HYBRID_LINUX",
		"HYPERPOD_LINUX",
	}
	// accessPolicies are the policies EKS offers, which TinyStack stores but
	// never evaluates.
	accessPolicies = []string{
		"AmazonEKSAdminPolicy", "AmazonEKSAdminViewPolicy", "AmazonEKSClusterAdminPolicy", "AmazonEKSEditPolicy",
		"AmazonEKSViewPolicy",
	}
	principalPattern = regexp.MustCompile(`^arn:aws[a-z-]*:iam::(\d{12}):(role|user)/(?:.*/)?([^/]+)$|` +
		`^arn:aws[a-z-]*:iam::(\d{12}):root$`)
)

// accessEntry is an access entry's record: its description, its access
// policies, and its create token.
type accessEntry struct {
	Entry    api.AccessEntry
	Token    string
	Policies []api.AssociatedAccessPolicy
}

func entryKey(scope request.Scope, clusterName, principal string) string {
	return owned(scope, "access", clusterName, url.PathEscape(principal))
}

func entryNotFound(principal string) error {
	return notFound("The specified principalArn could not be found. You can view your available access entries "+
		"with 'list-access-entries'. (%s)", principal)
}

// newEntry describes a new access entry with EKS's default user name.
func newEntry(
	scope request.Scope,
	c *cluster,
	principal, kind, username string,
	groups []string,
	tags map[string]string,
	now time.Time,
) *accessEntry {
	m := principalPattern.FindStringSubmatch(principal)
	account, which, name := m[1], m[2], m[3]
	if m[4] != "" {
		account, which, name = m[4], "root", "root"
	}

	if username == "" {
		switch {
		case kind == "FARGATE_LINUX":
			username = "system:node:{{SessionName}}"
		case kind != standard:
			username = "system:node:{{EC2PrivateDNSName}}"
		case which == "role":
			username = "arn:aws:sts::" + account + ":assumed-role/" + name + "/{{SessionName}}"
		default:
			username = principal
		}
	}

	return &accessEntry{Policies: []api.AssociatedAccessPolicy{}, Entry: api.AccessEntry{
		ClusterName: new(c.Name), PrincipalArn: new(principal), KubernetesGroups: orEmpty(groups),
		AccessEntryArn: new(arn(scope, "access-entry/"+c.Name+"/"+which+"/"+account+"/"+name+"/"+newUUID())),
		CreatedAt:      seconds(now), ModifiedAt: seconds(now), Tags: cloneTags(tags), Username: new(username),
		Type: new(kind),
	}}
}

func checkGroups(kind string, groups []string, username *string) error {
	if kind != standard && (len(groups) > 0 || username != nil) {
		return invalidParameter("Only STANDARD access entries can have kubernetesGroups or a username.")
	}

	for _, g := range groups {
		if strings.HasPrefix(g, "system:") {
			return invalidParameter("kubernetesGroups cannot start with system:")
		}
	}

	return nil
}

// apiCluster returns the cluster name names, if it takes access entries.
func apiCluster(tx storage.ReadTx, scope request.Scope, name *string) (*cluster, error) {
	c, err := loadCluster(tx, scope, name)
	if err != nil {
		return nil, err
	}

	if c.AuthMode == configMap {
		return nil, invalidRequest("The cluster's authentication mode must be set to one of [API, " +
			"API_AND_CONFIG_MAP] to perform this operation.")
	}

	return c, nil
}

func (s *Service) CreateAccessEntry(
	ctx context.Context,
	in *api.CreateAccessEntryRequest,
) (*api.CreateAccessEntryResponse, error) {
	scope, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	kind := value(orElse(in.Type, new(standard)))
	principal := value(in.PrincipalArn)
	switch {
	case !principalPattern.MatchString(principal):
		return nil, invalidParameter("The specified principalArn is invalid: invalid ARN")
	case !slices.Contains(entryTypes, kind):
		return nil, invalidParameter("The access entry type %s is not supported.", kind)
	case kind != standard && !strings.Contains(principal, ":role/"):
		return nil, invalidParameter("Access entries of type %s need an IAM role.", kind)
	}

	if checkErr := checkGroups(kind, in.KubernetesGroups, in.Username); checkErr != nil {
		return nil, checkErr
	}

	if checkErr := checkTags(in.Tags); checkErr != nil {
		return nil, checkErr
	}

	var out *api.AccessEntry
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		c, loadErr := apiCluster(tx, scope, in.ClusterName)
		if loadErr != nil {
			return loadErr
		}

		existing, ok, readErr := read[accessEntry](tx, entryKey(scope, c.Name, principal))
		switch {
		case readErr != nil:
			return readErr
		case ok && value(in.ClientRequestToken) != "" && existing.Token == *in.ClientRequestToken:
			out = &existing.Entry
			return nil
		case ok:
			return inUse("The specified access entry resource is already in use on this cluster.")
		}

		e := newEntry(scope, c, principal, kind, value(in.Username), in.KubernetesGroups, in.Tags, s.now().UTC())
		e.Token = value(in.ClientRequestToken)
		out = &e.Entry
		return write(tx, entryKey(scope, c.Name, principal), e)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateAccessEntryResponse{AccessEntry: out}, nil
}

// changeEntry runs fn on an access entry; fn writes it back if it changes.
func (s *Service) changeEntry(
	ctx context.Context,
	clusterName, principal *string,
	fn func(storage.WriteTx, request.Scope, *cluster, *accessEntry) error,
) error {
	scope, err := identity(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		c, loadErr := apiCluster(tx, scope, clusterName)
		if loadErr != nil {
			return loadErr
		}

		e, ok, readErr := read[accessEntry](tx, entryKey(scope, c.Name, value(principal)))
		if readErr != nil || !ok {
			return errorsOr(readErr, entryNotFound(value(principal)))
		}

		return fn(tx, scope, c, e)
	})
}

func (s *Service) DescribeAccessEntry(
	ctx context.Context,
	in *api.DescribeAccessEntryRequest,
) (*api.DescribeAccessEntryResponse, error) {
	var out *api.AccessEntry
	err := s.changeEntry(ctx, in.ClusterName, in.PrincipalArn,
		func(_ storage.WriteTx, _ request.Scope, _ *cluster, e *accessEntry) error {
			out = &e.Entry
			return nil
		})
	if err != nil {
		return nil, err
	}

	return &api.DescribeAccessEntryResponse{AccessEntry: out}, nil
}

func (s *Service) ListAccessEntries(
	ctx context.Context,
	in *api.ListAccessEntriesRequest,
) (*api.ListAccessEntriesResponse, error) {
	scope, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.ListAccessEntriesResponse{AccessEntries: []string{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		c, loadErr := apiCluster(tx, scope, in.ClusterName)
		if loadErr != nil {
			return loadErr
		}

		entries, readErr := records[accessEntry](tx, prefix(scope, "access")+c.Name+"/")
		if readErr != nil {
			return readErr
		}

		var principals []string
		for _, e := range entries {
			if in.AssociatedPolicyArn == nil ||
				slices.ContainsFunc(e.Policies, func(p api.AssociatedAccessPolicy) bool {
					return *p.PolicyArn == *in.AssociatedPolicyArn
				}) {
				principals = append(principals, *e.Entry.PrincipalArn)
			}
		}

		slices.Sort(principals)
		principals, out.NextToken, readErr = page(principals, func(p string) string { return p }, in.NextToken,
			in.MaxResults)
		out.AccessEntries = append(out.AccessEntries, principals...)
		return readErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) UpdateAccessEntry(
	ctx context.Context,
	in *api.UpdateAccessEntryRequest,
) (*api.UpdateAccessEntryResponse, error) {
	var out *api.AccessEntry
	err := s.changeEntry(ctx, in.ClusterName, in.PrincipalArn,
		func(tx storage.WriteTx, scope request.Scope, c *cluster, e *accessEntry) error {
			if err := checkGroups(*e.Entry.Type, in.KubernetesGroups, in.Username); err != nil {
				return err
			}

			if in.KubernetesGroups != nil {
				e.Entry.KubernetesGroups = in.KubernetesGroups
			}

			e.Entry.Username = orElse(in.Username, e.Entry.Username)
			e.Entry.ModifiedAt = seconds(s.now().UTC())
			out = &e.Entry
			return write(tx, entryKey(scope, c.Name, *e.Entry.PrincipalArn), e)
		})
	if err != nil {
		return nil, err
	}

	return &api.UpdateAccessEntryResponse{AccessEntry: out}, nil
}

func (s *Service) DeleteAccessEntry(
	ctx context.Context,
	in *api.DeleteAccessEntryRequest,
) (*api.DeleteAccessEntryResponse, error) {
	err := s.changeEntry(ctx, in.ClusterName, in.PrincipalArn,
		func(tx storage.WriteTx, scope request.Scope, c *cluster, e *accessEntry) error {
			tx.Delete(entryKey(scope, c.Name, *e.Entry.PrincipalArn))
			return nil
		})
	if err != nil {
		return nil, err
	}

	return &api.DeleteAccessEntryResponse{}, nil
}

func checkScope(scope *api.AccessScope) error {
	switch value(value(scope).Type) {
	case kindCluster:
		if len(scope.Namespaces) > 0 {
			return invalidParameter("A cluster access scope takes no namespaces.")
		}
	case "namespace":
		if len(scope.Namespaces) == 0 {
			return invalidParameter("A namespace access scope needs namespaces.")
		}
	default:
		return invalidParameter("The access scope type must be cluster or namespace.")
	}

	return nil
}

// AssociateAccessPolicy associates a policy with a STANDARD access entry, or
// changes the scope of one it has.
func (s *Service) AssociateAccessPolicy(
	ctx context.Context,
	in *api.AssociateAccessPolicyRequest,
) (*api.AssociateAccessPolicyResponse, error) {
	name, ok := strings.CutPrefix(value(in.PolicyArn), policyPrefix)
	if !ok || !slices.Contains(accessPolicies, name) {
		return nil, invalidParameter("The specified policyArn is invalid: %s", value(in.PolicyArn))
	}

	if err := checkScope(in.AccessScope); err != nil {
		return nil, err
	}

	if in.AccessScope.Namespaces == nil {
		in.AccessScope.Namespaces = []string{}
	}

	out := &api.AssociateAccessPolicyResponse{ClusterName: in.ClusterName, PrincipalArn: in.PrincipalArn}
	err := s.changeEntry(ctx, in.ClusterName, in.PrincipalArn,
		func(tx storage.WriteTx, scope request.Scope, c *cluster, e *accessEntry) error {
			if *e.Entry.Type != standard {
				return invalidRequest("Access policies can only be associated with STANDARD access entries.")
			}

			now := seconds(s.now().UTC())
			i := slices.IndexFunc(
				e.Policies,
				func(p api.AssociatedAccessPolicy) bool { return *p.PolicyArn == *in.PolicyArn },
			)
			if i < 0 {
				e.Policies = append(e.Policies, api.AssociatedAccessPolicy{PolicyArn: in.PolicyArn, AssociatedAt: now})
				i = len(e.Policies) - 1
			}

			e.Policies[i].AccessScope, e.Policies[i].ModifiedAt = in.AccessScope, now
			out.AssociatedAccessPolicy = &e.Policies[i]
			return write(tx, entryKey(scope, c.Name, *e.Entry.PrincipalArn), e)
		})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DisassociateAccessPolicy(
	ctx context.Context,
	in *api.DisassociateAccessPolicyRequest,
) (*api.DisassociateAccessPolicyResponse, error) {
	err := s.changeEntry(ctx, in.ClusterName, in.PrincipalArn,
		func(tx storage.WriteTx, scope request.Scope, c *cluster, e *accessEntry) error {
			before := len(e.Policies)
			e.Policies = slices.DeleteFunc(e.Policies, func(p api.AssociatedAccessPolicy) bool {
				return *p.PolicyArn == value(in.PolicyArn)
			})
			if len(e.Policies) == before {
				return notFound("The specified policy is not associated with the access entry.")
			}

			return write(tx, entryKey(scope, c.Name, *e.Entry.PrincipalArn), e)
		})
	if err != nil {
		return nil, err
	}

	return &api.DisassociateAccessPolicyResponse{}, nil
}

func (s *Service) ListAssociatedAccessPolicies(
	ctx context.Context,
	in *api.ListAssociatedAccessPoliciesRequest,
) (*api.ListAssociatedAccessPoliciesResponse, error) {
	out := &api.ListAssociatedAccessPoliciesResponse{
		ClusterName: in.ClusterName, PrincipalArn: in.PrincipalArn,
		AssociatedAccessPolicies: []api.AssociatedAccessPolicy{},
	}
	err := s.changeEntry(ctx, in.ClusterName, in.PrincipalArn,
		func(_ storage.WriteTx, _ request.Scope, _ *cluster, e *accessEntry) error {
			policies, next, err := page(e.Policies, func(p api.AssociatedAccessPolicy) string { return *p.PolicyArn },
				in.NextToken, in.MaxResults)
			out.AssociatedAccessPolicies = append(out.AssociatedAccessPolicies, policies...)
			out.NextToken = next
			return err
		})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) ListAccessPolicies(
	_ context.Context,
	in *api.ListAccessPoliciesRequest,
) (*api.ListAccessPoliciesResponse, error) {
	names, next, err := page(accessPolicies, func(n string) string { return n }, in.NextToken, in.MaxResults)
	if err != nil {
		return nil, err
	}

	out := &api.ListAccessPoliciesResponse{AccessPolicies: []api.AccessPolicy{}, NextToken: next}
	for _, n := range names {
		out.AccessPolicies = append(out.AccessPolicies, api.AccessPolicy{Name: new(n), Arn: new(policyPrefix + n)})
	}

	return out, nil
}
