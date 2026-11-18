// Package keyspaces implements the Amazon Keyspaces control plane.
package keyspaces

import (
	"sync"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/keyspaces"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName     = "keyspaces"
	kindKeyspace    = "keyspace"
	kindTable       = "table"
	kindType        = "type"
	active          = "ACTIVE"
	creating        = "CREATING"
	updating        = "UPDATING"
	singleRegion    = "SINGLE_REGION"
	multiRegion     = "MULTI_REGION"
	enabled         = "ENABLED"
	localTransition = time.Second
)

type Service struct {
	store     storage.Store
	runtime   Runtime
	bg        *background.Group
	runtimeMu sync.Mutex
}

func New(store storage.Store) *Service {
	return &Service{store: store}
}

type record struct {
	Replication *api.ReplicationSpecification
	Table       *api.GetTableResponse
	Tags        map[string]string
	Created     time.Time
	ReadyAt     time.Time
	Keyspace    string
	Name        string
	Transition  string
	Fields      []api.FieldDefinition
	TypeRefs    []string
	Pending     uint64
	Depth       int64
}

func (s *Service) status(r *record) string {
	if s.runtime != nil && r.Pending != 0 || s.runtime == nil && time.Now().Before(r.ReadyAt) {
		return r.Transition
	}

	return active
}

func arnPrefix(scope request.Scope) string {
	return "arn:aws:cassandra:" + scope.Region + ":" + scope.AccountID + ":/keyspace/"
}

func resourceARN(scope request.Scope, keyspace, table string) string {
	arn := arnPrefix(scope) + keyspace + "/"
	if table != "" {
		arn += "table/" + table
	}

	return arn
}

func childKey(scope request.Scope, kind, keyspace, name string) string {
	return scope.Key(serviceName, kind, keyspace+"/"+name)
}

func systemKeyspace(name string) bool {
	switch name {
	case "system", "system_schema", "system_schema_mcs", "system_multiregion_info":
		return true
	default:
		return false
	}
}

func writable(keyspace string) error {
	switch keyspace {
	case "system_auth", "system_cluster_metadata", "system_distributed", "system_traces", "system_views",
		"system_virtual_schema":
		return awserror.New("AccessDeniedException", "Cassandra reserves that keyspace")
	default:
		if systemKeyspace(keyspace) {
			return awserror.New("AccessDeniedException", "System keyspaces are read-only")
		}
	}

	return nil
}

func missing() error {
	return awserror.New("ResourceNotFoundException", "The requested Keyspaces resource does not exist")
}

func conflict(message string) error {
	return awserror.New("ConflictException", message)
}

func readRecord(tx storage.ReadTx, key string) (*record, error) {
	r, found, err := storage.Read[record](tx, key)
	if err == nil && !found {
		err = missing()
	}

	return r, err
}

func requireKeyspace(tx storage.ReadTx, scope request.Scope, name string) error {
	if systemKeyspace(name) {
		return nil
	}

	_, err := readRecord(tx, scope.Key(serviceName, kindKeyspace, name))
	return err
}

func resourcePage[T any](items []T, id func(T) string, token *string, maxResults *int64) (
	page []T, next *string, err error,
) {
	limit := *common.OrElse(maxResults, new(int64(100)))
	if limit < 1 || limit > 1000 {
		return nil, nil, awserror.Validation("MaxResults must be between 1 and 1000")
	}

	return common.Page(items, id, token, int(limit), awserror.Validation("Invalid next token"))
}

func createRecord(tx storage.WriteTx, key string) error {
	if _, found := tx.Get(key); found {
		return conflict("A resource with that name already exists")
	}

	return nil
}

func (s *Service) writeChange(
	tx storage.WriteTx, scope request.Scope, key string, r *record, status string, statements ...string,
) error {
	if r != nil {
		if s.status(r) != creating || status == creating {
			r.Transition = status
		}

		r.ReadyAt = time.Now().Add(localTransition)
	}

	_, seeded := tx.Get(runtimeSeedKey)
	if seeded || s.runtime != nil {
		sequence, err := enqueue(tx, scope, key, statements, s.runtime != nil)
		if err != nil {
			return err
		}

		if r != nil {
			r.Pending = sequence
		}
	}

	if r == nil {
		tx.Delete(key)
		return nil
	}

	return storage.Write(tx, key, r)
}
