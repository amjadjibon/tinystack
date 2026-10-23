// Package inspector2 stores regional Inspector scanning settings and filters.
// ENABLING and DISABLING resolve to their final states within the write transaction.
// No scanners run, and findings and coverage are always empty.
package inspector2

import (
	"fmt"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/inspector2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName            = "inspector2"
	kindFilter             = "filter"
	kindAccount            = "account"
	accountSettings        = "settings"
	statusEnabled          = "ENABLED"
	statusDisabled         = "DISABLED"
	statusSuccess          = "SUCCESS"
	resourceEC2            = "EC2"
	resourceECR            = "ECR"
	resourceLambda         = "LAMBDA"
	resourceLambdaCode     = "LAMBDA_CODE"
	resourceCodeRepository = "CODE_REPOSITORY"
	invalidBounds          = "Filter lower bound exceeds upper bound."
)

// Service implements the records-only Inspector control plane.
type Service struct{ store storage.Store }

// New returns a service backed by store.
func New(store storage.Store) *Service { return &Service{store: store} }

func missing() error { return awserror.New("ResourceNotFoundException", "Filter was not found.") }

func ownAccount(scope request.Scope, ids []string) error {
	for _, id := range ids {
		if id != scope.AccountID {
			return awserror.Validation("Only the caller's own account is supported.")
		}
	}

	return nil
}

func filterKey(scope request.Scope, arn string) (string, error) {
	prefix := fmt.Sprintf("arn:aws:inspector2:%s:%s:filter/", scope.Region, scope.AccountID)
	if !strings.HasPrefix(arn, prefix) || strings.TrimPrefix(arn, prefix) == "" {
		return "", missing()
	}

	return scope.Key(serviceName, kindFilter, arn), nil
}

func readFilter(tx storage.ReadTx, scope request.Scope, arn string) (*api.Filter, error) {
	key, err := filterKey(scope, arn)
	if err != nil {
		return nil, err
	}

	rec, found, err := storage.Read[api.Filter](tx, key)
	if err != nil {
		return nil, err
	}

	if !found {
		return nil, missing()
	}

	return rec, nil
}

func limit(pageLimit *int64) int {
	if pageLimit == nil {
		return 100
	}

	return int(*pageLimit)
}

func emptyPage(token *string) error {
	_, _, err := common.Page([]string{}, func(s string) string { return s }, token, 100,
		awserror.Validation("Invalid nextToken."))
	return err
}

var (
	_ api.API           = (*Service)(nil)
	_ catalog.Inventory = (*Service)(nil)
)
