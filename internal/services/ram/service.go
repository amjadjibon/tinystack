// Package ram implements the AWS Resource Access Manager (RAM) control plane.
// It stores resource shares and resource/principal associations as records without
// cross-account invitations, managed permissions, or real cross-account resource authorization.
package ram

import (
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ram"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName         = "ram"
	shareKind           = "resource-share"
	associationKind     = "association"
	tokenKind           = "client-token"
	featureSet          = "STANDARD"
	statusActive        = "ACTIVE"
	statusDeleted       = "DELETED"
	statusAssociated    = "ASSOCIATED"
	statusDisassociated = "DISASSOCIATED"
)

var _ api.API = (*Service)(nil)

type (
	Service struct{ store storage.Store }

	resourceShareRecord struct {
		Share *api.ResourceShare
		Tags  map[string]string
	}

	associationRecord struct {
		Association *api.ResourceShareAssociation
	}

	// tokenRecord remembers which share a CreateResourceShare clientToken produced.
	tokenRecord struct {
		ShareID string
		Name    string
	}
)

func New(store storage.Store) *Service { return &Service{store: store} }

var (
	accountIDPattern = regexp.MustCompile(`^\d{12}$`)
	orgPattern       = regexp.MustCompile(`^arn:aws:organizations::\d{12}:organization/o-[a-z0-9]{10,32}$`)
	ouPattern        = regexp.MustCompile(
		`^arn:aws:organizations::\d{12}:ou/o-[a-z0-9]{10,32}/ou-[a-z0-9]{4,32}-[a-z0-9]{8,32}$`,
	)
	iamPattern = regexp.MustCompile(`^arn:aws:iam::\d{12}:(role|user)/.+$`)
)

func invalidParameter(format string, args ...any) error {
	return awserror.New("InvalidParameterException", fmt.Sprintf(format, args...))
}

func malformedArn(format string, args ...any) error {
	return awserror.New("MalformedArnException", fmt.Sprintf(format, args...))
}

func unknownResource(arn string) error {
	return awserror.New("UnknownResourceException", fmt.Sprintf("ResourceShare with ARN %s does not exist", arn))
}

func operationNotPermitted(format string, args ...any) error {
	return awserror.New("OperationNotPermittedException", fmt.Sprintf(format, args...))
}

func idempotentMismatch() error {
	return awserror.New(
		"IdempotentParameterMismatchException",
		"The client token was already used with different parameters.",
	)
}

func pageLimit(maxResults *int64) (int, error) {
	if maxResults == nil {
		return 500, nil
	}

	if *maxResults < 1 || *maxResults > 500 {
		return 0, invalidParameter("maxResults must be between 1 and 500.")
	}

	return int(*maxResults), nil
}

func invalidNextToken(format string, args ...any) error {
	return awserror.New("InvalidNextTokenException", fmt.Sprintf(format, args...))
}

func checkTagLimit(err error) error {
	if errors.Is(err, tagging.ErrLimit) {
		return awserror.New("TagLimitExceededException", err.Error())
	}

	return invalidParameter("%s", err.Error())
}

func newShareID() string {
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		common.RandomID(8, common.AlphabetLowerHex),
		common.RandomID(4, common.AlphabetLowerHex),
		common.RandomID(4, common.AlphabetLowerHex),
		common.RandomID(4, common.AlphabetLowerHex),
		common.RandomID(12, common.AlphabetLowerHex),
	)
}

func shareARN(scope request.Scope, id string) string {
	return fmt.Sprintf("arn:aws:ram:%s:%s:resource-share/%s", scope.Region, scope.AccountID, id)
}

func parseShareARN(s string) (id string, err error) {
	parts := strings.Split(s, ":")
	if len(parts) != 6 || parts[0] != "arn" || parts[1] != "aws" || parts[2] != "ram" {
		return "", malformedArn("The format of the specified ARN is not valid.")
	}

	res := parts[5]
	if !strings.HasPrefix(res, "resource-share/") {
		return "", malformedArn("The format of the specified ARN is not valid.")
	}

	id = strings.TrimPrefix(res, "resource-share/")
	if id == "" {
		return "", malformedArn("The format of the specified ARN is not valid.")
	}

	return id, nil
}

func validateResourceARN(arnStr string) error {
	parts := strings.Split(arnStr, ":")
	if len(parts) < 6 || parts[0] != "arn" || parts[1] != "aws" {
		return malformedArn("The specified ARN %s is not valid.", arnStr)
	}

	return nil
}

func isValidPrincipal(p string) bool {
	return accountIDPattern.MatchString(p) ||
		orgPattern.MatchString(p) ||
		ouPattern.MatchString(p) ||
		iamPattern.MatchString(p)
}

func isExternalPrincipal(p, accountID string) bool {
	if accountIDPattern.MatchString(p) {
		return p != accountID
	}

	parts := strings.Split(p, ":")
	if len(parts) >= 5 && parts[4] != "" {
		return parts[4] != accountID
	}

	return false
}

func associationKey(scope request.Scope, shareID, assocType, entity string) string {
	return scope.Key(serviceName, associationKind, shareID+"/"+assocType+"/"+hex.EncodeToString([]byte(entity)))
}

func associationSharePrefix(scope request.Scope, shareID string) string {
	return scope.Prefix(serviceName, associationKind) + shareID + "/"
}

func associationTypePrefix(scope request.Scope, shareID, assocType string) string {
	return scope.Prefix(serviceName, associationKind) + shareID + "/" + assocType + "/"
}
