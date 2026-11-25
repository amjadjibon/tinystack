package iam

import (
	"context"
	"maps"
	"regexp"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/iam"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	iampolicy "github.com/amjadjibon/tinystack/internal/services/iam/policy"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	accountAliasGlobalPrefix = "aws/iam/account-alias/"
	kindAccountAlias         = "account-alias"
	kindPasswordPolicy       = "password-policy"

	entityUser     = "User"
	entityGroup    = "Group"
	entityRole     = "Role"
	entityLocalPol = "LocalManagedPolicy"
	entityAWSPol   = "AWSManagedPolicy"

	minPasswordLength = 6
	maxPasswordLength = 128
	minPasswordAge    = 1
	maxPasswordAge    = 1095
	minPasswordReuse  = 1
	maxPasswordReuse  = 24
)

var accountAliasPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)

type passwordPolicyRecord struct {
	MinimumPasswordLength      int64
	MaxPasswordAge             int64 `json:",omitempty"`
	PasswordReusePrevention    int64 `json:",omitempty"`
	RequireSymbols             bool
	RequireNumbers             bool
	RequireUppercaseCharacters bool
	RequireLowercaseCharacters bool
	AllowUsersToChangePassword bool
	HardExpiry                 bool `json:",omitempty"`
}

func checkAccountAlias(alias *string) (string, error) {
	if err := required("accountAlias", alias); err != nil {
		return "", err
	}

	v := *alias
	if !accountAliasPattern.MatchString(v) || strings.Contains(v, "--") {
		return "", validation("The specified value for accountAlias is invalid. It must be 3 to 63 lowercase " +
			"alphanumeric characters or non-consecutive hyphens, and must not start or end with a hyphen.")
	}

	return v, nil
}

func (s *Service) CreateAccountAlias(
	ctx context.Context,
	in *api.CreateAccountAliasRequest,
) (*api.CreateAccountAliasOutput, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	alias, err := checkAccountAlias(in.AccountAlias)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		globalKey := accountAliasGlobalPrefix + alias
		if _, exists := tx.Get(globalKey); exists {
			return alreadyExists("The account alias %s already exists.", alias)
		}

		accountKey := key(m.AccountID, kindAccountAlias, "current")
		if prev, ok, readErr := storage.Read[string](tx, accountKey); readErr != nil {
			return readErr
		} else if ok && *prev != "" {
			tx.Delete(accountAliasGlobalPrefix + *prev)
		}

		if writeErr := storage.Write(tx, globalKey, m.AccountID); writeErr != nil {
			return writeErr
		}

		return storage.Write(tx, accountKey, alias)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateAccountAliasOutput{}, nil
}

func (s *Service) DeleteAccountAlias(
	ctx context.Context,
	in *api.DeleteAccountAliasRequest,
) (*api.DeleteAccountAliasOutput, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	alias, err := checkAccountAlias(in.AccountAlias)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		accountKey := key(m.AccountID, kindAccountAlias, "current")
		current, ok, readErr := storage.Read[string](tx, accountKey)
		if readErr != nil {
			return readErr
		}

		if !ok || *current != alias {
			return noSuchEntity("The account alias %s cannot be found.", alias)
		}

		tx.Delete(accountKey)
		tx.Delete(accountAliasGlobalPrefix + alias)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteAccountAliasOutput{}, nil
}

func (s *Service) ListAccountAliases(
	ctx context.Context,
	in *api.ListAccountAliasesRequest,
) (*api.ListAccountAliasesResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	var aliases []string
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		current, ok, readErr := storage.Read[string](tx, key(m.AccountID, kindAccountAlias, "current"))
		if readErr != nil {
			return readErr
		}

		if ok && *current != "" {
			aliases = []string{*current}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	selected, truncated, next, err := page(aliases, func(v string) string { return v }, in.Marker, in.MaxItems)
	if err != nil {
		return nil, err
	}

	if selected == nil {
		selected = []string{}
	}

	return &api.ListAccountAliasesResponse{AccountAliases: selected, IsTruncated: truncated, Marker: next}, nil
}

func (s *Service) UpdateAccountPasswordPolicy(
	ctx context.Context,
	in *api.UpdateAccountPasswordPolicyRequest,
) (*api.UpdateAccountPasswordPolicyOutput, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	minLen := int64(minPasswordLength)
	if in.MinimumPasswordLength != nil {
		minLen = *in.MinimumPasswordLength
		if minLen < minPasswordLength || minLen > maxPasswordLength {
			return nil, validation(
				"MinimumPasswordLength must be between %d and %d", minPasswordLength, maxPasswordLength,
			)
		}
	}

	var maxAge int64
	if in.MaxPasswordAge != nil {
		maxAge = *in.MaxPasswordAge
		if maxAge < minPasswordAge || maxAge > maxPasswordAge {
			return nil, validation("MaxPasswordAge must be between %d and %d", minPasswordAge, maxPasswordAge)
		}
	}

	var reuse int64
	if in.PasswordReusePrevention != nil {
		reuse = *in.PasswordReusePrevention
		if reuse < minPasswordReuse || reuse > maxPasswordReuse {
			return nil, validation(
				"PasswordReusePrevention must be between %d and %d",
				minPasswordReuse,
				maxPasswordReuse,
			)
		}
	}

	rec := &passwordPolicyRecord{
		MinimumPasswordLength:      minLen,
		RequireSymbols:             common.Value(in.RequireSymbols),
		RequireNumbers:             common.Value(in.RequireNumbers),
		RequireUppercaseCharacters: common.Value(in.RequireUppercaseCharacters),
		RequireLowercaseCharacters: common.Value(in.RequireLowercaseCharacters),
		AllowUsersToChangePassword: common.Value(in.AllowUsersToChangePassword),
		MaxPasswordAge:             maxAge,
		PasswordReusePrevention:    reuse,
		HardExpiry:                 common.Value(in.HardExpiry),
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		return storage.Write(tx, key(m.AccountID, kindPasswordPolicy, "current"), rec)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateAccountPasswordPolicyOutput{}, nil
}

func (s *Service) GetAccountPasswordPolicy(
	ctx context.Context,
	_ *api.GetAccountPasswordPolicyInput,
) (*api.GetAccountPasswordPolicyResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	var rec *passwordPolicyRecord
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var ok bool
		var readErr error
		rec, ok, readErr = storage.Read[passwordPolicyRecord](tx, key(m.AccountID, kindPasswordPolicy, "current"))
		if readErr != nil {
			return readErr
		}

		if !ok {
			return noSuchEntity("The Password Policy with domain name %s cannot be found.", m.AccountID)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	out := &api.PasswordPolicy{
		MinimumPasswordLength:      new(rec.MinimumPasswordLength),
		RequireSymbols:             new(rec.RequireSymbols),
		RequireNumbers:             new(rec.RequireNumbers),
		RequireUppercaseCharacters: new(rec.RequireUppercaseCharacters),
		RequireLowercaseCharacters: new(rec.RequireLowercaseCharacters),
		AllowUsersToChangePassword: new(rec.AllowUsersToChangePassword),
		ExpirePasswords:            new(rec.MaxPasswordAge > 0),
		HardExpiry:                 new(rec.HardExpiry),
	}
	if rec.MaxPasswordAge > 0 {
		out.MaxPasswordAge = new(rec.MaxPasswordAge)
	}

	if rec.PasswordReusePrevention > 0 {
		out.PasswordReusePrevention = new(rec.PasswordReusePrevention)
	}

	return &api.GetAccountPasswordPolicyResponse{PasswordPolicy: out}, nil
}

func (s *Service) DeleteAccountPasswordPolicy(
	ctx context.Context,
	_ *api.DeleteAccountPasswordPolicyInput,
) (*api.DeleteAccountPasswordPolicyOutput, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		k := key(m.AccountID, kindPasswordPolicy, "current")
		if _, ok := tx.Get(k); !ok {
			return noSuchEntity("The Password Policy with domain name %s cannot be found.", m.AccountID)
		}

		tx.Delete(k)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteAccountPasswordPolicyOutput{}, nil
}

func (s *Service) GetAccountSummary(
	ctx context.Context,
	_ *api.GetAccountSummaryInput,
) (*api.GetAccountSummaryResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	summary := map[string]int64{
		"UsersQuota":                        5000,
		"GroupsQuota":                       300,
		"RolesQuota":                        1000,
		"PoliciesQuota":                     1500,
		"InstanceProfilesQuota":             1000,
		"ServerCertificatesQuota":           20,
		"AccessKeysPerUserQuota":            maxAccessKeys,
		"GroupsPerUserQuota":                maxGroupsPerUser,
		"AttachedPoliciesPerUserQuota":      maxAttached,
		"AttachedPoliciesPerGroupQuota":     maxAttached,
		"AttachedPoliciesPerRoleQuota":      maxAttached,
		"PolicyVersionsInUseQuota":          10000,
		"VersionsPerPolicyQuota":            maxPolicyVersions,
		"UserPolicySizeQuota":               2048,
		"GroupPolicySizeQuota":              5120,
		"RolePolicySizeQuota":               10240,
		"PolicySizeQuota":                   6144,
		"AssumeRolePolicySizeQuota":         2048,
		"SigningCertificatesPerUserQuota":   2,
		"GlobalEndpointTokenVersion":        1,
		"AccountMFAEnabled":                 0,
		"AccountAccessKeysPresent":          0,
		"AccountSigningCertificatesPresent": 0,
	}

	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		users, listErr := list[user](tx, m.AccountID, kindUser)
		if listErr != nil {
			return listErr
		}

		groups, listErr := list[group](tx, m.AccountID, kindGroup)
		if listErr != nil {
			return listErr
		}

		roles, listErr := list[role](tx, m.AccountID, kindRole)
		if listErr != nil {
			return listErr
		}

		policies, listErr := list[policy](tx, m.AccountID, kindPolicy)
		if listErr != nil {
			return listErr
		}

		profiles, listErr := list[profile](tx, m.AccountID, kindProfile)
		if listErr != nil {
			return listErr
		}

		var policyVersions int64
		for _, p := range policies {
			policyVersions += int64(len(p.Versions))
		}

		mfas, listErr := list[virtualMFA](tx, m.AccountID, kindMFA)
		if listErr != nil {
			return listErr
		}

		var mfasInUse int64
		for _, d := range mfas {
			if d.UserName != "" {
				mfasInUse++
			}
		}

		certs := tx.Keys(key(m.AccountID, kindServerCert, ""))
		oidcProviders := tx.Keys(key(m.AccountID, kindOIDC, ""))
		samlProviders := tx.Keys(key(m.AccountID, kindSAML, ""))

		summary["Users"] = int64(len(users))
		summary["Groups"] = int64(len(groups))
		summary["Roles"] = int64(len(roles))
		summary["Policies"] = int64(len(policies))
		summary["PolicyVersionsInUse"] = policyVersions
		summary["InstanceProfiles"] = int64(len(profiles))
		summary["ServerCertificates"] = int64(len(certs))
		summary["MFADevices"] = int64(len(mfas))
		summary["MFADevicesInUse"] = mfasInUse
		summary["Providers"] = int64(len(oidcProviders) + len(samlProviders))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.GetAccountSummaryResponse{SummaryMap: summary}, nil
}

type authDetailItem struct {
	user   *api.UserDetail
	group  *api.GroupDetail
	role   *api.RoleDetail
	policy *api.ManagedPolicyDetail
	key    string
}

func inlinePolicyDetails(policies map[string]string) []api.PolicyDetail {
	if len(policies) == 0 {
		return nil
	}

	out := make([]api.PolicyDetail, 0, len(policies))
	for _, name := range slices.Sorted(maps.Keys(policies)) {
		out = append(out, api.PolicyDetail{PolicyName: new(name), PolicyDocument: encodeDocument(policies[name])})
	}

	return out
}

func attachedPolicyList(attached []string) []api.AttachedPolicy {
	if len(attached) == 0 {
		return nil
	}

	refs := make([]policyRef, 0, len(attached))
	for _, a := range attached {
		if ref, err := parsePolicyARN(a); err == nil {
			refs = append(refs, ref)
		}
	}

	slices.SortFunc(refs, func(a, b policyRef) int { return strings.Compare(a.arn(), b.arn()) })
	out := make([]api.AttachedPolicy, 0, len(refs))
	for _, ref := range refs {
		out = append(out, api.AttachedPolicy{PolicyName: new(ref.name), PolicyArn: new(ref.arn())})
	}

	return out
}

func managedPolicyDetailOutput(owner string, p *policy) *api.ManagedPolicyDetail {
	versions := make([]api.PolicyVersion, 0, len(p.Versions))
	for _, v := range p.Versions {
		versions = append(versions, versionOutput(p, v, true))
	}

	out := &api.ManagedPolicyDetail{
		PolicyName:                    new(p.Name),
		PolicyId:                      new(p.ID),
		Arn:                           new(policyARN(owner, p)),
		Path:                          new(p.Path),
		DefaultVersionId:              new(p.DefaultVersion),
		AttachmentCount:               new(p.Attachments),
		PermissionsBoundaryUsageCount: new(p.BoundaryUses),
		IsAttachable:                  new(true),
		CreateDate:                    millis(p.CreatedAt),
		UpdateDate:                    millis(p.UpdatedAt),
		PolicyVersionList:             versions,
	}
	if p.Description != "" {
		out.Description = new(p.Description)
	}

	return out
}

func (s *Service) GetAccountAuthorizationDetails(
	ctx context.Context,
	in *api.GetAccountAuthorizationDetailsRequest,
) (*api.GetAccountAuthorizationDetailsResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	want := map[string]bool{}
	if len(in.Filter) == 0 {
		want[entityUser], want[entityGroup], want[entityRole] = true, true, true
		want[entityLocalPol], want[entityAWSPol] = true, true
	} else {
		for _, f := range in.Filter {
			switch f {
			case entityUser, entityRole, entityGroup, entityLocalPol, entityAWSPol:
				want[f] = true
			default:
				return nil, validation("Invalid Filter value %q", f)
			}
		}
	}

	var items []authDetailItem
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if want[entityUser] {
			users, listErr := list[user](tx, m.AccountID, kindUser)
			if listErr != nil {
				return listErr
			}

			for _, u := range users {
				memberOf, groupErr := groupsOf(tx, m.AccountID, u.Name)
				if groupErr != nil {
					return groupErr
				}

				groupNames := make([]string, 0, len(memberOf))
				for _, g := range memberOf {
					groupNames = append(groupNames, g.Name)
				}

				detail := &api.UserDetail{
					Path:                    new(u.Path),
					UserName:                new(u.Name),
					UserId:                  new(u.ID),
					Arn:                     new(userARN(m.AccountID, u)),
					CreateDate:              millis(u.CreatedAt),
					UserPolicyList:          inlinePolicyDetails(u.Policies),
					GroupList:               groupNames,
					AttachedManagedPolicies: attachedPolicyList(u.Attached),
					PermissionsBoundary:     boundaryOutput(u.Boundary),
				}
				if len(u.Tags) > 0 {
					detail.Tags = tagList(u.Tags)
				}

				items = append(items, authDetailItem{key: "1:" + strings.ToLower(u.Name), user: detail})
			}
		}

		if want[entityGroup] {
			groups, listErr := list[group](tx, m.AccountID, kindGroup)
			if listErr != nil {
				return listErr
			}

			for _, g := range groups {
				detail := &api.GroupDetail{
					Path:                    new(g.Path),
					GroupName:               new(g.Name),
					GroupId:                 new(g.ID),
					Arn:                     new(groupARN(m.AccountID, g)),
					CreateDate:              millis(g.CreatedAt),
					GroupPolicyList:         inlinePolicyDetails(g.Policies),
					AttachedManagedPolicies: attachedPolicyList(g.Attached),
				}
				items = append(items, authDetailItem{key: "2:" + strings.ToLower(g.Name), group: detail})
			}
		}

		if want[entityRole] {
			roles, listErr := list[role](tx, m.AccountID, kindRole)
			if listErr != nil {
				return listErr
			}

			for _, r := range roles {
				profiles, profErr := profilesWithRole(tx, m.AccountID, r.Name)
				if profErr != nil {
					return profErr
				}

				var profileList []api.InstanceProfile
				for _, p := range profiles {
					described, describeErr := profileOutput(tx, m.AccountID, p, false)
					if describeErr != nil {
						return describeErr
					}

					profileList = append(profileList, *described)
				}

				detail := &api.RoleDetail{
					Path:                     new(r.Path),
					RoleName:                 new(r.Name),
					RoleId:                   new(r.ID),
					Arn:                      new(roleARN(m.AccountID, r)),
					CreateDate:               millis(r.CreatedAt),
					AssumeRolePolicyDocument: encodeDocument(r.AssumeRolePolicy),
					InstanceProfileList:      profileList,
					RolePolicyList:           inlinePolicyDetails(r.Policies),
					AttachedManagedPolicies:  attachedPolicyList(r.Attached),
					PermissionsBoundary:      boundaryOutput(r.Boundary),
					RoleLastUsed:             roleLastUsedOutput(r),
				}
				if len(r.Tags) > 0 {
					detail.Tags = tagList(r.Tags)
				}

				items = append(items, authDetailItem{key: "3:" + strings.ToLower(r.Name), role: detail})
			}
		}

		if want[entityLocalPol] {
			policies, listErr := list[policy](tx, m.AccountID, kindPolicy)
			if listErr != nil {
				return listErr
			}

			for _, p := range policies {
				items = append(items, authDetailItem{
					key:    "4:" + strings.ToLower(p.Name),
					policy: managedPolicyDetailOutput(m.AccountID, p),
				})
			}
		}

		if want[entityAWSPol] {
			awsPolicies, listErr := awsManagedPolicies(tx, m.AccountID)
			if listErr != nil {
				return listErr
			}

			for _, p := range awsPolicies {
				if p.Attachments == 0 && p.BoundaryUses == 0 {
					continue
				}

				items = append(items, authDetailItem{
					key:    "5:" + strings.ToLower(p.Name) + "\x00" + strings.ToLower(p.Path),
					policy: managedPolicyDetailOutput(awsPartition, p),
				})
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	sortKey := func(item authDetailItem) string { return item.key }
	slices.SortFunc(items, func(a, b authDetailItem) int { return strings.Compare(a.key, b.key) })
	selected, truncated, next, err := page(items, sortKey, in.Marker, in.MaxItems)
	if err != nil {
		return nil, err
	}

	out := &api.GetAccountAuthorizationDetailsResponse{
		UserDetailList:  []api.UserDetail{},
		GroupDetailList: []api.GroupDetail{},
		RoleDetailList:  []api.RoleDetail{},
		Policies:        []api.ManagedPolicyDetail{},
		IsTruncated:     truncated,
		Marker:          next,
	}
	for _, item := range selected {
		switch {
		case item.user != nil:
			out.UserDetailList = append(out.UserDetailList, *item.user)
		case item.group != nil:
			out.GroupDetailList = append(out.GroupDetailList, *item.group)
		case item.role != nil:
			out.RoleDetailList = append(out.RoleDetailList, *item.role)
		case item.policy != nil:
			out.Policies = append(out.Policies, *item.policy)
		}
	}

	return out, nil
}

type policyEntityItem struct {
	group *api.PolicyGroup
	user  *api.PolicyUser
	role  *api.PolicyRole
	key   string
}

func (s *Service) ListEntitiesForPolicy(
	ctx context.Context,
	in *api.ListEntitiesForPolicyRequest,
) (*api.ListEntitiesForPolicyResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	ref, err := requiredPolicyARN(in.PolicyArn)
	if err != nil {
		return nil, err
	}

	entityFilter := common.Value(in.EntityFilter)
	switch entityFilter {
	case "", entityUser, entityRole, entityGroup, entityLocalPol, entityAWSPol:
	default:
		return nil, validation("Invalid EntityFilter value %q", entityFilter)
	}

	usage, err := checkPolicyUsageFilter(in.PolicyUsageFilter)
	if err != nil {
		return nil, err
	}

	if in.PathPrefix != nil && !pathPrefix.MatchString(*in.PathPrefix) {
		return nil, validation("The specified value for pathPrefix is invalid.")
	}

	prefix := common.Value(in.PathPrefix)
	var items []policyEntityItem
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		p, owner, loadErr := loadReadablePolicy(tx, m.AccountID, ref)
		if loadErr != nil {
			return loadErr
		}

		targetARN := policyARN(owner, p)
		matchesUsage := func(attached []string, boundary string) bool {
			hasAttached := slices.ContainsFunc(attached, func(a string) bool { return strings.EqualFold(a, targetARN) })
			hasBoundary := boundary != "" && strings.EqualFold(boundary, targetARN)
			switch usage {
			case usagePermissionsPolicy:
				return hasAttached
			case usagePermissionsBoundary:
				return hasBoundary
			default:
				return hasAttached || hasBoundary
			}
		}

		if entityFilter == "" || entityFilter == entityGroup {
			groups, listErr := list[group](tx, m.AccountID, kindGroup)
			if listErr != nil {
				return listErr
			}

			for _, g := range groups {
				if strings.HasPrefix(g.Path, prefix) && matchesUsage(g.Attached, "") {
					items = append(items, policyEntityItem{
						key:   "1:" + strings.ToLower(g.Name),
						group: &api.PolicyGroup{GroupName: new(g.Name), GroupId: new(g.ID)},
					})
				}
			}
		}

		if entityFilter == "" || entityFilter == entityUser {
			users, listErr := list[user](tx, m.AccountID, kindUser)
			if listErr != nil {
				return listErr
			}

			for _, u := range users {
				if strings.HasPrefix(u.Path, prefix) && matchesUsage(u.Attached, u.Boundary) {
					items = append(items, policyEntityItem{
						key:  "2:" + strings.ToLower(u.Name),
						user: &api.PolicyUser{UserName: new(u.Name), UserId: new(u.ID)},
					})
				}
			}
		}

		if entityFilter == "" || entityFilter == entityRole {
			roles, listErr := list[role](tx, m.AccountID, kindRole)
			if listErr != nil {
				return listErr
			}

			for _, r := range roles {
				if strings.HasPrefix(r.Path, prefix) && matchesUsage(r.Attached, r.Boundary) {
					items = append(items, policyEntityItem{
						key:  "3:" + strings.ToLower(r.Name),
						role: &api.PolicyRole{RoleName: new(r.Name), RoleId: new(r.ID)},
					})
				}
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	sortKey := func(item policyEntityItem) string { return item.key }
	slices.SortFunc(items, func(a, b policyEntityItem) int { return strings.Compare(a.key, b.key) })
	selected, truncated, next, err := page(items, sortKey, in.Marker, in.MaxItems)
	if err != nil {
		return nil, err
	}

	out := &api.ListEntitiesForPolicyResponse{
		PolicyGroups: []api.PolicyGroup{},
		PolicyUsers:  []api.PolicyUser{},
		PolicyRoles:  []api.PolicyRole{},
		IsTruncated:  truncated,
		Marker:       next,
	}
	for _, item := range selected {
		switch {
		case item.group != nil:
			out.PolicyGroups = append(out.PolicyGroups, *item.group)
		case item.user != nil:
			out.PolicyUsers = append(out.PolicyUsers, *item.user)
		case item.role != nil:
			out.PolicyRoles = append(out.PolicyRoles, *item.role)
		}
	}

	return out, nil
}

func (s *Service) GetContextKeysForCustomPolicy(
	_ context.Context,
	in *api.GetContextKeysForCustomPolicyRequest,
) (*api.GetContextKeysForPolicyResponse, error) {
	if len(in.PolicyInputList) == 0 {
		return nil, invalidInput("PolicyInputList is required")
	}

	keys, err := iampolicy.ContextKeys(in.PolicyInputList)
	if err != nil {
		return nil, invalidInput("%s", err.Error())
	}

	return &api.GetContextKeysForPolicyResponse{ContextKeyNames: keys}, nil
}

func (s *Service) GetContextKeysForPrincipalPolicy(
	ctx context.Context,
	in *api.GetContextKeysForPrincipalPolicyRequest,
) (*api.GetContextKeysForPolicyResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	principal, err := simulationPrincipal(common.Value(in.PolicySourceArn))
	if err != nil {
		return nil, err
	}

	if principal.Account != m.AccountID {
		return nil, invalidInput("PolicySourceArn must belong to the caller's account")
	}

	snapshot, err := s.simulationAuthorization(ctx, &principal)
	if err != nil {
		return nil, err
	}

	docs := make([]string, 0, len(snapshot.Documents)+len(snapshot.Boundary)+len(in.PolicyInputList))
	for _, d := range snapshot.Documents {
		docs = append(docs, d.JSON)
	}

	for _, d := range snapshot.Boundary {
		docs = append(docs, d.JSON)
	}

	docs = append(docs, in.PolicyInputList...)
	if len(docs) == 0 {
		return &api.GetContextKeysForPolicyResponse{ContextKeyNames: []string{}}, nil
	}

	keys, err := iampolicy.ContextKeys(docs)
	if err != nil {
		return nil, invalidInput("%s", err.Error())
	}

	return &api.GetContextKeysForPolicyResponse{ContextKeyNames: keys}, nil
}
