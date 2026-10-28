package integration_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	sdk "github.com/aws/aws-sdk-go-v2/service/mq"
	"github.com/aws/aws-sdk-go-v2/service/mq/types"
	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/services/dockercli"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/sqlite"
)

func awaitMQDocker(t *testing.T, ctx context.Context, c *sdk.Client, id *string) *sdk.DescribeBrokerOutput {
	t.Helper()
	for ctx.Err() == nil {
		out, err := c.DescribeBroker(ctx, &sdk.DescribeBrokerInput{BrokerId: id})
		if err != nil {
			t.Fatal(err)
		}

		if out.BrokerState == types.BrokerStateRunning {
			return out
		}

		time.Sleep(time.Second)
	}

	t.Fatal(ctx.Err())
	return nil
}

func mqDockerResources(t *testing.T, arn string, cleanup bool) (containersCount, volumesCount int) {
	t.Helper()
	ctx := context.Background()
	containers, err := dockercli.ListLabeled(ctx, "tinystack.mq")
	if err != nil {
		t.Fatal(err)
	}

	volumes, err := dockercli.ListLabeledVolumes(ctx, "tinystack.mq")
	if err != nil {
		t.Fatal(err)
	}

	nc, nv := 0, 0
	for id, owner := range containers {
		if strings.HasSuffix(owner, "|"+arn) {
			nc++
			if cleanup {
				_, _ = dockercli.Run(ctx, nil, "rm", "-f", id)
			}
		}
	}

	for name, owner := range volumes {
		if strings.HasSuffix(owner, "|"+arn) {
			nv++
			if cleanup {
				_, _ = dockercli.Run(ctx, nil, "volume", "rm", name)
			}
		}
	}

	return nc, nv
}

func mqAMQP(t *testing.T, endpoint, password string) (*amqp.Connection, *amqp.Channel) {
	t.Helper()
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}

	u.User = url.UserPassword("admin", password)
	conn, err := amqp.Dial(u.String())
	if err != nil {
		t.Fatal(err)
	}

	ch, err := conn.Channel()
	if err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}

	return conn, ch
}

func TestMQDockerRabbitLifecycle(t *testing.T) {
	if os.Getenv("TINYSTACK_DOCKER_TESTS") != "1" {
		t.Skip("set TINYSTACK_DOCKER_TESTS=1")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	cfg := testConfig()
	cfg.MQ.Brokers = config.BrokersDocker
	dir := t.TempDir()
	store, err := sqlite.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}

	s, stop := serverStore(t, cfg, &fakeRuntime{}, store)
	t.Cleanup(func() {
		stop()
		_ = store.Close()
	})
	c := mqClient(s, "test", "us-east-1")
	in := mqInput()
	in.EngineType, in.EngineVersion, in.Logs.Audit = types.EngineTypeRabbitmq, new("3.13"), nil
	created, err := c.CreateBroker(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		stop()
		mqDockerResources(t, *created.BrokerArn, true)
	})
	initial := mqBroker(t, c, created.BrokerId)
	if initial.BrokerState != types.BrokerStateCreationInProgress || len(initial.BrokerInstances) != 0 {
		t.Fatalf("initial %+v", initial)
	}

	b := awaitMQDocker(t, ctx, c, created.BrokerId)
	if len(b.BrokerInstances) != 1 {
		t.Fatalf("instances %+v", b.BrokerInstances)
	}

	_, err = c.UpdateBroker(ctx, &sdk.UpdateBrokerInput{BrokerId: created.BrokerId, EngineVersion: new("99.0")})
	mqStatus(t, err, "BadRequestException", 400)
	updated, err := c.UpdateBroker(
		ctx,
		&sdk.UpdateBrokerInput{BrokerId: created.BrokerId, EngineVersion: new("3.13.7")},
	)
	if err != nil || *updated.EngineVersion != "3.13.7" {
		t.Fatalf("pending update %+v %v", updated, err)
	}

	pending := mqBroker(t, c, created.BrokerId)
	if *pending.EngineVersion != "3.13" || pending.PendingEngineVersion == nil ||
		*pending.PendingEngineVersion != "3.13.7" {
		t.Fatalf("pending version %+v", pending)
	}

	size, inspectErr := dockercli.Run(
		ctx,
		nil,
		"image",
		"inspect",
		"--format",
		"{{.Size}}",
		"rabbitmq:3.13.7-management",
	)
	if inspectErr != nil {
		t.Fatal(inspectErr)
	}

	t.Logf("rabbitmq:3.13.7-management: %s bytes", size)
	endpoint := b.BrokerInstances[0].Endpoints[0]
	wrong, _ := url.Parse(endpoint)
	wrong.User = url.UserPassword("admin", "wrong-password")
	if conn, dialErr := amqp.Dial(wrong.String()); dialErr == nil {
		_ = conn.Close()
		t.Fatal("wrong password accepted")
	}

	conn, ch := mqAMQP(t, endpoint, *in.Users[0].Password)
	if _, err = ch.QueueDeclare("durable", true, false, false, false, nil); err != nil {
		t.Fatal(err)
	}

	if err = ch.Confirm(false); err != nil {
		t.Fatal(err)
	}

	confirms := ch.NotifyPublish(make(chan amqp.Confirmation, 1))
	if err = ch.PublishWithContext(ctx, "", "durable", false, false, amqp.Publishing{
		DeliveryMode: amqp.Persistent, Body: []byte("survives-restart"),
	}); err != nil {
		t.Fatal(err)
	}

	select {
	case confirmation := <-confirms:
		if !confirmation.Ack {
			t.Fatal("publish nack")
		}

	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	_ = ch.Close()
	_ = conn.Close()
	if _, err = c.RebootBroker(ctx, &sdk.RebootBrokerInput{BrokerId: created.BrokerId}); err != nil {
		t.Fatal(err)
	}

	rebooting := mqBroker(t, c, created.BrokerId)
	if rebooting.BrokerState != types.BrokerStateRebootInProgress {
		t.Fatalf("state %s", rebooting.BrokerState)
	}

	b = awaitMQDocker(t, ctx, c, created.BrokerId)
	if *b.EngineVersion != "3.13.7" || b.PendingEngineVersion != nil {
		t.Fatalf("reboot version %+v", b)
	}

	stop()
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = sqlite.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}

	s, stop = serverStore(t, cfg, &fakeRuntime{}, store)
	c = mqClient(s, "test", "us-east-1")
	b = awaitMQDocker(t, ctx, c, created.BrokerId)
	conn, ch = mqAMQP(t, b.BrokerInstances[0].Endpoints[0], *in.Users[0].Password)
	message, ok, err := ch.Get("durable", true)
	if err != nil || !ok || string(message.Body) != "survives-restart" {
		t.Fatalf("durable message %v %v %s", err, ok, message.Body)
	}

	_ = ch.Close()
	_ = conn.Close()
	if _, err = c.DeleteBroker(ctx, &sdk.DeleteBrokerInput{BrokerId: created.BrokerId}); err != nil {
		t.Fatal(err)
	}

	for ctx.Err() == nil {
		nc, nv := mqDockerResources(t, *created.BrokerArn, false)
		if nc == 0 && nv == 0 {
			return
		}

		time.Sleep(time.Second)
	}

	t.Fatal("delete retained resources")
}

func TestMQDockerActiveMQ(t *testing.T) {
	if os.Getenv("TINYSTACK_DOCKER_TESTS") != "1" {
		t.Skip("set TINYSTACK_DOCKER_TESTS=1")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	cfg := testConfig()
	cfg.MQ.Brokers = config.BrokersDocker
	store, err := sqlite.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	defer store.Close()
	s, stop := serverStore(t, cfg, &fakeRuntime{}, store)
	defer stop()
	c := mqClient(s, "test", "us-east-1")
	in := mqInput()
	in.Users[0].ConsoleAccess = new(true)
	in.Users[0].Password = new("valid-${jetty.port}-password")
	in.Users[0].Groups = []string{"operators"}
	in.Users = append(in.Users, types.User{
		Username: new(
			"worker",
		),
		Password:      new("worker-valid-password"),
		ConsoleAccess: new(false),
		Groups:        []string{"workers"},
	})
	created, err := c.CreateBroker(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	defer func() {
		stop()
		mqDockerResources(t, *created.BrokerArn, true)
	}()
	b := awaitMQDocker(t, ctx, c, created.BrokerId)
	instance := b.BrokerInstances[0]
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, *instance.ConsoleURL+"/admin/", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	req.SetBasicAuth(*in.Users[0].Username, *in.Users[0].Password)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("console HTTP %d", res.StatusCode)
	}

	req.SetBasicAuth("worker", "worker-valid-password")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("non-console user: HTTP %d", res.StatusCode)
	}

	endpoint, _ := url.Parse(instance.Endpoints[2])
	conn, err := net.DialTimeout("tcp", endpoint.Host, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	defer conn.Close()
	if err = conn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatal(err)
	}

	reader := bufio.NewReader(conn)
	_, err = fmt.Fprintf(
		conn,
		"CONNECT\naccept-version:1.2\nhost:localhost\nlogin:admin\npasscode:%s\n\n\x00",
		*in.Users[0].Password,
	)
	if err != nil {
		t.Fatal(err)
	}

	frame, err := reader.ReadString(0)
	if err != nil || !strings.Contains(frame, "CONNECTED") {
		t.Fatalf("connect %q %v", frame, err)
	}

	_, err = io.WriteString(
		conn,
		"SEND\ndestination:/queue/test\ncontent-length:5\n\nhello\x00"+
			"SUBSCRIBE\nid:one\ndestination:/queue/test\nack:auto\n\n\x00",
	)
	if err != nil {
		t.Fatal(err)
	}

	frame, err = reader.ReadString(0)
	if err != nil || !strings.Contains(frame, "MESSAGE") || !strings.Contains(frame, "hello") {
		t.Fatalf("message %q %v", frame, err)
	}

	_, err = io.WriteString(conn, "SEND\ndestination:/queue/upgrade\npersistent:true\nreceipt:stored\n\ndurable\x00")
	if err != nil {
		t.Fatal(err)
	}

	frame, err = reader.ReadString(0)
	if err != nil || !strings.Contains(frame, "RECEIPT") {
		t.Fatalf("persist receipt %q %v", frame, err)
	}

	_ = conn.Close()
	_, err = c.UpdateBroker(ctx, &sdk.UpdateBrokerInput{BrokerId: created.BrokerId, EngineVersion: new("5.19")})
	if err != nil {
		t.Fatal(err)
	}

	pending := mqBroker(t, c, created.BrokerId)
	if *pending.EngineVersion != "5.18" || pending.PendingEngineVersion == nil ||
		*pending.PendingEngineVersion != "5.19" {
		t.Fatalf("ActiveMQ pending version %+v", pending)
	}

	if _, err = c.RebootBroker(ctx, &sdk.RebootBrokerInput{BrokerId: created.BrokerId}); err != nil {
		t.Fatal(err)
	}

	b = awaitMQDocker(t, ctx, c, created.BrokerId)
	if *b.EngineVersion != "5.19" || b.PendingEngineVersion != nil {
		t.Fatalf("ActiveMQ reboot version %+v", b)
	}

	endpoint, _ = url.Parse(b.BrokerInstances[0].Endpoints[2])
	upgraded, err := net.DialTimeout("tcp", endpoint.Host, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	defer upgraded.Close()
	if err = upgraded.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatal(err)
	}

	upgradedReader := bufio.NewReader(upgraded)
	_, err = fmt.Fprintf(upgraded,
		"CONNECT\naccept-version:1.2\nhost:localhost\nlogin:admin\npasscode:%s\n\n\x00", *in.Users[0].Password)
	if err != nil {
		t.Fatal(err)
	}

	frame, err = upgradedReader.ReadString(0)
	if err != nil || !strings.Contains(frame, "CONNECTED") {
		t.Fatalf("upgraded connect %q %v", frame, err)
	}

	_, err = io.WriteString(upgraded, "SUBSCRIBE\nid:upgraded\ndestination:/queue/upgrade\nack:auto\n\n\x00")
	if err != nil {
		t.Fatal(err)
	}

	frame, err = upgradedReader.ReadString(0)
	if err != nil || !strings.Contains(frame, "MESSAGE") || !strings.Contains(frame, "durable") {
		t.Fatalf("upgraded durable message %q %v", frame, err)
	}

	for _, image := range []string{"apache/activemq-classic:5.18.7", "apache/activemq-classic:5.19.0"} {
		size, inspectErr := dockercli.Run(ctx, nil, "image", "inspect", "--format", "{{.Size}}", image)
		if inspectErr != nil {
			t.Fatal(inspectErr)
		}

		t.Logf("%s: %s bytes", image, size)
	}

	if status, body := post(t, s, "/_tinystack/reset", nil); status != 200 {
		t.Fatalf("reset %d %s", status, body)
	}

	if nc, nv := mqDockerResources(t, *created.BrokerArn, false); nc != 0 || nv != 0 {
		t.Fatal("reset retained resources")
	}
}

func TestMQDockerOwnedOrphans(t *testing.T) {
	if os.Getenv("TINYSTACK_DOCKER_TESTS") != "1" {
		t.Skip("set TINYSTACK_DOCKER_TESTS=1")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	store, err := sqlite.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	defer store.Close()
	owner := fmt.Sprintf("mq-test-%d", time.Now().UnixNano())
	err = store.Update(ctx, func(tx storage.WriteTx) error { return storage.Write(tx, "aws/mq/runtime-owner", owner) })
	if err != nil {
		t.Fatal(err)
	}

	orphan, foreign := owner+"-orphan", owner+"-foreign"
	defer func() {
		for _, name := range []string{orphan, foreign} {
			_, _ = dockercli.Run(context.Background(), nil, "volume", "rm", name)
		}
	}()
	for _, resource := range []struct{ name, identity string }{{orphan, owner}, {foreign, owner + "-other"}} {
		_, err = dockercli.Run(
			ctx,
			nil,
			"volume",
			"create",
			"--label",
			"tinystack.mq="+resource.identity+"|missing",
			resource.name,
		)
		if err != nil {
			t.Fatal(err)
		}

	}

	cfg := testConfig()
	cfg.MQ.Brokers = config.BrokersDocker
	_, stop := serverStore(t, cfg, &fakeRuntime{}, store)
	defer stop()
	for dockerExists(ctx, "volume", orphan) && ctx.Err() == nil {
		time.Sleep(time.Second)
	}

	if ctx.Err() != nil || !dockerExists(ctx, "volume", foreign) {
		t.Fatal("orphan cleanup crossed store ownership")
	}
}
