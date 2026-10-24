package iotwireless

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"time"

	iotdata "github.com/amjadjibon/tinystack/internal/aws/generated/iotdataplane"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/iotwireless"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// The radio parameters of a simulated uplink are fixed, so the statistics derived from one are deterministic.
const (
	testPayload     = "Test"
	testResult      = "Test"
	uplinkPort      = int64(1)
	uplinkDataRate  = int64(3)
	uplinkFrequency = int64(868300000)
	uplinkRssi      = -64.0
	uplinkSnr       = 9.5
	// ingestPrefix starts the topic that runs one IoT rule, as Basic Ingest does.
	ingestPrefix = "$aws/rules/"
)

// uplinkFPort is the port a simulated uplink arrives on: the first application port a device configures, or 1.
func uplinkFPort(d *api.GetWirelessDeviceResponse) (port int64, destination *string) {
	if d.LoRaWAN.FPorts != nil && len(d.LoRaWAN.FPorts.Applications) > 0 {
		app := d.LoRaWAN.FPorts.Applications[0]
		if app.FPort != nil {
			return *app.FPort, app.DestinationName
		}
	}

	return uplinkPort, nil
}

// TestWirelessDevice simulates an uplink of the payload "Test" from a device: it records the uplink in the device's
// and the gateways' statistics and routes the uplink, as AWS delivers LoRaWAN uplinks, to the destination of the
// uplink's port. A rule destination runs the IoT rule as Basic Ingest does, and a topic destination publishes to the
// topic. The destination's role is not assumed.
func (s *Service) TestWirelessDevice(
	ctx context.Context,
	in *api.TestWirelessDeviceRequest,
) (*api.TestWirelessDeviceResponse, error) {
	var topic string
	var message []byte
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := load(tx, scope, kindDevice, *in.Id)
		if err != nil {
			return err
		}

		port, routed := uplinkFPort(r.Device)
		destination, err := load(
			tx,
			scope,
			kindDestination,
			common.Value(common.OrElse(routed, r.Device.DestinationName)),
		)
		if err != nil {
			return err
		}

		gateways, err := storage.Records[record](tx, scope.Prefix(serviceName, kindGateway))
		if err != nil {
			return err
		}

		now := iso(time.Now())
		r.LastUplink = now
		r.Uplink = &api.LoRaWANDeviceMetadata{
			DevEui: r.Device.LoRaWAN.DevEui, FPort: new(port), DataRate: new(uplinkDataRate),
			Frequency: new(uplinkFrequency), Timestamp: new(now),
		}
		seen := []map[string]any{}
		for _, g := range gateways {
			g.LastUplink = now
			if checkErr := storage.Write(tx, scope.Key(serviceName, g.Kind, g.ID), g); checkErr != nil {
				return checkErr
			}

			r.Uplink.Gateways = append(r.Uplink.Gateways, api.LoRaWANGatewayMetadata{
				GatewayEui: g.Gateway.LoRaWAN.GatewayEui, Rssi: new(uplinkRssi), Snr: new(uplinkSnr),
			})
			seen = append(seen, map[string]any{
				"GatewayEui": *g.Gateway.LoRaWAN.GatewayEui, "Rssi": uplinkRssi, "Snr": uplinkSnr,
			})
		}

		if checkErr := storage.Write(tx, scope.Key(serviceName, r.Kind, r.ID), r); checkErr != nil {
			return checkErr
		}

		message, err = json.Marshal(map[string]any{
			"WirelessDeviceId": r.ID,
			"PayloadData":      base64.StdEncoding.EncodeToString([]byte(testPayload)),
			"WirelessMetadata": map[string]any{"LoRaWAN": map[string]any{
				"DevEui": *r.Device.LoRaWAN.DevEui, "FPort": port, "DataRate": strconv.FormatInt(uplinkDataRate, 10),
				"Frequency": strconv.FormatInt(uplinkFrequency, 10), "Timestamp": now, "Gateways": seen,
			}},
		})
		if err != nil {
			return err
		}

		d := destination.Destination
		topic = *d.Expression
		if *d.ExpressionType == expressionRule {
			topic = ingestPrefix + topic
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// IoT Core runs the rules of the message after this call returns, so a failing action does not fail the test.
	if _, err = s.things.DataPlane().
		Publish(ctx, &iotdata.PublishRequest{Topic: new(topic), Payload: message}); err != nil {
		return nil, err
	}

	return &api.TestWirelessDeviceResponse{Result: new(testResult)}, nil
}

// GetWirelessDeviceStatistics reports the last simulated uplink of a device.
func (s *Service) GetWirelessDeviceStatistics(
	ctx context.Context,
	in *api.GetWirelessDeviceStatisticsRequest,
) (*api.GetWirelessDeviceStatisticsResponse, error) {
	var r *record
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		var err error
		r, err = load(tx, scope, kindDevice, *in.WirelessDeviceId)
		return err
	})
	if err != nil {
		return nil, err
	}

	out := &api.GetWirelessDeviceStatisticsResponse{
		WirelessDeviceId: new(r.ID), LoRaWAN: &api.LoRaWANDeviceMetadata{DevEui: r.Device.LoRaWAN.DevEui},
	}
	if r.Uplink != nil {
		out.LastUplinkReceivedAt, out.LoRaWAN = new(r.LastUplink), r.Uplink
	}

	return out, nil
}
