package integration_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	sestypes "github.com/aws/aws-sdk-go-v2/service/sesv2/types"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/amjadjibon/tinystack/internal/delivery"
)

func sesClient(s *httptest.Server) *sesv2.Client {
	return sesv2.New(sesv2.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
		}),
		Retryer: aws.NopRetryer{},
	})
}

// drain receives n messages from a queue, returning their bodies.
func drain(t *testing.T, c *sqs.Client, queueURL string, n int) []string {
	t.Helper()
	var bodies []string
	for deadline := time.Now().Add(5 * time.Second); len(bodies) < n && time.Now().Before(deadline); {
		out, err := c.ReceiveMessage(context.Background(), &sqs.ReceiveMessageInput{
			QueueUrl: &queueURL, MaxNumberOfMessages: 10, WaitTimeSeconds: 1,
		})
		if err != nil {
			t.Fatal(err)
		}

		for _, m := range out.Messages {
			bodies = append(bodies, aws.ToString(m.Body))
			if _, err = c.DeleteMessage(context.Background(), &sqs.DeleteMessageInput{
				QueueUrl: &queueURL, ReceiptHandle: m.ReceiptHandle,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}

	if len(bodies) != n {
		t.Fatalf("wanted %d messages, got %q", n, bodies)
	}

	return bodies
}

// sesMessages returns the email the delivery log recorded, newest first.
func sesMessages(t *testing.T, s *httptest.Server) []map[string]any {
	t.Helper()
	var out []map[string]any
	records := deliveries(t, s, 1)
	for i := range records {
		d := &records[i]
		if d.Via != delivery.ViaSES || !strings.Contains(d.Source, ":identity/") {
			continue
		}

		var m map[string]any
		if err := json.Unmarshal([]byte(d.Payload), &m); err != nil {
			t.Fatal(err)
		}

		m["status"], m["target"] = d.Status, d.Target
		out = append(out, m)
	}

	return out
}

func TestSESIdentitiesAndTemplates(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := sesClient(s)
	created, err := c.CreateEmailIdentity(ctx, &sesv2.CreateEmailIdentityInput{
		EmailIdentity: aws.String(
			"example.com",
		),
		Tags: []sestypes.Tag{{Key: aws.String("team"), Value: aws.String("a")}},
	})
	if err != nil || created.IdentityType != sestypes.IdentityTypeDomain || !created.VerifiedForSendingStatus ||
		created.DkimAttributes.Status != sestypes.DkimStatusSuccess || len(created.DkimAttributes.Tokens) != 3 {
		t.Fatalf("%+v %v", created, err)
	}

	if _, err = c.CreateEmailIdentity(ctx, &sesv2.CreateEmailIdentityInput{
		EmailIdentity: aws.String("Ana@Other.example"),
	}); err != nil {
		t.Fatal(err)
	}

	for _, bad := range []string{"example.com", "ana@other.example", "not a domain", "a@@b"} {
		_, err = c.CreateEmailIdentity(ctx, &sesv2.CreateEmailIdentityInput{EmailIdentity: aws.String(bad)})
		if code := errorCode(err); code != "AlreadyExistsException" && code != "BadRequestException" {
			t.Fatalf("%s: %v", bad, err)
		}
	}

	got, err := c.GetEmailIdentity(ctx, &sesv2.GetEmailIdentityInput{EmailIdentity: aws.String("ana@other.example")})
	if err != nil || got.IdentityType != sestypes.IdentityTypeEmailAddress ||
		got.VerificationStatus != sestypes.VerificationStatusSuccess {
		t.Fatalf("%+v %v", got, err)
	}

	list, err := c.ListEmailIdentities(ctx, &sesv2.ListEmailIdentitiesInput{PageSize: aws.Int32(1)})
	if err != nil || len(list.EmailIdentities) != 1 || list.NextToken == nil ||
		aws.ToString(list.EmailIdentities[0].IdentityName) != "Ana@Other.example" {
		t.Fatalf("%+v %v", list, err)
	}

	arn := "arn:aws:ses:us-east-1:000000000000:identity/example.com"
	if _, err = c.TagResource(ctx, &sesv2.TagResourceInput{
		ResourceArn: aws.String(arn), Tags: []sestypes.Tag{{Key: aws.String("env"), Value: aws.String("dev")}},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.UntagResource(ctx, &sesv2.UntagResourceInput{
		ResourceArn: aws.String(arn), TagKeys: []string{"team"},
	}); err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListTagsForResource(ctx, &sesv2.ListTagsForResourceInput{ResourceArn: aws.String(arn)})
	if err != nil || len(tags.Tags) != 1 || aws.ToString(tags.Tags[0].Key) != "env" {
		t.Fatalf("%+v %v", tags, err)
	}

	content := &sestypes.EmailTemplateContent{
		Subject: aws.String("Hi {{name}}"),
		Text:    aws.String("{{#each items}}{{@index}}:{{this}} {{/each}}{{#if vip}}VIP{{else}}regular{{/if}}"),
		Html:    aws.String("<p>{{name}}</p>{{{raw}}}"),
	}
	if _, err = c.CreateEmailTemplate(ctx, &sesv2.CreateEmailTemplateInput{
		TemplateName: aws.String("welcome"), TemplateContent: content,
	}); err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateEmailTemplate(ctx, &sesv2.CreateEmailTemplateInput{
		TemplateName:    aws.String("broken"),
		TemplateContent: &sestypes.EmailTemplateContent{Subject: aws.String("{{#if x}}never closed")},
	})
	if errorCode(err) != "BadRequestException" {
		t.Fatalf("broken template: %v", err)
	}

	rendered, err := c.TestRenderEmailTemplate(ctx, &sesv2.TestRenderEmailTemplateInput{
		TemplateName: aws.String("welcome"),
		TemplateData: aws.String(`{"name":"<Ana>","items":["a","b"],"vip":true,"raw":"<b>x</b>"}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		"Subject: Hi <Ana>", "0:a 1:b VIP", "<p>&lt;Ana&gt;</p><b>x</b>", "multipart/alternative",
	} {
		if !strings.Contains(aws.ToString(rendered.RenderedTemplate), want) {
			t.Fatalf("rendered template lacks %q:\n%s", want, aws.ToString(rendered.RenderedTemplate))
		}
	}

	_, err = c.TestRenderEmailTemplate(ctx, &sesv2.TestRenderEmailTemplateInput{
		TemplateName: aws.String("welcome"), TemplateData: aws.String(`{"items":[]}`),
	})
	if errorCode(err) != "BadRequestException" || !strings.Contains(err.Error(), "Attribute 'name'") {
		t.Fatalf("missing data: %v", err)
	}

	if _, err = c.UpdateEmailTemplate(ctx, &sesv2.UpdateEmailTemplateInput{
		TemplateName: aws.String("welcome"), TemplateContent: &sestypes.EmailTemplateContent{
			Subject: aws.String("Welcome {{name}}"), Text: aws.String("Hello {{name}}"),
		},
	}); err != nil {
		t.Fatal(err)
	}

	templates, err := c.ListEmailTemplates(ctx, &sesv2.ListEmailTemplatesInput{})
	if err != nil || len(templates.TemplatesMetadata) != 1 || templates.TemplatesMetadata[0].CreatedTimestamp == nil {
		t.Fatalf("%+v %v", templates, err)
	}

	if _, err = c.DeleteEmailTemplate(
		ctx,
		&sesv2.DeleteEmailTemplateInput{TemplateName: aws.String("welcome")},
	); err != nil {
		t.Fatal(err)
	}

	if _, err = c.GetEmailTemplate(
		ctx,
		&sesv2.GetEmailTemplateInput{TemplateName: aws.String("welcome")},
	); errorCode(
		err,
	) != "NotFoundException" {
		t.Fatalf("deleted template: %v", err)
	}

	if _, err = c.DeleteEmailIdentity(
		ctx,
		&sesv2.DeleteEmailIdentityInput{EmailIdentity: aws.String("example.com")},
	); err != nil {
		t.Fatal(err)
	}
}

func TestSESSendingAndEvents(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := sesClient(s)
	q := sqsClient(s, "test")
	topicQueue, topicQueueARN := queueARN(t, q, "ses-sns")
	busQueue, busQueueARN := queueARN(t, q, "ses-bus")
	topic, err := snsClient(s, "test").CreateTopic(ctx, &sns.CreateTopicInput{Name: aws.String("ses-events")})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = snsClient(s, "test").Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: topic.TopicArn, Protocol: aws.String("sqs"), Endpoint: aws.String(topicQueueARN),
		Attributes: map[string]string{"RawMessageDelivery": "true"},
	}); err != nil {
		t.Fatal(err)
	}

	events := eventsClient(s, "test")
	if _, err = events.PutRule(ctx, &eventbridge.PutRuleInput{
		Name: aws.String("ses"), EventPattern: aws.String(`{"source":["aws.ses"],"detail-type":["Email Bounced"]}`),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = events.PutTargets(ctx, &eventbridge.PutTargetsInput{
		Rule: aws.String("ses"), Targets: []ebtypes.Target{{Id: aws.String("q"), Arn: aws.String(busQueueARN)}},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.CreateConfigurationSet(ctx, &sesv2.CreateConfigurationSetInput{
		ConfigurationSetName: aws.String("tracked"),
	}); err != nil {
		t.Fatal(err)
	}

	for name, d := range map[string]*sestypes.EventDestinationDefinition{
		"topic": {
			Enabled: true, SnsDestination: &sestypes.SnsDestination{TopicArn: topic.TopicArn},
			MatchingEventTypes: []sestypes.EventType{"SEND", "DELIVERY", "BOUNCE", "COMPLAINT", "RENDERING_FAILURE"},
		},
		"bus": {
			Enabled: true, MatchingEventTypes: []sestypes.EventType{"BOUNCE"},
			EventBridgeDestination: &sestypes.EventBridgeDestination{
				EventBusArn: aws.String("arn:aws:events:us-east-1:000000000000:event-bus/default"),
			},
		},
	} {
		if _, err = c.CreateConfigurationSetEventDestination(ctx, &sesv2.CreateConfigurationSetEventDestinationInput{
			ConfigurationSetName: aws.String("tracked"), EventDestinationName: aws.String(name), EventDestination: d,
		}); err != nil {
			t.Fatal(err)
		}
	}

	_, err = c.CreateConfigurationSetEventDestination(ctx, &sesv2.CreateConfigurationSetEventDestinationInput{
		ConfigurationSetName: aws.String("tracked"), EventDestinationName: aws.String("metrics"),
		EventDestination: &sestypes.EventDestinationDefinition{
			MatchingEventTypes: []sestypes.EventType{"SEND"},
			CloudWatchDestination: &sestypes.CloudWatchDestination{
				DimensionConfigurations: []sestypes.CloudWatchDimensionConfiguration{},
			},
		},
	})
	if errorCode(err) != "NotImplemented" && errorCode(err) != "BadRequestException" {
		t.Fatalf("CloudWatch destination: %v", err)
	}

	destinations, err := c.GetConfigurationSetEventDestinations(ctx,
		&sesv2.GetConfigurationSetEventDestinationsInput{ConfigurationSetName: aws.String("tracked")})
	if err != nil || len(destinations.EventDestinations) != 2 {
		t.Fatalf("%+v %v", destinations, err)
	}

	simple := &sestypes.EmailContent{Simple: &sestypes.Message{
		Subject: &sestypes.Content{Data: aws.String("Hello")},
		Body:    &sestypes.Body{Text: &sestypes.Content{Data: aws.String("Hi there")}},
	}}
	_, err = c.SendEmail(ctx, &sesv2.SendEmailInput{
		FromEmailAddress: aws.String("me@unverified.example"), Content: simple,
		Destination: &sestypes.Destination{ToAddresses: []string{"you@example.net"}},
	})
	if errorCode(err) != "MessageRejected" {
		t.Fatalf("unverified sender: %v", err)
	}

	if _, err = c.CreateEmailIdentity(ctx, &sesv2.CreateEmailIdentityInput{
		EmailIdentity: aws.String("example.com"), ConfigurationSetName: aws.String("tracked"),
	}); err != nil {
		t.Fatal(err)
	}

	// The identity's configuration set publishes the message's events; the
	// mailbox simulator bounces bounce@.
	sent, err := c.SendEmail(ctx, &sesv2.SendEmailInput{
		FromEmailAddress: aws.String("Shop <noreply@example.com>"), Content: simple,
		Destination: &sestypes.Destination{
			ToAddresses: []string{"success@simulator.amazonses.com", "bounce@simulator.amazonses.com"},
		},
		EmailTags: []sestypes.MessageTag{{Name: aws.String("campaign"), Value: aws.String("spring")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	kinds := map[string]map[string]any{}
	for _, body := range drain(t, q, topicQueue, 3) {
		var e map[string]any
		if err = json.Unmarshal([]byte(body), &e); err != nil {
			t.Fatal(err)
		}

		mail, _ := e["mail"].(map[string]any)
		if mail["messageId"] != aws.ToString(sent.MessageId) {
			t.Fatalf("event for another message: %s", body)
		}

		kinds[e["eventType"].(string)] = e
	}

	delivered, _ := kinds["Delivery"]["delivery"].(map[string]any)
	if kinds["Send"] == nil || kinds["Bounce"] == nil ||
		!slices.Equal(toStrings(delivered["recipients"]), []string{"success@simulator.amazonses.com"}) {
		t.Fatalf("events = %v", kinds)
	}

	tags, _ := kinds["Send"]["mail"].(map[string]any)["tags"].(map[string]any)
	if !slices.Equal(toStrings(tags["campaign"]), []string{"spring"}) ||
		!slices.Equal(toStrings(tags["ses:configuration-set"]), []string{"tracked"}) {
		t.Fatalf("tags = %v", tags)
	}

	var bounced struct {
		Detail     map[string]any `json:"detail"`
		Source     string         `json:"source"`
		DetailType string         `json:"detail-type"`
	}
	if err = json.Unmarshal([]byte(drain(t, q, busQueue, 1)[0]), &bounced); err != nil {
		t.Fatal(err)
	}

	if bounced.Source != "aws.ses" || bounced.DetailType != "Email Bounced" || bounced.Detail["eventType"] != "Bounce" {
		t.Fatalf("EventBridge event = %+v", bounced)
	}

	messages := sesMessages(t, s)
	if len(messages) != 1 || messages[0]["subject"] != "Hello" || messages[0]["text"] != "Hi there" ||
		messages[0]["status"] != "delivered" {
		t.Fatalf("delivery log = %v", messages)
	}

	// A template with missing data is accepted but fails to render.
	if _, err = c.CreateEmailTemplate(ctx, &sesv2.CreateEmailTemplateInput{
		TemplateName: aws.String("note"), TemplateContent: &sestypes.EmailTemplateContent{
			Subject: aws.String("For {{name}}"), Text: aws.String("Code {{code}}"),
		},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.SendEmail(ctx, &sesv2.SendEmailInput{
		FromEmailAddress: aws.String("noreply@example.com"),
		Destination:      &sestypes.Destination{ToAddresses: []string{"ana@example.net"}},
		Content: &sestypes.EmailContent{Template: &sestypes.Template{
			TemplateName: aws.String("note"), TemplateData: aws.String(`{"name":"Ana"}`),
		}},
	}); err != nil {
		t.Fatal(err)
	}

	failure := drain(t, q, topicQueue, 2)
	if !slices.ContainsFunc(failure, func(b string) bool {
		return strings.Contains(b, `"eventType":"Rendering Failure"`) && strings.Contains(b, "Attribute 'code'")
	}) {
		t.Fatalf("rendering failure events = %q", failure)
	}

	// Bulk: each entry's replacement data overrides the default's.
	bulk, err := c.SendBulkEmail(ctx, &sesv2.SendBulkEmailInput{
		FromEmailAddress: aws.String("noreply@example.com"), ConfigurationSetName: aws.String("tracked"),
		DefaultContent: &sestypes.BulkEmailContent{Template: &sestypes.Template{
			TemplateName: aws.String("note"), TemplateData: aws.String(`{"name":"you","code":"0"}`),
		}},
		BulkEmailEntries: []sestypes.BulkEmailEntry{
			{
				Destination: &sestypes.Destination{ToAddresses: []string{"bo@example.net"}},
				ReplacementEmailContent: &sestypes.ReplacementEmailContent{
					ReplacementTemplate: &sestypes.ReplacementTemplate{
						ReplacementTemplateData: aws.String(`{"code":"42"}`),
					},
				},
			},
			{Destination: &sestypes.Destination{}},
		},
	})
	if err != nil || len(bulk.BulkEmailEntryResults) != 2 ||
		bulk.BulkEmailEntryResults[0].Status != sestypes.BulkEmailStatusSuccess ||
		bulk.BulkEmailEntryResults[1].Status != sestypes.BulkEmailStatusInvalidParameter {
		t.Fatalf("%+v %v", bulk, err)
	}

	drain(t, q, topicQueue, 2)
	if m := sesMessages(t, s)[0]; m["subject"] != "For you" || m["text"] != "Code 42" ||
		m["target"] != "bo@example.net" {
		t.Fatalf("bulk message = %v", m)
	}

	account, err := c.GetAccount(ctx, &sesv2.GetAccountInput{})
	if err != nil || account.SendQuota.SentLast24Hours != 3 || !account.ProductionAccessEnabled {
		t.Fatalf("%+v %v", account, err)
	}

	// A configuration set whose sending is paused refuses to send.
	if _, err = c.CreateConfigurationSet(ctx, &sesv2.CreateConfigurationSetInput{
		ConfigurationSetName: aws.String("paused"), SendingOptions: &sestypes.SendingOptions{SendingEnabled: false},
	}); err != nil {
		t.Fatal(err)
	}

	_, err = c.SendEmail(ctx, &sesv2.SendEmailInput{
		FromEmailAddress: aws.String("noreply@example.com"), ConfigurationSetName: aws.String("paused"),
		Destination: &sestypes.Destination{ToAddresses: []string{"ana@example.net"}}, Content: simple,
	})
	if errorCode(err) != "SendingPausedException" {
		t.Fatalf("paused: %v", err)
	}

	raw := "From: noreply@example.com\r\nTo: Cy <cy@example.net>\r\nSubject: =?UTF-8?Q?Caf=C3=A9?=\r\n\r\nbody"
	if _, err = c.SendEmail(ctx, &sesv2.SendEmailInput{
		Content: &sestypes.EmailContent{Raw: &sestypes.RawMessage{Data: []byte(raw)}},
	}); err != nil {
		t.Fatal(err)
	}

	if m := sesMessages(t, s)[0]; m["subject"] != "Café" || m["target"] != `"Cy" <cy@example.net>` {
		t.Fatalf("raw message = %v", m)
	}

	sets, err := c.ListConfigurationSets(ctx, &sesv2.ListConfigurationSetsInput{})
	if err != nil || !slices.Equal(sets.ConfigurationSets, []string{"paused", "tracked"}) {
		t.Fatalf("%+v %v", sets, err)
	}
}

func toStrings(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, e := range list {
		s, _ := e.(string)
		out = append(out, s)
	}

	return out
}
