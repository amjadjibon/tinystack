package iotwireless

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/iotwireless"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestModelValidation(t *testing.T) {
	s := New(memory.New(nil, nil), nil, nil)
	ctx := request.WithMetadata(
		context.Background(),
		request.Metadata{Scope: request.Scope{AccountID: "123456789012", Region: "us-east-1"}},
	)
	for _, tc := range []struct {
		input any
		op    string
	}{
		{&api.CreateDestinationRequest{}, "CreateDestination"},
		{
			&api.CreateWirelessGatewayRequest{LoRaWAN: &api.LoRaWANGateway{GatewayEui: new("invalid")}},
			"CreateWirelessGateway",
		},
		{&api.CreateWirelessDeviceRequest{Type: new("invalid"), DestinationName: new("destination")}, "CreateWirelessDevice"},
		{&api.ListDestinationsRequest{MaxResults: new(int64(251))}, "ListDestinations"},
		{
			&api.PutResourceLogLevelRequest{
				ResourceIdentifier: new("id"), ResourceType: new("WirelessDevice"), LogLevel: new("invalid"),
			},
			"PutResourceLogLevel",
		},
	} {
		t.Run(tc.op, func(t *testing.T) {
			_, err := api.Descriptor(s).Operations[tc.op].Invoke(ctx, tc.input)
			var got *awserror.Error
			if !errors.As(err, &got) || got.Code != "ValidationException" {
				t.Fatalf("validation: %v", err)
			}
		})
	}
}

// Check the durable worker at explicit timestamps, including two visits of the same index item.
func TestDurableProgression(t *testing.T) {
	store := memory.New(nil, nil)
	scope := request.Scope{AccountID: "123456789012", Region: "us-east-1"}
	now := time.Now().UTC()
	group := &record{
		Kind:      kindMulticast,
		ID:        "group",
		Multicast: &api.GetMulticastGroupResponse{Status: new(statusPending)},
		Session:   &api.LoRaWANMulticastSession{SessionStartTime: new(now), SessionTimeout: new(int64(60))},
	}
	task := &record{
		Kind: kindFuota,
		ID:   "task",
		Fuota: &api.GetFuotaTaskResponse{
			Status:  new(statusWaiting),
			LoRaWAN: &api.LoRaWANFuotaTaskGetInfo{StartTime: new(now)},
		},
	}
	for _, r := range []*record{group, task} {
		item := &workItem{Scope: scope, Kind: r.Kind, ID: r.ID}
		err := store.Update(context.Background(), func(tx storage.WriteTx) error {
			if checkErr := storage.Write(tx, scope.Key(serviceName, r.Kind, r.ID), r); checkErr != nil {
				return checkErr
			}

			if err := storage.Write(tx, indexKey(scope, r.Kind, r.ID), item); err != nil {
				return err
			}

			if err := step(tx, item, now); err != nil {
				return err
			}

			return step(tx, item, now)
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	err := store.Update(context.Background(), func(tx storage.WriteTx) error {
		g, err := load(tx, scope, kindMulticast, "group")
		if err != nil {
			return err
		}

		f, err := load(tx, scope, kindFuota, "task")
		if err != nil {
			return err
		}

		if *g.Multicast.Status != statusSessionSet || *f.Fuota.Status != statusInSession {
			t.Fatal("worker advanced more than once")
		}

		for _, r := range []*record{g, f} {
			if err := step(tx, &workItem{Scope: scope, Kind: r.Kind, ID: r.ID}, now.Add(61*time.Second)); err != nil {
				return err
			}

			if _, found := tx.Get(indexKey(scope, r.Kind, r.ID)); found {
				t.Fatal("completed index retained")
			}
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
