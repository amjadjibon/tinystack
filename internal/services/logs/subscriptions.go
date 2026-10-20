package logs

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"regexp"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/logs"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// checkMessage is the data of the test message sent to a destination as a subscription filter is saved.
const checkMessage = "CWL CONTROL MESSAGE: Checking health of destination."

const (
	// maxSubscriptions is how many subscription filters a group may have.
	maxSubscriptions     = 2
	distributionByStream = "ByLogStream"
	distributionRandom   = "Random"
)

var (
	filterName = regexp.MustCompile(`^[^:*]{1,512}$`)
	// destination matches the ARNs subscription filters deliver to: a Lambda
	// function, a Kinesis stream, or a Firehose delivery stream.
	destination = regexp.MustCompile(
		`^arn:aws[a-zA-Z-]*:(lambda:[a-z0-9-]+:\d{12}:function:.+|kinesis:[a-z0-9-]+:\d{12}:stream/.+|` +
			`firehose:[a-z0-9-]+:\d{12}:deliverystream/.+)$`,
	)
)

// subscription is a subscription filter, kept in its group's record.
type subscription struct {
	Name         string
	Pattern      string
	Destination  string
	Role         string `json:",omitempty"`
	Distribution string
	Created      int64
}

// UseCaller lets subscription filters deliver to Lambda, Kinesis, and Firehose.
// Call it before serving requests.
func (s *Service) UseCaller(caller *catalog.Caller) { s.caller = caller }

// UseServiceAuthorizer makes subscription filters require a Lambda destination's resource policy to allow
// logs.amazonaws.com, and a Kinesis or Firehose destination's role to trust it and to allow the write. Call it before
// serving requests.
func (s *Service) UseServiceAuthorizer(authorizer catalog.ServiceAuthorizer) {
	s.authorizer = authorizer
}

// delivered is what a write appended to a stream, for its group's filters.
type delivered struct {
	scope  request.Scope
	group  *group
	stream string
	events []event
}

func (s *Service) PutSubscriptionFilter(
	ctx context.Context,
	in *api.PutSubscriptionFilterRequest,
) (*api.PutSubscriptionFilterOutput, error) {
	name := common.Value(in.FilterName)
	if !filterName.MatchString(name) {
		return nil, invalid("filterName must be 1 to 512 characters without ':' or '*'")
	}

	if _, err := compilePattern(common.Value(in.FilterPattern)); err != nil {
		return nil, err
	}

	if len(common.Value(in.FilterPattern)) > 1024 {
		return nil, invalid("filterPattern must be at most 1024 characters")
	}

	arn := common.Value(in.DestinationArn)
	if !destination.MatchString(arn) {
		return nil, invalid("destinationArn must be a Lambda function, Kinesis stream, or Firehose delivery stream")
	}

	distribution := common.Value(in.Distribution)
	switch distribution {
	case "":
		distribution = distributionByStream
	case distributionByStream, distributionRandom:
	default:
		return nil, invalid("distribution must be ByLogStream or Random")
	}

	var scope request.Scope
	err := s.view(ctx, in.LogGroupName, nil, func(_ storage.ReadTx, groupScope request.Scope, _ *group) error {
		scope = groupScope
		return nil
	})
	if err != nil {
		return nil, err
	}

	role := common.Value(in.RoleArn)
	if checkErr := s.checkDestination(ctx, scope, common.Value(in.LogGroupName), arn, role); checkErr != nil {
		return nil, checkErr
	}

	sub := subscription{
		Name: name, Pattern: common.Value(in.FilterPattern), Destination: arn, Role: role,
		Distribution: distribution, Created: s.now().UnixMilli(),
	}
	err = s.change(ctx, in.LogGroupName, func(g *group) error {
		if i := slices.IndexFunc(g.Subscriptions, func(o subscription) bool { return o.Name == name }); i >= 0 {
			g.Subscriptions[i] = sub
			return nil
		}

		if len(g.Subscriptions) == maxSubscriptions {
			return awserror.New("LimitExceededException", "Resource limit exceeded.")
		}

		g.Subscriptions = append(g.Subscriptions, sub)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.PutSubscriptionFilterOutput{}, nil
}

// checkDestination checks, as AWS does with the test message it sends as a filter is saved, that CloudWatch Logs may
// deliver to the destination.
// https://docs.aws.amazon.com/AmazonCloudWatch/latest/logs/SubscriptionFilters.html
func (s *Service) checkDestination(ctx context.Context, scope request.Scope, group, arn, role string) error {
	test := subscriptionDelivery(scope, group, arn, role, "test", checkMessage)
	ctx = request.WithMetadata(context.WithoutCancel(ctx), request.Metadata{Scope: scope, Service: "logs"})
	err := s.authorizer.Authorize(ctx, test)
	switch {
	case err == nil:
		return nil
	case test.Role == nil:
		// https://repost.aws/questions/QUenIyUlYJQlmukRBpyl-fLw/cloudwatch-logs-subscription-error
		return invalid("Could not execute the lambda function. Make sure you have given CloudWatch Logs permission " +
			"to execute your function.")
	}

	if strings.Contains(arn, ":firehose:") {
		// https://repost.aws/questions/QUxCLxD0uFQKWT98miK5mwdQ/regarding-subscription-filter
		return invalid("Could not deliver test message to specified Firehose stream. " +
			"Check if the given Firehose stream is in ACTIVE state.")
	}

	return invalid("Could not deliver test message to specified destination: %s", awserror.From(err).Message)
}

// subscriptionDelivery is a delivery of data to a subscription filter's destination: a Lambda function, whose
// resource policy must allow CloudWatch Logs, or a Kinesis or Firehose stream written under the filter's role.
func subscriptionDelivery(scope request.Scope, group, arn, role, partition, data string) catalog.ServiceDelivery {
	d := catalog.ServiceDelivery{
		Principal: "logs.amazonaws.com", SourceARN: groupARN(scope, group) + ":*", SourceAccount: scope.AccountID,
	}
	switch {
	case strings.Contains(arn, ":lambda:"):
		d.Target = arn
	case strings.Contains(arn, ":kinesis:"):
		d.Role = &catalog.DeliveryRole{
			ARN: role, Session: "CloudWatchLogs", Service: "kinesis", Operation: "PutRecord",
			Params: map[string]any{"StreamARN": arn, "Data": data, "PartitionKey": partition},
		}
	default:
		_, name, _ := strings.Cut(arn, ":deliverystream/")
		d.Role = &catalog.DeliveryRole{
			ARN: role, Session: "CloudWatchLogs", Service: "firehose", Operation: "PutRecord",
			Params: map[string]any{"DeliveryStreamName": name, "Record": map[string]any{"Data": data}},
		}
	}

	return d
}

func (s *Service) DescribeSubscriptionFilters(
	ctx context.Context,
	in *api.DescribeSubscriptionFiltersRequest,
) (*api.DescribeSubscriptionFiltersResponse, error) {
	if l := common.Value(in.Limit); in.Limit != nil && (l < 1 || l > maxDescribe) {
		return nil, invalid("limit must be from 1 to %d", maxDescribe)
	}

	out := &api.DescribeSubscriptionFiltersResponse{SubscriptionFilters: []api.SubscriptionFilter{}}
	err := s.view(ctx, in.LogGroupName, nil, func(_ storage.ReadTx, _ request.Scope, g *group) error {
		for _, sub := range g.Subscriptions {
			if strings.HasPrefix(sub.Name, common.Value(in.FilterNamePrefix)) {
				out.SubscriptionFilters = append(out.SubscriptionFilters, api.SubscriptionFilter{
					FilterName: new(sub.Name), LogGroupName: new(g.Name), FilterPattern: new(sub.Pattern),
					DestinationArn: new(sub.Destination), RoleArn: optional(sub.Role),
					Distribution: new(sub.Distribution), CreationTime: new(sub.Created),
				})
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DeleteSubscriptionFilter(
	ctx context.Context,
	in *api.DeleteSubscriptionFilterRequest,
) (*api.DeleteSubscriptionFilterOutput, error) {
	err := s.change(ctx, in.LogGroupName, func(g *group) error {
		i := slices.IndexFunc(
			g.Subscriptions,
			func(o subscription) bool { return o.Name == common.Value(in.FilterName) },
		)
		if i < 0 {
			return notFound("The specified subscription filter does not exist.")
		}

		g.Subscriptions = slices.Delete(g.Subscriptions, i, i+1)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteSubscriptionFilterOutput{}, nil
}

func optional(s string) *string {
	if s == "" {
		return nil
	}

	return &s
}

// forward delivers committed events to their group's subscription filters,
// as CloudWatch Logs does: each filter gets the events its pattern matches,
// gzipped in a DATA_MESSAGE. A failed delivery is logged and dropped.
func (s *Service) forward(ctx context.Context, d *delivered) {
	if s.caller == nil || d == nil || len(d.events) == 0 {
		return
	}

	for _, sub := range d.group.Subscriptions {
		match, err := compilePattern(sub.Pattern)
		if err != nil {
			continue
		}

		var events []map[string]any
		for _, e := range d.events {
			if match(e.Message) {
				events = append(events, map[string]any{"id": e.ID, "timestamp": e.Timestamp, "message": e.Message})
			}
		}

		if len(events) == 0 {
			continue
		}

		data, err := json.Marshal(map[string]any{
			"messageType": "DATA_MESSAGE", "owner": d.scope.AccountID, "logGroup": d.group.Name,
			"logStream": d.stream, "subscriptionFilters": []string{sub.Name}, "logEvents": events,
		})
		if err == nil {
			err = s.deliver(ctx, d, sub.Destination, sub.Role, data)
		}

		if err != nil {
			slog.Warn("delivering to a log subscription failed", "group", d.group.Name, "filter", sub.Name,
				"destination", sub.Destination, "error", err)
		}
	}
}

// deliver sends one gzipped DATA_MESSAGE to a subscription's destination.
func (s *Service) deliver(ctx context.Context, d *delivered, arn, role string, data []byte) error {
	var zipped bytes.Buffer
	w := gzip.NewWriter(&zipped)
	if _, err := w.Write(data); err != nil {
		return err
	}

	if err := w.Close(); err != nil {
		return err
	}

	ctx = request.WithMetadata(context.WithoutCancel(ctx), request.Metadata{Scope: d.scope})
	del := subscriptionDelivery(d.scope, d.group.Name, arn, role, d.stream, zipped.String())
	if del.Role != nil {
		_, err := s.authorizer.Call(ctx, s.caller, del)
		return err
	}

	if err := s.authorizer.Authorize(ctx, del); err != nil {
		return err
	}

	payload, err := json.Marshal(map[string]any{
		"awslogs": map[string]string{"data": base64.StdEncoding.EncodeToString(zipped.Bytes())},
	})
	if err != nil {
		return err
	}

	_, err = s.caller.Call(ctx, "lambda", "Invoke",
		map[string]any{"FunctionName": arn, "InvocationType": "Event", "Payload": string(payload)})
	return err
}
