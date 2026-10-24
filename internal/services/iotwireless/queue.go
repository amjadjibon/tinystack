package iotwireless

import (
	"context"
	"fmt"
	"time"
	"uuid"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/iotwireless"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// queueAll is the MessageId that deletes a device's whole downlink queue.
const queueAll = "*"

// queued is a downlink message in a queue. Seq orders the queue and is the paging token's ID.
type queued struct {
	api.DownlinkQueueMessage
	Seq     string
	Payload string
}

// enqueue appends a message to the queue under prefix and returns its ID. There is no radio, so a message stays
// queued until it is deleted.
func enqueue(
	tx storage.WriteTx,
	scope request.Scope,
	kind, owner, payload string,
	m api.DownlinkQueueMessage,
) (string, error) {
	id := uuid.NewV4().String()
	now := time.Now()
	m.MessageId, m.ReceivedAt = new(id), new(iso(now))
	seq := fmt.Sprintf("%020d-%s", now.UnixNano(), id)
	err := storage.Write(tx, scope.Key(serviceName, kind, owner+"/"+seq), queued{
		DownlinkQueueMessage: m, Seq: seq, Payload: payload,
	})

	return id, err
}

// SendDataToWirelessDevice queues a downlink message for a LoRaWAN device, whose port it must name.
func (s *Service) SendDataToWirelessDevice(
	ctx context.Context,
	in *api.SendDataToWirelessDeviceRequest,
) (*api.SendDataToWirelessDeviceResponse, error) {
	if in.WirelessMetadata == nil || in.WirelessMetadata.LoRaWAN == nil || in.WirelessMetadata.LoRaWAN.FPort == nil {
		return nil, invalid("WirelessMetadata.LoRaWAN.FPort is required")
	}

	lw := in.WirelessMetadata.LoRaWAN
	var id string
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if _, err := load(tx, scope, kindDevice, *in.Id); err != nil {
			return err
		}

		if p := lw.ParticipatingGateways; p != nil {
			for _, g := range p.GatewayList {
				if _, err := load(tx, scope, kindGateway, common.Value(g.GatewayId)); err != nil {
					return err
				}
			}
		}

		var err error
		id, err = enqueue(tx, scope, kindMessage, *in.Id, *in.PayloadData, api.DownlinkQueueMessage{
			TransmitMode: in.TransmitMode, LoRaWAN: lw,
		})
		return err
	})
	if err != nil {
		return nil, err
	}

	return &api.SendDataToWirelessDeviceResponse{MessageId: new(id)}, nil
}

// SendDataToMulticastGroup queues a downlink message for a multicast group. The API has no operation to list a
// group's queue.
func (s *Service) SendDataToMulticastGroup(
	ctx context.Context,
	in *api.SendDataToMulticastGroupRequest,
) (*api.SendDataToMulticastGroupResponse, error) {
	if in.WirelessMetadata.LoRaWAN == nil || in.WirelessMetadata.LoRaWAN.FPort == nil {
		return nil, invalid("WirelessMetadata.LoRaWAN.FPort is required")
	}

	var id string
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if _, err := load(tx, scope, kindMulticast, *in.Id); err != nil {
			return err
		}

		var err error
		id, err = enqueue(tx, scope, kindGroupMessage, *in.Id, *in.PayloadData, api.DownlinkQueueMessage{
			LoRaWAN: &api.LoRaWANSendDataToDevice{FPort: in.WirelessMetadata.LoRaWAN.FPort},
		})
		return err
	})
	if err != nil {
		return nil, err
	}

	return &api.SendDataToMulticastGroupResponse{MessageId: new(id)}, nil
}

// checkDeviceType fails when a request names a wireless device type other than the device's.
func checkDeviceType(d *api.GetWirelessDeviceResponse, requested *string) error {
	if requested != nil && *requested != common.Value(d.Type) {
		return invalid("The wireless device %s is not a %s device", common.Value(d.Id), *requested)
	}

	return nil
}

// ListQueuedMessages lists a device's queued downlink messages, oldest first.
func (s *Service) ListQueuedMessages(
	ctx context.Context,
	in *api.ListQueuedMessagesRequest,
) (*api.ListQueuedMessagesResponse, error) {
	var messages []*queued
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		r, err := load(tx, scope, kindDevice, *in.Id)
		if err != nil {
			return err
		}

		if checkErr := checkDeviceType(r.Device, in.WirelessDeviceType); checkErr != nil {
			return checkErr
		}

		messages, err = storage.Records[queued](tx, scope.Key(serviceName, kindMessage, r.ID+"/"))
		return err
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(
		messages,
		func(m *queued) string { return m.Seq },
		in.NextToken,
		pageSize(in.MaxResults),
		badToken(),
	)
	if err != nil {
		return nil, err
	}

	out := &api.ListQueuedMessagesResponse{NextToken: next, DownlinkQueueMessagesList: []api.DownlinkQueueMessage{}}
	for _, m := range page {
		out.DownlinkQueueMessagesList = append(out.DownlinkQueueMessagesList, m.DownlinkQueueMessage)
	}

	return out, nil
}

// DeleteQueuedMessages deletes one queued message of a device, or all of them for the MessageId "*".
func (s *Service) DeleteQueuedMessages(
	ctx context.Context,
	in *api.DeleteQueuedMessagesRequest,
) (*api.DeleteQueuedMessagesResponse, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := load(tx, scope, kindDevice, *in.Id)
		if err != nil {
			return err
		}

		if checkErr := checkDeviceType(r.Device, in.WirelessDeviceType); checkErr != nil {
			return checkErr
		}

		prefix := scope.Key(serviceName, kindMessage, r.ID+"/")
		if *in.MessageId == queueAll {
			for _, key := range tx.Keys(prefix) {
				tx.Delete(key)
			}

			return nil
		}

		messages, err := storage.Records[queued](tx, prefix)
		if err != nil {
			return err
		}

		for _, m := range messages {
			if common.Value(m.MessageId) == *in.MessageId {
				tx.Delete(prefix + m.Seq)
				return nil
			}
		}

		return missing("Queued message", *in.MessageId)
	})

	return &api.DeleteQueuedMessagesResponse{}, err
}
