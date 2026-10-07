package integration_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iot"
	iottypes "github.com/aws/aws-sdk-go-v2/service/iot/types"
	"github.com/aws/aws-sdk-go-v2/service/iotdataplane"
)

func TestIoTFleetIndexing(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := iotClient(s, "test")
	data := dataClient(s, "test")
	_, err := c.SearchIndex(ctx, &iot.SearchIndexInput{QueryString: aws.String("thingName:*")})
	wantCode(t, err, "ResourceNotFoundException")

	_, err = c.UpdateIndexingConfiguration(ctx, &iot.UpdateIndexingConfigurationInput{
		ThingIndexingConfiguration: &iottypes.ThingIndexingConfiguration{
			ThingIndexingMode:       iottypes.ThingIndexingModeRegistry,
			NamedShadowIndexingMode: iottypes.NamedShadowIndexingModeOn,
		},
	})
	wantCode(t, err, "InvalidRequestException")
	if _, err = c.UpdateIndexingConfiguration(ctx, &iot.UpdateIndexingConfigurationInput{
		ThingIndexingConfiguration: &iottypes.ThingIndexingConfiguration{
			ThingIndexingMode:             iottypes.ThingIndexingModeRegistryAndShadow,
			ThingConnectivityIndexingMode: iottypes.ThingConnectivityIndexingModeStatus,
			NamedShadowIndexingMode:       iottypes.NamedShadowIndexingModeOn,
			Filter:                        &iottypes.IndexingFilter{NamedShadowNames: []string{"config"}},
		},
		ThingGroupIndexingConfiguration: &iottypes.ThingGroupIndexingConfiguration{
			ThingGroupIndexingMode: iottypes.ThingGroupIndexingModeOn,
		},
	}); err != nil {
		t.Fatal(err)
	}

	config, err := c.GetIndexingConfiguration(ctx, &iot.GetIndexingConfigurationInput{})
	if err != nil ||
		config.ThingIndexingConfiguration.ThingIndexingMode != iottypes.ThingIndexingModeRegistryAndShadow ||
		len(config.ThingIndexingConfiguration.ManagedFields) == 0 {
		t.Fatalf("config = %+v, %v", config, err)
	}

	index, err := c.DescribeIndex(ctx, &iot.DescribeIndexInput{IndexName: aws.String("AWS_Things")})
	if err != nil || aws.ToString(index.Schema) != "REGISTRY_AND_SHADOW_AND_CONNECTIVITY_STATUS" {
		t.Fatalf("index = %+v, %v", index, err)
	}

	indices, err := c.ListIndices(ctx, &iot.ListIndicesInput{})
	if err != nil || len(indices.IndexNames) != 2 {
		t.Fatalf("indices = %+v, %v", indices, err)
	}

	if _, err = c.CreateThingGroup(ctx, &iot.CreateThingGroupInput{
		ThingGroupName:       aws.String("kitchen"),
		ThingGroupProperties: &iottypes.ThingGroupProperties{ThingGroupDescription: aws.String("the kitchen")},
	}); err != nil {
		t.Fatal(err)
	}

	for name, floor := range map[string]string{"lamp-1": "3", "lamp-2": "5", "sensor-1": "3"} {
		if _, err = c.CreateThing(ctx, &iot.CreateThingInput{
			ThingName:        aws.String(name),
			AttributePayload: &iottypes.AttributePayload{Attributes: map[string]string{"floor": floor}},
		}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err = c.AddThingToThingGroup(ctx, &iot.AddThingToThingGroupInput{
		ThingName: aws.String("lamp-2"), ThingGroupName: aws.String("kitchen"),
	}); err != nil {
		t.Fatal(err)
	}

	for name, body := range map[string]string{
		"":       `{"state":{"reported":{"power":"on","level":7},"desired":{"level":9}}}`,
		"config": `{"state":{"reported":{"mode":"eco"}}}`,
	} {
		in := &iotdataplane.UpdateThingShadowInput{ThingName: aws.String("lamp-1"), Payload: []byte(body)}
		if name != "" {
			in.ShadowName = aws.String(name)
		}

		if _, err = data.UpdateThingShadow(ctx, in); err != nil {
			t.Fatal(err)
		}
	}

	connected(t, mqttURL(s), "lamp-2")
	search := func(query string) []string {
		t.Helper()
		out, searchErr := c.SearchIndex(ctx, &iot.SearchIndexInput{QueryString: aws.String(query)})
		if searchErr != nil {
			t.Fatalf("%s: %v", query, searchErr)
		}

		var names []string
		for _, d := range out.Things {
			names = append(names, aws.ToString(d.ThingName))
		}

		slices.Sort(names)
		return names
	}

	for query, want := range map[string]string{
		"thingName:lamp*":                                                 "lamp-1,lamp-2",
		"attributes.floor>4":                                              "lamp-2",
		"shadow.reported.level:[5 TO 8]":                                  "lamp-1",
		"shadow.reported.level:{7 TO 8]":                                  "",
		"shadow.hasDelta:true":                                            "lamp-1",
		"thingGroupNames:kitchen AND NOT attributes.floor:3":              "lamp-2",
		"connectivity.connected:true":                                     "lamp-2",
		"shadow.name.config.reported.mode:eco":                            "lamp-1",
		"(attributes.floor:3 OR thingGroupNames:kitchen) thingName:lamp*": "lamp-1,lamp-2",
		`thingName:"sensor-1"`:                                            "sensor-1",
	} {
		if got := strings.Join(search(query), ","); got != want {
			t.Errorf("%s = %q, want %q", query, got, want)
		}
	}

	shadowed, err := c.SearchIndex(ctx, &iot.SearchIndexInput{QueryString: aws.String("thingName:lamp-1")})
	if err != nil || len(shadowed.Things) != 1 ||
		!strings.Contains(aws.ToString(shadowed.Things[0].Shadow), `"power":"on"`) ||
		aws.ToBool(shadowed.Things[0].Connectivity.Connected) {
		t.Fatalf("lamp-1 document = %+v, %v", shadowed, err)
	}

	for _, query := range []string{"thingName:(", "thingName", "AND thingName:x", "level:[1 TO"} {
		_, err = c.SearchIndex(ctx, &iot.SearchIndexInput{QueryString: aws.String(query)})
		wantCode(t, err, "InvalidQueryException")
	}

	groups, err := c.SearchIndex(ctx, &iot.SearchIndexInput{
		IndexName: aws.String("AWS_ThingGroups"), QueryString: aws.String("thingGroupName:kit*"),
	})
	if err != nil || len(groups.ThingGroups) != 1 ||
		aws.ToString(groups.ThingGroups[0].ThingGroupDescription) != "the kitchen" {
		t.Fatalf("groups = %+v, %v", groups, err)
	}
}
