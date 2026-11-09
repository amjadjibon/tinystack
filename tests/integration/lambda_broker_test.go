package integration_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	sdklambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/lambda/types"
	mqtypes "github.com/aws/aws-sdk-go-v2/service/mq/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"

	"github.com/amjadjibon/tinystack/internal/services/lambda"
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
	if _, createErr := c.CreateEventSourceMapping(ctx, input); createErr == nil {
		t.Fatal("accepted multiple topics")
	}

	input.Topics = []string{"one"}
	input.SourceAccessConfigurations = []types.SourceAccessConfiguration{{
		Type: types.SourceAccessTypeBasicAuth,
		URI:  new("arn:aws:secretsmanager:us-east-1:000000000000:secret:missing"),
	}}
	if _, createErr := c.CreateEventSourceMapping(ctx, input); createErr == nil {
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
		FunctionName:            new("managed-brokers"),
		EventSourceArn:          cluster.ClusterArn,
		MaximumRetryAttempts:    new(int32(1)),
		ProvisionedPollerConfig: &types.ProvisionedPollerConfig{},
		Topics:                  []string{"orders"},
		StartingPosition:        types.EventSourcePositionLatest,
		AmazonManagedKafkaEventSourceConfig: &types.AmazonManagedKafkaEventSourceConfig{
			ConsumerGroupId: new("managed"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if *created.MaximumRetryAttempts != 1 || *created.ProvisionedPollerConfig.MinimumPollers != 1 {
		t.Fatalf("MSK options: %+v", created)
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

// kafkaFailureRuntime fails offset 1, either as a function error or a partial response.
type kafkaFailureRuntime struct {
	batches chan []int64
	mode    string
}

func (r *kafkaFailureRuntime) Invoke(_ context.Context, _ *lambda.Function, payload []byte) (lambda.Result, error) {
	var event struct {
		Records map[string][]struct {
			Offset int64 `json:"offset"`
		} `json:"records"`
	}

	if err := json.Unmarshal(payload, &event); err != nil {
		return lambda.Result{}, err
	}

	var offsets []int64
	bad := false
	for _, records := range event.Records {
		for _, record := range records {
			offsets = append(offsets, record.Offset)
			bad = bad || record.Offset == 1
		}
	}

	r.batches <- offsets
	if bad && r.mode == "partial" {
		return lambda.Result{
			Payload: []byte(`{"batchItemFailures":[{"itemIdentifier":{"partition":"orders-0","offset":1}}]}`),
		}, nil
	}

	return lambda.Result{Payload: []byte(`{}`), Error: bad}, nil
}

func (*kafkaFailureRuntime) Release(string) {}
func (*kafkaFailureRuntime) Close()         {}

func TestLambdaKafkaFailureHandling(t *testing.T) {
	for _, mode := range []string{"retry", "partial", "bisect"} {
		t.Run(mode, func(t *testing.T) {
			cluster, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(1, "orders"))
			if err != nil {
				t.Fatal(err)
			}

			defer cluster.Close()
			runtime := &kafkaFailureRuntime{mode: mode, batches: make(chan []int64, 32)}
			s := serverWith(t, runtime)
			c := lambdaClient(s, "test")
			q := sqsClient(s, "test")
			queue := createQueue(t, q, "kafka-failures", nil)
			createFunction(t, c, "kafka-failures", "app.fail")
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			producer, err := kgo.NewClient(kgo.SeedBrokers(cluster.ListenAddrs()...))
			if err != nil {
				t.Fatal(err)
			}

			defer producer.Close()
			for range 4 {
				if produceErr := producer.ProduceSync(ctx, &kgo.Record{Topic: "orders", Value: []byte("event")}).
					FirstErr(); produceErr != nil {
					t.Fatal(produceErr)
				}
			}

			input := &sdklambda.CreateEventSourceMappingInput{
				FunctionName: new("kafka-failures"), Topics: []string{"orders"},
				StartingPosition: types.EventSourcePositionTrimHorizon,
				SelfManagedEventSource: &types.SelfManagedEventSource{Endpoints: map[string][]string{
					"KAFKA_BOOTSTRAP_SERVERS": cluster.ListenAddrs(),
				}},
				BatchSize: new(int32(4)), MaximumBatchingWindowInSeconds: new(int32(1)),
				MaximumRetryAttempts: new(int32(2)), MaximumRecordAgeInSeconds: new(int32(3600)),
				DestinationConfig: &types.DestinationConfig{OnFailure: &types.OnFailure{
					Destination: new("arn:aws:sqs:us-east-1:000000000000:kafka-failures"),
				}},
			}

			if _, createErr := c.CreateEventSourceMapping(ctx, input); createErr == nil {
				t.Fatal("accepted retries without provisioned mode")
			}

			input.ProvisionedPollerConfig = &types.ProvisionedPollerConfig{
				MinimumPollers: new(int32(1)),
				MaximumPollers: new(int32(2)),
			}

			input.BisectBatchOnFunctionError = new(mode == "bisect")
			if mode == "partial" {
				input.FunctionResponseTypes = []types.FunctionResponseType{
					types.FunctionResponseTypeReportBatchItemFailures,
				}
			}

			created, err := c.CreateEventSourceMapping(ctx, input)
			if err != nil {
				t.Fatal(err)
			}

			if *created.MaximumRetryAttempts != 2 || *created.MaximumRecordAgeInSeconds != 3600 ||
				*created.ProvisionedPollerConfig.MaximumPollers != 2 {
				t.Fatalf("create options: %+v", created)
			}

			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			var message string
			for message == "" {
				out, receiveErr := q.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: new(queue)})
				if receiveErr != nil {
					t.Fatal(receiveErr)
				}

				if len(out.Messages) > 0 {
					message = *out.Messages[0].Body
					break
				}

				select {
				case <-ctx.Done():
					t.Fatal("no failure destination record")
				case <-ticker.C:
				}
			}

			var record struct {
				Info struct {
					RecordsInfo map[string]any `json:"recordsInfo"`
					BatchSize   int            `json:"batchSize"`
				} `json:"KafkaBatchInfo"`
				RequestContext struct {
					Count int `json:"approximateInvokeCount"`
				} `json:"requestContext"`
			}

			if decodeErr := json.Unmarshal([]byte(message), &record); decodeErr != nil {
				t.Fatal(decodeErr)
			}

			if record.RequestContext.Count != 3 || len(record.Info.RecordsInfo) != 1 {
				t.Fatalf("failure record: %s", message)
			}

			expected := [][]int64{{0, 1, 2, 3}, {0, 1, 2, 3}, {0, 1, 2, 3}}
			if mode == "partial" {
				expected = [][]int64{{0, 1, 2, 3}, {1}, {1}}
			}

			if mode == "bisect" {
				expected = [][]int64{{0, 1, 2, 3}, {0, 1}, {0}, {1}, {1}, {1}, {2, 3}}
			}

			for _, want := range expected {
				select {
				case got := <-runtime.batches:
					if !slices.Equal(got, want) {
						t.Fatalf("batch %v, want %v", got, want)
					}

				case <-ctx.Done():
					t.Fatal("missing invocation")
				}
			}

			awaitKafkaOffset(t, ctx, producer, *created.UUID, 4)
			if produceErr := producer.ProduceSync(ctx, &kgo.Record{Topic: "orders", Value: []byte("later")}).
				FirstErr(); produceErr != nil {
				t.Fatal(produceErr)
			}

			select {
			case got := <-runtime.batches:
				if !slices.Equal(got, []int64{4}) {
					t.Fatalf("later batch: %v", got)
				}

			case <-ctx.Done():
				t.Fatal("partition blocked after discard")
			}

			awaitKafkaOffset(t, ctx, producer, *created.UUID, 5)
			updated, err := c.UpdateEventSourceMapping(ctx, &sdklambda.UpdateEventSourceMappingInput{
				UUID:                       created.UUID,
				Enabled:                    new(false),
				MaximumRetryAttempts:       new(int32(0)),
				MaximumRecordAgeInSeconds:  new(int32(-1)),
				BisectBatchOnFunctionError: new(true),
				FunctionResponseTypes: []types.FunctionResponseType{
					types.FunctionResponseTypeReportBatchItemFailures,
				},
				ProvisionedPollerConfig: &types.ProvisionedPollerConfig{MaximumPollers: new(int32(3))},
			})
			if err != nil {
				t.Fatal(err)
			}

			got, err := c.GetEventSourceMapping(ctx, &sdklambda.GetEventSourceMappingInput{UUID: created.UUID})
			if err != nil {
				t.Fatal(err)
			}

			if *updated.MaximumRetryAttempts != 0 || *got.MaximumRecordAgeInSeconds != -1 ||
				!*got.BisectBatchOnFunctionError || len(got.FunctionResponseTypes) != 1 ||
				*got.ProvisionedPollerConfig.MaximumPollers != 3 || *got.ProvisionedPollerConfig.MinimumPollers != 1 {
				t.Fatalf("update/get: %+v", got)
			}

			for _, retries := range []int32{-2, 10001} {
				if _, err := c.UpdateEventSourceMapping(ctx, &sdklambda.UpdateEventSourceMappingInput{
					UUID: created.UUID, MaximumRetryAttempts: new(retries),
				}); err == nil {
					t.Fatalf("accepted retries %d", retries)
				}
			}
		})
	}
}

func awaitKafkaOffset(t *testing.T, ctx context.Context, client *kgo.Client, group string, want int64) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		response, err := client.Request(ctx, &kmsg.OffsetFetchRequest{
			Group: group, Topics: []kmsg.OffsetFetchRequestTopic{{Topic: "orders", Partitions: []int32{0}}},
			Groups: []kmsg.OffsetFetchRequestGroup{{Group: group, Topics: []kmsg.OffsetFetchRequestGroupTopic{
				{Topic: "orders", Partitions: []int32{0}},
			}}},
		})
		if err != nil {
			t.Fatal(err)
		}

		fetched := response.(*kmsg.OffsetFetchResponse)
		if len(fetched.Topics) == 1 && fetched.Topics[0].Partitions[0].Offset == want {
			return
		}

		if len(fetched.Groups) == 1 && len(fetched.Groups[0].Topics) == 1 &&
			fetched.Groups[0].Topics[0].Partitions[0].Offset == want {
			return
		}

		select {
		case <-ctx.Done():
			t.Fatalf("offset %d not committed", want)
		case <-ticker.C:
		}
	}
}
