package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	sdklambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/lambda/types"
	mqtypes "github.com/aws/aws-sdk-go-v2/service/mq/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
)

func TestLambdaKafkaMappingRoundTrip(t *testing.T) {
	s := serverWith(t, &fakeRuntime{})
	c := lambdaClient(s, "test")
	createFunction(t, c, "kafka-mapping", "app.echo")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	input := &sdklambda.CreateEventSourceMappingInput{
		FunctionName:     new("kafka-mapping"),
		Topics:           []string{"orders"},
		StartingPosition: types.EventSourcePositionTrimHorizon,
		SelfManagedEventSource: &types.SelfManagedEventSource{Endpoints: map[string][]string{
			"KAFKA_BOOTSTRAP_SERVERS": {"127.0.0.1:19099"},
		}},
		SelfManagedKafkaEventSourceConfig: &types.SelfManagedKafkaEventSourceConfig{
			ConsumerGroupId: new("orders-consumer"),
		},
		Enabled: new(false),
	}
	out, err := c.CreateEventSourceMapping(ctx, input)
	if err != nil {
		t.Fatal(err)
	}

	got, err := c.GetEventSourceMapping(ctx, &sdklambda.GetEventSourceMappingInput{UUID: out.UUID})
	if err != nil {
		t.Fatal(err)
	}

	if len(got.Topics) != 1 || got.Topics[0] != "orders" || got.SelfManagedEventSource == nil ||
		*got.SelfManagedKafkaEventSourceConfig.ConsumerGroupId != "orders-consumer" {
		t.Fatalf("round trip: %+v", got)
	}

	updated, err := c.UpdateEventSourceMapping(ctx, &sdklambda.UpdateEventSourceMappingInput{
		UUID: out.UUID, BatchSize: new(int32(25)), Enabled: new(true),
	})
	if err != nil {
		t.Fatal(err)
	}

	if *updated.BatchSize != 25 || len(updated.Topics) != 1 {
		t.Fatalf("update: %+v", updated)
	}

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		got, err = c.GetEventSourceMapping(ctx, &sdklambda.GetEventSourceMappingInput{UUID: out.UUID})
		if err != nil {
			t.Fatal(err)
		}

		if strings.HasPrefix(*got.LastProcessingResult, "PROBLEM:") {
			break
		}

		select {
		case <-ctx.Done():
			t.Fatal("mapping did not report unavailable broker")
		case <-ticker.C:
		}
	}

	list, err := c.ListEventSourceMappings(ctx, &sdklambda.ListEventSourceMappingsInput{})
	if err != nil || len(list.EventSourceMappings) != 1 || len(list.EventSourceMappings[0].Topics) != 1 {
		t.Fatalf("list: %+v, %v", list, err)
	}

	input.Topics = []string{"one", "two"}
	if _, err := c.CreateEventSourceMapping(ctx, input); err == nil {
		t.Fatal("accepted multiple topics")
	}

	input.Topics = []string{"one"}
	input.SourceAccessConfigurations = []types.SourceAccessConfiguration{{
		Type: types.SourceAccessTypeBasicAuth,
		URI:  new("arn:aws:secretsmanager:us-east-1:000000000000:secret:missing"),
	}}
	if _, err := c.CreateEventSourceMapping(ctx, input); err == nil {
		t.Fatal("accepted missing secret")
	}

	if _, err := c.DeleteEventSourceMapping(ctx, &sdklambda.DeleteEventSourceMappingInput{UUID: out.UUID}); err != nil {
		t.Fatal(err)
	}
}

func TestLambdaSelfManagedKafkaDataPath(t *testing.T) {
	cluster, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(1, "orders"))
	if err != nil {
		t.Fatal(err)
	}

	defer cluster.Close()
	runtime := &fakeRuntime{calls: make(chan invocation, 10)}
	s := serverWith(t, runtime)
	c := lambdaClient(s, "test")
	createFunction(t, c, "kafka-data", "app.fail")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	producer, err := kgo.NewClient(kgo.SeedBrokers(cluster.ListenAddrs()...))
	if err != nil {
		t.Fatal(err)
	}

	defer producer.Close()
	if produceErr := producer.ProduceSync(ctx, &kgo.Record{Topic: "orders", Value: []byte(`{"id":7}`)}).
		FirstErr(); produceErr != nil {
		t.Fatal(produceErr)
	}

	created, err := c.CreateEventSourceMapping(ctx, &sdklambda.CreateEventSourceMappingInput{
		FunctionName: new(
			"kafka-data",
		),
		Topics:           []string{"orders"},
		StartingPosition: types.EventSourcePositionTrimHorizon,
		SelfManagedEventSource: &types.SelfManagedEventSource{Endpoints: map[string][]string{
			"KAFKA_BOOTSTRAP_SERVERS": cluster.ListenAddrs(),
		}},
		FilterCriteria: &types.FilterCriteria{Filters: []types.Filter{{Pattern: new(`{"value":{"id":[7]}}`)}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	select {
	case call := <-runtime.calls:
		if !strings.Contains(string(call.payload), `"eventSource":"SelfManagedKafka"`) ||
			!strings.Contains(string(call.payload), `"orders-0"`) {
			t.Fatalf("payload: %s", call.payload)
		}
	case <-ctx.Done():
		t.Fatal("Kafka invocation timed out")
	}

	_, err = c.UpdateFunctionConfiguration(ctx, &sdklambda.UpdateFunctionConfigurationInput{
		FunctionName: new("kafka-data"), Handler: new("app.echo"),
	})
	if err != nil {
		t.Fatal(err)
	}

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		offsets, fetchErr := producer.Request(ctx, &kmsg.OffsetFetchRequest{
			Group:  *created.UUID,
			Topics: []kmsg.OffsetFetchRequestTopic{{Topic: "orders", Partitions: []int32{0}}},
			Groups: []kmsg.OffsetFetchRequestGroup{
				{
					Group:  *created.UUID,
					Topics: []kmsg.OffsetFetchRequestGroupTopic{{Topic: "orders", Partitions: []int32{0}}},
				},
			},
		})
		if fetchErr != nil {
			t.Fatal(fetchErr)
		}

		fetched := offsets.(*kmsg.OffsetFetchResponse)
		if len(fetched.Topics) == 1 && fetched.Topics[0].Partitions[0].Offset == 1 {
			break
		}

		if len(fetched.Groups) == 1 && len(fetched.Groups[0].Topics) == 1 &&
			fetched.Groups[0].Topics[0].Partitions[0].Offset == 1 {
			break
		}

		select {
		case <-ctx.Done():
			t.Fatal("offset not committed")
		case <-ticker.C:
		}
	}

	if _, updateErr := c.UpdateEventSourceMapping(ctx, &sdklambda.UpdateEventSourceMappingInput{
		UUID: created.UUID, Enabled: new(false),
	}); updateErr != nil {
		t.Fatal(updateErr)
	}
}

func TestLambdaManagedBrokerMappings(t *testing.T) {
	s := server(t)
	c := lambdaClient(s, "test")
	createFunction(t, c, "managed-brokers", "app.echo")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	msk := mskClient(s, "test", "us-east-1")
	cluster, err := msk.CreateCluster(ctx, mskInput(t, s, "us-east-1"))
	if err != nil {
		t.Fatal(err)
	}

	created, err := c.CreateEventSourceMapping(ctx, &sdklambda.CreateEventSourceMappingInput{
		FunctionName:     new("managed-brokers"),
		EventSourceArn:   cluster.ClusterArn,
		Topics:           []string{"orders"},
		StartingPosition: types.EventSourcePositionLatest,
		AmazonManagedKafkaEventSourceConfig: &types.AmazonManagedKafkaEventSourceConfig{
			ConsumerGroupId: new("managed"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	awaitBrokerProblem(t, ctx, c, created.UUID)
	mq := mqClient(s, "test", "us-east-1")
	in := mqInput()
	in.EngineType, in.EngineVersion, in.Logs.Audit = mqtypes.EngineTypeRabbitmq, new("3.13"), nil
	broker, err := mq.CreateBroker(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	secrets := secretsClient(s, "test", "us-east-1")
	secret, err := secrets.CreateSecret(ctx, &secretsmanager.CreateSecretInput{
		Name:         new("broker-login"),
		SecretString: new(`{"username":"admin","password":"sample-valid-password"}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	created, err = c.CreateEventSourceMapping(ctx, &sdklambda.CreateEventSourceMappingInput{
		FunctionName:   new("managed-brokers"),
		EventSourceArn: broker.BrokerArn,
		Queues:         []string{"orders"},
		SourceAccessConfigurations: []types.SourceAccessConfiguration{
			{Type: types.SourceAccessTypeBasicAuth, URI: secret.ARN},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(created.Queues) != 1 || len(created.SourceAccessConfigurations) != 1 {
		t.Fatalf("MQ: %+v", created)
	}

	updated, updateErr := c.UpdateEventSourceMapping(ctx, &sdklambda.UpdateEventSourceMappingInput{
		UUID: created.UUID, Enabled: new(false), SourceAccessConfigurations: []types.SourceAccessConfiguration{
			{Type: types.SourceAccessTypeBasicAuth, URI: secret.ARN},
		},
	})
	if updateErr != nil || len(updated.SourceAccessConfigurations) != 1 ||
		*updated.SourceAccessConfigurations[0].URI != *secret.ARN {
		t.Fatalf("MQ update: %+v, %v", updated, updateErr)
	}

	in = mqInput()
	in.BrokerName, in.CreatorRequestId = new("active-broker"), new("active-request")
	active, err := mq.CreateBroker(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateEventSourceMapping(ctx, &sdklambda.CreateEventSourceMappingInput{
		FunctionName: new("managed-brokers"), EventSourceArn: active.BrokerArn, Queues: []string{"orders"},
	})
	if err == nil || !strings.Contains(err.Error(), "ActiveMQ") {
		t.Fatalf("ActiveMQ: %v", err)
	}
}

func awaitBrokerProblem(t *testing.T, ctx context.Context, c *sdklambda.Client, id *string) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		got, err := c.GetEventSourceMapping(ctx, &sdklambda.GetEventSourceMappingInput{UUID: id})
		if err != nil {
			t.Fatal(err)
		}

		if strings.HasPrefix(*got.LastProcessingResult, "PROBLEM:") {
			return
		}

		select {
		case <-ctx.Done():
			t.Fatal("no broker diagnostic")
		case <-ticker.C:
		}
	}
}

func TestCloudFormationKafkaMapping(t *testing.T) {
	s := server(t)
	c := lambdaClient(s, "test")
	createFunction(t, c, "cfn-kafka", "app.echo")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stacks := cfnClient(s)
	_, err := stacks.CreateStack(ctx, &cfn.CreateStackInput{StackName: new("kafka-mapping"), TemplateBody: new(`{
 "Resources":{"Mapping":{"Type":"AWS::Lambda::EventSourceMapping","Properties":{
 "FunctionName":"cfn-kafka","Enabled":false,"Topics":["orders"],"StartingPosition":"TRIM_HORIZON",
 "SelfManagedEventSource":{"Endpoints":{"KAFKA_BOOTSTRAP_SERVERS":["localhost:19099"]}},
 "SelfManagedKafkaEventSourceConfig":{"ConsumerGroupId":"cfn-orders"}
 }}}}`)})
	if err != nil {
		t.Fatal(err)
	}

	stack := settled(ctx, t, stacks, "kafka-mapping")
	if stack.StackStatus != cfntypes.StackStatusCreateComplete {
		t.Fatalf("stack: %+v", stack)
	}

	mappings, err := c.ListEventSourceMappings(ctx, &sdklambda.ListEventSourceMappingsInput{})
	if err != nil {
		t.Fatal(err)
	}

	if len(mappings.EventSourceMappings) != 1 ||
		mappings.EventSourceMappings[0].SelfManagedKafkaEventSourceConfig == nil ||
		*mappings.EventSourceMappings[0].SelfManagedKafkaEventSourceConfig.ConsumerGroupId != "cfn-orders" {
		t.Fatalf("mapping: %+v", mappings)
	}
}
