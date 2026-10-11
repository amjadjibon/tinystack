package kafka

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/kafka"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

type cursor struct{ Name, ARN string }

func (s *Service) ListClusters(ctx context.Context, in *api.ListClustersRequest) (*api.ListClustersResponse, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}
	limit := int64(100)
	if in.MaxResults != nil {
		limit = *in.MaxResults
	}
	if limit < 1 || limit > 100 {
		return nil, invalid("MaxResults must be 1-100 (default 100)")
	}
	filter := common.Value(in.ClusterNameFilter)
	sum := sha256.Sum256([]byte(scope.AccountID + "/" + scope.Region + "/kafka/ListClusters/" + filter))
	prefix := fmt.Sprintf("%x:", sum)
	after := cursor{}
	if in.NextToken != nil {
		raw, err := base64.RawURLEncoding.DecodeString(*in.NextToken)
		if err != nil || !strings.HasPrefix(string(raw), prefix) {
			return nil, invalid("Invalid NextToken for this scope, API and filter")
		}
		if err = json.Unmarshal(
			raw[len(prefix):],
			&after,
		); err != nil || !namePattern.MatchString(after.Name) ||
			!arnPattern.MatchString(after.ARN) {
			return nil, invalid("Invalid NextToken cursor")
		}
	}
	var rows []*api.ClusterInfo
	e = s.store.View(ctx, func(tx storage.ReadTx) error {
		var err error
		rows, err = storage.Records[api.ClusterInfo](tx, scope.Prefix(serviceName, clusterKind))
		return err
	})
	if e != nil {
		return nil, e
	}
	slices.SortFunc(rows, func(a, b *api.ClusterInfo) int {
		if c := strings.Compare(*a.ClusterName, *b.ClusterName); c != 0 {
			return c
		}
		return strings.Compare(*a.ClusterArn, *b.ClusterArn)
	})
	out := &api.ListClustersResponse{ClusterInfoList: []api.ClusterInfo{}}
	for _, r := range rows {
		if !strings.HasPrefix(*r.ClusterName, filter) || *r.ClusterName < after.Name ||
			*r.ClusterName == after.Name && *r.ClusterArn <= after.ARN {
			continue
		}
		if int64(len(out.ClusterInfoList)) == limit {
			last := out.ClusterInfoList[len(out.ClusterInfoList)-1]
			raw, err := json.Marshal(cursor{Name: *last.ClusterName, ARN: *last.ClusterArn})
			if err != nil {
				return nil, err
			}
			out.NextToken = new(base64.RawURLEncoding.EncodeToString(append([]byte(prefix), raw...)))
			break
		}
		out.ClusterInfoList = append(out.ClusterInfoList, *common.Clone(r))
	}
	return out, nil
}
