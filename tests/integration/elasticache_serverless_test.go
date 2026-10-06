package integration_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/elasticache"
	"github.com/aws/aws-sdk-go-v2/service/elasticache/types"
)

func TestElastiCacheServerless_Lifecycle(t *testing.T) {
	s := server(t)
	c := cacheClient(s)
	ctx := context.Background()

	name := "my-serverless-cache"

	// 1. Create serverless cache
	createOut, err := c.CreateServerlessCache(ctx, &elasticache.CreateServerlessCacheInput{
		ServerlessCacheName: aws.String(name),
		Engine:              aws.String("redis"),
		MajorEngineVersion:  aws.String("7"),
		Description:         aws.String("My serverless Redis"),
		CacheUsageLimits: &types.CacheUsageLimits{
			DataStorage: &types.DataStorage{
				Maximum: aws.Int32(10),
				Minimum: aws.Int32(1),
				Unit:    types.DataStorageUnitGb,
			},
		},
		Tags: []types.Tag{
			{Key: aws.String("env"), Value: aws.String("dev")},
		},
	})
	if err != nil {
		t.Fatalf("CreateServerlessCache failed: %v", err)
	}

	sc := createOut.ServerlessCache
	if aws.ToString(sc.ServerlessCacheName) != name {
		t.Fatalf("expected name %s, got %s", name, aws.ToString(sc.ServerlessCacheName))
	}
	if aws.ToString(sc.Status) != "available" {
		t.Fatalf("expected status available, got %s", aws.ToString(sc.Status))
	}
	if sc.Endpoint == nil || aws.ToString(sc.Endpoint.Address) == "" {
		t.Fatalf("expected non-empty endpoint address")
	}
	if sc.ReaderEndpoint == nil || aws.ToString(sc.ReaderEndpoint.Address) == "" {
		t.Fatalf("expected non-empty reader endpoint address")
	}
	if aws.ToInt32(sc.Endpoint.Port) != 6379 {
		t.Fatalf("expected endpoint port 6379, got %d", aws.ToInt32(sc.Endpoint.Port))
	}

	// 2. Describe serverless cache by name
	descOut, err := c.DescribeServerlessCaches(ctx, &elasticache.DescribeServerlessCachesInput{
		ServerlessCacheName: aws.String(name),
	})
	if err != nil {
		t.Fatalf("DescribeServerlessCaches failed: %v", err)
	}
	if len(descOut.ServerlessCaches) != 1 {
		t.Fatalf("expected 1 cache, got %d", len(descOut.ServerlessCaches))
	}

	// 3. Modify serverless cache
	modOut, err := c.ModifyServerlessCache(ctx, &elasticache.ModifyServerlessCacheInput{
		ServerlessCacheName: aws.String(name),
		Description:         aws.String("Updated description"),
	})
	if err != nil {
		t.Fatalf("ModifyServerlessCache failed: %v", err)
	}
	if aws.ToString(modOut.ServerlessCache.Description) != "Updated description" {
		t.Fatalf("expected description updated, got %s", aws.ToString(modOut.ServerlessCache.Description))
	}

	// 4. Delete serverless cache
	delOut, err := c.DeleteServerlessCache(ctx, &elasticache.DeleteServerlessCacheInput{
		ServerlessCacheName: aws.String(name),
	})
	if err != nil {
		t.Fatalf("DeleteServerlessCache failed: %v", err)
	}
	if aws.ToString(delOut.ServerlessCache.Status) != "deleting" {
		t.Fatalf("expected status deleting, got %s", aws.ToString(delOut.ServerlessCache.Status))
	}

	// 5. Verify gone
	_, err = c.DescribeServerlessCaches(ctx, &elasticache.DescribeServerlessCachesInput{
		ServerlessCacheName: aws.String(name),
	})
	if err == nil {
		t.Fatalf("expected error after delete, got nil")
	}
}

func TestElastiCacheServerless_Engines(t *testing.T) {
	s := server(t)
	c := cacheClient(s)
	ctx := context.Background()

	// Valkey
	vOut, err := c.CreateServerlessCache(ctx, &elasticache.CreateServerlessCacheInput{
		ServerlessCacheName: aws.String("valkey-serverless"),
		Engine:              aws.String("valkey"),
	})
	if err != nil {
		t.Fatalf("CreateServerlessCache valkey failed: %v", err)
	}
	if aws.ToInt32(vOut.ServerlessCache.Endpoint.Port) != 6379 {
		t.Fatalf("expected valkey port 6379, got %d", aws.ToInt32(vOut.ServerlessCache.Endpoint.Port))
	}

	// Memcached
	mOut, err := c.CreateServerlessCache(ctx, &elasticache.CreateServerlessCacheInput{
		ServerlessCacheName: aws.String("memcached-serverless"),
		Engine:              aws.String("memcached"),
	})
	if err != nil {
		t.Fatalf("CreateServerlessCache memcached failed: %v", err)
	}
	if aws.ToInt32(mOut.ServerlessCache.Endpoint.Port) != 11211 {
		t.Fatalf("expected memcached port 11211, got %d", aws.ToInt32(mOut.ServerlessCache.Endpoint.Port))
	}

	// List all
	listOut, err := c.DescribeServerlessCaches(ctx, &elasticache.DescribeServerlessCachesInput{})
	if err != nil {
		t.Fatalf("DescribeServerlessCaches list failed: %v", err)
	}
	if len(listOut.ServerlessCaches) != 2 {
		t.Fatalf("expected 2 serverless caches, got %d", len(listOut.ServerlessCaches))
	}
}

func TestElastiCacheServerless_Tags(t *testing.T) {
	s := server(t)
	c := cacheClient(s)
	ctx := context.Background()

	name := "tagged-serverless"
	createOut, err := c.CreateServerlessCache(ctx, &elasticache.CreateServerlessCacheInput{
		ServerlessCacheName: aws.String(name),
		Engine:              aws.String("redis"),
		Tags: []types.Tag{
			{Key: aws.String("env"), Value: aws.String("staging")},
		},
	})
	if err != nil {
		t.Fatalf("CreateServerlessCache failed: %v", err)
	}

	arn := aws.ToString(createOut.ServerlessCache.ARN)

	// List tags
	tagsOut, err := c.ListTagsForResource(ctx, &elasticache.ListTagsForResourceInput{
		ResourceName: aws.String(arn),
	})
	if err != nil {
		t.Fatalf("ListTagsForResource failed: %v", err)
	}
	if len(tagsOut.TagList) != 1 || aws.ToString(tagsOut.TagList[0].Key) != "env" {
		t.Fatalf("expected tag env=staging, got %+v", tagsOut.TagList)
	}

	// Add tag
	_, err = c.AddTagsToResource(ctx, &elasticache.AddTagsToResourceInput{
		ResourceName: aws.String(arn),
		Tags: []types.Tag{
			{Key: aws.String("project"), Value: aws.String("testing")},
		},
	})
	if err != nil {
		t.Fatalf("AddTagsToResource failed: %v", err)
	}

	// Remove tag
	_, err = c.RemoveTagsFromResource(ctx, &elasticache.RemoveTagsFromResourceInput{
		ResourceName: aws.String(arn),
		TagKeys:      []string{"env"},
	})
	if err != nil {
		t.Fatalf("RemoveTagsFromResource failed: %v", err)
	}

	tagsOut2, err := c.ListTagsForResource(ctx, &elasticache.ListTagsForResourceInput{
		ResourceName: aws.String(arn),
	})
	if err != nil {
		t.Fatalf("ListTagsForResource after remove failed: %v", err)
	}
	if len(tagsOut2.TagList) != 1 || aws.ToString(tagsOut2.TagList[0].Key) != "project" {
		t.Fatalf("expected tag project=testing, got %+v", tagsOut2.TagList)
	}
}

func TestElastiCacheServerless_Failures(t *testing.T) {
	s := server(t)
	c := cacheClient(s)
	ctx := context.Background()

	// 1. Invalid names
	invalidNames := []string{
		"-invalid",
		"invalid-",
		"inv--alid",
		"invalid!",
	}
	for _, inName := range invalidNames {
		_, err := c.CreateServerlessCache(ctx, &elasticache.CreateServerlessCacheInput{
			ServerlessCacheName: aws.String(inName),
			Engine:              aws.String("redis"),
		})
		if err == nil {
			t.Fatalf("expected error for invalid name %q, got nil", inName)
		}
	}

	// 2. Duplicate name
	validName := "dup-check"
	_, err := c.CreateServerlessCache(ctx, &elasticache.CreateServerlessCacheInput{
		ServerlessCacheName: aws.String(validName),
		Engine:              aws.String("redis"),
	})
	if err != nil {
		t.Fatalf("initial create failed: %v", err)
	}

	_, err = c.CreateServerlessCache(ctx, &elasticache.CreateServerlessCacheInput{
		ServerlessCacheName: aws.String(validName),
		Engine:              aws.String("redis"),
	})
	if err == nil {
		t.Fatalf("expected already exists error, got nil")
	}

	// 3. Not found
	_, err = c.DescribeServerlessCaches(ctx, &elasticache.DescribeServerlessCachesInput{
		ServerlessCacheName: aws.String("does-not-exist"),
	})
	if err == nil {
		t.Fatalf("expected not found error, got nil")
	}
}

func TestElastiCacheServerless_CloudFormation(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	cfnClient := cfn.New(cfn.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
		}),
		Retryer: aws.NopRetryer{},
	})

	stackName := "cfn-serverless-stack"
	template := `{
		"Resources": {
			"MyCache": {
				"Type": "AWS::ElastiCache::ServerlessCache",
				"Properties": {
					"ServerlessCacheName": "cfn-serverless",
					"Engine": "redis",
					"MajorEngineVersion": "7",
					"Description": "Initial CFN Serverless Cache"
				}
			}
		},
		"Outputs": {
			"Address": {
				"Value": {"Fn::GetAtt": ["MyCache", "Endpoint.Address"]}
			},
			"Port": {
				"Value": {"Fn::GetAtt": ["MyCache", "Endpoint.Port"]}
			},
			"Arn": {
				"Value": {"Fn::GetAtt": ["MyCache", "Arn"]}
			}
		}
	}`

	_, err := cfnClient.CreateStack(ctx, &cfn.CreateStackInput{
		StackName:    aws.String(stackName),
		TemplateBody: aws.String(template),
	})
	if err != nil {
		t.Fatalf("CreateStack failed: %v", err)
	}

	wantStatus(ctx, t, cfnClient, stackName, cfntypes.StackStatusCreateComplete)

	descStackOut, err := cfnClient.DescribeStacks(ctx, &cfn.DescribeStacksInput{
		StackName: aws.String(stackName),
	})
	if err != nil {
		t.Fatalf("DescribeStacks failed: %v", err)
	}
	if len(descStackOut.Stacks) != 1 {
		t.Fatalf("expected 1 stack, got %d", len(descStackOut.Stacks))
	}

	st := descStackOut.Stacks[0]
	var hasAddress, hasPort, hasArn bool
	for _, o := range st.Outputs {
		switch aws.ToString(o.OutputKey) {
		case "Address":
			hasAddress = true
			if aws.ToString(o.OutputValue) == "" {
				t.Fatalf("expected non-empty output address")
			}
		case "Port":
			hasPort = true
			if aws.ToString(o.OutputValue) != "6379" {
				t.Fatalf("expected port 6379, got %s", aws.ToString(o.OutputValue))
			}
		case "Arn":
			hasArn = true
			if aws.ToString(o.OutputValue) == "" {
				t.Fatalf("expected non-empty output arn")
			}
		}
	}
	if !hasAddress || !hasPort || !hasArn {
		t.Fatalf("missing required outputs from stack: %+v", st.Outputs)
	}

	// Update stack
	updateTemplate := `{
		"Resources": {
			"MyCache": {
				"Type": "AWS::ElastiCache::ServerlessCache",
				"Properties": {
					"ServerlessCacheName": "cfn-serverless",
					"Engine": "redis",
					"MajorEngineVersion": "7",
					"Description": "Updated CFN Serverless Cache"
				}
			}
		}
	}`
	_, err = cfnClient.UpdateStack(ctx, &cfn.UpdateStackInput{
		StackName:    aws.String(stackName),
		TemplateBody: aws.String(updateTemplate),
	})
	if err != nil {
		t.Fatalf("UpdateStack failed: %v", err)
	}
	wantStatus(ctx, t, cfnClient, stackName, cfntypes.StackStatusUpdateComplete)

	// Delete stack
	_, err = cfnClient.DeleteStack(ctx, &cfn.DeleteStackInput{
		StackName: aws.String(stackName),
	})
	if err != nil {
		t.Fatalf("DeleteStack failed: %v", err)
	}
}
