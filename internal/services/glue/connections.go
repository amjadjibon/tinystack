package glue

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/glue"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const kindConnection = "connection"

type connectionRecord struct {
	Connection api.Connection
	Tags       map[string]string
}

// ProtectConnections redacts request and response bodies that can contain connection credentials.
//
//nolint:gocritic // Startup decoration preserves the generated value descriptor contract.
func ProtectConnections(d catalog.Service) catalog.Service {
	for _, name := range []string{"CreateConnection", "UpdateConnection", "GetConnection", "GetConnections"} {
		op := d.Operations[name]
		op.Sensitive = true
		d.Operations[name] = op
	}

	return d
}

func loadConnection(tx storage.ReadTx, scope request.Scope, n string) (*connectionRecord, error) {
	if err := orchestrationName(new(n)); err != nil {
		return nil, err
	}

	r, ok, err := storage.Read[connectionRecord](tx, orchestrationKey(scope, kindConnection, n))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, notFound("Connection %s not found", n)
	}

	return r, nil
}

func connectionType(t string) error {
	switch t {
	case "JDBC", "MONGODB", "NETWORK", "KAFKA", "CUSTOM", "MARKETPLACE":
		return nil
	default:
		return invalid("Unsupported ConnectionType %q", t)
	}
}

func connectionCriteria(criteria []string) error {
	if len(criteria) > 10 {
		return invalid("MatchCriteria can contain at most 10 strings")
	}

	for _, criterion := range criteria {
		if err := orchestrationName(new(criterion)); err != nil {
			return invalid("Invalid MatchCriteria entry")
		}
	}

	return nil
}

func validateConnection(in *api.ConnectionInput) error {
	if in == nil {
		return invalid("ConnectionInput is required")
	}

	if err := orchestrationName(in.Name); err != nil {
		return err
	}

	if err := connectionType(common.Value(in.ConnectionType)); err != nil {
		return err
	}

	if err := connectionCriteria(in.MatchCriteria); err != nil {
		return err
	}

	if len(common.Value(in.Description)) > 2048 {
		return invalid("Description exceeds 2048 bytes")
	}

	p := in.ConnectionProperties
	if len(p) > 100 {
		return invalid("ConnectionProperties can contain at most 100 entries")
	}

	credentials := p["SECRET_ID"] != "" ||
		(p["USERNAME"] != "" && (p["PASSWORD"] != "" || p["ENCRYPTED_PASSWORD"] != ""))
	switch *in.ConnectionType {
	case "JDBC":
		if p["JDBC_CONNECTION_URL"] == "" && (p["HOST"] == "" || p["PORT"] == "" || p["JDBC_ENGINE"] == "") {
			return invalid("JDBC requires JDBC_CONNECTION_URL or HOST, PORT and JDBC_ENGINE")
		}

		if !credentials {
			return invalid("JDBC requires credentials or SECRET_ID")
		}
	case "MONGODB":
		if p["CONNECTION_URL"] == "" || !credentials {
			return invalid("MONGODB requires CONNECTION_URL and credentials or SECRET_ID")
		}
	case "KAFKA":
		if p["KAFKA_BOOTSTRAP_SERVERS"] == "" {
			return invalid("KAFKA requires KAFKA_BOOTSTRAP_SERVERS")
		}
	case "NETWORK":
		r := in.PhysicalConnectionRequirements
		if r == nil || common.Value(r.SubnetId) == "" || len(r.SecurityGroupIdList) == 0 {
			return invalid("NETWORK requires a subnet and at least one security group")
		}
	case "CUSTOM", "MARKETPLACE":
		if p["CONNECTOR_TYPE"] == "" || p["CONNECTOR_URL"] == "" ||
			p["CONNECTOR_CLASS_NAME"] == "" || p["CONNECTION_URL"] == "" {
			return invalid("Connector connections require CONNECTOR_TYPE, CONNECTOR_URL, " +
				"CONNECTOR_CLASS_NAME and CONNECTION_URL")
		}

		if strings.EqualFold(p["CONNECTOR_TYPE"], "JDBC") && !credentials {
			return invalid("A JDBC connector requires credentials or SECRET_ID")
		}
	}

	return nil
}

func (s *Service) connectionOf(ctx context.Context, in *api.ConnectionInput, created *float64) api.Connection {
	return api.Connection{
		Name: in.Name, Description: in.Description, ConnectionType: in.ConnectionType,
		MatchCriteria: in.MatchCriteria, ConnectionProperties: in.ConnectionProperties,
		PhysicalConnectionRequirements: in.PhysicalConnectionRequirements, CreationTime: created,
		LastUpdatedTime: new(common.Unix(s.now())), LastUpdatedBy: new(request.CallerARN(ctx)),
	}
}

func (s *Service) CreateConnection(
	ctx context.Context, in *api.CreateConnectionRequest,
) (*api.CreateConnectionResponse, error) {
	if err := validateConnection(in.ConnectionInput); err != nil {
		return nil, err
	}

	if err := checkTags(in.Tags); err != nil {
		return nil, err
	}

	err := s.update(ctx, in.CatalogId, func(tx storage.WriteTx, scope request.Scope) error {
		n := *in.ConnectionInput.Name
		key := orchestrationKey(scope, kindConnection, n)
		if _, ok := tx.Get(key); ok {
			return alreadyExists("Connection %s already exists", n)
		}

		r := connectionRecord{
			Connection: s.connectionOf(ctx, in.ConnectionInput, new(common.Unix(s.now()))),
			Tags:       in.Tags,
		}
		return storage.Write(tx, key, r)
	})
	return &api.CreateConnectionResponse{}, err
}

func (s *Service) UpdateConnection(
	ctx context.Context, in *api.UpdateConnectionRequest,
) (*api.UpdateConnectionResponse, error) {
	if err := validateConnection(in.ConnectionInput); err != nil {
		return nil, err
	}

	n := common.Value(in.Name)
	if *in.ConnectionInput.Name != n {
		return nil, invalid("Renaming connections is not supported")
	}

	err := s.update(ctx, in.CatalogId, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := loadConnection(tx, scope, n)
		if err != nil {
			return err
		}

		r.Connection = s.connectionOf(ctx, in.ConnectionInput, r.Connection.CreationTime)
		return storage.Write(tx, orchestrationKey(scope, kindConnection, n), r)
	})
	return &api.UpdateConnectionResponse{}, err
}

func describeConnection(connection *api.Connection, hide bool) api.Connection {
	c := *connection
	if hide {
		c.ConnectionProperties = maps.Clone(c.ConnectionProperties)
		maps.DeleteFunc(c.ConnectionProperties, func(k, _ string) bool {
			return strings.HasSuffix(strings.ToUpper(k), "PASSWORD")
		})
	}

	return c
}

func (s *Service) GetConnection(ctx context.Context, in *api.GetConnectionRequest) (*api.GetConnectionResponse, error) {
	out := &api.GetConnectionResponse{}
	err := s.view(ctx, in.CatalogId, func(tx storage.ReadTx, scope request.Scope) error {
		r, err := loadConnection(tx, scope, common.Value(in.Name))
		if err != nil {
			return err
		}

		out.Connection = new(describeConnection(&r.Connection, common.Value(in.HidePassword)))
		return nil
	})
	return out, err
}

func (s *Service) GetConnections(
	ctx context.Context,
	in *api.GetConnectionsRequest,
) (*api.GetConnectionsResponse, error) {
	f := common.OrElse(in.Filter, &api.GetConnectionsFilter{})
	if f.ConnectionType != nil {
		if err := connectionType(*f.ConnectionType); err != nil {
			return nil, err
		}
	}

	if err := connectionCriteria(f.MatchCriteria); err != nil {
		return nil, err
	}

	out := &api.GetConnectionsResponse{}
	err := s.view(ctx, in.CatalogId, func(tx storage.ReadTx, scope request.Scope) error {
		rows, err := storage.Records[connectionRecord](tx, scope.Prefix(service, kindConnection))
		if err != nil {
			return err
		}

		var selected []api.Connection
		for _, r := range rows {
			if f.ConnectionType != nil && *f.ConnectionType != *r.Connection.ConnectionType {
				continue
			}

			if slices.ContainsFunc(f.MatchCriteria, func(c string) bool {
				return !slices.Contains(r.Connection.MatchCriteria, c)
			}) {
				continue
			}

			selected = append(selected, describeConnection(&r.Connection, common.Value(in.HidePassword)))
		}

		out.ConnectionList, out.NextToken, err = page(selected, func(c api.Connection) string {
			return orchestrationKey(scope, kindConnection, *c.Name)
		}, in.NextToken, in.MaxResults)
		return err
	})
	return out, err
}

func (s *Service) DeleteConnection(
	ctx context.Context, in *api.DeleteConnectionRequest,
) (*api.DeleteConnectionResponse, error) {
	err := s.update(ctx, in.CatalogId, func(tx storage.WriteTx, scope request.Scope) error {
		n := common.Value(in.ConnectionName)
		if _, err := loadConnection(tx, scope, n); err != nil {
			return err
		}

		tx.Delete(orchestrationKey(scope, kindConnection, n))
		return nil
	})
	return &api.DeleteConnectionResponse{}, err
}

func (s *Service) BatchDeleteConnection(
	ctx context.Context, in *api.BatchDeleteConnectionRequest,
) (*api.BatchDeleteConnectionResponse, error) {
	if in.ConnectionNameList == nil || len(in.ConnectionNameList) > 25 {
		return nil, invalid("ConnectionNameList is required and accepts at most 25 names")
	}

	out := &api.BatchDeleteConnectionResponse{Errors: map[string]api.ErrorDetail{}}
	err := s.update(ctx, in.CatalogId, func(tx storage.WriteTx, scope request.Scope) error {
		for _, n := range in.ConnectionNameList {
			if slices.Contains(out.Succeeded, n) {
				continue
			}

			if _, err := loadConnection(tx, scope, n); err != nil {
				detail := awserror.From(err)
				if detail.StatusCode >= 500 {
					return err
				}

				out.Errors[n] = api.ErrorDetail{ErrorCode: new(detail.Code), ErrorMessage: new(detail.Message)}
				continue
			}

			tx.Delete(orchestrationKey(scope, kindConnection, n))
			out.Succeeded = append(out.Succeeded, n)
		}

		return nil
	})
	return out, err
}
