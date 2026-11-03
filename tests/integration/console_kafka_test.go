package integration_test

import (
	"encoding/base64"
	"net/http"
	"testing"
)

func TestConsoleKafkaLifecycle(t *testing.T) {
	srv := server(t)
	const account, region = "000000000000", "us-east-1"
	call := func(service, operation string, input map[string]any) map[string]any {
		t.Helper()
		req := consoleLambdaRequest(t, srv, service, operation, account, region, input)
		return consoleResponse(t, srv, req, http.StatusOK)["output"].(map[string]any)
	}
	kafka := func(op string, input map[string]any) map[string]any { t.Helper(); return call("kafka", op, input) }
	var caps map[string]any
	if internal(t, srv, http.MethodGet, consoleActionsPath, &caps) != http.StatusOK || !consolePage(caps, "kafka") ||
		caps["kafka_runtime"] != "records" {
		t.Fatal(caps)
	}
	vpc := call("ec2", "CreateVpc", map[string]any{"CidrBlock": "10.31.0.0/16"})["Vpc"].(map[string]any)["VpcId"]
	subnet := func(cidr, zone string) any {
		t.Helper()
		out := call("ec2", "CreateSubnet", map[string]any{"VpcId": vpc, "CidrBlock": cidr, "AvailabilityZone": zone})

		return out["Subnet"].(map[string]any)["SubnetId"]
	}
	subnets := []any{subnet("10.31.0.0/24", "us-east-1a"), subnet("10.31.1.0/24", "us-east-1b")}
	group := call("ec2", "CreateSecurityGroup", map[string]any{
		"VpcId": vpc, "GroupName": "msk", "Description": "MSK console test",
	})["GroupId"]
	created := kafka("CreateCluster", map[string]any{
		"ClusterName":         "console-msk",
		"KafkaVersion":        "3.9.x",
		"NumberOfBrokerNodes": 2,
		"BrokerNodeGroupInfo": map[string]any{
			"ClientSubnets":  subnets,
			"SecurityGroups": []any{group},
			"InstanceType":   "kafka.m5.large",
			"StorageInfo":    map[string]any{"EbsStorageInfo": map[string]any{"VolumeSize": 100}},
		},
		"ClientAuthentication": map[string]any{"Unauthenticated": map[string]any{"Enabled": true}},
		"EncryptionInfo": map[string]any{
			"EncryptionInTransit": map[string]any{"ClientBroker": "TLS", "InCluster": true},
		},
		"Tags": map[string]any{"env": "test"},
	})
	arn := created["ClusterArn"]
	input := map[string]any{"ClusterArn": arn}
	if len(kafka("ListClusters", map[string]any{})["ClusterInfoList"].([]any)) != 1 {
		t.Fatal("missing cluster")
	}
	c := kafka("DescribeCluster", input)["ClusterInfo"].(map[string]any)
	oldVersion := c["CurrentVersion"]
	kafka(
		"UpdateMonitoring",
		map[string]any{"ClusterArn": arn, "CurrentVersion": oldVersion, "EnhancedMonitoring": "PER_BROKER"},
	)
	c = kafka("DescribeCluster", input)["ClusterInfo"].(map[string]any)
	if c["EnhancedMonitoring"] != "PER_BROKER" || c["CurrentVersion"] == oldVersion || c["CreationTime"] == nil {
		t.Fatal(c)
	}
	if kafka("GetBootstrapBrokers", input)["BootstrapBrokerStringTls"] == nil {
		t.Fatal("missing bootstrap brokers")
	}
	nodes := kafka("ListNodes", map[string]any{"ClusterArn": arn, "MaxResults": 1})
	if len(nodes["NodeInfoList"].([]any)) != 1 || nodes["NextToken"] == nil {
		t.Fatal(nodes)
	}
	if len(
		kafka("ListNodes", map[string]any{"ClusterArn": arn, "NextToken": nodes["NextToken"]})["NodeInfoList"].([]any),
	) != 1 {
		t.Fatal("missing next node")
	}
	kafka("TagResource", map[string]any{"ResourceArn": arn, "Tags": map[string]any{"team": "core"}})
	kafka("UntagResource", map[string]any{"ResourceArn": arn, "TagKeys": []any{"env"}})
	tags := kafka("ListTagsForResource", map[string]any{"ResourceArn": arn})["Tags"].(map[string]any)
	if tags["team"] != "core" || tags["env"] != nil {
		t.Fatal(tags)
	}
	config := kafka("CreateConfiguration", map[string]any{
		"Name":             "console-config",
		"ServerProperties": base64.StdEncoding.EncodeToString([]byte("auto.create.topics.enable=true")),
	})
	configARN := config["Arn"]
	if len(kafka("ListConfigurations", map[string]any{})["Configurations"].([]any)) != 1 {
		t.Fatal("configuration not listed")
	}
	kafka("DescribeConfiguration", map[string]any{"Arn": configARN})
	kafka("UpdateConfiguration", map[string]any{
		"Arn":              configARN,
		"ServerProperties": base64.StdEncoding.EncodeToString([]byte("auto.create.topics.enable=false")),
	})
	revisions := kafka("ListConfigurationRevisions", map[string]any{"Arn": configARN})
	if len(revisions["Revisions"].([]any)) != 2 {
		t.Fatal(revisions)
	}
	text := kafka("DescribeConfigurationRevision", map[string]any{"Arn": configARN, "Revision": 1})
	if text["ServerProperties"] != base64.StdEncoding.EncodeToString([]byte("auto.create.topics.enable=true")) {
		t.Fatal(text)
	}
	kafka("DeleteConfiguration", map[string]any{"Arn": configARN})
	if len(kafka("ListClustersV2", map[string]any{})["ClusterInfoList"].([]any)) != 1 {
		t.Fatal("V2 cluster missing")
	}
	kafka("DescribeClusterV2", input)
	denied := consoleResponse(
		t,
		srv,
		consoleLambdaRequest(t, srv, "kafka", "ListClusterOperations", account, region, map[string]any{}),
		http.StatusBadRequest,
	)
	if denied["code"] != "UnknownOperationException" {
		t.Fatal(denied)
	}
	kafka("DeleteCluster", map[string]any{"ClusterArn": arn, "CurrentVersion": c["CurrentVersion"]})
	if len(kafka("ListClusters", map[string]any{})["ClusterInfoList"].([]any)) != 0 {
		t.Fatal("cluster retained")
	}
}
