// Package backup stores regional vaults, immutable plan versions and resource
// selections. It never schedules work, encrypts data or creates recovery points.
package backup

import (
	"fmt"
	"strings"
	"time"
	"uuid"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/backup"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName = "backup"
	kindVault   = "vault"
	kindPlan    = "plan"
)

var _ api.API = (*Service)(nil)

type Service struct{ store storage.Store }

func New(store storage.Store) *Service { return &Service{store: store} }

type version struct {
	Deleted *float64
	ID      string
	Plan    api.BackupPlan
	Created float64
}
type selection struct {
	Token   *string
	ID      string
	Value   api.BackupSelection
	Created float64
}

// One plan record owns its versions and selections. All changes, including
// token/name checks and tags, commit atomically with their parent record.
type record struct {
	Tags                map[string]string
	Selections          map[string]selection
	Token               *string
	KeyARN              *string
	Deleted             *float64
	Name, ID, ARN, Kind string
	Versions            []version
	Created             float64
}

func failure(code, message string) error { return awserror.New(code, message) }
func invalid(message string) error       { return failure("InvalidParameterValueException", message) }

func missing() error {
	return failure("ResourceNotFoundException", "Resource does not exist in this account and Region")
}
func stamp() float64 { return float64(time.Now().UTC().UnixMilli()) / 1000 }
func id() string     { return uuid.NewV4().String() }
func arn(scope request.Scope, kind, name string) string {
	return fmt.Sprintf("arn:aws:backup:%s:%s:%s:%s", scope.Region, scope.AccountID, kind, name)
}

func read(tx storage.ReadTx, scope request.Scope, kind, name string) (*record, error) {
	if name == "" {
		return nil, failure("MissingParameterValueException", "Resource identifier is required")
	}
	r, ok, err := storage.Read[record](tx, scope.Key(serviceName, kind, name))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, missing()
	}
	return r, nil
}

func save(tx storage.WriteTx, scope request.Scope, r *record) error {
	return storage.Write(tx, scope.Key(serviceName, r.Kind, r.ID), r)
}

func records(tx storage.ReadTx, scope request.Scope, kind string) ([]*record, error) {
	out := []*record{}
	for _, key := range tx.Keys(scope.Prefix(serviceName, kind)) {
		r, _, err := storage.Read[record](tx, key)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func activePlan(tx storage.ReadTx, scope request.Scope, name string) (*record, error) {
	r, err := read(tx, scope, kindPlan, name)
	if err != nil {
		return nil, err
	}
	if r.Deleted != nil {
		return nil, missing()
	}
	return r, nil
}

func byARN(tx storage.ReadTx, scope request.Scope, value string) (*record, error) {
	if value == "" {
		return nil, failure("MissingParameterValueException", "ResourceArn is required")
	}
	prefix := arn(scope, "", "")
	prefix = strings.TrimSuffix(prefix, ":")
	if strings.HasPrefix(value, prefix) {
		for _, kind := range []string{kindVault, kindPlan} {
			rows, err := records(tx, scope, kind)
			if err != nil {
				return nil, err
			}
			for _, r := range rows {
				if r.ARN == value && r.Deleted == nil {
					return r, nil
				}
			}
		}
	}
	return nil, missing()
}
