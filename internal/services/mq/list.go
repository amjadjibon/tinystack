package mq

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/mq"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) ListBrokers(ctx context.Context, in *api.ListBrokersRequest) (*api.ListBrokersResponse, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}
	limit := int64(20)
	if in.MaxResults != nil {
		limit = *in.MaxResults
	}
	if limit < 5 || limit > 100 {
		return nil, invalid("MaxResults must be 5-100 (default 20)")
	}
	sum := sha256.Sum256([]byte(scope.AccountID + "/" + scope.Region + "/mq/ListBrokers"))
	prefix := fmt.Sprintf("%x:", sum)
	after := ""
	if in.NextToken != nil && *in.NextToken != "" {
		raw, err := base64.RawURLEncoding.DecodeString(*in.NextToken)
		if err != nil || !strings.HasPrefix(string(raw), prefix) {
			return nil, invalid("Invalid NextToken for this scope and API")
		}
		after = strings.TrimPrefix(string(raw), prefix)
		if !idPattern.MatchString(after) {
			return nil, invalid("Invalid NextToken cursor")
		}
	}
	var rows []*record
	e = s.store.View(ctx, func(tx storage.ReadTx) error {
		var err error
		rows, err = storage.Records[record](tx, scope.Prefix(serviceName, brokerKind))
		return err
	})
	if e != nil {
		return nil, e
	}
	slices.SortFunc(rows, func(a, b *record) int { return strings.Compare(*a.Broker.BrokerId, *b.Broker.BrokerId) })
	out := &api.ListBrokersResponse{BrokerSummaries: []api.BrokerSummary{}}
	for _, r := range rows {
		b := r.Broker
		if *b.BrokerId <= after {
			continue
		}
		if int64(len(out.BrokerSummaries)) == limit {
			last := out.BrokerSummaries[len(out.BrokerSummaries)-1].BrokerId
			out.NextToken = new(base64.RawURLEncoding.EncodeToString([]byte(prefix + *last)))
			break
		}
		out.BrokerSummaries = append(out.BrokerSummaries, api.BrokerSummary{
			BrokerId: b.BrokerId, BrokerArn: b.BrokerArn,
			BrokerName: b.BrokerName, Created: b.Created, EngineType: b.EngineType, DeploymentMode: b.DeploymentMode,
			HostInstanceType: b.HostInstanceType,
		})
	}
	return common.Clone(out), nil
}
