package mq

import (
	"reflect"
	"testing"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/mq"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestBrokerRabbitMQInstanceCreationAndDowngrade(t *testing.T) {
	input := func(host string) *api.CreateBrokerRequest {
		in := brokerInput()
		in.EngineType = new(rabbitMQ)
		in.EngineVersion = new("3.13")
		in.HostInstanceType = new(host)
		in.Logs = &api.Logs{General: new(false)}
		return in
	}
	t.Run("deprecated micro creation", func(t *testing.T) {
		s := New(&memory.Store{}, nil)
		_, err := s.CreateBroker(localContext(), input(micro))
		wantError(t, err, "BadRequestException", 400)
		listed, err := s.ListBrokers(localContext(), &api.ListBrokersRequest{})
		if err != nil || len(listed.BrokerSummaries) != 0 {
			t.Fatal("rejected creation left broker metadata", err)
		}
	})
	t.Run("m5 to micro downgrade", func(t *testing.T) {
		s := New(&memory.Store{}, nil)
		created, err := s.CreateBroker(localContext(), input(large))
		if err != nil {
			t.Fatal(err)
		}
		before := describe(t, s, created.BrokerId)
		_, err = s.UpdateBroker(localContext(), &api.UpdateBrokerRequest{
			BrokerId: created.BrokerId, HostInstanceType: new(micro), Logs: &api.Logs{General: new(true)},
		})
		wantError(t, err, "BadRequestException", 400)
		if !reflect.DeepEqual(before, describe(t, s, created.BrokerId)) {
			t.Fatal("rejected host downgrade partially changed configuration")
		}
	})
}
