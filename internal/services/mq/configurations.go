package mq

import (
	"context"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"uuid"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/mq"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	configurationKind              = "configuration"
	customConfigurationUnsupported = "Custom configurations are unsupported in Docker mode"
	invalidPageToken               = "Invalid NextToken"
)

// Configurations and their immutable revisions share one scoped configuration/<id> record.
type configurationRecord struct {
	Configuration api.Configuration
	Revisions     []api.DescribeConfigurationRevisionResponse
}

func configuration(tx storage.ReadTx, scope request.Scope, id string) (*configurationRecord, error) {
	r, ok, err := storage.Read[configurationRecord](tx, scope.Key(serviceName, configurationKind, id))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, missing()
	}

	return r, nil
}

func mqPage[T any](
	rows []T,
	id func(T) string,
	maxResults *int64,
	token *string,
	scope string,
) (selected []T, nextToken *string, pageErr error) {
	limit := int64(20)
	if maxResults != nil {
		limit = *maxResults
	}

	if limit < 5 || limit > 100 {
		return nil, nil, invalid("MaxResults must be 5-100")
	}

	var cursor *string
	if token != nil && *token != "" {
		raw, err := base64.RawURLEncoding.DecodeString(*token)
		if err != nil || !strings.HasPrefix(string(raw), scope+"|") {
			return nil, nil, invalid(invalidPageToken)
		}

		cursor = new(base64.RawURLEncoding.EncodeToString([]byte(strings.TrimPrefix(string(raw), scope+"|"))))
	}

	out, next, err := common.Page(rows, id, cursor, int(limit), invalid(invalidPageToken))
	if next != nil {
		raw, _ := base64.RawURLEncoding.DecodeString(*next)
		next = new(base64.RawURLEncoding.EncodeToString([]byte(scope + "|" + string(raw))))
	}

	return out, next, err
}

func (s *Service) CreateConfiguration(
	ctx context.Context,
	in *api.CreateConfigurationRequest,
) (*api.CreateConfigurationResponse, error) {
	if s.runtime != nil {
		return nil, invalid(customConfigurationUnsupported)
	}

	if !configurationNamePattern.MatchString(common.Value(in.Name)) {
		return nil, invalid("Invalid configuration name")
	}

	engine, version := common.Value(in.EngineType), common.Value(in.EngineVersion)
	if engine != activeMQ && engine != rabbitMQ {
		return nil, invalid("Invalid engine type")
	}

	if _, err := brokerImage(engine, version); err != nil {
		return nil, invalid(err.Error())
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	id := "c-" + uuid.NewV4().String()
	now := new(time.Now().UTC())
	rev := &api.ConfigurationRevision{Created: now, Revision: new(int64(1)), Description: new("Initial configuration")}
	data := "<broker xmlns=\"http://activemq.apache.org/schema/core\"/>"
	if engine == rabbitMQ {
		data = "# RabbitMQ configuration\n"
	}

	r := &configurationRecord{
		Configuration: api.Configuration{
			Id: new(id),
			Arn: new(
				fmt.Sprintf("arn:aws:mq:%s:%s:configuration:%s:%s", scope.Region, scope.AccountID, *in.Name, id),
			),
			Name:           in.Name,
			EngineType:     in.EngineType,
			EngineVersion:  in.EngineVersion,
			Created:        now,
			LatestRevision: rev,
		},
		Revisions: []api.DescribeConfigurationRevisionResponse{
			{
				ConfigurationId: new(id),
				Created:         now,
				Description:     rev.Description,
				Data:            new(base64.StdEncoding.EncodeToString([]byte(data))),
			},
		},
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		return storage.Write(tx, scope.Key(serviceName, configurationKind, id), r)
	})
	c := r.Configuration
	return &api.CreateConfigurationResponse{
		Arn:            c.Arn,
		Id:             c.Id,
		Name:           c.Name,
		Created:        c.Created,
		LatestRevision: c.LatestRevision,
	}, err
}

func (s *Service) DescribeConfiguration(
	ctx context.Context,
	in *api.DescribeConfigurationRequest,
) (*api.DescribeConfigurationResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.DescribeConfigurationResponse
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, e := configuration(tx, scope, common.Value(in.ConfigurationId))
		if e != nil {
			return e
		}

		c := r.Configuration
		out = &api.DescribeConfigurationResponse{
			Arn:            c.Arn,
			Id:             c.Id,
			Name:           c.Name,
			Created:        c.Created,
			LatestRevision: c.LatestRevision,
			Description:    c.Description,
			EngineType:     c.EngineType,
			EngineVersion:  c.EngineVersion,
		}

		return nil
	})
	return out, err
}

func (s *Service) ListConfigurations(
	ctx context.Context,
	in *api.ListConfigurationsRequest,
) (*api.ListConfigurationsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.ListConfigurationsResponse{MaxResults: common.OrElse(in.MaxResults, new(int64(20)))}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		records, e := storage.Records[configurationRecord](tx, scope.Prefix(serviceName, configurationKind))
		if e != nil {
			return e
		}

		rows := []api.Configuration{}
		for _, r := range records {
			rows = append(rows, r.Configuration)
		}

		out.Configurations, out.NextToken, e = mqPage(
			rows,
			func(c api.Configuration) string { return *c.Id },
			in.MaxResults,
			in.NextToken,
			scope.Key(serviceName, "ListConfigurations", ""),
		)
		return e
	})
	return out, err
}

func validateConfigurationData(engine, data string) error {
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil || len(raw) == 0 || len(raw) > 1024*1024 {
		return invalid("Data must be nonempty base64 configuration data, at most 1 MiB")
	}

	if engine == activeMQ {
		decoder := xml.NewDecoder(strings.NewReader(string(raw)))
		root := ""
		depth := 0
		for {
			token, e := decoder.Token()
			if errors.Is(e, io.EOF) {
				break
			}

			if e != nil {
				return invalid("Data must contain valid ActiveMQ XML")
			}

			switch t := token.(type) {
			case xml.StartElement:
				if depth == 0 {
					if root != "" {
						return invalid("XML requires one broker root")
					}

					root = t.Name.Local
				}

				depth++
			case xml.EndElement:
				depth--
			case xml.Directive:
				return invalid("XML directives are unsupported")
			}
		}

		if root != "broker" {
			return invalid("ActiveMQ configuration requires a broker root")
		}
	} else {
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "#") && !strings.Contains(line, "=") {
				return invalid("RabbitMQ data requires Cuttlefish key = value lines")
			}
		}
	}

	return nil
}

func (s *Service) UpdateConfiguration(
	ctx context.Context,
	in *api.UpdateConfigurationRequest,
) (*api.UpdateConfigurationResponse, error) {
	if s.runtime != nil {
		return nil, invalid(customConfigurationUnsupported)
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.UpdateConfigurationResponse
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := configuration(tx, scope, common.Value(in.ConfigurationId))
		if e != nil {
			return e
		}

		if e := validateConfigurationData(*r.Configuration.EngineType, common.Value(in.Data)); e != nil {
			return e
		}

		rev := &api.ConfigurationRevision{
			Created:     new(time.Now().UTC()),
			Description: in.Description,
			Revision:    new(int64(len(r.Revisions) + 1)),
		}

		r.Configuration.LatestRevision = rev
		r.Configuration.Description = in.Description
		r.Revisions = append(
			r.Revisions,
			api.DescribeConfigurationRevisionResponse{
				ConfigurationId: in.ConfigurationId,
				Created:         rev.Created,
				Description:     in.Description,
				Data:            in.Data,
			},
		)
		c := r.Configuration
		out = &api.UpdateConfigurationResponse{
			Arn:            c.Arn,
			Id:             c.Id,
			Name:           c.Name,
			Created:        c.Created,
			LatestRevision: rev,
		}

		return storage.Write(tx, scope.Key(serviceName, configurationKind, *c.Id), r)
	})
	return out, err
}

func (s *Service) DescribeConfigurationRevision(
	ctx context.Context,
	in *api.DescribeConfigurationRevisionRequest,
) (*api.DescribeConfigurationRevisionResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.DescribeConfigurationRevisionResponse
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, e := configuration(tx, scope, common.Value(in.ConfigurationId))
		if e != nil {
			return e
		}

		n, e := strconv.Atoi(common.Value(in.ConfigurationRevision))
		if e != nil || n < 1 {
			return invalid("Invalid configuration revision")
		}

		if n > len(r.Revisions) {
			return missing()
		}

		out = common.Clone(&r.Revisions[n-1])
		return nil
	})
	return out, err
}

func (s *Service) ListConfigurationRevisions(
	ctx context.Context,
	in *api.ListConfigurationRevisionsRequest,
) (*api.ListConfigurationRevisionsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.ListConfigurationRevisionsResponse{
		ConfigurationId: in.ConfigurationId,
		MaxResults:      common.OrElse(in.MaxResults, new(int64(20))),
	}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, e := configuration(tx, scope, common.Value(in.ConfigurationId))
		if e != nil {
			return e
		}

		rows := []api.ConfigurationRevision{}
		for i, rev := range r.Revisions {
			rows = append(
				rows,
				api.ConfigurationRevision{
					Created:     rev.Created,
					Description: rev.Description,
					Revision:    new(int64(i + 1)),
				},
			)
		}

		out.Revisions, out.NextToken, e = mqPage(
			rows,
			func(c api.ConfigurationRevision) string { return fmt.Sprintf("%020d", *c.Revision) },
			in.MaxResults,
			in.NextToken,
			scope.Key(serviceName, "revisions", *in.ConfigurationId),
		)
		return e
	})
	return out, err
}

func (s *Service) DeleteConfiguration(
	ctx context.Context,
	in *api.DeleteConfigurationRequest,
) (*api.DeleteConfigurationResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, e := configuration(tx, scope, common.Value(in.ConfigurationId)); e != nil {
			return e
		}

		rows, e := storage.Records[record](tx, scope.Prefix(serviceName, brokerKind))
		if e != nil {
			return e
		}

		for _, r := range rows {
			c := r.Broker.Configurations
			if c != nil {
				refs := append([]api.ConfigurationId{}, c.History...)
				if c.Current != nil {
					refs = append(refs, *c.Current)
				}

				if c.Pending != nil {
					refs = append(refs, *c.Pending)
				}

				for _, ref := range refs {
					if common.Value(ref.Id) == common.Value(in.ConfigurationId) {
						return conflict("Configuration is associated with a broker")
					}
				}
			}
		}

		tx.Delete(scope.Key(serviceName, configurationKind, *in.ConfigurationId))
		return nil
	})
	return &api.DeleteConfigurationResponse{ConfigurationId: in.ConfigurationId}, err
}

func (s *Service) selectConfiguration(
	tx storage.ReadTx,
	scope request.Scope,
	b *api.DescribeBrokerResponse,
	id *api.ConfigurationId,
	pending bool,
) error {
	if s.runtime != nil {
		return invalid(customConfigurationUnsupported)
	}

	r, err := configuration(tx, scope, common.Value(id.Id))
	if err != nil {
		return err
	}

	if common.Value(r.Configuration.EngineType) != common.Value(b.EngineType) ||
		common.Value(r.Configuration.EngineVersion) != common.Value(b.EngineVersion) {
		return invalid("Configuration engine and version must match the broker")
	}

	if id.Revision == nil || *id.Revision < 1 || *id.Revision > int64(len(r.Revisions)) {
		return invalid("Configuration revision does not exist")
	}

	if b.Configurations == nil {
		b.Configurations = &api.Configurations{}
	}

	if pending {
		b.Configurations.Pending = common.Clone(id)
	} else {
		b.Configurations.Current = common.Clone(id)
	}

	return nil
}

func pendingConfiguration(b *api.DescribeBrokerResponse) *api.ConfigurationId {
	if b.Configurations == nil {
		return nil
	}

	if b.Configurations.Pending != nil {
		return b.Configurations.Pending
	}

	return b.Configurations.Current
}
