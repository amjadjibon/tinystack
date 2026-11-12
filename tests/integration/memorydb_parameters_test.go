package integration_test

import (
	"context"
	"testing"

	sdk "github.com/aws/aws-sdk-go-v2/service/memorydb"
	"github.com/aws/aws-sdk-go-v2/service/memorydb/types"
)

func TestMemoryDBParameterGroups(t *testing.T) {
	s := server(t)
	c := memoryDBClient(s, "test", "us-east-1")
	ctx := context.Background()
	name := new("custom-redis")
	created, err := c.CreateParameterGroup(ctx, &sdk.CreateParameterGroupInput{
		ParameterGroupName: name, Family: new("memorydb_redis7"), Description: new("custom"),
		Tags: []types.Tag{{Key: new("team"), Value: new("cache")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateParameterGroup(
		ctx,
		&sdk.CreateParameterGroupInput{ParameterGroupName: name, Family: new("memorydb_redis7")},
	)
	wantMemoryDBError(t, err, "ParameterGroupAlreadyExistsFault")
	tags, err := c.ListTags(ctx, &sdk.ListTagsInput{ResourceArn: created.ParameterGroup.ARN})
	if err != nil || len(tags.TagList) != 1 {
		t.Fatal(tags, err)
	}

	for _, tc := range []struct{ name, value, code string }{
		{"timeout", "30", ""},
		{"maxmemory-policy", "allkeys-lru", ""},
		{"timeout", "1", "InvalidParameterValueException"},
		{"unknown", "1", "InvalidParameterValueException"},
		{"appendonly", "yes", "InvalidParameterValueException"},
	} {
		_, err = c.UpdateParameterGroup(ctx, &sdk.UpdateParameterGroupInput{
			ParameterGroupName: name,
			ParameterNameValues: []types.ParameterNameValue{
				{ParameterName: new(tc.name), ParameterValue: new(tc.value)},
			},
		})
		if tc.code != "" {
			wantMemoryDBError(t, err, tc.code)
		} else if err != nil {
			t.Fatal(err)
		}
	}

	tooMany := make([]types.ParameterNameValue, 21)
	for i := range tooMany {
		tooMany[i] = types.ParameterNameValue{ParameterName: new("timeout"), ParameterValue: new("30")}
	}

	_, err = c.UpdateParameterGroup(
		ctx,
		&sdk.UpdateParameterGroupInput{ParameterGroupName: name, ParameterNameValues: tooMany},
	)
	wantMemoryDBError(t, err, "InvalidParameterValueException")
	_, err = c.ResetParameterGroup(
		ctx,
		&sdk.ResetParameterGroupInput{
			ParameterGroupName: name,
			AllParameters:      true,
			ParameterNames:     []string{"timeout"},
		},
	)
	wantMemoryDBError(t, err, "InvalidParameterCombinationException")
	values := map[string]string{}
	var token *string
	for {
		page, e := c.DescribeParameters(
			ctx,
			&sdk.DescribeParametersInput{ParameterGroupName: name, MaxResults: new(int32(3)), NextToken: token},
		)
		if e != nil {
			t.Fatal(e)
		}

		for _, p := range page.Parameters {
			values[*p.Name] = *p.Value
		}

		token = page.NextToken
		if token == nil {
			break
		}
	}

	if values["timeout"] != "30" || values["maxmemory-policy"] != "allkeys-lru" {
		t.Fatal(values)
	}

	_, err = c.ResetParameterGroup(
		ctx,
		&sdk.ResetParameterGroupInput{ParameterGroupName: name, ParameterNames: []string{"timeout"}},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateCluster(ctx, &sdk.CreateClusterInput{
		ClusterName: new("custom-cluster"), NodeType: new("db.r6g.large"),
		ACLName: new("open-access"), Engine: new("redis"), EngineVersion: new("7.0"), ParameterGroupName: name,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteParameterGroup(ctx, &sdk.DeleteParameterGroupInput{ParameterGroupName: name})
	wantMemoryDBError(t, err, "InvalidParameterGroupStateFault")
	_, err = c.UpdateCluster(ctx, &sdk.UpdateClusterInput{ClusterName: new("custom-cluster"), Engine: new("valkey")})
	wantMemoryDBError(t, err, "InvalidParameterCombinationException")
	_, err = c.UpdateCluster(
		ctx,
		&sdk.UpdateClusterInput{ClusterName: new("custom-cluster"), ParameterGroupName: new("default.memorydb-redis7")},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.ResetParameterGroup(
		ctx,
		&sdk.ResetParameterGroupInput{ParameterGroupName: name, AllParameters: true},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteParameterGroup(ctx, &sdk.DeleteParameterGroupInput{ParameterGroupName: name})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DescribeParameterGroups(ctx, &sdk.DescribeParameterGroupsInput{ParameterGroupName: name})
	wantMemoryDBError(t, err, "ParameterGroupNotFoundFault")
	_, err = c.UpdateParameterGroup(ctx, &sdk.UpdateParameterGroupInput{
		ParameterGroupName:  new("default.memorydb-redis7"),
		ParameterNameValues: []types.ParameterNameValue{{ParameterName: new("timeout"), ParameterValue: new("30")}},
	})
	wantMemoryDBError(t, err, "InvalidParameterGroupStateFault")
}
