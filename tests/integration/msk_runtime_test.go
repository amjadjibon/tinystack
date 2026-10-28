package integration_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	sdk "github.com/aws/aws-sdk-go-v2/service/kafka"
	"github.com/aws/aws-sdk-go-v2/service/kafka/types"
)

func TestMSKRecordsBootstrapAuthenticationMatrix(t *testing.T) {
	s := server(t)
	c := mskClient(s, "test", "us-east-1")
	ctx := context.Background()
	in := mskInput(t, s, "us-east-1")
	for mask := range 16 {
		for _, enc := range []types.ClientBroker{
			types.ClientBrokerTls, types.ClientBrokerTlsPlaintext, types.ClientBrokerPlaintext,
		} {
			in.ClusterName = new(fmt.Sprintf("auth-%d-%s", mask, enc))
			in.ClientAuthentication = &types.ClientAuthentication{
				Unauthenticated: &types.Unauthenticated{
					Enabled: new(mask&1 != 0),
				},
				Tls: &types.Tls{Enabled: new(mask&2 != 0)},
				Sasl: &types.Sasl{
					Scram: &types.Scram{Enabled: new(mask&4 != 0)},
					Iam:   &types.Iam{Enabled: new(mask&8 != 0)},
				},
			}

			in.EncryptionInfo = &types.EncryptionInfo{
				EncryptionInTransit: &types.EncryptionInTransit{ClientBroker: enc},
			}

			created, err := c.CreateCluster(ctx, in)
			invalid := mask == 0 || enc == types.ClientBrokerPlaintext && mask&14 != 0 ||
				mask&12 != 0 && enc != types.ClientBrokerTls
			if invalid {
				mskStatus(t, err, "BadRequestException", 400)
				continue
			}

			if err != nil {
				t.Fatal(err)
			}

			b, err := c.GetBootstrapBrokers(ctx, &sdk.GetBootstrapBrokersInput{ClusterArn: created.ClusterArn})
			if err != nil {
				t.Fatal(err)
			}

			for _, check := range []struct {
				value   *string
				port    string
				enabled bool
			}{
				{b.BootstrapBrokerString, "9092", mask&3 != 0 && enc != types.ClientBrokerTls},
				{b.BootstrapBrokerStringTls, "9094", mask&3 != 0 && enc != types.ClientBrokerPlaintext},
				{b.BootstrapBrokerStringSaslScram, "9096", mask&4 != 0},
				{b.BootstrapBrokerStringSaslIam, "9098", mask&8 != 0},
			} {
				if (check.value != nil) != check.enabled {
					t.Fatalf("mask %d %s: %+v", mask, enc, b)
				}

				if check.enabled &&
					(!strings.Contains(*check.value, ".kafka.us-east-1.amazonaws.com:"+check.port) ||
						len(strings.Split(*check.value, ",")) != 4) {
					t.Fatal(*check.value)
				}
			}

			nodes, err := c.ListNodes(
				ctx,
				&sdk.ListNodesInput{ClusterArn: created.ClusterArn, MaxResults: new(int32(2))},
			)
			if err != nil || len(nodes.NodeInfoList) != 2 || nodes.NextToken == nil {
				t.Fatalf("%+v %v", nodes, err)
			}

			page, err := c.ListNodes(
				ctx,
				&sdk.ListNodesInput{ClusterArn: created.ClusterArn, NextToken: nodes.NextToken},
			)
			if err != nil || len(page.NodeInfoList) != 2 || page.NextToken != nil {
				t.Fatalf("%+v %v", page, err)
			}

			_, err = c.ListNodes(ctx, &sdk.ListNodesInput{ClusterArn: created.ClusterArn, NextToken: new("bad")})
			mskStatus(t, err, "BadRequestException", 400)
			other := mskClient(s, "team-b", "us-east-1")
			_, err = other.GetBootstrapBrokers(ctx, &sdk.GetBootstrapBrokersInput{ClusterArn: created.ClusterArn})
			mskStatus(t, err, "NotFoundException", 404)
			_, err = other.ListNodes(ctx, &sdk.ListNodesInput{ClusterArn: created.ClusterArn})
			mskStatus(t, err, "NotFoundException", 404)
			if _, err = c.DeleteCluster(ctx, &sdk.DeleteClusterInput{ClusterArn: created.ClusterArn}); err != nil {
				t.Fatal(err)
			}

			_, err = c.GetBootstrapBrokers(ctx, &sdk.GetBootstrapBrokersInput{ClusterArn: created.ClusterArn})
			mskStatus(t, err, "NotFoundException", 404)
			_, err = c.ListNodes(ctx, &sdk.ListNodesInput{ClusterArn: created.ClusterArn})
			mskStatus(t, err, "NotFoundException", 404)
		}
	}
}

func TestMSKRecordsBrokersSQLiteRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	c := mskClient(s, "test", "us-east-1")
	in := mskInput(t, s, "us-east-1")
	in.ClientAuthentication = &types.ClientAuthentication{
		Sasl:            &types.Sasl{Iam: &types.Iam{Enabled: new(true)}, Scram: &types.Scram{Enabled: new(true)}},
		Unauthenticated: &types.Unauthenticated{Enabled: new(true)},
	}

	in.EncryptionInfo = &types.EncryptionInfo{
		EncryptionInTransit: &types.EncryptionInTransit{
			ClientBroker: types.ClientBrokerTls,
			InCluster:    new(true),
		},
	}

	created, err := c.CreateCluster(ctx, in)
	if err != nil {
		stop()
		t.Fatal(err)
	}

	before, err := c.GetBootstrapBrokers(ctx, &sdk.GetBootstrapBrokersInput{ClusterArn: created.ClusterArn})
	if err != nil {
		stop()
		t.Fatal(err)
	}

	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	defer stop()
	c = mskClient(s, "test", "us-east-1")
	after, err := c.GetBootstrapBrokers(ctx, &sdk.GetBootstrapBrokersInput{ClusterArn: created.ClusterArn})
	if err != nil {
		t.Fatal(err)
	}

	for i, pair := range [][2]*string{
		{before.BootstrapBrokerStringTls, after.BootstrapBrokerStringTls},
		{before.BootstrapBrokerStringSaslIam, after.BootstrapBrokerStringSaslIam},
		{before.BootstrapBrokerStringSaslScram, after.BootstrapBrokerStringSaslScram},
	} {
		if pair[0] == nil || pair[1] == nil || *pair[0] != *pair[1] {
			t.Fatalf("listener %d changed on restart", i)
		}
	}

	nodes, err := c.ListNodes(ctx, &sdk.ListNodesInput{ClusterArn: created.ClusterArn})
	if err != nil || len(nodes.NodeInfoList) != 4 || len(nodes.NodeInfoList[0].BrokerNodeInfo.Endpoints) != 3 {
		t.Fatalf("%+v %v", nodes, err)
	}

	described := mskDescribe(t, c, created.ClusterArn)
	if !*described.ClientAuthentication.Sasl.Iam.Enabled || !*described.EncryptionInfo.EncryptionInTransit.InCluster {
		t.Fatal("security metadata lost")
	}
}

func TestMSKAuthenticationDefaultsAndCAInference(t *testing.T) {
	s := server(t)
	c := mskClient(s, "test", "us-east-1")
	ctx := context.Background()
	in := mskInput(t, s, "us-east-1")
	created, err := c.CreateCluster(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	brokers, err := c.GetBootstrapBrokers(ctx, &sdk.GetBootstrapBrokersInput{ClusterArn: created.ClusterArn})
	if err != nil || brokers.BootstrapBrokerString != nil || brokers.BootstrapBrokerStringTls == nil {
		t.Fatalf("%+v %v", brokers, err)
	}

	in.ClusterName = new("inferred-mtls")
	in.ClientAuthentication = &types.ClientAuthentication{
		Tls: &types.Tls{
			CertificateAuthorityArnList: []string{
				"arn:aws:acm-pca:us-east-1:000000000000:certificate-authority/12345678-1234-1234-1234-123456789012",
			},
		},
	}

	in.EncryptionInfo = &types.EncryptionInfo{
		EncryptionInTransit: &types.EncryptionInTransit{ClientBroker: types.ClientBrokerTlsPlaintext},
	}

	created, err = c.CreateCluster(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	brokers, err = c.GetBootstrapBrokers(ctx, &sdk.GetBootstrapBrokersInput{ClusterArn: created.ClusterArn})
	if err != nil || brokers.BootstrapBrokerString == nil || brokers.BootstrapBrokerStringTls == nil {
		t.Fatalf("%+v %v", brokers, err)
	}

	in.ClusterName = new("disabled-ca")
	in.ClientAuthentication.Tls.Enabled = new(false)
	_, err = c.CreateCluster(ctx, in)
	mskStatus(t, err, "BadRequestException", 400)
	in.ClientAuthentication.Tls.Enabled = new(true)
	in.EncryptionInfo.EncryptionInTransit.InCluster = new(false)
	_, err = c.CreateCluster(ctx, in)
	mskStatus(t, err, "BadRequestException", 400)
}
