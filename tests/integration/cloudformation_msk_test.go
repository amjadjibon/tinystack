package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"testing"

	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	sdk "github.com/aws/aws-sdk-go-v2/service/kafka"
	"github.com/aws/aws-sdk-go-v2/service/kafka/types"
)

func mskCFProperties(in *sdk.CreateClusterInput, name string, tags map[string]string) map[string]any {
	tagObject := map[string]any{}
	for k, v := range tags {
		tagObject[k] = v
	}
	return map[string]any{
		"ClusterName":         name,
		"KafkaVersion":        "3.9.x",
		"NumberOfBrokerNodes": 4,
		"EnhancedMonitoring":  "PER_BROKER",
		"Tags":                tagObject,
		"BrokerNodeGroupInfo": map[string]any{
			"ClientSubnets":  in.BrokerNodeGroupInfo.ClientSubnets,
			"InstanceType":   "kafka.m5.xlarge",
			"SecurityGroups": in.BrokerNodeGroupInfo.SecurityGroups,
			"StorageInfo":    map[string]any{"EBSStorageInfo": map[string]any{"VolumeSize": 234}},
		},
	}
}

func mskTemplate(p map[string]any, later bool) string {
	resources := map[string]any{"Cluster": map[string]any{"Type": "AWS::MSK::Cluster", "Properties": p}}
	if later {
		resources["Later"] = map[string]any{
			"Type":       "AWS::MSK::Cluster",
			"DependsOn":  "Cluster",
			"Properties": map[string]any{"ClusterName": "invalid-later"},
		}
	}
	raw, _ := json.Marshal(map[string]any{"Resources": resources, "Outputs": map[string]any{
		"Ref": map[string]any{
			"Value": map[string]any{"Ref": "Cluster"},
		},
		"Arn":     map[string]any{"Value": map[string]any{"Fn::GetAtt": []any{"Cluster", "Arn"}}},
		"Version": map[string]any{"Value": map[string]any{"Fn::GetAtt": []any{"Cluster", "CurrentVersion"}}},
	}})
	return string(raw)
}

func updateMSKStack(
	t *testing.T,
	cf *cfn.Client,
	name string,
	p map[string]any,
	later bool,
	status cfntypes.StackStatus,
) cfntypes.Stack {
	t.Helper()
	if _, e := cf.UpdateStack(
		context.Background(),
		&cfn.UpdateStackInput{StackName: new(name), TemplateBody: new(mskTemplate(p, later))},
	); e != nil {
		t.Fatal(e)
	}
	return wantStatus(context.Background(), t, cf, name, status)
}

func sameMSKConfig(a, b *types.ClusterInfo) bool {
	aa, bb := *a, *b
	aa.CurrentVersion = nil
	bb.CurrentVersion = nil
	return reflect.DeepEqual(aa, bb)
}

func TestCloudFormationMSKLifecycleDefaultsLiveRollbackReplacementCollisionAndDelete(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	cf := cfnClient(s)
	c := mskClient(s, "test", "us-east-1")
	in := mskInput(t, s, "us-east-1")
	name := "msk-stack"
	p := mskCFProperties(in, "cf-cluster", map[string]string{"owned": "old"})
	if _, e := cf.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: new(name), TemplateBody: new(mskTemplate(p, false))},
	); e != nil {
		t.Fatal(e)
	}
	st := wantStatus(ctx, t, cf, name, cfntypes.StackStatusCreateComplete)
	outputs := efsStackOutputs(st)
	arn := outputs["Ref"]
	before := mskDescribe(t, c, new(arn))
	if outputs["Arn"] != arn || outputs["Version"] != *before.CurrentVersion || *before.ClusterName != "cf-cluster" ||
		before.EnhancedMonitoring != types.EnhancedMonitoringPerBroker {
		t.Fatal("Ref/GetAtt/read")
	}
	// Removing optional monitoring restores the documented DEFAULT; removing
	// template-owned tags leaves external tags untouched in the same atomic write.
	_, e := c.TagResource(
		ctx,
		&sdk.TagResourceInput{ResourceArn: new(arn), Tags: map[string]string{"external": "actual"}},
	)
	if e != nil {
		t.Fatal(e)
	}
	delete(p, "EnhancedMonitoring")
	delete(p, "Tags")
	updateMSKStack(t, cf, name, p, false, cfntypes.StackStatusUpdateComplete)
	cleared := mskDescribe(t, c, new(arn))
	if cleared.EnhancedMonitoring != types.EnhancedMonitoringDefault || len(cleared.Tags) != 1 ||
		cleared.Tags["external"] != "actual" ||
		!cleared.CreationTime.Equal(*before.CreationTime) {
		t.Fatal("defaults/ownership")
	}
	_, e = c.UpdateMonitoring(
		ctx,
		&sdk.UpdateMonitoringInput{
			ClusterArn:         new(arn),
			CurrentVersion:     cleared.CurrentVersion,
			EnhancedMonitoring: types.EnhancedMonitoringPerTopicPerPartition,
		},
	)
	if e != nil {
		t.Fatal(e)
	}
	actual := mskDescribe(t, c, new(arn))
	next := mskCFProperties(in, "cf-cluster", map[string]string{"owned": "new"})
	next["EnhancedMonitoring"] = "PER_TOPIC_PER_BROKER"
	updateMSKStack(t, cf, name, next, true, cfntypes.StackStatusUpdateRollbackComplete)
	restored := mskDescribe(t, c, new(arn))
	if !sameMSKConfig(actual, restored) || *actual.CurrentVersion == *restored.CurrentVersion {
		t.Fatal("live-state compensation or ABA revision")
	}
	_, e = c.UpdateMonitoring(
		ctx,
		&sdk.UpdateMonitoringInput{
			ClusterArn:         new(arn),
			CurrentVersion:     actual.CurrentVersion,
			EnhancedMonitoring: types.EnhancedMonitoringDefault,
		},
	)
	mskStatus(t, e, "BadRequestException", 400)
	st = updateMSKStack(t, cf, name, next, false, cfntypes.StackStatusUpdateComplete)
	updated := mskDescribe(t, c, new(arn))
	if efsStackOutputs(st)["Ref"] != arn || updated.EnhancedMonitoring != types.EnhancedMonitoringPerTopicPerBroker ||
		len(updated.Tags) != 2 ||
		updated.Tags["external"] != "actual" {
		t.Fatal("mutable update")
	}
	// A later failure after a name swap restores the original incarnation exactly.
	replacement := mskCFProperties(in, "cf-replacement", map[string]string{"replacement": "pending"})
	updateMSKStack(t, cf, name, replacement, true, cfntypes.StackStatusUpdateRollbackComplete)
	if !reflect.DeepEqual(updated, mskDescribe(t, c, new(arn))) {
		t.Fatal("replacement rollback changed original")
	}
	list, e := c.ListClusters(ctx, &sdk.ListClustersInput{})
	if e != nil || len(list.ClusterInfoList) != 1 {
		t.Fatal("failed replacement leaked", list, e)
	}
	// Collision must preserve the current incarnation, even for a named replacement.
	collision := *in
	collision.ClusterName = new("cf-collision")
	other, e := c.CreateCluster(ctx, &collision)
	if e != nil {
		t.Fatal(e)
	}
	colliding := mskCFProperties(in, "cf-collision", nil)
	updateMSKStack(t, cf, name, colliding, false, cfntypes.StackStatusUpdateRollbackComplete)
	if !reflect.DeepEqual(updated, mskDescribe(t, c, new(arn))) {
		t.Fatal("name collision damaged original")
	}
	if _, e = c.DeleteCluster(ctx, &sdk.DeleteClusterInput{ClusterArn: other.ClusterArn}); e != nil {
		t.Fatal(e)
	}
	st = updateMSKStack(t, cf, name, replacement, false, cfntypes.StackStatusUpdateComplete)
	newARN := efsStackOutputs(st)["Ref"]
	if newARN == arn || *mskDescribe(t, c, new(newARN)).ClusterName != "cf-replacement" {
		t.Fatal("successful replacement")
	}
	_, e = c.DescribeCluster(ctx, &sdk.DescribeClusterInput{ClusterArn: new(arn)})
	mskStatus(t, e, "NotFoundException", 404)
	if _, e = cf.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new(name)}); e != nil {
		t.Fatal(e)
	}
	wantStatus(ctx, t, cf, *st.StackId, cfntypes.StackStatusDeleteComplete)
	_, e = c.DescribeCluster(ctx, &sdk.DescribeClusterInput{ClusterArn: new(newARN)})
	mskStatus(t, e, "NotFoundException", 404)
}

func TestCloudFormationMSKStrictUnsupportedChangesAndAtomicTagLimit(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	cf := cfnClient(s)
	c := mskClient(s, "test", "us-east-1")
	in := mskInput(t, s, "us-east-1")
	name := "msk-boundaries"
	tags := map[string]string{}
	for i := range 50 {
		tags[fmt.Sprintf("old%02d", i)] = "v"
	}
	p := mskCFProperties(in, "cf-boundaries", tags)
	if _, e := cf.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: new(name), TemplateBody: new(mskTemplate(p, false))},
	); e != nil {
		t.Fatal(e)
	}
	st := wantStatus(ctx, t, cf, name, cfntypes.StackStatusCreateComplete)
	arn := efsStackOutputs(st)["Ref"]
	before := mskDescribe(t, c, new(arn))
	changes := map[string]func(map[string]any){
		"version": func(p map[string]any) { p["KafkaVersion"] = "3.8.x" },
		"nodes":   func(p map[string]any) { p["NumberOfBrokerNodes"] = 6 },
		"host":    func(p map[string]any) { p["BrokerNodeGroupInfo"].(map[string]any)["InstanceType"] = "kafka.m5.large" },
		"storage": func(p map[string]any) {
			storage := p["BrokerNodeGroupInfo"].(map[string]any)["StorageInfo"].(map[string]any)
			storage["EBSStorageInfo"].(map[string]any)["VolumeSize"] = 300
		},
		"remove-storage": func(p map[string]any) { delete(p["BrokerNodeGroupInfo"].(map[string]any), "StorageInfo") },
		"remove-group": func(p map[string]any) {
			delete(p["BrokerNodeGroupInfo"].(map[string]any), "SecurityGroups")
		},
		"remove-name":           func(p map[string]any) { delete(p, "ClusterName") },
		"remove-version":        func(p map[string]any) { delete(p, "KafkaVersion") },
		"remove-nodes":          func(p map[string]any) { delete(p, "NumberOfBrokerNodes") },
		"remove-broker":         func(p map[string]any) { delete(p, "BrokerNodeGroupInfo") },
		"tag-null":              func(p map[string]any) { p["Tags"] = nil },
		"tag-list":              func(p map[string]any) { p["Tags"] = []any{} },
		"tag-unsupported-value": func(p map[string]any) { p["Tags"] = map[string]any{"key": false} },
		"nested-false": func(p map[string]any) {
			p["BrokerNodeGroupInfo"].(map[string]any)["ConnectivityInfo"] = false
		},
		"nested-null": func(p map[string]any) { p["BrokerNodeGroupInfo"].(map[string]any)["ZoneIds"] = nil },
		"nested-empty": func(p map[string]any) {
			storage := p["BrokerNodeGroupInfo"].(map[string]any)["StorageInfo"].(map[string]any)
			storage["EBSStorageInfo"].(map[string]any)["ProvisionedThroughput"] = map[string]any{}
		},
	}
	for label, change := range changes {
		t.Run(label, func(t *testing.T) {
			candidate := mskCFProperties(in, "cf-boundaries", tags)
			candidate["EnhancedMonitoring"] = "DEFAULT"
			change(candidate)
			updateMSKStack(t, cf, name, candidate, false, cfntypes.StackStatusUpdateRollbackComplete)
			if !reflect.DeepEqual(before, mskDescribe(t, c, new(arn))) {
				t.Fatal("rejected change mutated record")
			}
		})
	}
	disjoint := map[string]string{}
	for i := range 50 {
		disjoint[fmt.Sprintf("new%02d", i)] = ""
	}
	next := mskCFProperties(in, "cf-boundaries", disjoint)
	next["EnhancedMonitoring"] = "DEFAULT"
	updateMSKStack(t, cf, name, next, false, cfntypes.StackStatusUpdateComplete)
	after := mskDescribe(t, c, new(arn))
	if len(after.Tags) != 50 || after.EnhancedMonitoring != types.EnhancedMonitoringDefault {
		t.Fatal("50 disjoint tag keys")
	}
	delete(disjoint, "new49")
	next = mskCFProperties(in, "cf-boundaries", disjoint)
	updateMSKStack(t, cf, name, next, false, cfntypes.StackStatusUpdateComplete)
	if _, e := c.TagResource(
		ctx,
		&sdk.TagResourceInput{ResourceArn: new(arn), Tags: map[string]string{"external": "must-stay"}},
	); e != nil {
		t.Fatal(e)
	}
	full := mskDescribe(t, c, new(arn))
	disjoint["new49"] = ""
	overflow := mskCFProperties(in, "cf-boundaries", disjoint)
	overflow["EnhancedMonitoring"] = "PER_TOPIC_PER_PARTITION"
	updateMSKStack(t, cf, name, overflow, false, cfntypes.StackStatusUpdateRollbackComplete)
	if !reflect.DeepEqual(full, mskDescribe(t, c, new(arn))) {
		t.Fatal("overflow partial monitoring/tags/revision")
	}
	remove := mskCFProperties(in, "cf-boundaries", nil)
	delete(remove, "Tags")
	updateMSKStack(t, cf, name, remove, false, cfntypes.StackStatusUpdateComplete)
	after = mskDescribe(t, c, new(arn))
	if len(after.Tags) != 1 || after.Tags["external"] != "must-stay" {
		t.Fatal("ownership remove")
	}
}

func TestCloudFormationMSKNetworkReplacementCreateRollbackAndUnsupportedAttributes(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	cf := cfnClient(s)
	c := mskClient(s, "test", "us-east-1")
	in := mskInput(t, s, "us-east-1")
	p := mskCFProperties(in, "cf-network", nil)
	if _, e := cf.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: new("msk-create-rollback"), TemplateBody: new(mskTemplate(p, true))},
	); e != nil {
		t.Fatal(e)
	}
	wantStatus(ctx, t, cf, "msk-create-rollback", cfntypes.StackStatusRollbackComplete)
	list, e := c.ListClusters(ctx, &sdk.ListClustersInput{})
	if e != nil || len(list.ClusterInfoList) != 0 {
		t.Fatal("create rollback leaked")
	}
	for _, attr := range []string{"BootstrapBrokers", "BootstrapBrokerString", "ZookeeperConnectString", "State"} {
		var doc map[string]any
		json.Unmarshal([]byte(mskTemplate(p, false)), &doc)
		doc["Outputs"].(map[string]any)["Arn"] = map[string]any{
			"Value": map[string]any{"Fn::GetAtt": []any{"Cluster", attr}},
		}
		raw, _ := json.Marshal(doc)
		_, e = cf.CreateStack(
			ctx,
			&cfn.CreateStackInput{StackName: new("msk-unsupported-attr"), TemplateBody: new(string(raw))},
		)
		code(t, e, "ValidationError")
	}
	for _, member := range []string{
		"LoggingInfo",
		"OpenMonitoring",
		"ClientAuthentication",
		"EncryptionInfo",
		"ConfigurationInfo",
	} {
		q := maps.Clone(p)
		q[member] = nil
		_, e = cf.CreateStack(
			ctx,
			&cfn.CreateStackInput{StackName: new("msk-unsupported-prop"), TemplateBody: new(mskTemplate(q, false))},
		)
		code(t, e, "ValidationError")
	}
	if _, e = cf.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: new("msk-network-stack"), TemplateBody: new(mskTemplate(p, false))},
	); e != nil {
		t.Fatal(e)
	}
	st := wantStatus(ctx, t, cf, "msk-network-stack", cfntypes.StackStatusCreateComplete)
	arn := efsStackOutputs(st)["Ref"]
	before := mskDescribe(t, c, new(arn))
	// Reordering selected subnets is an immutable change. Keeping the existing
	// name requires a colliding create-first attempt, preserving the old cluster.
	q := mskCFProperties(in, "cf-network", nil)
	ids := in.BrokerNodeGroupInfo.ClientSubnets
	q["BrokerNodeGroupInfo"].(map[string]any)["ClientSubnets"] = []string{ids[1], ids[0]}
	updateMSKStack(t, cf, "msk-network-stack", q, false, cfntypes.StackStatusUpdateRollbackComplete)
	if !reflect.DeepEqual(before, mskDescribe(t, c, new(arn))) {
		t.Fatal("same-name immutable change deleted original")
	}
	q["ClusterName"] = "cf-network-new"
	st = updateMSKStack(t, cf, "msk-network-stack", q, false, cfntypes.StackStatusUpdateComplete)
	newARN := efsStackOutputs(st)["Ref"]
	if newARN == arn {
		t.Fatal("network replacement reused identity")
	}
	if !reflect.DeepEqual(mskDescribe(t, c, new(newARN)).BrokerNodeGroupInfo.ClientSubnets, []string{ids[1], ids[0]}) {
		t.Fatal("network replacement lost config")
	}
}
