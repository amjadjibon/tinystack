package mq

import (
	"strings"
	"testing"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/mq"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestBrokerImage(t *testing.T) {
	for _, test := range []struct{ engine, version, image string }{
		{rabbitMQ, "3.13", "rabbitmq:3.13.7-management"},
		{rabbitMQ, "4.0", "rabbitmq:4.0.9-management"},
		{rabbitMQ, "4.1", "rabbitmq:4.1.4-management"},
		{activeMQ, "5.17", "apache/activemq-classic:5.17.6"},
		{activeMQ, "5.18", "apache/activemq-classic:5.18.7"},
		{activeMQ, "5.19", "apache/activemq-classic:5.19.0"},
	} {
		image, err := brokerImage(test.engine, test.version)
		if err != nil || image != test.image {
			t.Fatalf("mapping: %s %v", image, err)
		}
	}

	if _, err := brokerImage(rabbitMQ, "unknown"); err == nil {
		t.Fatal("unknown version accepted")
	}
}

func TestActiveConfigAndEndpoints(t *testing.T) {
	broker, realm := activeConfig([]api.User{
		{
			Username:      new("admin"),
			Password:      new(`secure<&"password`),
			ConsoleAccess: new(true),
			Groups:        []string{"team"},
		},
		{Username: new("messaging"), Password: new("hidden-password")},
	})
	if !strings.Contains(broker, "c2VjdXJlPCYicGFzc3dvcmQ=") || !strings.Contains(broker, "dGVhbQ==") ||
		!strings.Contains(realm, "admin:") || strings.Contains(realm, "messaging") {
		t.Fatalf("config %s %s", broker, realm)
	}

	ports := map[int64]int64{61616: 10001, 5672: 10002, 61613: 10003, 1883: 10004, 61614: 10005, 8161: 10006}
	endpoints := brokerEndpoints(activeMQ, ports)
	if len(endpoints.Endpoints) != 5 || endpoints.Endpoints[0] != "tcp://127.0.0.1:10001" ||
		*endpoints.ConsoleURL != "http://127.0.0.1:10006" {
		t.Fatalf("endpoints %+v", endpoints)
	}

	rabbit := brokerEndpoints(rabbitMQ, map[int64]int64{5672: 20001, 15672: 20002})
	if rabbit.Endpoints[0] != "amqp://127.0.0.1:20001" || *rabbit.ConsoleURL != "http://127.0.0.1:20002" {
		t.Fatalf("rabbit endpoints %+v", rabbit)
	}
}

func TestCompatibleUpgrade(t *testing.T) {
	for _, test := range []struct {
		engine, before, after string
		want                  bool
	}{
		{activeMQ, "5.18", "5.19", true},
		{activeMQ, "5.19", "5.18", false},
		{rabbitMQ, "3.13", "3.13.7", true},
		{rabbitMQ, "3.13", "4.0", false},
		{activeMQ, "5.18", "unknown", false},
	} {
		if compatibleUpgrade(test.engine, test.before, test.after) != test.want {
			t.Fatalf("upgrade %+v", test)
		}
	}
}

func TestRebootModelValidation(t *testing.T) {
	s := New(&memory.Store{}, nil)
	_, err := api.Descriptor(s).Operations["RebootBroker"].Invoke(localContext(), &api.RebootBrokerRequest{})
	wantError(t, err, "BadRequestException", 400)
}

func TestDockerPendingVersionAndRebootState(t *testing.T) {
	ctx := localContext()
	s := New(&memory.Store{}, nil)
	s.runtime = &brokerRuntime{owner: "unit-test-store"}
	created, err := s.CreateBroker(ctx, brokerInput())
	if err != nil {
		t.Fatal(err)
	}

	initial := describe(t, s, created.BrokerId)
	if *initial.BrokerState != "CREATION_IN_PROGRESS" {
		t.Fatalf("initial state %s", *initial.BrokerState)
	}

	_, err = s.RebootBroker(ctx, &api.RebootBrokerRequest{BrokerId: created.BrokerId})
	wantError(t, err, "BadRequestException", 400)
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		t.Fatal(err)
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, readErr := read(tx, scope, *created.BrokerId)
		if readErr != nil {
			return readErr
		}

		r.Broker.BrokerState = new("RUNNING")
		return save(tx, scope, r)
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.UpdateBroker(ctx, &api.UpdateBrokerRequest{BrokerId: created.BrokerId, EngineVersion: new("5.19")})
	if err != nil {
		t.Fatal(err)
	}

	pending := describe(t, s, created.BrokerId)
	if *pending.EngineVersion != "5.18" || pending.PendingEngineVersion == nil ||
		*pending.PendingEngineVersion != "5.19" {
		t.Fatalf("pending version %+v", pending)
	}

	if _, err = s.RebootBroker(ctx, &api.RebootBrokerRequest{BrokerId: created.BrokerId}); err != nil {
		t.Fatal(err)
	}

	rebooting := describe(t, s, created.BrokerId)
	if *rebooting.BrokerState != "REBOOT_IN_PROGRESS" || len(rebooting.BrokerInstances) != 0 {
		t.Fatalf("reboot state %+v", rebooting)
	}
}

func TestDockerRejectsUserMutationsAndCustomConfigurations(t *testing.T) {
	s := New(&memory.Store{}, nil)
	s.runtime = &brokerRuntime{owner: "unit-test-store"}
	ctx := localContext()
	created, err := s.CreateBroker(ctx, brokerInput())
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.CreateUser(
		ctx,
		&api.CreateUserRequest{
			BrokerId: created.BrokerId,
			Username: new("second"),
			Password: new("valid-password-123"),
		},
	)
	wantError(t, err, "BadRequestException", 400)
	_, err = s.UpdateUser(
		ctx,
		&api.UpdateUserRequest{BrokerId: created.BrokerId, Username: new("admin"), Password: new("valid-password-123")},
	)
	wantError(t, err, "BadRequestException", 400)
	_, err = s.DeleteUser(ctx, &api.DeleteUserRequest{BrokerId: created.BrokerId, Username: new("admin")})
	wantError(t, err, "BadRequestException", 400)
	_, err = s.CreateConfiguration(
		ctx,
		&api.CreateConfigurationRequest{Name: new("custom"), EngineType: new(activeMQ), EngineVersion: new("5.18")},
	)
	wantError(t, err, "BadRequestException", 400)
	_, err = s.UpdateConfiguration(
		ctx,
		&api.UpdateConfigurationRequest{ConfigurationId: new("c-missing"), Data: new("data")},
	)
	wantError(t, err, "BadRequestException", 400)
	_, err = s.UpdateBroker(
		ctx,
		&api.UpdateBrokerRequest{
			BrokerId:      created.BrokerId,
			Configuration: &api.ConfigurationId{Id: new("c-missing"), Revision: new(int64(1))},
		},
	)
	wantError(t, err, "BadRequestException", 400)
}
