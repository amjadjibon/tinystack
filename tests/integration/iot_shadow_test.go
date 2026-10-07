package integration_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iotdataplane"
)

// shadowDoc decodes a shadow response.
func shadowDoc(t *testing.T, payload []byte) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(payload, &doc); err != nil {
		t.Fatalf("shadow payload %s: %v", payload, err)
	}

	return doc
}

// at reads a path of object fields from a decoded document.
func at(doc map[string]any, path string) any {
	var v any = doc
	for _, k := range strings.Split(path, ".") {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}

		v = m[k]
	}

	return v
}

func TestIoTShadows(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	data := dataClient(s, "test")
	watcher := connected(t, mqttURL(s), "watcher")
	watcher.subscribe("$aws/things/lamp/shadow/#")

	// The data plane creates the classic shadow with a desired state, and
	// devices hear the delta.
	up, err := data.UpdateThingShadow(ctx, &iotdataplane.UpdateThingShadowInput{
		ThingName: aws.String("lamp"),
		Payload:   []byte(`{"state":{"desired":{"color":"red","level":{"min":1,"max":5}}},"clientToken":"t1"}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	accepted := shadowDoc(t, up.Payload)
	if accepted["version"] != 1.0 || accepted["clientToken"] != "t1" ||
		at(accepted, "metadata.desired.color.timestamp") == nil {
		t.Fatalf("accepted = %v", accepted)
	}

	delta := shadowDoc(t, watcher.message("$aws/things/lamp/shadow/update/delta").Payload)
	if at(delta, "state.color") != "red" || at(delta, "state.level.max") != 5.0 || delta["version"] != 1.0 {
		t.Fatalf("delta = %v", delta)
	}

	// The device reports over MQTT; the delta shrinks to what still differs.
	device := connected(t, mqttURL(s), "lamp")
	device.subscribe("$aws/things/lamp/shadow/get/accepted", "$aws/things/lamp/shadow/update/rejected")
	if code := device.publish("$aws/things/lamp/shadow/update",
		`{"state":{"reported":{"color":"red","level":{"min":1,"max":3}}},"version":1,"clientToken":"t2"}`,
		false); code != 0 {
		t.Fatalf("puback 0x%x", code)
	}

	accepted = shadowDoc(t, watcher.message("$aws/things/lamp/shadow/update/accepted").Payload)
	if accepted["version"] != 2.0 || accepted["clientToken"] != "t2" {
		t.Fatalf("accepted = %v", accepted)
	}

	documents := shadowDoc(t, watcher.message("$aws/things/lamp/shadow/update/documents").Payload)
	if at(documents, "previous.version") != 1.0 || at(documents, "current.version") != 2.0 ||
		at(documents, "current.state.reported.color") != "red" {
		t.Fatalf("documents = %v", documents)
	}

	if code := device.publish("$aws/things/lamp/shadow/get", `{"clientToken":"t3"}`, false); code != 0 {
		t.Fatalf("puback 0x%x", code)
	}

	got := shadowDoc(t, device.message("$aws/things/lamp/shadow/get/accepted").Payload)
	if got["clientToken"] != "t3" || at(got, "state.delta.level.max") != 5.0 || at(got, "state.delta.color") != nil ||
		at(got, "state.delta.level.min") != nil || at(got, "metadata.reported.level.max.timestamp") == nil {
		t.Fatalf("get = %v", got)
	}

	// A stale version is rejected, over MQTT and through the data plane.
	if code := device.publish("$aws/things/lamp/shadow/update",
		`{"state":{"reported":{"color":"blue"}},"version":1,"clientToken":"t4"}`, false); code != 0 {
		t.Fatalf("puback 0x%x", code)
	}

	rejected := shadowDoc(t, device.message("$aws/things/lamp/shadow/update/rejected").Payload)
	if rejected["code"] != 409.0 || rejected["clientToken"] != "t4" {
		t.Fatalf("rejected = %v", rejected)
	}

	_, err = data.UpdateThingShadow(ctx, &iotdataplane.UpdateThingShadowInput{
		ThingName: aws.String("lamp"), Payload: []byte(`{"state":{"reported":{"x":1}},"version":7}`),
	})
	wantCode(t, err, "ConflictException")

	// Null deletes a field.
	if _, err = data.UpdateThingShadow(ctx, &iotdataplane.UpdateThingShadowInput{
		ThingName: aws.String("lamp"), Payload: []byte(`{"state":{"desired":{"level":null}}}`),
	}); err != nil {
		t.Fatal(err)
	}

	shadow, err := data.GetThingShadow(ctx, &iotdataplane.GetThingShadowInput{ThingName: aws.String("lamp")})
	if err != nil {
		t.Fatal(err)
	}

	got = shadowDoc(t, shadow.Payload)
	if at(got, "state.desired.level") != nil || at(got, "state.delta") != nil || got["version"] != 3.0 {
		t.Fatalf("shadow = %v", got)
	}

	// Invalid requests fail.
	for payload, code := range map[string]string{
		`not json`:                  "InvalidRequestException",
		`{"version":1}`:             "InvalidRequestException",
		`{"state":{"other":{}}}`:    "InvalidRequestException",
		`{"state":{"desired":"x"}}`: "InvalidRequestException",
		`{"state":{"desired":{"a":{"b":{"c":{"d":{"e":{"f":{"g":1}}}}}}}}}`: "InvalidRequestException",
		`{"state":{"desired":{"big":"` + strings.Repeat("x", 9000) + `"}}}`: "RequestEntityTooLargeException",
	} {
		_, updateErr := data.UpdateThingShadow(ctx, &iotdataplane.UpdateThingShadowInput{
			ThingName: aws.String("lamp"), Payload: []byte(payload),
		})
		wantCode(t, updateErr, code)
	}

	// Named shadows are separate, and listed by name.
	for _, name := range []string{"config", "battery"} {
		if _, err = data.UpdateThingShadow(ctx, &iotdataplane.UpdateThingShadowInput{
			ThingName: aws.String("lamp"), ShadowName: aws.String(name),
			Payload: []byte(`{"state":{"reported":{"ok":true}}}`),
		}); err != nil {
			t.Fatal(err)
		}
	}

	list, err := data.ListNamedShadowsForThing(ctx, &iotdataplane.ListNamedShadowsForThingInput{
		ThingName: aws.String("lamp"),
	})
	if err != nil || strings.Join(list.Results, ",") != "battery,config" || list.Timestamp == 0 {
		t.Fatalf("named shadows = %+v, %v", list, err)
	}

	// Deleting a shadow keeps its version for the next one.
	deleted, err := data.DeleteThingShadow(ctx, &iotdataplane.DeleteThingShadowInput{
		ThingName: aws.String("lamp"), ShadowName: aws.String("config"),
	})
	if err != nil || shadowDoc(t, deleted.Payload)["version"] != 1.0 {
		t.Fatalf("deleted = %+v, %v", deleted, err)
	}

	_, err = data.GetThingShadow(ctx, &iotdataplane.GetThingShadowInput{
		ThingName: aws.String("lamp"), ShadowName: aws.String("config"),
	})
	wantCode(t, err, "ResourceNotFoundException")
	_, err = data.DeleteThingShadow(ctx, &iotdataplane.DeleteThingShadowInput{
		ThingName: aws.String("lamp"), ShadowName: aws.String("config"),
	})
	wantCode(t, err, "ResourceNotFoundException")
	up, err = data.UpdateThingShadow(ctx, &iotdataplane.UpdateThingShadowInput{
		ThingName: aws.String("lamp"), ShadowName: aws.String("config"),
		Payload: []byte(`{"state":{"reported":{"ok":false}}}`),
	})
	if err != nil || shadowDoc(t, up.Payload)["version"] != 2.0 {
		t.Fatalf("recreated = %+v, %v", up, err)
	}

	list, err = data.ListNamedShadowsForThing(ctx, &iotdataplane.ListNamedShadowsForThingInput{
		ThingName: aws.String("lamp"), PageSize: aws.Int32(1),
	})
	if err != nil || len(list.Results) != 1 || list.NextToken == nil {
		t.Fatalf("first page = %+v, %v", list, err)
	}

	// The data plane can publish shadow requests too, and the reply goes to
	// the shadow's topics.
	if _, err := data.Publish(ctx, &iotdataplane.PublishInput{
		Topic: aws.String("$aws/things/lamp/shadow/get"), Payload: []byte(`{}`),
	}); err != nil {
		t.Fatal(err)
	}

	if got := shadowDoc(t, device.message("$aws/things/lamp/shadow/get/accepted").Payload); got["version"] != 3.0 {
		t.Fatalf("get = %v", got)
	}
}
