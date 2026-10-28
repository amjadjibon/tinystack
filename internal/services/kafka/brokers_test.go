package kafka

import (
	"strings"
	"testing"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/kafka"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestBrokerStrings(t *testing.T) {
	for mask := range 16 {
		for _, encryption := range []string{"PLAINTEXT", "TLS", "TLS_PLAINTEXT"} {
			r := &api.ClusterInfo{
				ClusterArn: new(
					"arn:aws:kafka:us-east-1:000000000000:cluster/cluster/12345678-1234-1234-1234-123456789012-2",
				),
				ClusterName:         new("cluster"),
				NumberOfBrokerNodes: new(int64(2)),
				EncryptionInfo: &api.EncryptionInfo{
					EncryptionInTransit: &api.EncryptionInTransit{ClientBroker: new(encryption)},
				},
				ClientAuthentication: &api.ClientAuthentication{
					Unauthenticated: &api.Unauthenticated{Enabled: new(mask&1 != 0)},
					Tls:             &api.Tls{Enabled: new(mask&2 != 0)},
					Sasl: &api.Sasl{
						Scram: &api.Scram{Enabled: new(mask&4 != 0)},
						Iam:   &api.Iam{Enabled: new(mask&8 != 0)},
					},
				},
			}

			b := bootstrap(r, nil)
			for _, check := range []struct {
				value   *string
				port    string
				enabled bool
			}{
				{b.BootstrapBrokerString, "9092", (mask&3 != 0) && encryption != "TLS"},
				{b.BootstrapBrokerStringTls, "9094", (mask&3 != 0) && encryption != "PLAINTEXT"},
				{b.BootstrapBrokerStringSaslScram, "9096", mask&4 != 0},
				{b.BootstrapBrokerStringSaslIam, "9098", mask&8 != 0},
			} {
				if (check.value != nil) != check.enabled {
					t.Fatalf("mask %d %s: %+v", mask, encryption, b)
				}

				if check.enabled {
					want := "b-1.cluster.12345678.c2.kafka.us-east-1.amazonaws.com:" + check.port +
						",b-2.cluster.12345678.c2.kafka.us-east-1.amazonaws.com:" + check.port
					if common.Value(check.value) != want {
						t.Fatalf("got %s want %s", common.Value(check.value), want)
					}
				}
			}
		}
	}
}

func TestRecordsBootstrapAndNodes(t *testing.T) {
	s := New(&memory.Store{}, netRecords())
	arn := create(t, s, input())
	brokers, err := s.GetBootstrapBrokers(local(), &api.GetBootstrapBrokersRequest{ClusterArn: arn})
	if err != nil || brokers.BootstrapBrokerString != nil ||
		!strings.Contains(common.Value(brokers.BootstrapBrokerStringTls), ":9094") {
		t.Fatalf("%+v %v", brokers, err)
	}

	nodes, err := s.ListNodes(local(), &api.ListNodesRequest{ClusterArn: arn, MaxResults: new(int64(1))})
	if err != nil || len(nodes.NodeInfoList) != 1 || nodes.NextToken == nil {
		t.Fatalf("%+v %v", nodes, err)
	}

	page, err := s.ListNodes(local(), &api.ListNodesRequest{ClusterArn: arn, NextToken: nodes.NextToken})
	if err != nil || len(page.NodeInfoList) != 1 || *page.NodeInfoList[0].BrokerNodeInfo.BrokerId != 2 {
		t.Fatalf("%+v %v", page, err)
	}

	_, err = api.Descriptor(s).Operations["ListNodes"].Invoke(
		local(),
		&api.ListNodesRequest{ClusterArn: arn, MaxResults: new(int64(0))},
	)
	if err == nil {
		t.Fatal("model range accepted")
	}
}

func TestBrokerOperationModelConstraints(t *testing.T) {
	s := New(&memory.Store{}, netRecords())
	for _, tc := range []struct {
		input any
		op    string
	}{
		{&api.GetBootstrapBrokersRequest{}, "GetBootstrapBrokers"},
		{&api.ListNodesRequest{}, "ListNodes"},
		{&api.ListNodesRequest{ClusterArn: new("cluster"), MaxResults: new(int64(101))}, "ListNodes"},
	} {
		_, err := api.Descriptor(s).Operations[tc.op].Invoke(local(), tc.input)
		wantError(t, err, "BadRequestException", 400)
	}

	in := input()
	in.EncryptionInfo = &api.EncryptionInfo{
		EncryptionInTransit: &api.EncryptionInTransit{ClientBroker: new("not-an-enum")},
	}

	_, err := api.Descriptor(s).Operations["CreateCluster"].Invoke(local(), in)
	wantError(t, err, "BadRequestException", 400)
}
