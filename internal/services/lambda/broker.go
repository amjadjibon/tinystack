package lambda

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
)

// brokerResources maps a broker service to the resource prefix of its ARNs.
var brokerResources = map[string]string{"kafka": "cluster/", "mq": "broker:"}

// brokerARN reports whether arn names a broker of service, "kafka" or "mq".
func brokerARN(arn, service string) bool {
	p := strings.SplitN(arn, ":", 6)
	return len(p) == 6 && strings.HasPrefix(arn, "arn:") && p[2] == service && p[3] != "" && p[4] != "" &&
		strings.HasPrefix(p[5], brokerResources[service])
}

func (mp *mapping) brokerSource() bool {
	return mp.SelfManagedEventSource != nil || brokerARN(mp.EventSourceArn, "kafka") ||
		brokerARN(mp.EventSourceArn, "mq")
}

func validateBrokerFields(in *api.CreateEventSourceMappingRequest) error {
	arn := common.Value(in.EventSourceArn)
	kafka := brokerARN(arn, "kafka") || in.SelfManagedEventSource != nil
	mq := brokerARN(arn, "mq")
	if in.SelfManagedEventSource != nil && arn != "" {
		return invalid("SelfManagedEventSource and EventSourceArn cannot be combined")
	}

	if (!kafka && in.Topics != nil) || (!mq && in.Queues != nil) ||
		(in.AmazonManagedKafkaEventSourceConfig != nil && !brokerARN(arn, "kafka")) ||
		(in.SelfManagedKafkaEventSourceConfig != nil && in.SelfManagedEventSource == nil) ||
		(!kafka && !mq && in.SourceAccessConfigurations != nil) {
		return invalid("Kafka and MQ fields must match the event source type")
	}

	return nil
}

func (s *Service) configureBroker(ctx context.Context, scope request.Scope,
	in *api.CreateEventSourceMappingRequest, mp *mapping,
) error {
	if mp.EventSourceArn != "" {
		parts := strings.SplitN(mp.EventSourceArn, ":", 6)
		if parts[3] != scope.Region || parts[4] != scope.AccountID {
			return invalid("The broker must be in the function account and region")
		}
	}

	mp.Topics, mp.Queues = in.Topics, in.Queues
	mp.SelfManagedEventSource = in.SelfManagedEventSource
	mp.AmazonManagedKafkaEventSourceConfig = in.AmazonManagedKafkaEventSourceConfig
	mp.SelfManagedKafkaEventSourceConfig = in.SelfManagedKafkaEventSourceConfig
	mp.SourceAccessConfigurations = in.SourceAccessConfigurations
	mp.DefaultBrokerWindow = in.MaximumBatchingWindowInSeconds == nil
	mp.BatchSize = 100
	if in.BatchSize != nil {
		mp.BatchSize = *in.BatchSize
	}

	mq := brokerARN(mp.EventSourceArn, "mq")
	if mq && (in.MaximumRetryAttempts != nil || in.FunctionResponseTypes != nil ||
		in.DestinationConfig != nil || in.BisectBatchOnFunctionError != nil || in.MaximumRecordAgeInSeconds != nil ||
		in.ProvisionedPollerConfig != nil) {
		return invalid("MQ does not support retry controls or destinations; use broker-side dead-lettering")
	}

	if mq {
		if len(mp.Queues) != 1 || mp.Queues[0] == "" || in.StartingPosition != nil || in.FilterCriteria != nil {
			return invalid(
				"MQ requires exactly one nonempty Queues entry and does not support StartingPosition or FilterCriteria",
			)
		}

		out, err := s.brokerCall(
			ctx,
			scope,
			mp,
			"mq",
			"DescribeBroker",
			map[string]any{"BrokerId": brokerID(mp.EventSourceArn)},
		)
		if err != nil {
			return invalid("Cannot describe MQ broker: %s", err)
		}

		if out["EngineType"] != "RABBITMQ" {
			return invalid("ActiveMQ event source mappings are not supported; only RabbitMQ is supported")
		}
	} else {
		if len(mp.Topics) != 1 || mp.Topics[0] == "" {
			return invalid("Kafka requires exactly one nonempty Topics entry")
		}

		mp.StartingPosition = common.Value(in.StartingPosition)
		if mp.StartingPosition != positionTrim && mp.StartingPosition != positionLatest {
			return invalid("Kafka StartingPosition must be TRIM_HORIZON or LATEST")
		}

		if mp.SelfManagedEventSource != nil {
			endpoints := mp.SelfManagedEventSource.Endpoints
			if len(endpoints) != 1 || len(endpoints["KAFKA_BOOTSTRAP_SERVERS"]) == 0 {
				return invalid("SelfManagedEventSource requires KAFKA_BOOTSTRAP_SERVERS endpoints")
			}

			for _, endpoint := range endpoints["KAFKA_BOOTSTRAP_SERVERS"] {
				host, port, endpointErr := net.SplitHostPort(endpoint)
				if endpointErr != nil || host == "" {
					return invalid("Invalid Kafka bootstrap endpoint: %s", endpoint)
				}

				if number, portErr := strconv.Atoi(port); portErr != nil || number < 1 || number > 65535 {
					return invalid("Invalid Kafka bootstrap port: %s", port)
				}
			}
		}
	}

	if group := consumerGroup(mp); group != nil && strings.TrimSpace(*group) == "" {
		return invalid("ConsumerGroupId must not be empty")
	}

	if !mq {
		mp.MaximumRetryAttempts = new(int64(-1))
		if in.MaximumRetryAttempts != nil {
			mp.MaximumRetryAttempts = in.MaximumRetryAttempts
		}

		if err := kafkaProvisioned(mp, in.ProvisionedPollerConfig, in.MaximumRetryAttempts != nil ||
			in.BisectBatchOnFunctionError != nil || in.MaximumRecordAgeInSeconds != nil || mp.ReportFailures); err != nil {
			return err
		}
	}

	if err := validateAccess(mp); err != nil {
		return err
	}

	if _, _, err := s.brokerCredentials(ctx, scope, mp); err != nil {
		return invalid("Cannot read broker credentials: %s", err)
	}

	return mp.checkLimits()
}

func brokerID(arn string) string {
	_, id, _ := strings.Cut(arn, ":broker:")
	_, id, _ = strings.Cut(id, ":")
	return id
}

func validateAccess(mp *mapping) error {
	seen := map[string]bool{}
	for _, access := range mp.SourceAccessConfigurations {
		typ, uri := common.Value(access.Type), common.Value(access.URI)
		supported := typ == "BASIC_AUTH" || (!brokerARN(mp.EventSourceArn, "mq") &&
			(typ == "SASL_SCRAM_256_AUTH" || typ == "SASL_SCRAM_512_AUTH"))
		if !supported || uri == "" || seen[typ] || len(seen) > 0 {
			return invalid(
				"Unsupported or duplicate SourceAccessConfiguration type %s; supply one credential secret",
				typ,
			)
		}

		seen[typ] = true
	}

	if brokerARN(mp.EventSourceArn, "mq") && !seen["BASIC_AUTH"] {
		return invalid("MQ requires SourceAccessConfigurations BASIC_AUTH with a Secrets Manager secret")
	}

	return nil
}

func updateBrokerFields(in *api.UpdateEventSourceMappingRequest, mp *mapping) error {
	if in.AmazonManagedKafkaEventSourceConfig != nil || in.SelfManagedKafkaEventSourceConfig != nil {
		return invalid("Kafka consumer group configuration cannot be updated")
	}

	if !mp.brokerSource() {
		if in.ProvisionedPollerConfig != nil {
			return invalid("ProvisionedPollerConfig is only supported for Kafka")
		}

		if in.SourceAccessConfigurations != nil {
			return invalid("SourceAccessConfigurations requires Kafka or MQ")
		}

		return nil
	}

	if brokerARN(mp.EventSourceArn, "mq") &&
		(in.MaximumRetryAttempts != nil || in.DestinationConfig != nil || in.BisectBatchOnFunctionError != nil ||
			in.MaximumRecordAgeInSeconds != nil || len(in.FunctionResponseTypes) > 0 ||
			in.FilterCriteria != nil || in.ProvisionedPollerConfig != nil) {
		return invalid("MQ does not support retry controls or destinations; use broker-side dead-lettering")
	}

	if !brokerARN(mp.EventSourceArn, "mq") {
		if err := kafkaProvisioned(mp, in.ProvisionedPollerConfig, in.MaximumRetryAttempts != nil ||
			in.BisectBatchOnFunctionError != nil || in.MaximumRecordAgeInSeconds != nil ||
			len(in.FunctionResponseTypes) > 0); err != nil {
			return err
		}
	}

	if in.SourceAccessConfigurations != nil {
		mp.SourceAccessConfigurations = in.SourceAccessConfigurations
	}

	return validateAccess(mp)
}

func (s *Service) brokerCall(ctx context.Context, scope request.Scope, mp *mapping, service, operation string,
	params map[string]any,
) (map[string]any, error) {
	if s.caller == nil {
		return nil, fmt.Errorf("broker services are unavailable")
	}

	callCtx := request.WithMetadata(ctx, request.Metadata{Scope: scope})
	d, err := s.executionDelivery(callCtx, scope, mp.FunctionName,
		&catalog.DeliveryRole{Service: service, Operation: operation, Params: params})
	if err != nil {
		return nil, err
	}

	return s.authorizer.Call(callCtx, s.caller, d)
}

func (s *Service) brokerCredentials(
	ctx context.Context,
	scope request.Scope,
	mp *mapping,
) (username, password string, err error) {
	if len(mp.SourceAccessConfigurations) == 0 {
		return "", "", nil
	}

	out, err := s.brokerCall(ctx, scope, mp, "secretsmanager", "GetSecretValue",
		map[string]any{"SecretId": common.Value(mp.SourceAccessConfigurations[0].URI)})
	if err != nil {
		return "", "", err
	}

	text, _ := out["SecretString"].(string)
	var credentials struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}

	decodeErr := json.Unmarshal([]byte(text), &credentials)
	if decodeErr != nil || credentials.Username == "" || credentials.Password == "" {
		return "", "", fmt.Errorf("secret must contain nonempty username and password strings")
	}

	return credentials.Username, credentials.Password, nil
}

//nolint:gocritic // The poller owns its mapping copy.
func (s *Service) pollBroker(ctx context.Context, scope request.Scope, mp mapping) {
	for ctx.Err() == nil {
		var err error
		if brokerARN(mp.EventSourceArn, "mq") {
			err = s.consumeRabbit(ctx, scope, &mp)
		} else {
			err = s.consumeKafka(ctx, scope, &mp)
		}

		if err != nil && ctx.Err() == nil {
			// Authentication errors must never expose secret values in API diagnostics.
			s.recordResult(ctx, scope, mp.UUID, "PROBLEM: Broker unavailable or authentication failed")
		}

		sleep(ctx, pollBackoff)
	}
}

func sameBrokerEndpoints(a, b *mapping) bool {
	if a.SelfManagedEventSource == nil || b.SelfManagedEventSource == nil {
		return a.SelfManagedEventSource == nil && b.SelfManagedEventSource == nil
	}

	return slices.Equal(a.SelfManagedEventSource.Endpoints["KAFKA_BOOTSTRAP_SERVERS"],
		b.SelfManagedEventSource.Endpoints["KAFKA_BOOTSTRAP_SERVERS"])
}

func consumerGroup(mp *mapping) *string {
	if mp.AmazonManagedKafkaEventSourceConfig != nil {
		return mp.AmazonManagedKafkaEventSourceConfig.ConsumerGroupId
	}

	if mp.SelfManagedKafkaEventSourceConfig != nil {
		return mp.SelfManagedKafkaEventSourceConfig.ConsumerGroupId
	}

	return nil
}
