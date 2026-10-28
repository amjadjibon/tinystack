package kafka

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/kafka"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// listeners captures the independently enabled MSK client listeners.
type listeners struct{ plain, tls, scram, iam, mtls bool }

func clusterListeners(r *api.ClusterInfo) listeners {
	auth := r.ClientAuthentication
	unauth, mtls, scram, iam := true, false, false, false
	if auth != nil {
		if auth.Tls != nil {
			mtls = common.Value(auth.Tls.Enabled)
			if auth.Tls.Enabled == nil {
				mtls = len(auth.Tls.CertificateAuthorityArnList) > 0
			}
		}

		if auth.Sasl != nil {
			if auth.Sasl.Scram != nil {
				scram = common.Value(auth.Sasl.Scram.Enabled)
			}

			if auth.Sasl.Iam != nil {
				iam = common.Value(auth.Sasl.Iam.Enabled)
			}
		}

		unauth = !mtls && !scram && !iam
		if auth.Unauthenticated != nil {
			unauth = common.Value(auth.Unauthenticated.Enabled)
		}
	}

	encryption := clientTLS
	if r.EncryptionInfo != nil && r.EncryptionInfo.EncryptionInTransit != nil {
		encryption = common.Value(common.OrElse(r.EncryptionInfo.EncryptionInTransit.ClientBroker, new(clientTLS)))
	}

	return listeners{
		plain: (unauth || mtls) && encryption != clientTLS,
		tls:   (unauth || mtls) && encryption != "PLAINTEXT",
		scram: scram,
		iam:   iam,
		mtls:  mtls,
	}
}

func brokerHost(r *api.ClusterInfo, node int64) string {
	parts := arnPattern.FindStringSubmatch(*r.ClusterArn)
	id, _, _ := strings.Cut(parts[4], "-")
	// Use a stable short DNS identifier from the cluster UUID and the ARN's generation suffix.
	_, generation, _ := strings.Cut(parts[4][36:], "-")
	if generation == "" {
		generation = "1"
	}

	name := strings.ToLower(strings.ReplaceAll(*r.ClusterName, "_", ""))
	return fmt.Sprintf("b-%d.%s.%s.c%s.kafka.%s.amazonaws.com", node, name, id, generation, parts[1])
}

func brokerEndpoints(r *api.ClusterInfo, node int64, endpoint *Started) []string {
	l := clusterListeners(r)
	ports := []struct {
		enabled bool
		port    int
	}{{l.plain, 9092}, {l.tls, 9094}, {l.scram, 9096}, {l.iam, 9098}}
	out := []string{}
	for _, p := range ports {
		if p.enabled {
			if endpoint != nil {
				port := endpoint.Port
				if p.port == 9094 {
					port = endpoint.TLSPort
				}

				out = append(out, fmt.Sprintf("127.0.0.1:%d", port))
			} else {
				out = append(out, fmt.Sprintf("%s:%d", brokerHost(r, node), p.port))
			}
		}
	}

	return out
}

func bootstrap(r *api.ClusterInfo, endpoint *Started) *api.GetBootstrapBrokersResponse {
	l := clusterListeners(r)
	out := &api.GetBootstrapBrokersResponse{}
	for _, listener := range []struct {
		field   **string
		port    int
		enabled bool
	}{
		{&out.BootstrapBrokerString, 9092, l.plain},
		{&out.BootstrapBrokerStringTls, 9094, l.tls},
		{&out.BootstrapBrokerStringSaslScram, 9096, l.scram},
		{&out.BootstrapBrokerStringSaslIam, 9098, l.iam},
	} {
		if !listener.enabled {
			continue
		}

		values := []string{}
		if endpoint != nil {
			port := endpoint.Port
			if listener.port == 9094 {
				port = endpoint.TLSPort
			}

			values = append(values, fmt.Sprintf("127.0.0.1:%d", port))
		} else {
			for n := int64(1); n <= *r.NumberOfBrokerNodes; n++ {
				values = append(values, fmt.Sprintf("%s:%d", brokerHost(r, n), listener.port))
			}
		}

		*listener.field = new(strings.Join(values, ","))
	}

	return out
}

func (s *Service) brokers(ctx context.Context, arn string) (*api.ClusterInfo, *Started, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, nil, err
	}

	var r *api.ClusterInfo
	var endpoint *Started
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var readErr error
		r, readErr = read(tx, scope, arn)
		if readErr != nil {
			return readErr
		}

		if s.runtime != nil {
			rt, found, rtErr := storage.Read[runtimeRecord](tx, runtimeKey(arn))
			if rtErr != nil {
				return rtErr
			}

			if !found || common.Value(r.State) != statusActive {
				return invalid("Cluster is not ACTIVE")
			}

			endpoint = &Started{Port: rt.Port, TLSPort: rt.TLSPort}
		}

		return nil
	})
	return r, endpoint, err
}

func (s *Service) GetBootstrapBrokers(
	ctx context.Context,
	in *api.GetBootstrapBrokersRequest,
) (*api.GetBootstrapBrokersResponse, error) {
	r, endpoint, err := s.brokers(ctx, common.Value(in.ClusterArn))
	if err != nil {
		return nil, err
	}

	return bootstrap(r, endpoint), nil
}

func (s *Service) ListNodes(ctx context.Context, in *api.ListNodesRequest) (*api.ListNodesResponse, error) {
	r, endpoint, err := s.brokers(ctx, common.Value(in.ClusterArn))
	if err != nil {
		return nil, err
	}

	limit := int64(100)
	if in.MaxResults != nil {
		limit = *in.MaxResults
	}

	count := *r.NumberOfBrokerNodes
	if s.runtime != nil {
		count = 1
	}

	nodes := []api.NodeInfo{}
	for n := int64(1); n <= count; n++ {
		nodes = append(nodes, api.NodeInfo{
			NodeType:           new("BROKER"),
			InstanceType:       r.BrokerNodeGroupInfo.InstanceType,
			AddedToClusterTime: new(r.CreationTime.Format(time.RFC3339)),
			BrokerNodeInfo: &api.BrokerNodeInfo{
				BrokerId: new(float64(n)),
				ClientSubnet: new(
					r.BrokerNodeGroupInfo.ClientSubnets[(n-1)%int64(len(r.BrokerNodeGroupInfo.ClientSubnets))],
				),
				CurrentBrokerSoftwareInfo: r.CurrentBrokerSoftwareInfo,
				Endpoints:                 brokerEndpoints(r, n, endpoint),
			},
		})
	}

	sum := sha256.Sum256([]byte(*r.ClusterArn))
	selected, next, err := common.Page(
		nodes,
		func(n api.NodeInfo) string { return fmt.Sprintf("%x:%03.0f", sum, *n.BrokerNodeInfo.BrokerId) },
		in.NextToken,
		int(limit),
		invalid("Invalid NextToken"),
	)
	if err != nil {
		return nil, err
	}

	// Tokens from another cluster are rejected rather than silently yielding an empty page.
	if in.NextToken != nil && !validNodeToken(*in.NextToken, fmt.Sprintf("%x:", sum), count) {
		return nil, invalid("Invalid NextToken")
	}

	return &api.ListNodesResponse{NodeInfoList: selected, NextToken: next}, nil
}

func validNodeToken(token, prefix string, count int64) bool {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || !strings.HasPrefix(string(raw), prefix) {
		return false
	}

	n, err := strconv.ParseInt(strings.TrimPrefix(string(raw), prefix), 10, 64)
	return err == nil && n > 0 && n <= count
}
