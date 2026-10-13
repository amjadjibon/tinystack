package glacier

import (
	"fmt"
	"regexp"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/glacier"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName     = "glacier"
	vaultKind       = "vault"
	maxVaults       = 1000
	maxTagsPerVault = 10
	maxPolicyBytes  = 20 * 1024
)

var (
	_ api.API = (*Service)(nil)

	vaultNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,255}$`)
)

type vaultRecord struct {
	Tags          map[string]string            `json:"Tags,omitempty"`
	Policy        *string                      `json:"Policy,omitempty"`
	Notifications *api.VaultNotificationConfig `json:"Notifications,omitempty"`
	VaultName     string                       `json:"VaultName"`
	CreationDate  string                       `json:"CreationDate"`
}

type Service struct {
	store storage.Store
}

func New(s storage.Store) *Service {
	return &Service{store: s}
}

func notFound(message string) error {
	err := awserror.New("ResourceNotFoundException", message)
	err.StatusCode = 404
	return err
}

func invalidParameter(message string) error {
	return awserror.New("InvalidParameterValueException", message)
}

func missingParameter(message string) error {
	return awserror.New("MissingParameterValueException", message)
}

func limitExceeded(message string) error {
	return awserror.New("LimitExceededException", message)
}

func validateAccount(scope request.Scope, accountID *string) error {
	if accountID == nil || *accountID == "" {
		return missingParameter("AccountId is required")
	}

	if *accountID != "-" && *accountID != scope.AccountID {
		return invalidParameter(fmt.Sprintf("Invalid accountId: %s", *accountID))
	}

	return nil
}

func validateVaultName(vaultName *string) error {
	if vaultName == nil || *vaultName == "" {
		return missingParameter("VaultName is required")
	}

	if !vaultNamePattern.MatchString(*vaultName) {
		return invalidParameter(fmt.Sprintf("Invalid vaultName: %s", *vaultName))
	}

	return nil
}

func vaultARN(region, accountID, vaultName string) string {
	return fmt.Sprintf("arn:aws:glacier:%s:%s:vaults/%s", region, accountID, vaultName)
}

func vaultKey(scope request.Scope, vaultName string) string {
	return scope.Key(serviceName, vaultKind, vaultName)
}
