package kinesis

import (
	"context"
	"regexp"
	"strconv"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/kinesis"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// maxConsumers is how many enhanced fan-out consumers a stream may have.
const maxConsumers = 20

var consumerName = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,128}$`)

// consumerTarget returns the stream and consumer name a request names by
// StreamARN and ConsumerName, or by ConsumerARN.
func consumerTarget(streamARN, name, consumerARN *string) (stream *string, consumer string, err error) {
	if consumerARN != nil {
		streamPart, rest, ok := strings.Cut(*consumerARN, "/consumer/")
		consumer, _, _ = strings.Cut(rest, ":")
		if !ok || consumer == "" {
			return nil, "", invalidArgument("Invalid ConsumerARN: %s", *consumerARN)
		}

		return &streamPart, consumer, nil
	}

	if streamARN == nil || name == nil {
		return nil, "", invalidArgument("Either ConsumerARN, or StreamARN and ConsumerName, are required")
	}

	return streamARN, *name, nil
}

func (c *consumer) describe(streamARN string) *api.ConsumerDescription {
	return &api.ConsumerDescription{
		ConsumerName: new(c.Name), ConsumerARN: new(c.ARN), ConsumerStatus: new(statusActive),
		ConsumerCreationTimestamp: new(c.Created), StreamARN: new(streamARN),
	}
}

func (s *Service) RegisterStreamConsumer(
	ctx context.Context,
	in *api.RegisterStreamConsumerInput,
) (*api.RegisterStreamConsumerOutput, error) {
	name := value(in.ConsumerName)
	if !consumerName.MatchString(name) {
		return nil, invalidArgument("ConsumerName must be 1 to 128 letters, digits, underscores, periods, and hyphens")
	}

	var c consumer
	err := s.change(ctx, nil, in.StreamARN, func(_ storage.WriteTx, scope request.Scope, st *stream) error {
		for _, existing := range st.Consumers {
			if existing.Name == name {
				return awserror.New("ResourceInUseException", "Consumer "+name+" already exists")
			}
		}

		if len(st.Consumers) == maxConsumers {
			return awserror.New("LimitExceededException", "A stream can have at most 20 consumers")
		}

		created := unix(s.now())
		c = consumer{
			Name: name, Created: created,
			ARN: StreamARN(scope, st.Name) + "/consumer/" + name + ":" + strconv.FormatInt(int64(created), 10),
		}
		st.Consumers = append(st.Consumers, c)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.RegisterStreamConsumerOutput{Consumer: &api.Consumer{
		ConsumerName: new(c.Name), ConsumerARN: new(c.ARN), ConsumerStatus: new(statusActive),
		ConsumerCreationTimestamp: new(c.Created),
	}}, nil
}

func findConsumer(st *stream, name string) (int, error) {
	for i := range st.Consumers {
		if st.Consumers[i].Name == name {
			return i, nil
		}
	}

	return -1, awserror.New("ResourceNotFoundException", "Consumer "+name+" does not exist")
}

func (s *Service) DescribeStreamConsumer(
	ctx context.Context,
	in *api.DescribeStreamConsumerInput,
) (*api.DescribeStreamConsumerOutput, error) {
	arn, name, err := consumerTarget(in.StreamARN, in.ConsumerName, in.ConsumerARN)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeStreamConsumerOutput{}
	err = s.view(ctx, nil, arn, func(_ storage.ReadTx, _ request.Scope, st *stream) error {
		i, findErr := findConsumer(st, name)
		if findErr == nil {
			out.ConsumerDescription = st.Consumers[i].describe(*arn)
		}

		return findErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) ListStreamConsumers(
	ctx context.Context,
	in *api.ListStreamConsumersInput,
) (*api.ListStreamConsumersOutput, error) {
	out := &api.ListStreamConsumersOutput{Consumers: []api.Consumer{}}
	err := s.view(ctx, nil, in.StreamARN, func(_ storage.ReadTx, _ request.Scope, st *stream) error {
		for _, c := range st.Consumers {
			out.Consumers = append(out.Consumers, api.Consumer{
				ConsumerName: new(c.Name), ConsumerARN: new(c.ARN), ConsumerStatus: new(statusActive),
				ConsumerCreationTimestamp: new(c.Created),
			})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DeregisterStreamConsumer(
	ctx context.Context,
	in *api.DeregisterStreamConsumerInput,
) (*api.DeregisterStreamConsumerOutput, error) {
	arn, name, err := consumerTarget(in.StreamARN, in.ConsumerName, in.ConsumerARN)
	if err != nil {
		return nil, err
	}

	err = s.change(ctx, nil, arn, func(_ storage.WriteTx, _ request.Scope, st *stream) error {
		i, findErr := findConsumer(st, name)
		if findErr == nil {
			st.Consumers = append(st.Consumers[:i], st.Consumers[i+1:]...)
		}

		return findErr
	})
	if err != nil {
		return nil, err
	}

	return &api.DeregisterStreamConsumerOutput{}, nil
}
