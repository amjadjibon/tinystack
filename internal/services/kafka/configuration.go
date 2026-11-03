package kafka

import (
	"context"
	"encoding/base64"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/kafka"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

var configurationNamePattern = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z-]*$`)

type configurationRecord struct {
	Configuration api.Configuration
	Revisions     []api.DescribeConfigurationRevisionResponse
}

func configurationKey(scope request.Scope, arn string) string {
	return scope.Key(serviceName, "configuration", arn)
}

func readConfiguration(tx storage.ReadTx, scope request.Scope, arn string) (*configurationRecord, error) {
	r, ok, err := storage.Read[configurationRecord](tx, configurationKey(scope, arn))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, configurationMissing()
	}

	return r, nil
}

func configurationMissing() error {
	e := awserror.New("NotFoundException", "Configuration or revision does not exist in this account and Region")
	e.StatusCode = 404
	return e
}

func checkProperties(properties []byte) error {
	if len(properties) == 0 || !utf8.Valid(properties) {
		return invalid("ServerProperties requires nonempty UTF-8 server.properties text")
	}

	for _, line := range strings.Split(string(properties), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}

		if !strings.ContainsAny(line, "=:") {
			return invalid("ServerProperties entries require key=value or key:value")
		}
	}

	return nil
}

func configurationRevision(
	arn, description *string,
	properties []byte,
	n int64,
) api.DescribeConfigurationRevisionResponse {
	return api.DescribeConfigurationRevisionResponse{
		Arn:              arn,
		Description:      description,
		ServerProperties: properties,
		Revision:         new(n),
		CreationTime:     new(time.Now().UTC()),
	}
}

func revisionSummary(r api.DescribeConfigurationRevisionResponse) *api.ConfigurationRevision {
	return &api.ConfigurationRevision{Revision: r.Revision, CreationTime: r.CreationTime, Description: r.Description}
}

func (s *Service) CreateConfiguration(
	ctx context.Context,
	in *api.CreateConfigurationRequest,
) (*api.CreateConfigurationResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if !configurationNamePattern.MatchString(common.Value(in.Name)) {
		return nil, invalid("Name must start with a letter or digit and contain only letters, digits or hyphens")
	}

	if propertiesErr := checkProperties(in.ServerProperties); propertiesErr != nil {
		return nil, propertiesErr
	}

	for _, v := range in.KafkaVersions {
		if !slices.Contains([]string{kafka38, kafka39}, v) {
			return nil, invalid("KafkaVersions supports 3.8.x and 3.9.x")
		}
	}

	arn := new(
		fmt.Sprintf(
			"arn:aws:kafka:%s:%s:configuration/%s/%s",
			scope.Region,
			scope.AccountID,
			*in.Name,
			uuid.NewV4().String()+"-1",
		),
	)
	rev := configurationRevision(arn, in.Description, in.ServerProperties, 1)
	c := api.Configuration{
		Arn:            arn,
		Name:           in.Name,
		Description:    in.Description,
		KafkaVersions:  in.KafkaVersions,
		CreationTime:   rev.CreationTime,
		LatestRevision: revisionSummary(rev),
		State:          new(statusActive),
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rows, e := storage.Records[configurationRecord](tx, scope.Prefix(serviceName, "configuration"))
		if e != nil {
			return e
		}

		for _, r := range rows {
			if common.Value(r.Configuration.Name) == *in.Name {
				return conflict("Configuration name already exists")
			}
		}

		return storage.Write(
			tx,
			configurationKey(scope, *arn),
			configurationRecord{Configuration: c, Revisions: []api.DescribeConfigurationRevisionResponse{rev}},
		)
	})
	return &api.CreateConfigurationResponse{
		Arn:            arn,
		Name:           c.Name,
		CreationTime:   c.CreationTime,
		LatestRevision: c.LatestRevision,
		State:          c.State,
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

	var r *configurationRecord
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		r, e = readConfiguration(tx, scope, common.Value(in.Arn))
		return e
	})
	if err != nil {
		return nil, err
	}

	c := r.Configuration
	return &api.DescribeConfigurationResponse{
		Arn:            c.Arn,
		Name:           c.Name,
		Description:    c.Description,
		KafkaVersions:  c.KafkaVersions,
		CreationTime:   c.CreationTime,
		LatestRevision: c.LatestRevision,
		State:          c.State,
	}, nil
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
		r, e := readConfiguration(tx, scope, common.Value(in.Arn))
		if e != nil {
			return e
		}

		n := common.Value(in.Revision)
		if n < 1 || n > int64(len(r.Revisions)) {
			return configurationMissing()
		}

		out = common.Clone(&r.Revisions[n-1])
		return nil
	})
	return out, err
}

func pageLimit(n *int64) (int, error) {
	v := common.Value(common.OrElse(n, new(int64(100))))
	if v < 1 || v > 100 {
		return 0, invalid("MaxResults must be 1-100")
	}

	return int(v), nil
}

func (s *Service) ListConfigurations(
	ctx context.Context,
	in *api.ListConfigurationsRequest,
) (*api.ListConfigurationsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	limit, err := pageLimit(in.MaxResults)
	if err != nil {
		return nil, err
	}

	rows := []api.Configuration{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		records, e := storage.Records[configurationRecord](tx, scope.Prefix(serviceName, "configuration"))
		if e != nil {
			return e
		}

		for _, r := range records {
			rows = append(rows, r.Configuration)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	rows, next, err := configurationPage(
		rows,
		"ListConfigurations/"+scope.Prefix(serviceName, "configuration"),
		func(c api.Configuration) string { return "ListConfigurations/" + configurationKey(scope, *c.Arn) },
		in.NextToken,
		limit,
		invalid("Invalid NextToken"),
	)
	return &api.ListConfigurationsResponse{Configurations: rows, NextToken: next}, err
}

func (s *Service) ListConfigurationRevisions(
	ctx context.Context,
	in *api.ListConfigurationRevisionsRequest,
) (*api.ListConfigurationRevisionsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	limit, err := pageLimit(in.MaxResults)
	if err != nil {
		return nil, err
	}

	rows := []api.ConfigurationRevision{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, e := readConfiguration(tx, scope, common.Value(in.Arn))
		if e != nil {
			return e
		}

		for _, rev := range r.Revisions {
			rows = append(rows, *revisionSummary(rev))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	rows, next, err := configurationPage(
		rows, "ListConfigurationRevisions/"+configurationKey(scope, common.Value(in.Arn))+"/",
		func(r api.ConfigurationRevision) string {
			return fmt.Sprintf(
				"ListConfigurationRevisions/%s/%020d",
				configurationKey(scope, common.Value(in.Arn)),
				*r.Revision,
			)
		}, in.NextToken, limit, invalid("Invalid NextToken"))
	return &api.ListConfigurationRevisionsResponse{Revisions: rows, NextToken: next}, err
}

func (s *Service) UpdateConfiguration(
	ctx context.Context,
	in *api.UpdateConfigurationRequest,
) (*api.UpdateConfigurationResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if propertiesErr := checkProperties(in.ServerProperties); propertiesErr != nil {
		return nil, propertiesErr
	}

	var latest *api.ConfigurationRevision
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := readConfiguration(tx, scope, common.Value(in.Arn))
		if e != nil {
			return e
		}

		rev := configurationRevision(in.Arn, in.Description, in.ServerProperties, int64(len(r.Revisions)+1))
		r.Revisions = append(r.Revisions, rev)
		latest = revisionSummary(rev)
		r.Configuration.LatestRevision = latest
		return storage.Write(tx, configurationKey(scope, *in.Arn), r)
	})
	return &api.UpdateConfigurationResponse{Arn: in.Arn, LatestRevision: latest}, err
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
		_, e := readConfiguration(tx, scope, common.Value(in.Arn))
		if e != nil {
			return e
		}

		clusters, e := storage.Records[api.ClusterInfo](tx, scope.Prefix(serviceName, clusterKind))
		if e != nil {
			return e
		}

		for _, c := range clusters {
			if c.CurrentBrokerSoftwareInfo != nil &&
				common.Value(c.CurrentBrokerSoftwareInfo.ConfigurationArn) == common.Value(in.Arn) {
				return invalid("Configuration is in use by a cluster")
			}
		}

		tx.Delete(configurationKey(scope, *in.Arn))
		return nil
	})
	return &api.DeleteConfigurationResponse{Arn: in.Arn, State: new("DELETING")}, err
}

func applyConfiguration(tx storage.ReadTx, scope request.Scope, r *api.ClusterInfo, info *api.ConfigurationInfo) error {
	if info == nil {
		return invalid("ConfigurationInfo is required")
	}

	c, err := readConfiguration(tx, scope, common.Value(info.Arn))
	if err != nil {
		return err
	}

	n := common.Value(info.Revision)
	if n < 1 || n > int64(len(c.Revisions)) {
		return invalid("Configuration revision does not exist")
	}

	if len(c.Configuration.KafkaVersions) > 0 &&
		!slices.Contains(c.Configuration.KafkaVersions, common.Value(r.CurrentBrokerSoftwareInfo.KafkaVersion)) {
		return invalid("Configuration does not support the cluster Kafka version")
	}

	r.CurrentBrokerSoftwareInfo.ConfigurationArn = common.Clone(info.Arn)
	r.CurrentBrokerSoftwareInfo.ConfigurationRevision = common.Clone(info.Revision)
	return nil
}

// configurationPage rejects tokens from a different account, Region or configuration.
func configurationPage[T any](
	rows []T, prefix string, id func(T) string, token *string, limit int, invalidToken error,
) (selected []T, next *string, err error) {
	if token != nil {
		raw, err := base64.RawURLEncoding.DecodeString(*token)
		if err != nil || !strings.HasPrefix(string(raw), prefix) {
			return nil, nil, invalidToken
		}
	}

	return common.Page(rows, id, token, limit, invalidToken)
}
