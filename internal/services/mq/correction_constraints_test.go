package mq

import (
	"reflect"
	"testing"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/mq"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestBrokerRabbitMQRejectedCreationDoesNotReserveToken(t *testing.T) {
	ctx := localContext()
	store := &memory.Store{}
	s := New(store, nil)
	in := brokerInput()
	in.EngineType, in.EngineVersion, in.Logs.Audit = new(rabbitMQ), new("3.13"), nil
	in.CreatorRequestId = new("rabbit-instance-token")
	_, err := s.CreateBroker(ctx, in)
	wantError(t, err, "BadRequestException", 400)
	if err = store.View(ctx, func(tx storage.ReadTx) error {
		if len(tx.Keys("")) != 0 {
			t.Fatal("rejected creation wrote broker or replay state")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	in.HostInstanceType = new(large)
	created, err := s.CreateBroker(ctx, in)
	if err != nil {
		t.Fatal("rejected creation reserved token or name", err)
	}
	replayed, err := s.CreateBroker(ctx, in)
	if err != nil || !reflect.DeepEqual(created, replayed) {
		t.Fatal("valid RabbitMQ replay", err)
	}
}

func TestBrokerRabbitMQLegacyCompatibilityAndCompensation(t *testing.T) {
	ctx := localContext()
	store := &memory.Store{}
	s := New(store, nil)
	in := brokerInput()
	in.EngineType, in.EngineVersion, in.HostInstanceType = new(rabbitMQ), new("3.13"), new(large)
	in.Logs.Audit = nil
	created, err := s.CreateBroker(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	// Seed the retained record as an older fixture would; current creation must not
	// provide a way to create a deprecated micro broker.
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Update(ctx, func(tx storage.WriteTx) error {
		r, readErr := read(tx, scope, *created.BrokerId)
		if readErr != nil {
			return readErr
		}
		r.Broker.HostInstanceType = new(micro)
		return save(tx, scope, r)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.UpdateBroker(ctx, &api.UpdateBrokerRequest{
		BrokerId: created.BrokerId, HostInstanceType: new(micro), Logs: &api.Logs{General: new(false)},
	}); err != nil {
		t.Fatal("legacy micro metadata update", err)
	}
	legacy := describe(t, s, created.BrokerId)
	before, upgraded, err := s.CloudFormationUpdate(ctx, &api.UpdateBrokerRequest{
		BrokerId: created.BrokerId, HostInstanceType: new(large), Logs: &api.Logs{General: new(true)},
	}, map[string]string{"owner": "local"}, map[string]string{"owner": "upgraded"})
	if err != nil || !reflect.DeepEqual(before, legacy) || *upgraded.HostInstanceType != large ||
		!*upgraded.Logs.General || upgraded.Tags["owner"] != "upgraded" {
		t.Fatal("legacy upgrade/snapshot", err)
	}
	_, _, err = s.CloudFormationUpdate(ctx, &api.UpdateBrokerRequest{
		BrokerId: created.BrokerId, HostInstanceType: new(micro), Logs: &api.Logs{General: new(false)},
	}, map[string]string{"owner": "upgraded"}, map[string]string{"replacement": "rejected"})
	wantError(t, err, "BadRequestException", 400)
	if !reflect.DeepEqual(upgraded, describe(t, s, created.BrokerId)) {
		t.Fatal("rejected downgrade changed config or owned tags")
	}
	if _, err = s.CloudFormationRestore(ctx, before); err != nil {
		t.Fatal("metadata compensation to actual legacy snapshot", err)
	}
	if !reflect.DeepEqual(legacy, describe(t, s, created.BrokerId)) {
		t.Fatal("legacy compensation lost actual metadata")
	}
}
