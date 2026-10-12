// Package ssoadmin stores Identity Center permission sets and account assignments.
// Status SUCCEEDED records metadata only: IAM roles and sign-in are not provisioned.
package ssoadmin

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ssoadmin"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/identitystore"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName    = "ssoadmin"
	permissionKind = "permission-set"
	assignmentKind = "assignment"
	managedKind    = "managed-policy"
)

var (
	_               api.API = (*Service)(nil)
	instancePattern         = regexp.MustCompile(
		`^arn:aws(-[a-z]{1,5}){0,3}:sso:::instance/(sso)?ins-[a-zA-Z0-9-.]{16}$`,
	)
	permissionPattern = regexp.MustCompile(
		`^arn:aws(-[a-z]{1,5}){0,3}:sso:::permissionSet/(sso)?ins-[a-zA-Z0-9-.]{16}/ps-[a-zA-Z0-9-./]{16}$`,
	)
	managedPattern = regexp.MustCompile(
		`^arn:aws(-[a-z]{1,5}){0,3}:iam::aws:policy((/[A-Za-z0-9.,+@=_-]+)*)/([A-Za-z0-9.,+=@_-]+)$`,
	)
	namePattern        = regexp.MustCompile(`^[\w+=,.@-]+$`)
	descriptionPattern = regexp.MustCompile(`^[\x{0009}\x{000A}\x{000D}\x{0020}-\x{007E}\x{00A1}-\x{00FF}]*$`)
	relayPattern       = regexp.MustCompile(`^[a-zA-Z0-9&$@#\\/%?=~\-_'"|!:,.;*+\[\] (){}]+$`)
	durationPattern    = regexp.MustCompile(
		`^(-?)P(?:(\d+)Y)?(?:(\d+)M)?(?:(\d+)([DW]))?(?:T(?:(\d+)H)?(?:(\d+)M)?(?:(\d+(?:\.\d+)?)S)?)?$`,
	)
	accountPattern   = regexp.MustCompile(`^\d{12}$`)
	principalPattern = regexp.MustCompile(
		`^([0-9a-f]{10}-|)[A-Fa-f0-9]{8}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{12}$`,
	)
	tagPattern     = regexp.MustCompile(`^[\p{L}\p{Z}\p{N}_.:/=+\-@]*$`)
	requestPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

type (
	Service          struct{ store storage.Store }
	permissionRecord struct {
		PermissionSet api.PermissionSet
		Tags          map[string]string
		InlinePolicy  string
	}
)

func New(store storage.Store) *Service { return &Service{store: store} }
func missing() error {
	return awserror.New("ResourceNotFoundException", "The specified Identity Center resource does not exist")
}

func conflict() error {
	return awserror.New("ConflictException", "The requested resource conflicts with existing records")
}

func checkScope(ctx context.Context, instance *string) (request.Scope, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return request.Scope{}, err
	}

	if instance == nil || !instancePattern.MatchString(*instance) {
		return request.Scope{}, awserror.Validation("Invalid InstanceArn")
	}

	expected, _ := identitystore.Instance(m.AccountID)
	if *instance != expected {
		return request.Scope{}, missing()
	}

	return request.Scope{AccountID: m.AccountID}, nil
}

func loadPermission(tx storage.ReadTx, scope request.Scope, arn *string) (*permissionRecord, string, error) {
	if arn == nil || !permissionPattern.MatchString(*arn) {
		return nil, "", awserror.Validation("Invalid PermissionSetArn")
	}

	key := scope.Key(serviceName, permissionKind, *arn)
	r, found, err := storage.Read[permissionRecord](tx, key)
	if err != nil {
		return nil, "", err
	}

	if !found {
		return nil, "", missing()
	}

	return r, key, nil
}

func checkString(value *string, required bool, maximum int, pattern *regexp.Regexp) error {
	if value == nil {
		if required {
			return awserror.Validation("A required member is missing")
		}

		return nil
	}

	if n := utf8.RuneCountInString(*value); n < 1 || n > maximum || !pattern.MatchString(*value) {
		return awserror.Validation("Member violates its modeled length or pattern")
	}

	return nil
}

func checkTags(tags map[string]string) error {
	if err := tagging.CheckSet(tags); err != nil {
		return awserror.Validation("%s", err)
	}

	for k, v := range tags {
		if !tagPattern.MatchString(k) || !tagPattern.MatchString(v) {
			return awserror.Validation("Invalid tag pattern")
		}
	}

	return nil
}

func tagMap(tags []api.Tag) (map[string]string, error) {
	if len(tags) > 75 {
		return nil, awserror.Validation("At most 75 tags may be supplied")
	}

	out, err := tagging.Map(
		tags,
		func(t api.Tag) (*string, *string) { return t.Key, t.Value },
		awserror.Validation(
			"Tag key and value are required",
		),
		func(string) error { return awserror.Validation("Duplicate tag key") },
	)
	if err != nil {
		return nil, err
	}

	return out, checkTags(out)
}

func tagList(tags map[string]string) []api.Tag {
	return tagging.List(tags, func(k, v string) api.Tag { return api.Tag{Key: new(k), Value: new(v)} })
}

func pageLimit(maximum *int64) (int, error) {
	if maximum == nil {
		return 100, nil
	}

	if *maximum < 1 || *maximum > 100 {
		return 0, awserror.Validation("MaxResults must be between 1 and 100")
	}

	return int(*maximum), nil
}

func permissionARN(account string) string {
	instance, _ := identitystore.Instance(account)
	id := strings.TrimPrefix(instance, "arn:aws:sso:::instance/")
	return fmt.Sprintf("arn:aws:sso:::permissionSet/%s/ps-%s", id, common.RandomID(16, "0123456789abcdef"))
}
