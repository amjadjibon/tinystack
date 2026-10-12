// Package rds implements scoped RDS management records and optional Docker databases.
package rds

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/rds"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName   = "rds"
	kindDB        = "db"
	kindCluster   = "cluster"
	kindSubnet    = "subgrp"
	kindParameter = "pg"
	available     = "available"
	engineFilter  = "engine"
	creating      = "creating"
	stopped       = "stopped"
	failed        = "failed"
	indexPrefix   = "aws/rds/index/"
)

const (
	EnginePostgres = "postgres"
	EngineMySQL    = "mysql"
	EngineMariaDB  = "mariadb"
	actionStart    = "start"
	subnetType     = "db-subnet-group"
)

var (
	_          api.API = (*Service)(nil)
	identifier         = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9-]*$`)
	sqlName            = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*$`)
)

// Records live at <account>/<region>/rds/<db|cluster|subgrp|pg>/<name>.
// SQL passwords are private storage fields; cluster passwords are discarded after validation.
// Credentials never appear in inventory or API output.
type record struct {
	Since        time.Time
	Name         string
	Kind         string
	Tags         map[string]string
	Instance     *api.DBInstance
	Cluster      *api.DBCluster
	Subnet       *api.DBSubnetGroup
	Parameter    *api.DBParameterGroup
	Password     string
	RootPassword string
	Container    string
	Volume       string
	Initialized  bool
}

type Service struct {
	store       storage.Store
	caller      *catalog.Caller
	runtime     Runtime
	bg          *background.Group
	mu          sync.Mutex
	orphansDone bool
}

func New(store storage.Store) *Service         { return &Service{store: store} }
func (s *Service) UseCaller(c *catalog.Caller) { s.caller = c }

func invalid(message string) error { return awserror.New("InvalidParameterValue", message) }
func resourceError(kind, suffix, name string) error {
	shape := "DBInstance"
	switch kind {
	case kindCluster:
		shape = "DBCluster"
	case kindSubnet:
		shape = "DBSubnetGroup"
	case kindParameter:
		shape = "DBParameterGroup"
	}

	return awserror.New(shape+suffix+"Fault", fmt.Sprintf("%s %s", shape, name))
}

func arn(scope request.Scope, kind, name string) string {
	return fmt.Sprintf("arn:aws:rds:%s:%s:%s:%s", scope.Region, scope.AccountID, kind, name)
}

func validName(name string, maximum int) bool {
	return name != "" && len(name) <= maximum && identifier.MatchString(name) &&
		!strings.Contains(name, "--") && !strings.HasSuffix(name, "-")
}

func readRecord(tx storage.ReadTx, scope request.Scope, kind, name string) (*record, error) {
	r, found, err := storage.Read[record](tx, scope.Key(serviceName, kind, strings.ToLower(name)))
	if err != nil {
		return nil, err
	}

	if !found {
		return nil, resourceError(kind, "NotFound", name)
	}

	return r, nil
}

func writeRecord(tx storage.WriteTx, scope request.Scope, r *record) error {
	return storage.Write(tx, scope.Key(serviceName, r.Kind, r.Name), r)
}

func (s *Service) insert(ctx context.Context, scope request.Scope, r *record) error {
	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		key := scope.Key(serviceName, r.Kind, r.Name)
		if _, found, err := storage.Read[record](tx, key); err != nil {
			return err
		} else if found {
			return resourceError(r.Kind, "AlreadyExists", r.Name)
		}

		if err := writeRecord(tx, scope, r); err != nil {
			return err
		}

		if r.Kind == kindDB {
			return storage.Write(tx, indexPrefix+scope.AccountID+"/"+scope.Region+"/"+r.Name, true)
		}

		return nil
	})
}

func instance(r *record) *api.DBInstance {
	out := common.Clone(r.Instance)
	out.TagList = tagList(r.Tags)
	return out
}

func pageRecords(items []*record, marker *string, maximum *int64) ([]*record, *string, error) {
	limit := int(common.Value(common.OrElse(maximum, new(int64(100)))))
	if limit < 20 || limit > 100 {
		return nil, nil, invalid("MaxRecords must be between 20 and 100")
	}

	return common.Page(items, func(r *record) string { return r.Name }, marker, limit, invalid("Invalid Marker"))
}

func (s *Service) list(ctx context.Context, scope request.Scope, kind, name string) ([]*record, error) {
	var out []*record
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		if name != "" {
			r, err := readRecord(tx, scope, kind, name)
			if err != nil {
				return err
			}

			out = append(out, r)
			return nil
		}

		for _, k := range tx.Keys(scope.Prefix(serviceName, kind)) {
			r, found, err := storage.Read[record](tx, k)
			if err != nil {
				return err
			}

			if found {
				out = append(out, r)
			}
		}

		return nil
	})
	return out, err
}
