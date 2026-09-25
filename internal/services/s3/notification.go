package s3

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	eventbridgeapi "github.com/amjadjibon/tinystack/internal/aws/generated/eventbridge"
	lambdaapi "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	snsapi "github.com/amjadjibon/tinystack/internal/aws/generated/sns"
	sqsapi "github.com/amjadjibon/tinystack/internal/aws/generated/sqs"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/delivery"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceSQS    = "sqs"
	serviceSNS    = "sns"
	serviceLambda = "lambda"
	fieldBucket   = "bucket"
	eventCreated  = "ObjectCreated:"
	eventRemoved  = "ObjectRemoved:"
	// notificationSubject is the subject of the SNS messages S3 publishes.
	notificationSubject = "Amazon S3 Notification"
)

// supportedEvents are the notification event types TinyStack emits.
var supportedEvents = []string{
	"s3:ObjectCreated:*", "s3:ObjectCreated:Put", "s3:ObjectCreated:Post", "s3:ObjectCreated:Copy",
	"s3:ObjectCreated:CompleteMultipartUpload", "s3:ObjectRemoved:*", "s3:ObjectRemoved:Delete",
	"s3:ObjectRemoved:DeleteMarkerCreated",
}

func invalidArgument(message string) error {
	return fail(http.StatusBadRequest, "InvalidArgument", message)
}

// destination is one queue, topic, or function configuration of a bucket's
// notification configuration.
type destination struct {
	filter *api.NotificationConfigurationFilter
	id     *string
	arn    string
	events []string
}

// destinations lists the configuration's queue, topic, and function destinations.
func destinations(cfg *api.NotificationConfiguration) []destination {
	var out []destination
	for _, q := range cfg.QueueConfigurations {
		out = append(out, destination{arn: value(q.QueueArn), events: q.Events, filter: q.Filter, id: q.Id})
	}

	for _, t := range cfg.TopicConfigurations {
		out = append(out, destination{arn: value(t.TopicArn), events: t.Events, filter: t.Filter, id: t.Id})
	}

	for _, f := range cfg.LambdaFunctionConfigurations {
		out = append(out, destination{
			arn: value(f.LambdaFunctionArn), events: f.Events, filter: f.Filter, id: f.Id,
		})
	}

	return out
}

// keyFilter returns a destination's prefix and suffix rules.
func keyFilter(f *api.NotificationConfigurationFilter) (prefix, suffix string, err error) {
	if f == nil || f.Key == nil {
		return "", "", nil
	}

	seen := map[string]bool{}
	for _, rule := range f.Key.FilterRules {
		name := strings.ToLower(value(rule.Name))
		if (name != "prefix" && name != "suffix") || seen[name] || len(value(rule.Value)) > 1024 {
			return "", "", invalidArgument("Filter rules must name prefix or suffix at most once each, " +
				"with values of up to 1024 characters")
		}

		seen[name] = true
		if name == "prefix" {
			prefix = value(rule.Value)
		} else {
			suffix = value(rule.Value)
		}
	}

	return prefix, suffix, nil
}

// sharesEvent reports whether two event lists have an event type in common,
// counting a wildcard as every type of its family.
func sharesEvent(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			xFamily, xAll := strings.CutSuffix(x, "*")
			yFamily, yAll := strings.CutSuffix(y, "*")
			if x == y || (xAll && strings.HasPrefix(y, xFamily)) || (yAll && strings.HasPrefix(x, yFamily)) {
				return true
			}
		}
	}

	return false
}

// checkNotifications validates a configuration for bucket in region and
// gives every destination an ID.
func checkNotifications(cfg *api.NotificationConfiguration, region string) error {
	ids := map[string]bool{}
	service := func(i int) string {
		switch {
		case i < len(cfg.QueueConfigurations):
			return serviceSQS
		case i < len(cfg.QueueConfigurations)+len(cfg.TopicConfigurations):
			return serviceSNS
		default:
			return serviceLambda
		}
	}

	all := destinations(cfg)
	for i, d := range all {
		if len(d.events) == 0 {
			return invalidArgument("Each notification configuration needs at least one event")
		}

		for _, e := range d.events {
			if !slices.Contains(supportedEvents, e) {
				return invalidArgument("TinyStack does not emit the event " + e)
			}
		}

		parts := strings.SplitN(d.arn, ":", 7)
		if len(parts) < 6 || parts[0] != "arn" || parts[2] != service(i) || parts[3] != region {
			return invalidArgument("The destination ARN " + d.arn + " is not valid: it must be a " + service(i) +
				" ARN in the bucket's region")
		}

		if service(i) == serviceSQS && strings.HasSuffix(d.arn, ".fifo") {
			return invalidArgument("FIFO queues are not supported as S3 notification destinations")
		}

		if _, _, err := keyFilter(d.filter); err != nil {
			return err
		}

		id := value(d.id)
		if len(id) > 255 || (id != "" && ids[id]) {
			return invalidArgument("Configuration IDs must be unique and at most 255 characters")
		}

		ids[id] = true
		for _, other := range all[:i] {
			if overlaps(d, other) {
				return invalidArgument("Configurations overlap. Configurations on the same bucket cannot share " +
					"a common event type.")
			}
		}
	}

	for i := range cfg.QueueConfigurations {
		setID(&cfg.QueueConfigurations[i].Id)
	}

	for i := range cfg.TopicConfigurations {
		setID(&cfg.TopicConfigurations[i].Id)
	}

	for i := range cfg.LambdaFunctionConfigurations {
		setID(&cfg.LambdaFunctionConfigurations[i].Id)
	}

	return nil
}

// overlaps reports whether an object event could match both destinations:
// they share an event type, and their prefixes and suffixes can both hold.
func overlaps(a, b destination) bool {
	aPrefix, aSuffix, _ := keyFilter(a.filter)
	bPrefix, bSuffix, _ := keyFilter(b.filter)
	return sharesEvent(a.events, b.events) &&
		(strings.HasPrefix(aPrefix, bPrefix) || strings.HasPrefix(bPrefix, aPrefix)) &&
		(strings.HasSuffix(aSuffix, bSuffix) || strings.HasSuffix(bSuffix, aSuffix))
}

func setID(id **string) {
	if value(*id) == "" {
		*id = new(rand.Text())
	}
}

// PutBucketNotificationConfiguration replaces the bucket's notification
// configuration. Unless SkipDestinationValidation is set, it first sends
// s3:TestEvent to each queue and topic and checks each function exists.
func (s *Service) PutBucketNotificationConfiguration(
	ctx context.Context,
	in *api.PutBucketNotificationConfigurationRequest,
) (*api.PutBucketNotificationConfigurationOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	cfg := in.NotificationConfiguration
	if cfg == nil {
		cfg = &api.NotificationConfiguration{}
	}

	if guardErr := checkNotifications(cfg, m.Region); guardErr != nil {
		return nil, guardErr
	}

	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		_, resolveErr := resolve(tx, m, in.Bucket, in.ExpectedBucketOwner)
		return resolveErr
	})
	if err != nil {
		return nil, err
	}

	if !value(in.SkipDestinationValidation) {
		if guardErr := s.validateDestinations(ctx, m, *in.Bucket, cfg); guardErr != nil {
			return nil, guardErr
		}
	}

	empty := len(destinations(cfg)) == 0 && cfg.EventBridgeConfiguration == nil
	err = s.updateBucket(ctx, in.Bucket, in.ExpectedBucketOwner, func(meta *bucketMeta) {
		meta.Notifications = cfg
		if empty {
			meta.Notifications = nil
		}
	})
	if err != nil {
		return nil, err
	}

	return &api.PutBucketNotificationConfigurationOutput{}, nil
}

// validateDestinations sends s3:TestEvent to each queue and topic and checks
// that each function exists, as S3 does before it saves a configuration.
//
//nolint:gocritic // Metadata is an immutable request snapshot.
func (s *Service) validateDestinations(
	ctx context.Context,
	m request.Metadata,
	bucket string,
	cfg *api.NotificationConfiguration,
) error {
	test, err := json.Marshal(map[string]string{
		"Service": "Amazon S3", "Event": "s3:TestEvent", "Time": s.now().UTC().Format(eventTime),
		"Bucket": bucket, "RequestId": m.RequestID, "HostId": m.RequestID,
	})
	if err != nil {
		return err
	}

	var failed []string
	for _, d := range destinations(cfg) {
		var sendErr error
		switch strings.SplitN(d.arn, ":", 4)[2] {
		case serviceSQS:
			sendErr = s.queues.Deliver(ctx, d.arn, &sqsapi.SendMessageBatchRequestEntry{MessageBody: new(string(test))})
		case serviceSNS:
			_, sendErr = s.topics.Publish(ctx, &snsapi.PublishInput{
				TopicArn: new(d.arn), Message: new(string(test)), Subject: new(notificationSubject),
			})
		default:
			_, sendErr = s.functions.GetFunction(ctx, &lambdaapi.GetFunctionRequest{FunctionName: new(d.arn)})
		}

		if sendErr != nil {
			failed = append(failed, d.arn)
		}
	}

	if len(failed) > 0 {
		return invalidArgument("Unable to validate the following destination configurations: " +
			strings.Join(failed, ", "))
	}

	return nil
}

func (s *Service) GetBucketNotificationConfiguration(
	ctx context.Context,
	in *api.GetBucketNotificationConfigurationRequest,
) (*api.NotificationConfiguration, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.NotificationConfiguration{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		key, resolveErr := resolve(tx, m, in.Bucket, in.ExpectedBucketOwner)
		if resolveErr != nil {
			return resolveErr
		}

		cfg, readErr := notifications(tx, key)
		if cfg != nil {
			out = cfg
		}

		return readErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// notifications returns the notification configuration of the bucket stored
// at key, or nil when it has none.
func notifications(tx storage.ReadTx, key string) (*api.NotificationConfiguration, error) {
	var meta bucketMeta
	if _, err := readJSON(tx, key, &meta); err != nil {
		return nil, err
	}

	return meta.Notifications, nil
}

// objectEvent is a committed object change to notify about.
type objectEvent struct {
	name   string // such as "ObjectCreated:Put"
	reason string // the API operation, for EventBridge
	bucket string
	key    string
	etag   string
	size   int64
}

const eventTime = "2006-01-02T15:04:05.000Z"

// notify delivers an object event to the configured destinations whose event
// types and key filters match, and to EventBridge when it is enabled. It runs
// after the change committed; failures are recorded in the delivery log,
// never returned, as S3 does not report them to the writer.
//
//nolint:gocritic // Metadata is an immutable request snapshot.
func (s *Service) notify(ctx context.Context, m request.Metadata, cfg *api.NotificationConfiguration, e objectEvent) {
	if cfg == nil {
		return
	}

	ctx = context.WithoutCancel(ctx)
	now := s.now().UTC()
	sequencer := fmt.Sprintf("%016X", now.UnixNano())
	source := "arn:aws:s3:::" + e.bucket
	for _, d := range destinations(cfg) {
		prefix, suffix, _ := keyFilter(d.filter)
		if !sharesEvent(d.events, []string{"s3:" + e.name}) || !strings.HasPrefix(e.key, prefix) ||
			!strings.HasSuffix(e.key, suffix) {
			continue
		}

		payload, err := s.record(m, e, value(d.id), sequencer, now)
		if err == nil {
			switch strings.SplitN(d.arn, ":", 4)[2] {
			case serviceSQS:
				err = s.queues.Deliver(
					ctx,
					d.arn,
					&sqsapi.SendMessageBatchRequestEntry{MessageBody: new(string(payload))},
				)
			case serviceSNS:
				_, err = s.topics.Publish(ctx, &snsapi.PublishInput{
					TopicArn: new(d.arn), Message: new(string(payload)), Subject: new(notificationSubject),
				})
			default:
				err = s.functions.InvokeAsync(ctx, d.arn, payload)
			}
		}

		s.deliveries.Add(delivery.ViaS3, source, d.arn, payload, err)
	}

	if cfg.EventBridgeConfiguration != nil {
		detailType := "Object Created"
		if strings.HasPrefix(e.name, eventRemoved) {
			detailType = "Object Deleted"
		}

		detail, err := s.eventDetail(m, e, sequencer)
		if err == nil {
			err = s.putEvent(ctx, detailType, source, detail)
		}

		s.deliveries.Add(delivery.ViaS3, source, "arn:aws:events:"+m.Region+":"+m.AccountID+":event-bus/default",
			detail, err)
	}
}

// record is the S3 event message a queue, topic, or function receives.
//
//nolint:gocritic // Metadata is an immutable request snapshot.
func (*Service) record(m request.Metadata, e objectEvent, id, sequencer string, now time.Time) ([]byte, error) {
	object := map[string]any{"key": url.QueryEscape(e.key), "sequencer": sequencer}
	if strings.HasPrefix(e.name, eventCreated) {
		object["size"], object["eTag"] = e.size, strings.Trim(e.etag, `"`)
	}

	return json.Marshal(map[string]any{"Records": []map[string]any{{
		"eventVersion":      "2.1",
		"eventSource":       "aws:s3",
		"awsRegion":         m.Region,
		"eventTime":         now.Format(eventTime),
		"eventName":         e.name,
		"userIdentity":      map[string]string{"principalId": "AWS:" + m.AccountID},
		"requestParameters": map[string]string{"sourceIPAddress": "127.0.0.1"},
		"responseElements":  map[string]string{"x-amz-request-id": m.RequestID, "x-amz-id-2": m.RequestID},
		"s3": map[string]any{
			"s3SchemaVersion": "1.0",
			"configurationId": id,
			fieldBucket: map[string]any{
				"name":          e.bucket,
				"ownerIdentity": map[string]string{"principalId": canonicalID(m.AccountID)},
				"arn":           "arn:aws:s3:::" + e.bucket,
			},
			"object": object,
		},
	}}})
}

// eventDetail is the detail of the EventBridge event for e.
//
//nolint:gocritic // Metadata is an immutable request snapshot.
func (*Service) eventDetail(m request.Metadata, e objectEvent, sequencer string) ([]byte, error) {
	object := map[string]any{"key": e.key, "sequencer": sequencer}
	detail := map[string]any{
		"version":           "0",
		"bucket":            map[string]string{"name": e.bucket},
		"object":            object,
		"request-id":        m.RequestID,
		"requester":         m.AccountID,
		"source-ip-address": "127.0.0.1",
		"reason":            e.reason,
	}

	if strings.HasPrefix(e.name, eventCreated) {
		object["size"], object["etag"] = e.size, strings.Trim(e.etag, `"`)
	} else {
		detail["deletion-type"] = "Permanently Deleted"
	}

	return json.Marshal(detail)
}

// putEvent sends an event of detailType about the bucket arn to the
// account's default event bus.
func (s *Service) putEvent(ctx context.Context, detailType, arn string, detail []byte) error {
	entry := eventbridgeapi.PutEventsRequestEntry{
		Source: new("aws.s3"), DetailType: &detailType, Detail: new(string(detail)), Resources: []string{arn},
	}

	out, err := s.events.PutEvents(ctx, &eventbridgeapi.PutEventsRequest{
		Entries: []eventbridgeapi.PutEventsRequestEntry{entry},
	})
	if err != nil {
		return err
	}

	if value(out.FailedEntryCount) > 0 {
		return errors.New(value(out.Entries[0].ErrorMessage))
	}

	return nil
}
