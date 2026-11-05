package integration_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/iot"
	iottypes "github.com/aws/aws-sdk-go-v2/service/iot/types"
	sdk "github.com/aws/aws-sdk-go-v2/service/iotwireless"
	"github.com/aws/aws-sdk-go-v2/service/iotwireless/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/smithy-go"
)

func wirelessClient(s *httptest.Server) *sdk.Client {
	return sdk.New(
		sdk.Options{
			Region:       "us-east-1",
			BaseEndpoint: new(s.URL),
			Credentials:  creds("test"),
			Retryer:      aws.NopRetryer{},
		},
	)
}

func wirelessOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wirelessError(t *testing.T, err error, code string) {
	t.Helper()
	var api smithy.APIError
	if !errors.As(err, &api) || api.ErrorCode() != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

type wirelessFixture struct {
	c                                        *sdk.Client
	device, profile, serviceProfile, gateway *string
	role, destination                        string
}

func wirelessSetup(t *testing.T, s *httptest.Server) wirelessFixture {
	t.Helper()
	ctx := context.Background()
	c := wirelessClient(s)
	role, err := iamClient(s, "test", "us-east-1").CreateRole(ctx, &iam.CreateRoleInput{
		RoleName: new("wireless"), AssumeRolePolicyDocument: new(`{"Version":"2012-10-17","Statement":[]}`),
	})
	wirelessOK(t, err)
	destination, err := c.CreateDestination(ctx, &sdk.CreateDestinationInput{
		Name: new("mqtt"), Expression: new("wireless/uplink"), ExpressionType: types.ExpressionTypeMqttTopic,
		RoleArn: role.Role.Arn, Tags: []types.Tag{{Key: new("team"), Value: new("wireless")}},
	})
	wirelessOK(t, err)
	profile, err := c.CreateDeviceProfile(ctx, &sdk.CreateDeviceProfileInput{
		Name:    new("device-profile"),
		LoRaWAN: &types.LoRaWANDeviceProfile{RfRegion: new("EU868"), MacVersion: new("1.0.3"), SupportsJoin: new(true)},
	})
	wirelessOK(t, err)
	sp, err := c.CreateServiceProfile(ctx, &sdk.CreateServiceProfileInput{
		Name:    new("service-profile"),
		LoRaWAN: &types.LoRaWANServiceProfile{AddGwMetadata: true},
	})
	wirelessOK(t, err)
	device, err := c.CreateWirelessDevice(ctx, &sdk.CreateWirelessDeviceInput{
		Type: types.WirelessDeviceTypeLoRaWAN, Name: new("device"), DestinationName: destination.Name,
		LoRaWAN: &types.LoRaWANDevice{
			DevEui: new("0011223344556677"), DeviceProfileId: profile.Id, ServiceProfileId: sp.Id,
			OtaaV1_0_x: &types.OtaaV1_0_x{AppEui: new("0011223344556677"), AppKey: new(strings.Repeat("a", 32))},
		},
	})
	wirelessOK(t, err)
	gateway, err := c.CreateWirelessGateway(ctx, &sdk.CreateWirelessGatewayInput{
		Name:    new("gateway"),
		LoRaWAN: &types.LoRaWANGateway{GatewayEui: new("1122334455667788"), RfRegion: new("EU868")},
	})
	wirelessOK(t, err)
	return wirelessFixture{
		c: c, role: *role.Role.Arn, destination: *destination.Name,
		device: device.Id, profile: profile.Id, serviceProfile: sp.Id, gateway: gateway.Id,
	}
}

func wirelessWait(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if ready() {
			return
		}

		select {
		case <-deadline.C:
			t.Fatal("wireless progression timed out")
		case <-ticker.C:
		}
	}
}

func TestWirelessResourcesAndUplink(t *testing.T) {
	t.Parallel()
	s := server(t)
	f := wirelessSetup(t, s)
	c, ctx := f.c, context.Background()
	things := iotClient(s, "test")
	thing, err := things.CreateThing(ctx, &iot.CreateThingInput{ThingName: new("wireless-device")})
	wirelessOK(t, err)
	_, err = c.AssociateWirelessDeviceWithThing(
		ctx,
		&sdk.AssociateWirelessDeviceWithThingInput{Id: f.device, ThingArn: thing.ThingArn},
	)
	wirelessOK(t, err)
	d, err := c.GetWirelessDevice(
		ctx,
		&sdk.GetWirelessDeviceInput{
			Identifier:     new("wireless-device"),
			IdentifierType: types.WirelessDeviceIdTypeThingName,
		},
	)
	wirelessOK(t, err)
	if *d.Id != *f.device {
		t.Fatalf("thing lookup: %+v", d)
	}

	d, err = c.GetWirelessDevice(
		ctx,
		&sdk.GetWirelessDeviceInput{
			Identifier:     new("0011223344556677"),
			IdentifierType: types.WirelessDeviceIdTypeDevEui,
		},
	)
	wirelessOK(t, err)
	if *d.LoRaWAN.DeviceProfileId != *f.profile {
		t.Fatalf("profile: %+v", d)
	}

	gwThing, err := things.CreateThing(ctx, &iot.CreateThingInput{ThingName: new("wireless-gateway")})
	wirelessOK(t, err)
	_, err = c.AssociateWirelessGatewayWithThing(
		ctx,
		&sdk.AssociateWirelessGatewayWithThingInput{Id: f.gateway, ThingArn: gwThing.ThingArn},
	)
	wirelessOK(t, err)
	cert, err := things.CreateKeysAndCertificate(ctx, &iot.CreateKeysAndCertificateInput{SetAsActive: true})
	wirelessOK(t, err)
	_, err = c.AssociateWirelessGatewayWithCertificate(
		ctx,
		&sdk.AssociateWirelessGatewayWithCertificateInput{Id: f.gateway, IotCertificateId: cert.CertificateId},
	)
	wirelessOK(t, err)
	gotCert, err := c.GetWirelessGatewayCertificate(ctx, &sdk.GetWirelessGatewayCertificateInput{Id: f.gateway})
	wirelessOK(t, err)
	if *gotCert.IotCertificateId != *cert.CertificateId {
		t.Fatal("certificate association lost")
	}

	g, err := c.GetWirelessGateway(
		ctx,
		&sdk.GetWirelessGatewayInput{
			Identifier:     new("1122334455667788"),
			IdentifierType: types.WirelessGatewayIdTypeGatewayEui,
		},
	)
	wirelessOK(t, err)
	if *g.ThingName != "wireless-gateway" {
		t.Fatalf("gateway association: %+v", g)
	}

	_, err = c.UpdateWirelessGateway(
		ctx,
		&sdk.UpdateWirelessGatewayInput{Id: f.gateway, Description: new("updated"), MaxEirp: new(float32(12))},
	)
	wirelessOK(t, err)
	_, err = c.UpdateWirelessDevice(
		ctx,
		&sdk.UpdateWirelessDeviceInput{
			Id:      f.device,
			Name:    new("updated"),
			LoRaWAN: &types.LoRaWANUpdateDevice{ServiceProfileId: f.serviceProfile},
		},
	)
	wirelessOK(t, err)
	watcher := connected(t, mqttURL(s), "wireless-watcher")
	watcher.subscribe("wireless/uplink")
	_, err = c.TestWirelessDevice(ctx, &sdk.TestWirelessDeviceInput{Id: f.device})
	wirelessOK(t, err)
	wirelessUplink(t, watcher.message("wireless/uplink").Payload, *f.device)
	stats, err := c.GetWirelessDeviceStatistics(ctx, &sdk.GetWirelessDeviceStatisticsInput{WirelessDeviceId: f.device})
	wirelessOK(t, err)
	if stats.LastUplinkReceivedAt == nil || len(stats.LoRaWAN.Gateways) != 1 {
		t.Fatalf("statistics: %+v", stats)
	}

	gatewayStats, err := c.GetWirelessGatewayStatistics(
		ctx,
		&sdk.GetWirelessGatewayStatisticsInput{WirelessGatewayId: f.gateway},
	)
	wirelessOK(t, err)
	if gatewayStats.ConnectionStatus != types.ConnectionStatusConnected {
		t.Fatalf("gateway statistics: %+v", gatewayStats)
	}

	q := sqsClient(s, "test")
	queue, err := q.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: new("wireless")})
	wirelessOK(t, err)
	createRule(t, things, "wireless_rule", "SELECT * FROM 'unused'", []iottypes.Action{sqsAction(*queue.QueueUrl)}, nil)
	_, err = c.CreateDestination(
		ctx,
		&sdk.CreateDestinationInput{
			Name:           new("rule"),
			Expression:     new("wireless_rule"),
			ExpressionType: types.ExpressionTypeRuleName,
			RoleArn:        new(f.role),
		},
	)
	wirelessOK(t, err)
	_, err = c.UpdateWirelessDevice(ctx, &sdk.UpdateWirelessDeviceInput{Id: f.device, DestinationName: new("rule")})
	wirelessOK(t, err)
	_, err = c.TestWirelessDevice(ctx, &sdk.TestWirelessDeviceInput{Id: f.device})
	wirelessOK(t, err)
	wirelessUplink(t, []byte(awaitBody(t, q, *queue.QueueUrl)), *f.device)
	_, err = c.UpdateDestination(ctx, &sdk.UpdateDestinationInput{Name: new("rule"), Description: new("updated")})
	wirelessOK(t, err)
	dest, err := c.GetDestination(ctx, &sdk.GetDestinationInput{Name: new("rule")})
	wirelessOK(t, err)
	if *dest.Description != "updated" {
		t.Fatal("destination update lost")
	}

	profile, err := c.GetDeviceProfile(ctx, &sdk.GetDeviceProfileInput{Id: f.profile})
	wirelessOK(t, err)
	if *profile.LoRaWAN.MacVersion != "1.0.3" {
		t.Fatal("profile configuration lost")
	}

	sp, err := c.GetServiceProfile(ctx, &sdk.GetServiceProfileInput{Id: f.serviceProfile})
	wirelessOK(t, err)
	if !sp.LoRaWAN.AddGwMetadata {
		t.Fatal("service profile configuration lost")
	}

	wirelessTags(t, c, []*string{dest.Arn, profile.Arn, sp.Arn, d.Arn, g.Arn})
	devices, err := c.ListWirelessDevices(
		ctx,
		&sdk.ListWirelessDevicesInput{
			DeviceProfileId:    f.profile,
			ServiceProfileId:   f.serviceProfile,
			DestinationName:    new("rule"),
			WirelessDeviceType: types.WirelessDeviceTypeLoRaWAN,
		},
	)
	wirelessOK(t, err)
	if len(devices.WirelessDeviceList) != 1 {
		t.Fatal("device filters")
	}

	gateways, err := c.ListWirelessGateways(ctx, &sdk.ListWirelessGatewaysInput{})
	wirelessOK(t, err)
	if len(gateways.WirelessGatewayList) != 1 {
		t.Fatal("gateway list")
	}

	_, err = c.DisassociateWirelessDeviceFromThing(ctx, &sdk.DisassociateWirelessDeviceFromThingInput{Id: f.device})
	wirelessOK(t, err)
	_, err = c.DisassociateWirelessGatewayFromThing(ctx, &sdk.DisassociateWirelessGatewayFromThingInput{Id: f.gateway})
	wirelessOK(t, err)
	_, err = c.DisassociateWirelessGatewayFromCertificate(
		ctx,
		&sdk.DisassociateWirelessGatewayFromCertificateInput{Id: f.gateway},
	)
	wirelessOK(t, err)
	_, err = c.DeleteWirelessDevice(ctx, &sdk.DeleteWirelessDeviceInput{Id: f.device})
	wirelessOK(t, err)
	_, err = c.DeleteWirelessGateway(ctx, &sdk.DeleteWirelessGatewayInput{Id: f.gateway})
	wirelessOK(t, err)
	_, err = c.DeleteDeviceProfile(ctx, &sdk.DeleteDeviceProfileInput{Id: f.profile})
	wirelessOK(t, err)
	_, err = c.DeleteServiceProfile(ctx, &sdk.DeleteServiceProfileInput{Id: f.serviceProfile})
	wirelessOK(t, err)
	for _, name := range []string{"rule", "mqtt"} {
		_, err = c.DeleteDestination(ctx, &sdk.DeleteDestinationInput{Name: new(name)})
		wirelessOK(t, err)
	}
}

func wirelessUplink(t *testing.T, payload []byte, id string) {
	t.Helper()
	var got struct {
		WirelessDeviceId, PayloadData string
		WirelessMetadata              struct {
			LoRaWAN struct {
				DevEui   string
				Gateways []any
				FPort    int
			}
		}
	}

	wirelessOK(t, json.Unmarshal(payload, &got))
	if got.WirelessDeviceId != id || got.PayloadData != base64.StdEncoding.EncodeToString([]byte("Test")) ||
		got.WirelessMetadata.LoRaWAN.DevEui != "0011223344556677" || got.WirelessMetadata.LoRaWAN.FPort != 1 {
		t.Fatalf("uplink: %s", payload)
	}
}

func wirelessTags(t *testing.T, c *sdk.Client, arns []*string) {
	t.Helper()
	ctx := context.Background()
	for _, arn := range arns {
		_, err := c.TagResource(
			ctx,
			&sdk.TagResourceInput{ResourceArn: arn, Tags: []types.Tag{{Key: new("test"), Value: new("value")}}},
		)
		wirelessOK(t, err)
		got, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: arn})
		wirelessOK(t, err)
		found := false
		for _, tag := range got.Tags {
			if *tag.Key == "test" && *tag.Value == "value" {
				found = true
			}
		}

		if !found {
			t.Fatalf("tag missing for %s", *arn)
		}

		_, err = c.UntagResource(ctx, &sdk.UntagResourceInput{ResourceArn: arn, TagKeys: []string{"test"}})
		wirelessOK(t, err)
	}
}

func TestWirelessQueueGroupsFuotaAndRestart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	t.Cleanup(func() { stop() })
	f := wirelessSetup(t, s)
	ctx, c := context.Background(), f.c
	m1, err := c.SendDataToWirelessDevice(
		ctx,
		&sdk.SendDataToWirelessDeviceInput{
			Id:               f.device,
			PayloadData:      new("SGVsbG8="),
			TransmitMode:     new(int32(0)),
			WirelessMetadata: &types.WirelessMetadata{LoRaWAN: &types.LoRaWANSendDataToDevice{FPort: new(int32(2))}},
		},
	)
	wirelessOK(t, err)
	_, err = c.SendDataToWirelessDevice(
		ctx,
		&sdk.SendDataToWirelessDeviceInput{
			Id:               f.device,
			PayloadData:      new("V29ybGQ="),
			TransmitMode:     new(int32(1)),
			WirelessMetadata: &types.WirelessMetadata{LoRaWAN: &types.LoRaWANSendDataToDevice{FPort: new(int32(3))}},
		},
	)
	wirelessOK(t, err)
	page, err := c.ListQueuedMessages(ctx, &sdk.ListQueuedMessagesInput{Id: f.device, MaxResults: 1})
	wirelessOK(t, err)
	if len(page.DownlinkQueueMessagesList) != 1 || page.NextToken == nil {
		t.Fatal("queue pagination")
	}

	next, err := c.ListQueuedMessages(ctx, &sdk.ListQueuedMessagesInput{Id: f.device, NextToken: page.NextToken})
	wirelessOK(t, err)
	if len(next.DownlinkQueueMessagesList) != 1 || *next.DownlinkQueueMessagesList[0].LoRaWAN.FPort != 3 {
		t.Fatal("queue second page")
	}

	_, err = c.DeleteQueuedMessages(ctx, &sdk.DeleteQueuedMessagesInput{Id: f.device, MessageId: m1.MessageId})
	wirelessOK(t, err)
	group, err := c.CreateMulticastGroup(
		ctx,
		&sdk.CreateMulticastGroupInput{
			Name:    new("group"),
			LoRaWAN: &types.LoRaWANMulticast{RfRegion: types.SupportedRfRegionEu868, DlClass: types.DlClassClassC},
		},
	)
	wirelessOK(t, err)
	_, err = c.UpdateMulticastGroup(ctx, &sdk.UpdateMulticastGroupInput{Id: group.Id, Description: new("updated")})
	wirelessOK(t, err)
	_, err = c.AssociateWirelessDeviceWithMulticastGroup(
		ctx,
		&sdk.AssociateWirelessDeviceWithMulticastGroupInput{Id: group.Id, WirelessDeviceId: f.device},
	)
	wirelessOK(t, err)
	_, err = c.SendDataToMulticastGroup(
		ctx,
		&sdk.SendDataToMulticastGroupInput{
			Id:          group.Id,
			PayloadData: new("VGVzdA=="),
			WirelessMetadata: &types.MulticastWirelessMetadata{
				LoRaWAN: &types.LoRaWANMulticastMetadata{FPort: new(int32(2))},
			},
		},
	)
	wirelessOK(t, err)
	bucket := s3Client(s, "test", "us-east-1")
	_, err = bucket.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("wireless-firmware")})
	wirelessOK(t, err)
	_, err = bucket.PutObject(
		ctx,
		&s3.PutObjectInput{Bucket: new("wireless-firmware"), Key: new("image"), Body: strings.NewReader("firmware")},
	)
	wirelessOK(t, err)
	task, err := c.CreateFuotaTask(
		ctx,
		&sdk.CreateFuotaTaskInput{
			Name:                new("task"),
			FirmwareUpdateImage: new("s3://wireless-firmware/image"),
			FirmwareUpdateRole:  new(f.role),
			LoRaWAN:             &types.LoRaWANFuotaTask{RfRegion: types.SupportedRfRegionEu868},
		},
	)
	wirelessOK(t, err)
	_, err = c.UpdateFuotaTask(ctx, &sdk.UpdateFuotaTaskInput{Id: task.Id, Description: new("updated")})
	wirelessOK(t, err)
	_, err = c.AssociateWirelessDeviceWithFuotaTask(
		ctx,
		&sdk.AssociateWirelessDeviceWithFuotaTaskInput{Id: task.Id, WirelessDeviceId: f.device},
	)
	wirelessOK(t, err)
	_, err = c.AssociateMulticastGroupWithFuotaTask(
		ctx,
		&sdk.AssociateMulticastGroupWithFuotaTaskInput{Id: task.Id, MulticastGroupId: group.Id},
	)
	wirelessOK(t, err)
	groups, err := c.ListMulticastGroupsByFuotaTask(ctx, &sdk.ListMulticastGroupsByFuotaTaskInput{Id: task.Id})
	wirelessOK(t, err)
	if len(groups.MulticastGroupList) != 1 {
		t.Fatal("task group association")
	}

	_, err = c.DisassociateMulticastGroupFromFuotaTask(
		ctx,
		&sdk.DisassociateMulticastGroupFromFuotaTaskInput{Id: task.Id, MulticastGroupId: group.Id},
	)
	wirelessOK(t, err)
	_, err = c.AssociateMulticastGroupWithFuotaTask(
		ctx,
		&sdk.AssociateMulticastGroupWithFuotaTaskInput{Id: task.Id, MulticastGroupId: group.Id},
	)
	wirelessOK(t, err)
	_, err = c.DisassociateWirelessDeviceFromFuotaTask(
		ctx,
		&sdk.DisassociateWirelessDeviceFromFuotaTaskInput{Id: task.Id, WirelessDeviceId: f.device},
	)
	wirelessOK(t, err)
	_, err = c.AssociateWirelessDeviceWithFuotaTask(
		ctx,
		&sdk.AssociateWirelessDeviceWithFuotaTaskInput{Id: task.Id, WirelessDeviceId: f.device},
	)
	wirelessOK(t, err)
	wirelessTags(t, c, []*string{group.Arn, task.Arn})
	_, err = c.StartMulticastGroupSession(
		ctx,
		&sdk.StartMulticastGroupSessionInput{
			Id: group.Id,
			LoRaWAN: &types.LoRaWANMulticastSession{
				SessionTimeout: new(int32(60)),
				DlDr:           new(int32(3)),
				DlFreq:         new(int32(868300000)),
			},
		},
	)
	wirelessOK(t, err)
	_, err = c.StartFuotaTask(
		ctx,
		&sdk.StartFuotaTaskInput{
			Id:      task.Id,
			LoRaWAN: &types.LoRaWANStartFuotaTask{StartTime: new(time.Now().Add(500 * time.Millisecond))},
		},
	)
	wirelessOK(t, err)
	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	c = wirelessClient(s)
	wirelessWait(t, func() bool {
		out, e := c.GetFuotaTask(ctx, &sdk.GetFuotaTaskInput{Id: task.Id})
		wirelessOK(t, e)
		return out.Status == types.FuotaTaskStatusFuotaDone
	})
	result, err := c.ListWirelessDevices(
		ctx,
		&sdk.ListWirelessDevicesInput{FuotaTaskId: task.Id, MulticastGroupId: group.Id},
	)
	wirelessOK(t, err)
	if len(result.WirelessDeviceList) != 1 ||
		result.WirelessDeviceList[0].FuotaDeviceStatus != types.FuotaDeviceStatusSuccessful {
		t.Fatalf("device task status: %+v", result)
	}

	wirelessWait(t, func() bool {
		out, e := c.GetMulticastGroup(ctx, &sdk.GetMulticastGroupInput{Id: group.Id})
		wirelessOK(t, e)
		return *out.Status == "Session_Set"
	})
	session, err := c.GetMulticastGroupSession(ctx, &sdk.GetMulticastGroupSessionInput{Id: group.Id})
	wirelessOK(t, err)
	if session.LoRaWAN.SessionStartTime == nil {
		t.Fatal("session not persisted")
	}

	queued, err := c.ListQueuedMessages(ctx, &sdk.ListQueuedMessagesInput{Id: f.device})
	wirelessOK(t, err)
	if len(queued.DownlinkQueueMessagesList) != 1 {
		t.Fatal("queue not persisted")
	}

	_, err = c.DeleteQueuedMessages(ctx, &sdk.DeleteQueuedMessagesInput{Id: f.device, MessageId: new("*")})
	wirelessOK(t, err)
	_, err = c.CancelMulticastGroupSession(ctx, &sdk.CancelMulticastGroupSessionInput{Id: group.Id})
	wirelessOK(t, err)
	_, err = c.DisassociateWirelessDeviceFromMulticastGroup(
		ctx,
		&sdk.DisassociateWirelessDeviceFromMulticastGroupInput{Id: group.Id, WirelessDeviceId: f.device},
	)
	wirelessOK(t, err)
	_, err = c.DeleteMulticastGroup(ctx, &sdk.DeleteMulticastGroupInput{Id: group.Id})
	wirelessOK(t, err)
	_, err = c.DeleteFuotaTask(ctx, &sdk.DeleteFuotaTaskInput{Id: task.Id})
	wirelessOK(t, err)
}

func TestWirelessPaginationErrorsAndSettings(t *testing.T) {
	t.Parallel()
	s := server(t)
	f := wirelessSetup(t, s)
	c, ctx := f.c, context.Background()
	for i := range 2 {
		_, err := c.CreateDeviceProfile(
			ctx,
			&sdk.CreateDeviceProfileInput{
				Name:     new(fmt.Sprintf("extra-%d", i)),
				Sidewalk: &types.SidewalkCreateDeviceProfile{},
			},
		)
		wirelessOK(t, err)
		_, err = c.CreateServiceProfile(ctx, &sdk.CreateServiceProfileInput{Name: new(fmt.Sprintf("extra-%d", i))})
		wirelessOK(t, err)
		_, err = c.CreateDestination(
			ctx,
			&sdk.CreateDestinationInput{
				Name:           new(fmt.Sprintf("extra-%d", i)),
				Expression:     new("topic"),
				ExpressionType: types.ExpressionTypeMqttTopic,
				RoleArn:        new(f.role),
			},
		)
		wirelessOK(t, err)
	}

	profiles, err := c.ListDeviceProfiles(ctx, &sdk.ListDeviceProfilesInput{MaxResults: 1})
	wirelessOK(t, err)
	more, err := c.ListDeviceProfiles(ctx, &sdk.ListDeviceProfilesInput{NextToken: profiles.NextToken})
	wirelessOK(t, err)
	if len(profiles.DeviceProfileList) != 1 || len(more.DeviceProfileList) != 2 {
		t.Fatal("device profile pagination")
	}

	services, err := c.ListServiceProfiles(ctx, &sdk.ListServiceProfilesInput{MaxResults: 1})
	wirelessOK(t, err)
	moreServices, err := c.ListServiceProfiles(ctx, &sdk.ListServiceProfilesInput{NextToken: services.NextToken})
	wirelessOK(t, err)
	if len(moreServices.ServiceProfileList) != 2 {
		t.Fatal("service profile pagination")
	}

	destinations, err := c.ListDestinations(ctx, &sdk.ListDestinationsInput{MaxResults: 1})
	wirelessOK(t, err)
	moreDestinations, err := c.ListDestinations(ctx, &sdk.ListDestinationsInput{NextToken: destinations.NextToken})
	wirelessOK(t, err)
	if len(moreDestinations.DestinationList) != 2 {
		t.Fatal("destination pagination")
	}

	_, err = c.ListMulticastGroups(ctx, &sdk.ListMulticastGroupsInput{})
	wirelessOK(t, err)
	_, err = c.ListFuotaTasks(ctx, &sdk.ListFuotaTasksInput{})
	wirelessOK(t, err)
	_, err = c.CreateWirelessDevice(
		ctx,
		&sdk.CreateWirelessDeviceInput{
			Type:            types.WirelessDeviceTypeLoRaWAN,
			DestinationName: new("mqtt"),
			LoRaWAN: &types.LoRaWANDevice{
				DevEui:     new("0011223344556677"),
				OtaaV1_0_x: &types.OtaaV1_0_x{AppEui: new("0011223344556677"), AppKey: new(strings.Repeat("b", 32))},
			},
		},
	)
	wirelessError(t, err, "ConflictException")
	_, err = c.CreateWirelessGateway(
		ctx,
		&sdk.CreateWirelessGatewayInput{
			LoRaWAN: &types.LoRaWANGateway{GatewayEui: new("1122334455667788"), RfRegion: new("EU868")},
		},
	)
	wirelessError(t, err, "ConflictException")
	_, err = c.CreateDestination(
		ctx,
		&sdk.CreateDestinationInput{
			Name:           new("invalid"),
			Expression:     new("a/#"),
			ExpressionType: types.ExpressionTypeMqttTopic,
			RoleArn:        new(f.role),
		},
	)
	wirelessError(t, err, "ValidationException")
	_, err = c.CreateDestination(
		ctx,
		&sdk.CreateDestinationInput{
			Name:           new("invalid"),
			Expression:     new("missing"),
			ExpressionType: types.ExpressionTypeRuleName,
			RoleArn:        new(f.role),
		},
	)
	wirelessError(t, err, "ResourceNotFoundException")
	_, err = c.CreateDestination(
		ctx,
		&sdk.CreateDestinationInput{
			Name:           new("invalid"),
			Expression:     new("topic"),
			ExpressionType: types.ExpressionTypeMqttTopic,
			RoleArn:        new("arn:aws:iam::000000000000:role/missing"),
		},
	)
	wirelessError(t, err, "ValidationException")
	_, err = c.CreateWirelessDevice(
		ctx,
		&sdk.CreateWirelessDeviceInput{Type: types.WirelessDeviceTypeSidewalk, DestinationName: new("mqtt")},
	)
	wirelessError(t, err, "ValidationException")
	_, err = c.GetDestination(ctx, &sdk.GetDestinationInput{Name: new("missing")})
	wirelessError(t, err, "ResourceNotFoundException")
	_, err = c.ListWirelessDevices(ctx, &sdk.ListWirelessDevicesInput{NextToken: new("%invalid")})
	wirelessError(t, err, "ValidationException")
	_, err = c.DeleteDeviceProfile(ctx, &sdk.DeleteDeviceProfileInput{Id: f.profile})
	wirelessError(t, err, "ConflictException")
	_, err = c.CreateWirelessDevice(
		ctx,
		&sdk.CreateWirelessDeviceInput{
			Type:            types.WirelessDeviceTypeLoRaWAN,
			DestinationName: new("missing"),
			LoRaWAN: &types.LoRaWANDevice{
				DevEui:     new("8877665544332211"),
				OtaaV1_0_x: &types.OtaaV1_0_x{AppEui: new("0011223344556677"), AppKey: new(strings.Repeat("b", 32))},
			},
		},
	)
	wirelessError(t, err, "ResourceNotFoundException")
	_, err = c.AssociateWirelessDeviceWithThing(
		ctx,
		&sdk.AssociateWirelessDeviceWithThingInput{
			Id:       f.device,
			ThingArn: new("arn:aws:iot:us-east-1:000000000000:thing/missing"),
		},
	)
	wirelessError(t, err, "ResourceNotFoundException")
	_, err = c.AssociateWirelessGatewayWithCertificate(
		ctx,
		&sdk.AssociateWirelessGatewayWithCertificateInput{
			Id:               f.gateway,
			IotCertificateId: new(strings.Repeat("a", 64)),
		},
	)
	wirelessError(t, err, "ResourceNotFoundException")
	endpoint, err := c.GetServiceEndpoint(
		ctx,
		&sdk.GetServiceEndpointInput{ServiceType: types.WirelessGatewayServiceTypeCups},
	)
	wirelessOK(t, err)
	if *endpoint.ServiceEndpoint != s.URL {
		t.Fatalf("endpoint: %+v", endpoint)
	}

	_, err = c.UpdateLogLevelsByResourceTypes(
		ctx,
		&sdk.UpdateLogLevelsByResourceTypesInput{DefaultLogLevel: types.LogLevelInfo},
	)
	wirelessOK(t, err)
	levels, err := c.GetLogLevelsByResourceTypes(ctx, &sdk.GetLogLevelsByResourceTypesInput{})
	wirelessOK(t, err)
	if levels.DefaultLogLevel != types.LogLevelInfo {
		t.Fatal("log levels")
	}

	_, err = c.PutResourceLogLevel(
		ctx,
		&sdk.PutResourceLogLevelInput{
			ResourceIdentifier: f.device,
			ResourceType:       new("WirelessDevice"),
			LogLevel:           types.LogLevelError,
		},
	)
	wirelessOK(t, err)
	level, err := c.GetResourceLogLevel(
		ctx,
		&sdk.GetResourceLogLevelInput{ResourceIdentifier: f.device, ResourceType: new("WirelessDevice")},
	)
	wirelessOK(t, err)
	if level.LogLevel != types.LogLevelError {
		t.Fatal("resource log")
	}

	_, err = c.ResetResourceLogLevel(
		ctx,
		&sdk.ResetResourceLogLevelInput{ResourceIdentifier: f.device, ResourceType: new("WirelessDevice")},
	)
	wirelessOK(t, err)
	_, err = c.ResetAllResourceLogLevels(ctx, &sdk.ResetAllResourceLogLevelsInput{})
	wirelessOK(t, err)
}

func TestWirelessCloudFormation(t *testing.T) {
	t.Parallel()
	s := server(t)
	ctx := context.Background()
	f := wirelessSetup(t, s)
	bucket := s3Client(s, "test", "us-east-1")
	_, err := bucket.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("wireless-cfn-firmware")})
	wirelessOK(t, err)
	_, err = bucket.PutObject(
		ctx,
		&s3.PutObjectInput{Bucket: new("wireless-cfn-firmware"), Key: new("image"), Body: strings.NewReader("image")},
	)
	wirelessOK(t, err)
	template := func(value string) string {
		return fmt.Sprintf(`{"Resources":{
   "Destination":{"Type":"AWS::IoTWireless::Destination",
"Properties":{"Name":"stack-destination",
"Expression":"stack/topic",
"ExpressionType":"MqttTopic",
"RoleArn":%q,
"Description":%q,
"Tags":[{"Key":"team",
"Value":%q}]}},

   "Profile":{"Type":"AWS::IoTWireless::DeviceProfile",
"Properties":{"Name":"stack-profile",
"LoRaWAN":{"RfRegion":"EU868",
"SupportsJoin":true},
"Tags":[{"Key":"team",
"Value":%q}]}},

   "Service":{"Type":"AWS::IoTWireless::ServiceProfile",
"Properties":{"LoRaWAN":{"AddGwMetadata":true},
"Tags":[{"Key":"team",
"Value":%q}]}},

   "Group":{"Type":"AWS::IoTWireless::MulticastGroup",
"Properties":{"Name":"stack-group",
"Description":%q,
"LoRaWAN":{"RfRegion":"EU868",
"DlClass":"ClassC"},
"Tags":[{"Key":"team",
"Value":%q}]}},

   "Task":{"Type":"AWS::IoTWireless::FuotaTask",
"Properties":{"Name":"stack-task",
"Description":%q,
"FirmwareUpdateImage":"s3://wireless-cfn-firmware/image",
"FirmwareUpdateRole":%q,
"LoRaWAN":{"RfRegion":"EU868"},
"AssociateMulticastGroup":{"Ref":"Group"},
"Tags":[{"Key":"team",
"Value":%q}]}}
  },
"Outputs":{"Destination":{"Value":{"Ref":"Destination"}},
"Profile":{"Value":{"Ref":"Profile"}},
"Service":{"Value":{"Ref":"Service"}},
"Group":{"Value":{"Ref":"Group"}},
"Task":{"Value":{"Ref":"Task"}}}}`,
			f.role,
			value,
			value,
			value,
			value,
			value,
			value,
			value,
			f.role,
			value)
	}

	cf := cfnClient(s)
	_, err = cf.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: new("wireless-stack"), TemplateBody: new(template("before"))},
	)
	wirelessOK(t, err)
	initial := wantStatus(ctx, t, cf, "wireless-stack", cfntypes.StackStatusCreateComplete)
	ids := outputsOf(new(initial))
	_, err = cf.UpdateStack(
		ctx,
		&cfn.UpdateStackInput{StackName: new("wireless-stack"), TemplateBody: new(template("after"))},
	)
	wirelessOK(t, err)
	updated := wantStatus(ctx, t, cf, "wireless-stack", cfntypes.StackStatusUpdateComplete)
	for key, id := range outputsOf(new(updated)) {
		if id != ids[key] {
			t.Fatalf("mutable %s replaced", key)
		}
	}

	group, err := f.c.GetMulticastGroup(ctx, &sdk.GetMulticastGroupInput{Id: new(ids["Group"])})
	wirelessOK(t, err)
	if *group.Description != "after" {
		t.Fatal("group template update lost")
	}

	task, err := f.c.GetFuotaTask(ctx, &sdk.GetFuotaTaskInput{Id: new(ids["Task"])})
	wirelessOK(t, err)
	if *task.Description != "after" {
		t.Fatal("task template update lost")
	}

	_, err = cf.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("wireless-stack")})
	wirelessOK(t, err)
	wirelessWait(t, func() bool {
		_, e := cf.DescribeStacks(ctx, &cfn.DescribeStacksInput{StackName: new("wireless-stack")})
		return e != nil
	})
}

func TestWirelessActivationsAndDevicePagination(t *testing.T) {
	t.Parallel()
	s := server(t)
	f := wirelessSetup(t, s)
	ctx, c := context.Background(), f.c
	key := new(strings.Repeat("a", 32))
	radios := []*types.LoRaWANDevice{
		{AbpV1_0_x: &types.AbpV1_0_x{
			DevAddr: new("00112233"), FCntStart: new(int32(5)),
			SessionKeys: &types.SessionKeysAbpV1_0_x{AppSKey: key, NwkSKey: key},
		}},
		{AbpV1_1: &types.AbpV1_1{
			DevAddr: new("11223344"), FCntStart: new(int32(6)),
			SessionKeys: &types.SessionKeysAbpV1_1{AppSKey: key, FNwkSIntKey: key, NwkSEncKey: key, SNwkSIntKey: key},
		}},
		{OtaaV1_1: &types.OtaaV1_1{JoinEui: new("0011223344556677"), AppKey: key, NwkKey: key}},
	}

	for i, radio := range radios {
		radio.DevEui = new(fmt.Sprintf("aabbccddeeff00%02x", i))
		radio.DeviceProfileId, radio.ServiceProfileId = f.profile, f.serviceProfile
		in := &sdk.CreateWirelessDeviceInput{
			ClientRequestToken: new(fmt.Sprintf("repeat-%d", i)),
			Type:               types.WirelessDeviceTypeLoRaWAN, DestinationName: new("mqtt"), LoRaWAN: radio,
		}

		made, err := c.CreateWirelessDevice(ctx, in)
		wirelessOK(t, err)
		again, err := c.CreateWirelessDevice(ctx, in)
		wirelessOK(t, err)
		if *made.Id != *again.Id {
			t.Fatal("idempotent create produced a new device")
		}

		changed := *in
		changed.Name = new("changed request")
		_, err = c.CreateWirelessDevice(ctx, &changed)
		wantAccessAnalyzerError(t, err, "ConflictException")
		got, err := c.GetWirelessDevice(ctx, &sdk.GetWirelessDeviceInput{
			Identifier:     made.Id,
			IdentifierType: types.WirelessDeviceIdTypeWirelessDeviceId,
		})
		wirelessOK(t, err)
		if radio.AbpV1_0_x != nil {
			if *got.LoRaWAN.AbpV1_0_x.SessionKeys.AppSKey != *key {
				t.Fatal("ABP 1.0 keys lost")
			}

			_, err = c.UpdateWirelessDevice(ctx, &sdk.UpdateWirelessDeviceInput{
				Id:      made.Id,
				LoRaWAN: &types.LoRaWANUpdateDevice{AbpV1_0_x: &types.UpdateAbpV1_0_x{FCntStart: new(int32(10))}},
			})
			wirelessOK(t, err)
		}

		if radio.AbpV1_1 != nil {
			if *got.LoRaWAN.AbpV1_1.SessionKeys.FNwkSIntKey != *key {
				t.Fatal("ABP 1.1 keys lost")
			}

			_, err = c.UpdateWirelessDevice(ctx, &sdk.UpdateWirelessDeviceInput{
				Id:      made.Id,
				LoRaWAN: &types.LoRaWANUpdateDevice{AbpV1_1: &types.UpdateAbpV1_1{FCntStart: new(int32(11))}},
			})
			wirelessOK(t, err)
		}

		if radio.OtaaV1_1 != nil && *got.LoRaWAN.OtaaV1_1.NwkKey != *key {
			t.Fatal("OTAA 1.1 keys lost")
		}

		in.Name = new("changed")
		_, err = c.CreateWirelessDevice(ctx, in)
		wirelessError(t, err, "ConflictException")
	}

	var token *string
	seen := map[string]bool{}
	for {
		page, err := c.ListWirelessDevices(
			ctx,
			&sdk.ListWirelessDevicesInput{MaxResults: 1, NextToken: token, DeviceProfileId: f.profile},
		)
		wirelessOK(t, err)
		for _, d := range page.WirelessDeviceList {
			if seen[*d.Id] {
				t.Fatal("device repeated across pages")
			}

			seen[*d.Id] = true
		}

		token = page.NextToken
		if token == nil {
			break
		}
	}

	if len(seen) != 4 {
		t.Fatalf("device pagination returned %d records", len(seen))
	}

	_, err := c.CreateWirelessGateway(ctx, &sdk.CreateWirelessGatewayInput{
		LoRaWAN: &types.LoRaWANGateway{GatewayEui: new("ffeeddccbbaa0099"), RfRegion: new("EU868")},
	})
	wirelessOK(t, err)
	gateways, err := c.ListWirelessGateways(ctx, &sdk.ListWirelessGatewaysInput{MaxResults: 1})
	wirelessOK(t, err)
	more, err := c.ListWirelessGateways(ctx, &sdk.ListWirelessGatewaysInput{NextToken: gateways.NextToken})
	wirelessOK(t, err)
	if len(gateways.WirelessGatewayList) != 1 || len(more.WirelessGatewayList) != 1 || gateways.NextToken == nil {
		t.Fatal("gateway pagination")
	}
}
