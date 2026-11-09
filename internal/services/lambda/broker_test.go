package lambda

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/twmb/franz-go/pkg/kgo"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
	"github.com/amjadjibon/tinystack/internal/aws/request"
)

func TestBrokerEvents(t *testing.T) {
	mp := &mapping{EventSourceArn: "arn:aws:kafka:us-east-1:000000000000:cluster/test/id", Queues: []string{"orders"}}
	payload := kafkaEvent(mp, []string{"localhost:19092"}, []*kgo.Record{{
		Topic: "orders", Partition: 2, Offset: 4,
		Timestamp: time.UnixMilli(1234), Key: []byte("key"), Value: []byte(`{"id":1}`),
		Headers: []kgo.RecordHeader{{Key: "type", Value: []byte("order")}},
	}})
	var event map[string]any
	if err := json.Unmarshal(payload, &event); err != nil {
		t.Fatal(err)
	}

	record := event["records"].(map[string]any)["orders-2"].([]any)[0].(map[string]any)
	if event["eventSource"] != "aws:kafka" || record["key"] != "a2V5" || record["offset"] != float64(4) ||
		record["timestamp"] != float64(1234) || record["timestampType"] != "CREATE_TIME" {
		t.Fatalf("Kafka event: %s", payload)
	}

	payload = rabbitEvent(mp, []amqp.Delivery{{Body: []byte("hello"), Redelivered: true, MessageId: "id"}})
	if err := json.Unmarshal(payload, &event); err != nil {
		t.Fatal(err)
	}

	record = event["rmqMessagesByQueue"].(map[string]any)["orders::/"].([]any)[0].(map[string]any)
	if event["eventSource"] != "aws:rmq" || record["data"] != "aGVsbG8=" || record["redelivered"] != true {
		t.Fatalf("RabbitMQ event: %s", payload)
	}
}

func TestBrokerValidation(t *testing.T) {
	for _, tc := range []struct {
		in   api.CreateEventSourceMappingRequest
		name string
	}{
		{name: "topics on SQS", in: api.CreateEventSourceMappingRequest{Topics: []string{"t"}}},
		{name: "queues on Kafka", in: api.CreateEventSourceMappingRequest{
			EventSourceArn: new("arn:aws:kafka:us-east-1:000000000000:cluster/test/id"), Queues: []string{"q"},
		}},
		{name: "ARN and endpoints", in: api.CreateEventSourceMappingRequest{
			EventSourceArn:         new("arn:aws:kafka:us-east-1:000000000000:cluster/test/id"),
			SelfManagedEventSource: &api.SelfManagedEventSource{},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateBrokerFields(&tc.in); err == nil {
				t.Fatal("accepted invalid field combination")
			}
		})
	}

	types := []string{"VPC_SUBNET", "VPC_SECURITY_GROUP", "CLIENT_CERTIFICATE_TLS_AUTH", "VIRTUAL_HOST", "unknown"}
	for _, typ := range types {
		t.Run(typ, func(t *testing.T) {
			mp := &mapping{
				SourceAccessConfigurations: []api.SourceAccessConfiguration{{Type: new(typ), URI: new("secret")}},
			}

			if err := validateAccess(mp); err == nil {
				t.Fatal("accepted unsupported access type")
			}
		})
	}
}

func TestKafkaSourceValidation(t *testing.T) {
	for name, in := range map[string]*api.CreateEventSourceMappingRequest{
		"missing topic": {StartingPosition: new(positionLatest), SelfManagedEventSource: &api.SelfManagedEventSource{
			Endpoints: map[string][]string{"KAFKA_BOOTSTRAP_SERVERS": {"localhost:19092"}},
		}},
		"two topics": {
			Topics: []string{"one", "two"}, StartingPosition: new(positionLatest),
			SelfManagedEventSource: &api.SelfManagedEventSource{
				Endpoints: map[string][]string{"KAFKA_BOOTSTRAP_SERVERS": {"localhost:19092"}},
			},
		},
		"missing position": {Topics: []string{"one"}, SelfManagedEventSource: &api.SelfManagedEventSource{
			Endpoints: map[string][]string{"KAFKA_BOOTSTRAP_SERVERS": {"localhost:19092"}},
		}},
		"timestamp position": {
			Topics: []string{"one"}, StartingPosition: new("AT_TIMESTAMP"),
			SelfManagedEventSource: &api.SelfManagedEventSource{
				Endpoints: map[string][]string{"KAFKA_BOOTSTRAP_SERVERS": {"localhost:19092"}},
			},
		},
		"missing endpoints": {
			Topics: []string{"one"}, StartingPosition: new(positionLatest),
			SelfManagedEventSource: &api.SelfManagedEventSource{},
		},
		"invalid endpoint": {
			Topics: []string{"one"}, StartingPosition: new(positionLatest),
			SelfManagedEventSource: &api.SelfManagedEventSource{
				Endpoints: map[string][]string{"KAFKA_BOOTSTRAP_SERVERS": {"invalid"}},
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := &Service{}
			if err := s.configureBroker(context.Background(), request.Scope{}, in, &mapping{}); err != nil {
				return
			}

			t.Fatal("accepted invalid Kafka mapping")
		})
	}
}
