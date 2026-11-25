package sns

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/sns"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/delivery"
)

const (
	protocolFirehose        = "firehose"
	attrSubscriptionRoleArn = "SubscriptionRoleArn"
)

var iamRoleARN = regexp.MustCompile(`^arn:aws[a-zA-Z-]*:iam::\d{12}:role/.+$`)

func parseFirehoseARN(arn string) (scope request.Scope, streamName string, err error) {
	parts := strings.Split(arn, ":")
	if len(parts) != 6 || parts[0] != arnScheme || parts[2] != protocolFirehose || parts[3] == "" ||
		!accountID.MatchString(parts[4]) {
		return request.Scope{}, "", invalid("Firehose endpoint ARN")
	}

	name, ok := strings.CutPrefix(parts[5], "deliverystream/")
	if !ok || name == "" {
		return request.Scope{}, "", invalid("Firehose endpoint ARN")
	}

	return request.Scope{AccountID: parts[4], Region: parts[3]}, name, nil
}

func checkRoleARN(arn string) error {
	if !iamRoleARN.MatchString(arn) {
		return invalid("SubscriptionRoleArn must be a valid IAM role ARN")
	}

	return nil
}

// deliverFirehose delivers a notification to a Firehose delivery stream via
// catalog.Caller, honoring RawMessageDelivery, role-assumed delivery
// authorization, and RedrivePolicy on failure.
func (s *Service) deliverFirehose(
	ctx context.Context,
	sub *subscription,
	envelope *notification,
	e *api.PublishBatchRequestEntry,
	placed *sequenced,
) error {
	body := envelope.Message
	if !sub.Raw {
		encoded, err := json.Marshal(envelope)
		if err != nil {
			return err
		}

		body = string(encoded)
	}

	targetScope, streamName, err := parseFirehoseARN(sub.Endpoint)
	if err == nil && s.caller == nil {
		err = errors.New("firehose delivery is unavailable")
	}

	if err == nil {
		firehoseCtx := request.WithMetadata(context.WithoutCancel(ctx), request.Metadata{
			Scope: targetScope, Service: serviceName,
		})
		del := catalog.ServiceDelivery{
			Principal:     "sns.amazonaws.com",
			SourceARN:     envelope.TopicArn,
			SourceAccount: strings.Split(envelope.TopicArn, ":")[4],
			Role: &catalog.DeliveryRole{
				ARN:       sub.SubscriptionRoleArn,
				Session:   "sns-firehose",
				Service:   protocolFirehose,
				Operation: "PutRecord",
				Params: map[string]any{
					"DeliveryStreamName": streamName,
					"Record":             map[string]any{"Data": body},
				},
			},
		}
		_, err = s.authorizer.Call(firehoseCtx, s.caller, del)
	}

	if s.deliveries != nil {
		s.deliveries.Add(delivery.ViaSNS, envelope.TopicArn, sub.Endpoint, []byte(body), err)
	}

	s.notified(envelope.TopicArn, err)
	if err != nil {
		s.redrive(ctx, envelope.TopicArn, sub, body, e.MessageAttributes, placed, err)
	}

	return nil
}
