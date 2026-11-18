package ssoadmin

import (
	"cmp"
	"slices"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/ssoadmin"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/identitystore"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Entitlement is a permission set that a user may use in an account, through an assignment to the user or to one of
// the user's groups.
type Entitlement struct {
	Account         string
	PermissionSet   string // name, the role name the access portal shows
	ARN             string
	InlinePolicy    string
	ManagedPolicies []string
	SessionSeconds  int64
}

// Entitlements lists what an Identity Center user of the instance account may sign in to, ordered by account and
// permission set name. It reads in the caller's transaction, so assignments and policies are consistent.
func Entitlements(tx storage.ReadTx, instanceAccount, userID string) ([]Entitlement, error) {
	groups, err := identitystore.UserGroups(tx, instanceAccount, userID)
	if err != nil {
		return nil, err
	}

	scope := request.Scope{AccountID: instanceAccount}
	assignments, err := storage.Records[api.AccountAssignment](tx, scope.Prefix(serviceName, assignmentKind))
	if err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	var out []Entitlement
	for _, a := range assignments {
		principal := *a.PrincipalId
		if *a.PrincipalType == "USER" && principal != userID ||
			*a.PrincipalType == "GROUP" && !slices.Contains(groups, principal) {
			continue
		}

		id := *a.AccountId + " " + *a.PermissionSetArn
		if seen[id] {
			continue
		}

		seen[id] = true
		permission, _, loadErr := loadPermission(tx, scope, a.PermissionSetArn)
		if loadErr != nil {
			return nil, loadErr
		}

		seconds, durationErr := SessionSeconds(common.Value(permission.PermissionSet.SessionDuration))
		if durationErr != nil {
			return nil, durationErr
		}

		managed, managedErr := managedPolicies(tx, scope, a.PermissionSetArn)
		if managedErr != nil {
			return nil, managedErr
		}

		e := Entitlement{
			Account: *a.AccountId, PermissionSet: *permission.PermissionSet.Name, ARN: *a.PermissionSetArn,
			InlinePolicy: permission.InlinePolicy, SessionSeconds: seconds,
		}
		for _, m := range managed {
			e.ManagedPolicies = append(e.ManagedPolicies, *m.Arn)
		}

		out = append(out, e)
	}

	slices.SortFunc(out, func(a, b Entitlement) int {
		return cmp.Or(cmp.Compare(a.Account, b.Account), cmp.Compare(a.PermissionSet, b.PermissionSet))
	})

	return out, nil
}
