package integration_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/organizations"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/aws-sdk-go-v2/service/servicediscovery"
)

func TestOrganizationsDiscoveryPersistentRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	org := organizations.New(
		organizations.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")},
	)
	created, err := org.CreateOrganization(ctx, &organizations.CreateOrganizationInput{})
	if err != nil {
		t.Fatal(err)
	}
	discovery := servicediscovery.New(
		servicediscovery.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")},
	)
	in := &servicediscovery.CreateHttpNamespaceInput{Name: new("persistent"), CreatorRequestId: new("create-once")}
	namespace, err := discovery.CreateHttpNamespace(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := discovery.CreateHttpNamespace(ctx, in)
	if err != nil || aws.ToString(repeated.OperationId) != aws.ToString(namespace.OperationId) {
		t.Fatalf("replay: %v %+v", err, repeated)
	}
	op, err := discovery.GetOperation(ctx, &servicediscovery.GetOperationInput{OperationId: namespace.OperationId})
	if err != nil {
		t.Fatal(err)
	}
	nsID := op.Operation.Targets["NAMESPACE"]
	svcIn := &servicediscovery.CreateServiceInput{
		Name:             new("api"),
		NamespaceId:      new(nsID),
		CreatorRequestId: new("service-once"),
	}
	svc, err := discovery.CreateService(ctx, svcIn)
	if err != nil {
		t.Fatal(err)
	}
	svcReplay, err := discovery.CreateService(ctx, svcIn)
	if err != nil || aws.ToString(svcReplay.Service.Id) != aws.ToString(svc.Service.Id) {
		t.Fatalf("service replay: %v", err)
	}
	register := &servicediscovery.RegisterInstanceInput{
		ServiceId:        svc.Service.Id,
		InstanceId:       new("one"),
		CreatorRequestId: new("register-once"),
		Attributes:       map[string]string{"version": "1"},
	}
	instance, err := discovery.RegisterInstance(ctx, register)
	if err != nil {
		t.Fatal(err)
	}
	instReplay, err := discovery.RegisterInstance(ctx, register)
	if err != nil || aws.ToString(instance.OperationId) != aws.ToString(instReplay.OperationId) {
		t.Fatalf("instance replay: %v %+v", err, instReplay)
	}
	register.Attributes["version"] = "2"
	_, err = discovery.RegisterInstance(ctx, register)
	code(t, err, "DuplicateRequest")
	tagging := resourcegroupstaggingapi.New(
		resourcegroupstaggingapi.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")},
	)
	tagged, err := tagging.TagResources(
		ctx,
		&resourcegroupstaggingapi.TagResourcesInput{
			ResourceARNList: []string{*svc.Service.Arn},
			Tags:            map[string]string{"env": "restart"},
		},
	)
	if err != nil || len(tagged.FailedResourcesMap) != 0 {
		t.Fatalf("inventory tagging: %v %+v", err, tagged)
	}
	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	t.Cleanup(stop)
	org = organizations.New(
		organizations.Options{Region: "eu-west-1", BaseEndpoint: new(s.URL), Credentials: creds("test")},
	)
	visible, err := org.DescribeOrganization(ctx, &organizations.DescribeOrganizationInput{})
	if err != nil || aws.ToString(visible.Organization.Id) != aws.ToString(created.Organization.Id) {
		t.Fatalf("global restart: %v", err)
	}
	discovery = servicediscovery.New(
		servicediscovery.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")},
	)
	stored, err := discovery.GetInstance(
		ctx,
		&servicediscovery.GetInstanceInput{ServiceId: svc.Service.Id, InstanceId: new("one")},
	)
	if err != nil || stored.Instance.Attributes["version"] != "1" {
		t.Fatalf("instance after restart: %v %+v", err, stored)
	}
	tags, err := discovery.ListTagsForResource(
		ctx,
		&servicediscovery.ListTagsForResourceInput{ResourceARN: svc.Service.Arn},
	)
	if err != nil || len(tags.Tags) != 1 {
		t.Fatalf("tags after restart: %v %+v", err, tags)
	}
	repeated, err = discovery.CreateHttpNamespace(ctx, in)
	if err != nil || aws.ToString(repeated.OperationId) != aws.ToString(namespace.OperationId) {
		t.Fatalf("replay after restart: %v", err)
	}
}

func TestOrganizationsDiscoveryCloudFormation(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	org := organizations.New(
		organizations.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")},
	)
	_, err := org.CreateOrganization(ctx, &organizations.CreateOrganizationInput{})
	if err != nil {
		t.Fatal(err)
	}
	roots, err := org.ListRoots(ctx, &organizations.ListRootsInput{})
	if err != nil {
		t.Fatal(err)
	}
	template := fmt.Sprintf(`{"Resources":{
 "OU":{"Type":"AWS::Organizations::OrganizationalUnit","Properties":{"Name":"StackOU","ParentId":"%s",
 "Tags":[{"Key":"env","Value":"stack"}]}},
 "Policy":{"Type":"AWS::Organizations::Policy","Properties":{"Name":"StackPolicy","Description":"stack",
 "Type":"SERVICE_CONTROL_POLICY",
 "Content":{"Version":"2012-10-17","Statement":[]},"TargetIds":["%s"]}},
 "Namespace":{"Type":"AWS::ServiceDiscovery::HttpNamespace","Properties":{"Name":"stack.local"}},
 "Service":{"Type":"AWS::ServiceDiscovery::Service","Properties":{"Name":"api","NamespaceId":{"Ref":"Namespace"}}},
 "Instance":{"Type":"AWS::ServiceDiscovery::Instance","Properties":{"ServiceId":{"Ref":"Service"},"InstanceId":"one",
 "InstanceAttributes":{"version":"1"}}}
 },"Outputs":{"Service":{"Value":{"Ref":"Service"}}}}`, *roots.Roots[0].Id, *roots.Roots[0].Id)
	c := cloudformation.New(
		cloudformation.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")},
	)
	_, err = c.CreateStack(
		ctx,
		&cloudformation.CreateStackInput{StackName: new("discovery-orgs"), TemplateBody: new(template)},
	)
	if err != nil {
		t.Fatal(err)
	}
	stack := wantStatus(ctx, t, c, "discovery-orgs", cfntypes.StackStatusCreateComplete)

	discovery := servicediscovery.New(
		servicediscovery.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")},
	)
	instances, err := discovery.ListInstances(
		ctx,
		&servicediscovery.ListInstancesInput{ServiceId: stack.Outputs[0].OutputValue},
	)
	if err != nil || len(instances.Instances) != 1 {
		t.Fatalf("stack instances: %v %+v", err, instances)
	}
	_, err = c.DeleteStack(ctx, &cloudformation.DeleteStackInput{StackName: new("discovery-orgs")})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "Cloud Map stack deletion", func() bool {
		namespaces, listErr := discovery.ListNamespaces(ctx, &servicediscovery.ListNamespacesInput{})
		return listErr == nil && len(namespaces.Namespaces) == 0
	})
}
