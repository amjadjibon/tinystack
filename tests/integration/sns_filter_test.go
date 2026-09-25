package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	snstypes "github.com/aws/aws-sdk-go-v2/service/sns/types"
)

func TestSNSFilterPolicies(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := snsClient(s, "test")
	queues := sqsClient(s, "test")
	topic, err := c.CreateTopic(ctx, &sns.CreateTopicInput{Name: aws.String("orders")})
	if err != nil {
		t.Fatal(err)
	}

	subscribe := func(queue string, attributes map[string]string) string {
		t.Helper()
		out, subErr := c.Subscribe(ctx, &sns.SubscribeInput{
			TopicArn: topic.TopicArn, Protocol: aws.String("sqs"), Endpoint: aws.String(queue),
			Attributes: attributes,
		})
		if subErr != nil {
			t.Fatal(queue, subErr)
		}

		return aws.ToString(out.SubscriptionArn)
	}

	bigURL, bigQueue := queueARN(t, queues, "big-eu-orders")
	paidURL, paidQueue := queueARN(t, queues, "paid-orders")
	allURL, allQueue := queueARN(t, queues, "all-orders")
	// Attribute policies see String, Number, and String.Array attributes.
	bigARN := subscribe(bigQueue, map[string]string{
		"RawMessageDelivery": "true",
		"FilterPolicy":       `{"region":[{"prefix":"eu-"}],"total":[{"numeric":[">=",100]}],"tags":["gift"]}`,
	})
	// Body policies see the message's JSON fields, nested included.
	subscribe(paidQueue, map[string]string{
		"RawMessageDelivery": "true",
		"FilterPolicyScope":  "MessageBody",
		"FilterPolicy":       `{"order":{"state":["PAID"]},"$or":[{"vip":[true]},{"total":[{"numeric":[">",500]}]}]}`,
	})
	subscribe(allQueue, map[string]string{"RawMessageDelivery": "true"})

	publish := func(body string, attributes map[string]snstypes.MessageAttributeValue) {
		t.Helper()
		if _, pubErr := c.Publish(ctx, &sns.PublishInput{
			TopicArn: topic.TopicArn, Message: aws.String(body), MessageAttributes: attributes,
		}); pubErr != nil {
			t.Fatal(pubErr)
		}
	}

	attr := func(dataType, v string) snstypes.MessageAttributeValue {
		return snstypes.MessageAttributeValue{DataType: aws.String(dataType), StringValue: aws.String(v)}
	}

	for _, m := range []struct {
		attributes map[string]snstypes.MessageAttributeValue
		body       string
		big, paid  bool
	}{
		{
			body: `{"order":{"state":"PAID"},"vip":true}`, big: true, paid: true,
			attributes: map[string]snstypes.MessageAttributeValue{
				"region": attr("String", "eu-west-1"), "total": attr("Number", "250"),
				"tags": attr("String.Array", `["gift","rush"]`),
			},
		},
		{
			// A total sent as a String is not a number.
			body: `{"order":{"state":"NEW"},"vip":true}`,
			attributes: map[string]snstypes.MessageAttributeValue{
				"region": attr("String", "eu-west-1"), "total": attr("String", "250"),
				"tags": attr("String", "gift"),
			},
		},
		{
			body: `{"order":{"state":"PAID"},"total":900}`, paid: true,
			attributes: map[string]snstypes.MessageAttributeValue{
				"region": attr("String", "us-east-1"), "total": attr("Number", "900"), "tags": attr("String", "gift"),
			},
		},
		// A body that is not JSON matches no body policy.
		{body: "plain text"},
	} {
		publish(m.body, m.attributes)
		for _, q := range []struct {
			url  string
			want bool
		}{{bigURL, m.big}, {paidURL, m.paid}, {allURL, true}} {
			if got := receiveOne(t, queues, q.url); (got == m.body) != q.want || (got != "" && got != m.body) {
				t.Fatalf("%s got %q for %q", q.url, got, m.body)
			}
		}
	}

	// The policy and its scope are subscription attributes.
	attributes, err := c.GetSubscriptionAttributes(ctx, &sns.GetSubscriptionAttributesInput{
		SubscriptionArn: aws.String(bigARN),
	})
	if err != nil || attributes.Attributes["FilterPolicyScope"] != "MessageAttributes" ||
		!json.Valid([]byte(attributes.Attributes["FilterPolicy"])) {
		t.Fatalf("attributes = %v, %v", attributes, err)
	}

	set := func(name, v string) error {
		_, setErr := c.SetSubscriptionAttributes(ctx, &sns.SetSubscriptionAttributesInput{
			SubscriptionArn: aws.String(bigARN), AttributeName: aws.String(name), AttributeValue: aws.String(v),
		})
		return setErr
	}

	if err = set("FilterPolicy", `{"region":[{"anything-but":["us-east-1"]}]}`); err != nil {
		t.Fatal(err)
	}

	publish("west", map[string]snstypes.MessageAttributeValue{"region": attr("String", "us-west-2")})
	publish("east", map[string]snstypes.MessageAttributeValue{"region": attr("String", "us-east-1")})
	publish("none", nil)
	if got := receiveOne(t, queues, bigURL); got != "west" {
		t.Fatalf("after update got %q", got)
	}

	// Removing the policy delivers everything again.
	if err = set("FilterPolicy", ""); err != nil {
		t.Fatal(err)
	}

	publish("any", nil)
	if got := receiveOne(t, queues, bigURL); got != "any" {
		t.Fatalf("after removing got %q", got)
	}

	attributes, err = c.GetSubscriptionAttributes(ctx, &sns.GetSubscriptionAttributesInput{
		SubscriptionArn: aws.String(bigARN),
	})
	if _, ok := attributes.Attributes["FilterPolicy"]; err != nil || ok {
		t.Fatalf("attributes after removing = %v, %v", attributes, err)
	}

	// Invalid policies are rejected on Subscribe and SetSubscriptionAttributes.
	var badParam *snstypes.InvalidParameterException
	for name, policy := range map[string]string{
		"not JSON":        `{"region":`,
		"not an array":    `{"region":"eu"}`,
		"unknown":         `{"region":[{"bogus":"x"}]}`,
		"wildcard":        `{"region":[{"wildcard":"eu-*"}]}`,
		"nested":          `{"order":{"state":["PAID"]}}`,
		"numeric range":   `{"total":[{"numeric":[">",2e9]}]}`,
		"too many keys":   `{"a":["1"],"b":["1"],"c":["1"],"d":["1"],"e":["1"],"f":["1"]}`,
		"too complex":     `{"a":["1","2","3","4","5","6"],"b":["1","2","3","4","5"],"c":["1","2","3","4","5","6"]}`,
		"bad cidr":        `{"ip":[{"cidr":"10.0.0.0"}]}`,
		"empty $or group": `{"$or":[{"a":["1"]}]}`,
	} {
		if err = set("FilterPolicy", policy); !errors.As(err, &badParam) {
			t.Fatalf("%s: %v", name, err)
		}
	}

	if err = set("FilterPolicyScope", "Everything"); !errors.As(err, &badParam) {
		t.Fatalf("bad scope: %v", err)
	}

	// Changing the scope rechecks the policy: a nested policy needs MessageBody.
	if err = set("FilterPolicyScope", "MessageBody"); err != nil {
		t.Fatal(err)
	}

	if err = set("FilterPolicy", `{"order":{"state":["PAID"]}}`); err != nil {
		t.Fatal(err)
	}

	if err = set("FilterPolicyScope", "MessageAttributes"); !errors.As(err, &badParam) {
		t.Fatalf("scope change kept a nested policy: %v", err)
	}

	// Subscribing again with a different policy is a conflict; the same one is idempotent.
	for policy, conflict := range map[string]bool{`{"region":["eu"]}`: true, "": false} {
		attributes := map[string]string{"RawMessageDelivery": "true"}
		if policy != "" {
			attributes["FilterPolicy"] = policy
		}

		_, err = c.Subscribe(ctx, &sns.SubscribeInput{
			TopicArn: topic.TopicArn, Protocol: aws.String("sqs"), Endpoint: aws.String(allQueue),
			Attributes: attributes,
		})
		if conflict != errors.As(err, &badParam) || (!conflict && err != nil) {
			t.Fatalf("resubscribe with %q: %v", policy, err)
		}
	}

	// Number attributes must hold numbers.
	if _, err = c.Publish(ctx, &sns.PublishInput{
		TopicArn: topic.TopicArn, Message: aws.String("x"),
		MessageAttributes: map[string]snstypes.MessageAttributeValue{"total": attr("Number", "lots")},
	}); !errors.As(err, &badParam) {
		t.Fatalf("non-numeric Number attribute: %v", err)
	}
}
