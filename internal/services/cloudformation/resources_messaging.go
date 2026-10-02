package cloudformation

import (
	"context"
	"maps"
	"slices"
)

// queueAttributes are the AWS::SQS::Queue properties that are queue
// attributes, with the values SQS's defaults restore when one is removed.
var queueAttributes = map[string]string{
	memberContentBasedDeduplication: textFalse, "DeduplicationScope": "", "DelaySeconds": "0",
	"FifoThroughputLimit": "", "KmsDataKeyReusePeriodSeconds": "", memberKmsMasterKeyID: "",
	"MaximumMessageSize": "262144", "MessageRetentionPeriod": "345600", "ReceiveMessageWaitTimeSeconds": "0",
	"RedriveAllowPolicy": "", "RedrivePolicy": "", "SqsManagedSseEnabled": "", "VisibilityTimeout": "30",
}

// topicAttributes are the AWS::SNS::Topic properties that are topic
// attributes, with the values that restore them when one is removed.
var topicAttributes = map[string]string{
	"DisplayName":                   "",
	memberContentBasedDeduplication: textFalse,
	memberKmsMasterKeyID:            "",
}

// subscriptionAttributes are the AWS::SNS::Subscription properties that are
// subscription attributes.
var subscriptionAttributes = map[string]string{
	"RawMessageDelivery": textFalse, "FilterPolicy": "", "FilterPolicyScope": "MessageAttributes", "RedrivePolicy": "",
}

func init() {
	register("AWS::SQS::Queue", &resourceType{
		name: memberQueueName,
		properties: append(slices.Sorted(maps.Keys(queueAttributes)),
			memberQueueName, "FifoQueue", memberTags),
		attributes: []string{memberArn, memberQueueName, memberQueueURL},
		replace:    []string{memberQueueName, "FifoQueue"},
		create:     createQueue,
		update:     updateQueue,
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "sqs", "DeleteQueue", map[string]any{memberQueueURL: r.Physical})
			if gone(err, "QueueDoesNotExist") {
				return nil
			}

			return err
		},
	})
	register("AWS::SNS::Topic", &resourceType{
		name: memberTopicName,
		properties: []string{
			memberTopicName,
			"DisplayName",
			"FifoTopic",
			memberContentBasedDeduplication,
			memberKmsMasterKeyID,
			memberTags,
		},
		attributes: []string{memberTopicArn, memberTopicName},
		replace:    []string{memberTopicName, "FifoTopic"},
		create:     createTopic,
		update:     updateTopic,
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "sns", "DeleteTopic", map[string]any{memberTopicArn: r.Physical})
			return err
		},
	})
	register("AWS::SNS::Subscription", &resourceType{
		properties: append(
			slices.Sorted(maps.Keys(subscriptionAttributes)),
			memberTopicArn,
			propertyProtocol,
			propertyEndpoint,
		),
		attributes: []string{memberArn},
		replace:    []string{memberTopicArn, propertyProtocol, propertyEndpoint},
		create:     createSubscription,
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			err := setAttributes(ctx, c, "SetSubscriptionAttributes", "SubscriptionArn", r, p, subscriptionAttributes)
			return map[string]any{memberArn: r.Physical}, err
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "sns", "Unsubscribe", map[string]any{"SubscriptionArn": r.Physical})
			if gone(err, "NotFoundException") {
				return nil
			}

			return err
		},
	})
}

// queueAttributeValues renders a queue's attribute properties as SQS
// attributes.
func queueAttributeValues(p props) map[string]any {
	out := map[string]any{}
	for k := range queueAttributes {
		if p.has(k) {
			out[k] = p.attribute(k)
		}
	}

	return out
}

func createQueue(ctx context.Context, c *client, p props) (physical string, attributes map[string]any, err error) {
	name := p.str(memberQueueName)
	fifo := p.truth("FifoQueue")
	if name == "" {
		name = c.generate(80)
		if fifo {
			name = c.generate(75) + ".fifo"
		}
	}

	attributes = queueAttributeValues(p)
	if fifo {
		attributes["FifoQueue"] = textTrue
	}

	params := map[string]any{memberQueueName: name, memberAttributes: attributes}
	if tags := p.tags(); len(tags) > 0 {
		params[memberTags] = tagMap(tags)
	}

	out, err := c.call(ctx, "sqs", "CreateQueue", params)
	if err != nil {
		return "", nil, err
	}

	url := outText(out, memberQueueURL)
	return url, queueAttributesOf(c, name, url), nil
}

func queueAttributesOf(c *client, name, url string) map[string]any {
	arn := "arn:aws:sqs:" + c.scope.Region + ":" + c.scope.AccountID + ":" + name
	return map[string]any{memberArn: arn, memberQueueName: name, memberQueueURL: url}
}

func updateQueue(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
	old := props(r.Properties)
	attributes := map[string]any{}
	for k, restore := range queueAttributes {
		switch {
		case !p.changed(old, k):
		case p.has(k):
			attributes[k] = queueAttributeValues(p)[k]
		default:
			attributes[k] = restore
		}
	}

	if len(attributes) > 0 {
		if _, err := c.call(ctx, "sqs", "SetQueueAttributes", map[string]any{
			memberQueueURL: r.Physical, memberAttributes: attributes,
		}); err != nil {
			return nil, err
		}
	}

	if err := retag(ctx, c, old, p, &tagger{
		target: map[string]any{memberQueueURL: r.Physical}, service: "sqs", tag: "TagQueue", untag: "UntagQueue",
	}); err != nil {
		return nil, err
	}

	return r.Attributes, nil
}

func createTopic(ctx context.Context, c *client, p props) (physical string, attributes map[string]any, err error) {
	name := p.str(memberTopicName)
	fifo := p.truth("FifoTopic")
	if name == "" {
		name = c.generate(256)
		if fifo {
			name = c.generate(251) + ".fifo"
		}
	}

	attributes = map[string]any{}
	for k := range topicAttributes {
		if p.has(k) {
			attributes[k] = p.str(k)
		}
	}

	if fifo {
		attributes["FifoTopic"] = textTrue
	}

	params := map[string]any{memberName: name, memberAttributes: attributes}
	if tags := p.tags(); len(tags) > 0 {
		params[memberTags] = tagList(tags, memberKey, memberValue)
	}

	out, err := c.call(ctx, "sns", "CreateTopic", params)
	if err != nil {
		return "", nil, err
	}

	arn := outText(out, memberTopicArn)
	return arn, map[string]any{memberTopicArn: arn, memberTopicName: name}, nil
}

func updateTopic(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
	if err := setAttributes(ctx, c, "SetTopicAttributes", memberTopicArn, r, p, topicAttributes); err != nil {
		return nil, err
	}

	if err := retag(ctx, c, r.Properties, p, &tagger{
		target: map[string]any{
			memberResourceArn: r.Physical,
		},
		service: "sns",
		tag:     opTagResource,
		untag:   opUntagResource,
		key:     memberKey,
		value:   memberValue,
	}); err != nil {
		return nil, err
	}

	return r.Attributes, nil
}

// setAttributes sets each SNS attribute that changed, one call each, as
// SNS's Set*Attributes operations take one attribute at a time.
func setAttributes(
	ctx context.Context, c *client, operation, arnMember string, r *resource, p props, attributes map[string]string,
) error {
	old := props(r.Properties)
	for _, k := range slices.Sorted(maps.Keys(attributes)) {
		if !p.changed(old, k) {
			continue
		}

		v := attributes[k]
		if p.has(k) {
			v = p.attribute(k)
		}

		if _, err := c.call(ctx, "sns", operation, map[string]any{
			arnMember: r.Physical, memberAttributeName: k, "AttributeValue": v,
		}); err != nil {
			return err
		}
	}

	return nil
}

func createSubscription(
	ctx context.Context,
	c *client,
	p props,
) (physical string, attributes map[string]any, err error) {
	attributes = map[string]any{}
	for k := range subscriptionAttributes {
		if p.has(k) {
			attributes[k] = p.attribute(k)
		}
	}

	out, err := c.call(ctx, "sns", "Subscribe", map[string]any{
		memberTopicArn: p.str(
			memberTopicArn,
		),
		propertyProtocol:        p.str(propertyProtocol),
		propertyEndpoint:        p.str(propertyEndpoint),
		memberAttributes:        attributes,
		"ReturnSubscriptionArn": true,
	})
	if err != nil {
		return "", nil, err
	}

	arn := outText(out, "SubscriptionArn")
	return arn, map[string]any{memberArn: arn}, nil
}
