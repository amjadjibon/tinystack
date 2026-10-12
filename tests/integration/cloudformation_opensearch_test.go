package integration_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	sdk "github.com/aws/aws-sdk-go-v2/service/opensearch"
)

func openSearchTemplate(instanceCount int, envTag string) string {
	raw, _ := json.Marshal(map[string]any{
		"Resources": map[string]any{
			"MyDomain": map[string]any{
				"Type": "AWS::OpenSearchService::Domain",
				"Properties": map[string]any{
					"DomainName":    "cfn-test-domain",
					"EngineVersion": "OpenSearch_2.11",
					"ClusterConfig": map[string]any{
						"InstanceCount": instanceCount,
					},
					"EBSOptions": map[string]any{
						"EBSEnabled": true,
						"VolumeType": "gp2",
						"VolumeSize": 10,
					},
					"Tags": []any{
						map[string]any{"Key": "Env", "Value": envTag},
					},
				},
			},
		},
		"Outputs": map[string]any{
			"DomainRef": map[string]any{"Value": map[string]any{"Ref": "MyDomain"}},
			"Arn":       map[string]any{"Value": map[string]any{"Fn::GetAtt": []any{"MyDomain", "Arn"}}},
			"DomainArn": map[string]any{"Value": map[string]any{"Fn::GetAtt": []any{"MyDomain", "DomainArn"}}},
			"DomainEndpoint": map[string]any{
				"Value": map[string]any{"Fn::GetAtt": []any{"MyDomain", "DomainEndpoint"}},
			},
			"Id": map[string]any{"Value": map[string]any{"Fn::GetAtt": []any{"MyDomain", "Id"}}},
		},
	})
	return string(raw)
}

func TestCloudFormationOpenSearchLifecycle(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	cf := cfnClient(s)
	osC := opensearchClient(s, "test", "us-east-1")

	// 1. Create Stack
	_, err := cf.CreateStack(
		ctx,
		&cfn.CreateStackInput{
			StackName:    aws.String("opensearch-stack"),
			TemplateBody: aws.String(openSearchTemplate(1, "Dev")),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	st := wantStatus(ctx, t, cf, "opensearch-stack", cfntypes.StackStatusCreateComplete)
	outputs := outputsOf(&st)

	if outputs["DomainRef"] != "cfn-test-domain" {
		t.Fatalf("unexpected DomainRef: %s", outputs["DomainRef"])
	}
	if outputs["Arn"] == "" || outputs["Arn"] != outputs["DomainArn"] ||
		!strings.HasPrefix(outputs["Arn"], "arn:aws:es:us-east-1:") {
		t.Fatalf("unexpected Arn/DomainArn outputs: %+v", outputs)
	}
	if !strings.Contains(outputs["DomainEndpoint"], "search-cfn-test-domain-") {
		t.Fatalf("unexpected DomainEndpoint: %s", outputs["DomainEndpoint"])
	}
	if !strings.HasSuffix(outputs["Id"], "/cfn-test-domain") {
		t.Fatalf("unexpected Id: %s", outputs["Id"])
	}

	// Verify domain in OpenSearch service
	desc, err := osC.DescribeDomain(ctx, &sdk.DescribeDomainInput{
		DomainName: aws.String("cfn-test-domain"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if *desc.DomainStatus.ClusterConfig.InstanceCount != 1 {
		t.Fatalf("expected instance count 1, got %d", *desc.DomainStatus.ClusterConfig.InstanceCount)
	}

	tags, err := osC.ListTags(ctx, &sdk.ListTagsInput{
		ARN: desc.DomainStatus.ARN,
	})
	if err != nil || len(tags.TagList) != 1 || aws.ToString(tags.TagList[0].Value) != "Dev" {
		t.Fatalf("unexpected tags: %+v (err: %v)", tags, err)
	}

	// 2. Update Stack
	_, err = cf.UpdateStack(
		ctx,
		&cfn.UpdateStackInput{
			StackName:    aws.String("opensearch-stack"),
			TemplateBody: aws.String(openSearchTemplate(3, "Prod")),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	st = wantStatus(ctx, t, cf, "opensearch-stack", cfntypes.StackStatusUpdateComplete)
	updatedOutputs := outputsOf(&st)
	if updatedOutputs["DomainRef"] != outputs["DomainRef"] {
		t.Fatalf("domain was replaced unexpectedly: %s vs %s", updatedOutputs["DomainRef"], outputs["DomainRef"])
	}

	// Verify updated properties and tags
	desc, err = osC.DescribeDomain(ctx, &sdk.DescribeDomainInput{
		DomainName: aws.String("cfn-test-domain"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if *desc.DomainStatus.ClusterConfig.InstanceCount != 3 {
		t.Fatalf("expected instance count 3, got %d", *desc.DomainStatus.ClusterConfig.InstanceCount)
	}

	tags, err = osC.ListTags(ctx, &sdk.ListTagsInput{
		ARN: desc.DomainStatus.ARN,
	})
	if err != nil || len(tags.TagList) != 1 || aws.ToString(tags.TagList[0].Value) != "Prod" {
		t.Fatalf("unexpected updated tags: %+v (err: %v)", tags, err)
	}

	// 3. Delete Stack
	_, err = cf.DeleteStack(ctx, &cfn.DeleteStackInput{
		StackName: aws.String("opensearch-stack"),
	})
	if err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, cf, *st.StackId, cfntypes.StackStatusDeleteComplete)

	// Verify domain is gone
	_, err = osC.DescribeDomain(ctx, &sdk.DescribeDomainInput{
		DomainName: aws.String("cfn-test-domain"),
	})
	if err == nil {
		t.Fatal("expected DescribeDomain to fail after stack deletion")
	}
}
