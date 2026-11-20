package glue

import (
	"context"
	"regexp"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/glue"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	kindRegistry    = "registry"
	kindSchema      = "schema"
	defaultRegistry = "default-registry"
	schemaAvailable = "AVAILABLE"
	schemaDeleting  = "DELETING"
	schemaPageSize  = 100
)

var registryNamePattern = regexp.MustCompile(`^[a-zA-Z0-9-_$#.]{1,255}$`)

type registryRecord struct {
	api.GetRegistryResponse
	Tags map[string]string
}

func glueResourcePrefix(scope request.Scope, kind string) string {
	return "arn:aws:glue:" + scope.Region + ":" + scope.AccountID + ":" + kind + "/"
}

func registryIdentifier(scope request.Scope, id *api.RegistryId) (string, error) {
	if id == nil || (id.RegistryArn == nil) == (id.RegistryName == nil) {
		return "", invalid("Provide exactly one of RegistryName or RegistryArn.")
	}

	n := common.Value(id.RegistryName)
	if id.RegistryArn != nil {
		var found bool
		n, found = strings.CutPrefix(*id.RegistryArn, glueResourcePrefix(scope, kindRegistry))
		if !found {
			return "", notFound("Registry %s not found.", *id.RegistryArn)
		}

		if strings.HasPrefix(n, defaultRegistry+":") {
			n = defaultRegistry
		}
	}

	if !registryNamePattern.MatchString(n) {
		return "", invalid("Invalid registry name.")
	}

	return n, nil
}

func loadRegistry(tx storage.ReadTx, scope request.Scope, n string) (*registryRecord, error) {
	r, found, err := storage.Read[registryRecord](tx, scope.Key(service, kindRegistry, n))
	if err != nil || !found {
		return nil, common.ErrorsOr(err, notFound("Registry %s not found.", n))
	}

	return r, nil
}

func findRegistry(tx storage.ReadTx, scope request.Scope, id *api.RegistryId) (*registryRecord, error) {
	n, err := registryIdentifier(scope, id)
	if err != nil {
		return nil, err
	}

	r, err := loadRegistry(tx, scope, n)
	if err != nil {
		return nil, err
	}

	if id.RegistryArn != nil && *id.RegistryArn != *r.RegistryArn {
		return nil, notFound("Registry %s not found.", *id.RegistryArn)
	}

	return r, nil
}

func (s *Service) newRegistry(
	tx storage.WriteTx, scope request.Scope, n string, description *string, tags map[string]string,
) (*registryRecord, error) {
	key := scope.Key(service, kindRegistry, n)
	if _, found := tx.Get(key); found {
		return nil, alreadyExists("Registry %s already exists.", n)
	}

	arn := glueResourcePrefix(scope, kindRegistry) + n
	if n == defaultRegistry {
		arn += ":" + common.RandomID(5, "abcdefghijklmnopqrstuvwxyz")
	}

	now := s.now().UTC().Format(time.RFC3339Nano)
	r := &registryRecord{
		GetRegistryResponse: api.GetRegistryResponse{
			RegistryName: new(n), RegistryArn: new(arn), Description: description,
			CreatedTime: new(now), UpdatedTime: new(now), Status: new(schemaAvailable),
		},
		Tags: tags,
	}
	return r, storage.Write(tx, key, r)
}

func (s *Service) CreateRegistry(
	ctx context.Context,
	in *api.CreateRegistryInput,
) (*api.CreateRegistryResponse, error) {
	n := common.Value(in.RegistryName)
	if !registryNamePattern.MatchString(n) || len(common.Value(in.Description)) > 2048 {
		return nil, invalid("Invalid registry name or description.")
	}

	if err := checkTags(in.Tags); err != nil {
		return nil, err
	}

	out := &api.CreateRegistryResponse{}
	err := s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := s.newRegistry(tx, scope, n, in.Description, in.Tags)
		if err != nil {
			return err
		}

		*out = api.CreateRegistryResponse{
			RegistryName: r.RegistryName, RegistryArn: r.RegistryArn, Description: r.Description, Tags: r.Tags,
		}
		return nil
	})
	return out, err
}

func (s *Service) GetRegistry(ctx context.Context, in *api.GetRegistryInput) (*api.GetRegistryResponse, error) {
	out := &api.GetRegistryResponse{}
	err := s.view(ctx, nil, func(tx storage.ReadTx, scope request.Scope) error {
		r, err := findRegistry(tx, scope, in.RegistryId)
		if err != nil {
			return err
		}

		*out = r.GetRegistryResponse
		return nil
	})
	return out, err
}

func registryPage(keys []string, prefix string, token *string, maxResults *int64) (
	selected []string, next *string, err error,
) {
	maximum := common.Value(common.OrElse(maxResults, new(int64(25))))
	if maximum < 1 || maximum > schemaPageSize {
		return nil, nil, invalid("MaxResults must be between 1 and %d.", schemaPageSize)
	}

	return common.Page(keys, func(key string) string { return strings.TrimPrefix(key, prefix) },
		token, int(maximum), invalid("Invalid NextToken."))
}

func (s *Service) ListRegistries(
	ctx context.Context,
	in *api.ListRegistriesInput,
) (*api.ListRegistriesResponse, error) {
	out := &api.ListRegistriesResponse{Registries: []api.RegistryListItem{}}
	err := s.view(ctx, nil, func(tx storage.ReadTx, scope request.Scope) error {
		prefix := scope.Prefix(service, kindRegistry)
		keys, next, err := registryPage(tx.Keys(prefix), prefix, in.NextToken, in.MaxResults)
		if err != nil {
			return err
		}

		out.NextToken = next
		for _, key := range keys {
			r, _, err := storage.Read[registryRecord](tx, key)
			if err != nil {
				return err
			}

			out.Registries = append(out.Registries, api.RegistryListItem(r.GetRegistryResponse))
		}

		return nil
	})
	return out, err
}

func (s *Service) DeleteRegistry(
	ctx context.Context,
	in *api.DeleteRegistryInput,
) (*api.DeleteRegistryResponse, error) {
	out := &api.DeleteRegistryResponse{}
	err := s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := findRegistry(tx, scope, in.RegistryId)
		if err != nil {
			return err
		}

		prefix := scope.Prefix(service, kindSchema)
		for _, key := range tx.Keys(prefix + *r.RegistryName + "/") {
			if err := deleteSchema(tx, scope, strings.TrimPrefix(key, prefix)); err != nil {
				return err
			}
		}

		tx.Delete(scope.Key(service, kindRegistry, *r.RegistryName))
		*out = api.DeleteRegistryResponse{
			RegistryArn: r.RegistryArn, RegistryName: r.RegistryName,
			Status: new(schemaDeleting),
		}
		return nil
	})
	return out, err
}
