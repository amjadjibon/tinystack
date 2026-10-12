// Package identitystore stores Identity Center users, groups and memberships.
// Records are account-wide; no directory synchronization or sign-in is performed.
package identitystore

import (
	"context"
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/identitystore"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const serviceName = "identitystore"

var (
	_            api.API = (*Service)(nil)
	storePattern         = regexp.MustCompile(
		`^(arn:aws[a-z-]*:identitystore::\d{12}:identitystore/)?` +
			`(d-[0-9a-f]{10}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`,
	)
	idPattern = regexp.MustCompile(
		`^([0-9a-f]{10}-|)[A-Fa-f0-9]{8}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{12}$`,
	)
	textPattern = regexp.MustCompile(`^[\p{L}\p{M}\p{S}\p{N}\p{P}\t\n\r \x{00a0}\x{3000}]+$`)
	userPattern = regexp.MustCompile(`^[\p{L}\p{M}\p{S}\p{N}\p{P}]+$`)
)

type Service struct{ store storage.Store }

func New(store storage.Store) *Service { return &Service{store: store} }

// Instance returns the stable instance ARN and identity store ID for an account.
func Instance(account string) (arn, storeID string) {
	sum := sha256.Sum256([]byte(account))
	return fmt.Sprintf("arn:aws:sso:::instance/ssoins-%x", sum[:8]), fmt.Sprintf("d-%x", sum[:5])
}

func missing() error {
	return awserror.New("ResourceNotFoundException", "The specified identity store resource does not exist")
}

func conflict() error {
	return awserror.New("ConflictException", "The requested identity store resource already exists")
}

func checkScope(ctx context.Context, id *string) (request.Scope, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return request.Scope{}, err
	}

	if id == nil || !storePattern.MatchString(*id) {
		return request.Scope{}, awserror.Validation("Invalid IdentityStoreId")
	}

	_, expected := Instance(m.AccountID)
	if *id != expected {
		return request.Scope{}, missing()
	}

	return request.Scope{AccountID: m.AccountID}, nil
}

func resourceID(id *string, kind string) (string, error) {
	value := common.Value(id)
	if strings.HasPrefix(value, "arn:") {
		prefix := "arn:aws:identitystore:::" + kind + "/"
		if !strings.HasPrefix(value, prefix) {
			return "", awserror.Validation("Invalid resource ARN")
		}

		value = strings.TrimPrefix(value, prefix)
	}

	if !idPattern.MatchString(value) {
		return "", awserror.Validation("Invalid resource ID")
	}

	return value, nil
}

func newID() string {
	value := common.RandomID(32, "0123456789abcdef")
	return value[:8] + "-" + value[8:12] + "-" + value[12:16] + "-" + value[16:20] + "-" + value[20:]
}

func load[T any](tx storage.ReadTx, scope request.Scope, kind string, id *string) (r *T, key string, err error) {
	value, err := resourceID(id, kind)
	if err != nil {
		return nil, "", err
	}

	key = scope.Key(serviceName, kind, value)
	r, found, err := storage.Read[T](tx, key)
	if err != nil {
		return nil, "", err
	}

	if !found {
		return nil, "", missing()
	}

	return r, key, nil
}

// PrincipalExists checks assignment references using the caller's transaction,
// so a principal cannot disappear between validation and the assignment write.
func PrincipalExists(tx storage.ReadTx, account, principalType, id string) bool {
	kind := "user"
	if principalType == "GROUP" {
		kind = "group"
	}

	_, found := tx.Get((request.Scope{AccountID: account}).Key(serviceName, kind, id))
	return found
}

func checkText(value *string, required bool, maximum int, pattern *regexp.Regexp) error {
	if value == nil {
		if required {
			return awserror.Validation("A required attribute is missing")
		}

		return nil
	}

	if n := utf8.RuneCountInString(*value); n < 1 || n > maximum || !pattern.MatchString(*value) {
		return awserror.Validation("Attribute violates its modeled length or pattern")
	}

	return nil
}

func checkUser(u *api.User) error {
	if err := checkText(u.UserName, true, 128, userPattern); err != nil {
		return err
	}

	if err := checkText(u.DisplayName, false, 1024, textPattern); err != nil {
		return err
	}

	if u.Name != nil {
		for _, value := range []*string{u.Name.GivenName, u.Name.FamilyName} {
			if err := checkText(value, false, 1024, textPattern); err != nil {
				return err
			}
		}
	}

	if u.Emails != nil {
		if len(u.Emails) != 1 {
			return awserror.Validation("Emails must contain one primary email")
		}

		e := &u.Emails[0]
		if err := checkText(e.Value, true, 1024, textPattern); err != nil {
			return err
		}

		if err := checkText(e.Type, false, 1024, textPattern); err != nil {
			return err
		}

		if e.Primary == nil {
			e.Primary = new(true)
		}

		if !*e.Primary {
			return awserror.Validation("Email must be primary")
		}
	}

	return nil
}

func checkGroup(g *api.Group) error {
	if err := checkText(g.DisplayName, true, 1024, textPattern); err != nil {
		return err
	}

	return checkText(g.Description, false, 1024, textPattern)
}

func checkUnique[T any](
	tx storage.ReadTx,
	scope request.Scope,
	kind, id, name string,
	identity func(*T) (string, string),
) error {
	records, err := storage.Records[T](tx, scope.Prefix(serviceName, kind))
	if err != nil {
		return err
	}

	for _, r := range records {
		otherID, otherName := identity(r)
		if otherID != id && strings.EqualFold(otherName, name) {
			return conflict()
		}
	}

	return nil
}

func userIdentity(u *api.User) (id, name string)   { return *u.UserId, *u.UserName }
func groupIdentity(g *api.Group) (id, name string) { return *g.GroupId, *g.DisplayName }

func pageLimit(maximum *int64) (int, error) {
	if maximum == nil {
		return 100, nil
	}

	if *maximum < 1 || *maximum > 100 {
		return 0, awserror.Validation("MaxResults must be between 1 and 100")
	}

	return int(*maximum), nil
}

func filterValue(filters []api.Filter, path string) (string, error) {
	if len(filters) > 1 {
		return "", awserror.Validation("At most one filter is supported")
	}

	if len(filters) == 0 {
		return "", nil
	}

	f := filters[0]
	if !strings.EqualFold(common.Value(f.AttributePath), path) {
		return "", awserror.Validation("Unsupported filter path")
	}

	if err := checkText(f.AttributeValue, true, 1024, textPattern); err != nil {
		return "", err
	}

	return *f.AttributeValue, nil
}
