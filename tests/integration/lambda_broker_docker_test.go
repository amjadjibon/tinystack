package integration_test

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	kafkatypes "github.com/aws/aws-sdk-go-v2/service/kafka/types"
	sdklambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	mqtypes "github.com/aws/aws-sdk-go-v2/service/mq/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/amjadjibon/tinystack/internal/config"
)

func TestLambdaMSKDockerMapping(t *testing.T) {
	if os.Getenv("TINYSTACK_DOCKER_TESTS") != "1" {
		t.Skip("set TINYSTACK_DOCKER_TESTS=1")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cfg := testConfig()
	cfg.Kafka.Clusters = config.ContainersDocker
	runtime := &fakeRuntime{calls: make(chan invocation, 10)}
	s := serverConfig(t, cfg, runtime)
	msk := mskClient(s, "test", "us-east-1")
	input := mskInput(t, s, "us-east-1")
	input.EncryptionInfo = &kafkatypes.EncryptionInfo{EncryptionInTransit: &kafkatypes.EncryptionInTransit{
		ClientBroker: kafkatypes.ClientBrokerPlaintext, InCluster: new(true),
	}}
	cluster, err := msk.CreateCluster(ctx, input)
	if err != nil {
		t.Fatal(err)
	}

	brokers := awaitMSKDocker(t, ctx, msk, cluster.ClusterArn)
	producer, err := kgo.NewClient(kgo.SeedBrokers(strings.Split(*brokers.BootstrapBrokerString, ",")...))
	if err != nil {
		t.Fatal(err)
	}

	defer producer.Close()
	if produceErr := producer.ProduceSync(ctx, &kgo.Record{Topic: "lambda-orders", Value: []byte("order")}).
		FirstErr(); produceErr != nil {
		t.Fatal(produceErr)
	}

	c := lambdaClient(s, "test")
	createFunction(t, c, "msk-consumer", "app.echo")
	_, err = c.CreateEventSourceMapping(ctx, &sdklambda.CreateEventSourceMappingInput{
		FunctionName: new("msk-consumer"), EventSourceArn: cluster.ClusterArn, Topics: []string{"lambda-orders"},
		StartingPosition: lambdatypes.EventSourcePositionTrimHorizon,
	})
	if err != nil {
		t.Fatal(err)
	}

	select {
	case call := <-runtime.calls:
		if !strings.Contains(string(call.payload), `"eventSource":"aws:kafka"`) {
			t.Fatalf("event: %s", call.payload)
		}
	case <-ctx.Done():
		t.Fatal("MSK mapping did not invoke")
	}
}

func TestLambdaMQDockerMapping(t *testing.T) {
	if os.Getenv("TINYSTACK_DOCKER_TESTS") != "1" {
		t.Skip("set TINYSTACK_DOCKER_TESTS=1")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cfg := testConfig()
	cfg.MQ.Brokers = config.BrokersDocker
	runtime := &fakeRuntime{calls: make(chan invocation, 10)}
	s := serverConfig(t, cfg, runtime)
	mq := mqClient(s, "test", "us-east-1")
	input := mqInput()
	input.EngineType, input.EngineVersion, input.Logs.Audit = mqtypes.EngineTypeRabbitmq, new("3.13"), nil
	broker, err := mq.CreateBroker(ctx, input)
	if err != nil {
		t.Fatal(err)
	}

	ready := awaitMQDocker(t, ctx, mq, broker.BrokerId)
	address, err := url.Parse(ready.BrokerInstances[0].Endpoints[0])
	if err != nil {
		t.Fatal(err)
	}

	address.User = url.UserPassword("admin", "sample-valid-password")
	conn, err := amqp.Dial(address.String())
	if err != nil {
		t.Fatal(err)
	}

	defer conn.Close()
	channel, err := conn.Channel()
	if err != nil {
		t.Fatal(err)
	}

	if _, declareErr := channel.QueueDeclare("orders", false, false, false, false, nil); declareErr != nil {
		t.Fatal(declareErr)
	}

	if publishErr := channel.PublishWithContext(
		ctx,
		"",
		"orders",
		false,
		false,
		amqp.Publishing{Body: []byte("order")},
	); publishErr != nil {
		t.Fatal(publishErr)
	}

	secret, err := secretsClient(s, "test", "us-east-1").CreateSecret(ctx, &secretsmanager.CreateSecretInput{
		Name: new("mq-login"), SecretString: new(`{"username":"admin","password":"sample-valid-password"}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	c := lambdaClient(s, "test")
	createFunction(t, c, "mq-consumer", "app.echo")
	_, err = c.CreateEventSourceMapping(ctx, &sdklambda.CreateEventSourceMappingInput{
		FunctionName: new("mq-consumer"), EventSourceArn: broker.BrokerArn, Queues: []string{"orders"},
		SourceAccessConfigurations: []lambdatypes.SourceAccessConfiguration{{
			Type: lambdatypes.SourceAccessTypeBasicAuth, URI: secret.ARN,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	select {
	case call := <-runtime.calls:
		if !strings.Contains(string(call.payload), `"eventSource":"aws:rmq"`) {
			t.Fatalf("event: %s", call.payload)
		}
	case <-ctx.Done():
		t.Fatal("MQ mapping did not invoke")
	}
}
