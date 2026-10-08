package integration_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ses"
	sesclassic "github.com/aws/aws-sdk-go-v2/service/ses/types"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	sestypes "github.com/aws/aws-sdk-go-v2/service/sesv2/types"
)

func sesClassicClient(s *httptest.Server) *ses.Client {
	return ses.New(ses.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
		}),
		Retryer: aws.NopRetryer{},
	})
}

// TestSESClassicSharesIdentities verifies identities with API v1 and v2 and
// reads each with the other, as both APIs see one set of identities.
func TestSESClassicSharesIdentities(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	v1, v2 := sesClassicClient(s), sesClient(s)
	if _, err := v1.VerifyEmailIdentity(
		ctx,
		&ses.VerifyEmailIdentityInput{EmailAddress: aws.String("ana@a.example")},
	); err != nil {
		t.Fatal(err)
	}

	// Verifying twice is allowed in API v1.
	if _, err := v1.VerifyEmailAddress(
		ctx,
		&ses.VerifyEmailAddressInput{EmailAddress: aws.String("ana@a.example")},
	); err != nil {
		t.Fatal(err)
	}

	if _, err := v1.VerifyEmailAddress(ctx, &ses.VerifyEmailAddressInput{
		EmailAddress: aws.String("bo@b.example"),
	}); err != nil {
		t.Fatal(err)
	}

	domain, err := v1.VerifyDomainIdentity(ctx, &ses.VerifyDomainIdentityInput{Domain: aws.String("example.com")})
	if err != nil || aws.ToString(domain.VerificationToken) == "" {
		t.Fatalf("%+v %v", domain, err)
	}

	dkim, err := v1.VerifyDomainDkim(ctx, &ses.VerifyDomainDkimInput{Domain: aws.String("example.com")})
	if err != nil || len(dkim.DkimTokens) != 3 {
		t.Fatalf("%+v %v", dkim, err)
	}

	if _, err = v1.VerifyDomainIdentity(
		ctx,
		&ses.VerifyDomainIdentityInput{Domain: aws.String("not a domain")},
	); err == nil {
		t.Fatal("an invalid domain was verified")
	}

	// API v2 sees them, and its DKIM tokens are API v1's.
	got, err := v2.GetEmailIdentity(ctx, &sesv2.GetEmailIdentityInput{EmailIdentity: aws.String("example.com")})
	if err != nil || got.IdentityType != sestypes.IdentityTypeDomain ||
		!slices.Equal(got.DkimAttributes.Tokens, dkim.DkimTokens) {
		t.Fatalf("%+v %v", got, err)
	}

	// An identity created with API v2 is visible to API v1.
	if _, err = v2.CreateEmailIdentity(ctx, &sesv2.CreateEmailIdentityInput{
		EmailIdentity: aws.String("cy@c.example"),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = v2.CreateEmailIdentity(
		ctx,
		&sesv2.CreateEmailIdentityInput{EmailIdentity: aws.String("other.org")},
	); err != nil {
		t.Fatal(err)
	}

	all, err := v1.ListIdentities(ctx, &ses.ListIdentitiesInput{})
	if err != nil || len(all.Identities) != 5 {
		t.Fatalf("%+v %v", all, err)
	}

	addresses, err := v1.ListIdentities(
		ctx,
		&ses.ListIdentitiesInput{IdentityType: sesclassic.IdentityTypeEmailAddress},
	)
	if err != nil || !slices.Equal(addresses.Identities, []string{"ana@a.example", "bo@b.example", "cy@c.example"}) {
		t.Fatalf("%+v %v", addresses, err)
	}

	domains, err := v1.ListIdentities(ctx, &ses.ListIdentitiesInput{
		IdentityType: sesclassic.IdentityTypeDomain, MaxItems: aws.Int32(1),
	})
	if err != nil || !slices.Equal(domains.Identities, []string{"example.com"}) || domains.NextToken == nil {
		t.Fatalf("%+v %v", domains, err)
	}

	rest, err := v1.ListIdentities(ctx, &ses.ListIdentitiesInput{
		IdentityType: sesclassic.IdentityTypeDomain, NextToken: domains.NextToken,
	})
	if err != nil || !slices.Equal(rest.Identities, []string{"other.org"}) {
		t.Fatalf("%+v %v", rest, err)
	}

	verified, err := v1.ListVerifiedEmailAddresses(ctx, &ses.ListVerifiedEmailAddressesInput{})
	if err != nil || len(verified.VerifiedEmailAddresses) != 3 {
		t.Fatalf("%+v %v", verified, err)
	}

	attributes, err := v1.GetIdentityVerificationAttributes(ctx, &ses.GetIdentityVerificationAttributesInput{
		Identities: []string{"example.com", "cy@c.example", "unknown@x.example"},
	})
	if err != nil || len(attributes.VerificationAttributes) != 2 {
		t.Fatalf("%+v %v", attributes, err)
	}

	d := attributes.VerificationAttributes["example.com"]
	if d.VerificationStatus != sesclassic.VerificationStatusSuccess ||
		aws.ToString(d.VerificationToken) != aws.ToString(domain.VerificationToken) {
		t.Fatalf("%+v", d)
	}

	a := attributes.VerificationAttributes["cy@c.example"]
	if a.VerificationStatus != sesclassic.VerificationStatusSuccess || a.VerificationToken != nil {
		t.Fatalf("%+v", a)
	}

	// Deleting is idempotent, and API v2 sees the deletion.
	for range 2 {
		if _, err = v1.DeleteIdentity(ctx, &ses.DeleteIdentityInput{Identity: aws.String("example.com")}); err != nil {
			t.Fatal(err)
		}
	}

	_, err = v2.GetEmailIdentity(ctx, &sesv2.GetEmailIdentityInput{EmailIdentity: aws.String("example.com")})
	if errorCode(err) != "NotFoundException" {
		t.Fatalf("deleted identity: %v", err)
	}
}

// TestSESClassicSending sends with API v1 and checks the send quota and
// statistics, the delivery log, and the templates and configuration sets it
// shares with API v2.
func TestSESClassicSending(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	v1, v2 := sesClassicClient(s), sesClient(s)
	send := &ses.SendEmailInput{
		Source: aws.String("ana@a.example"),
		Destination: &sesclassic.Destination{
			ToAddresses: []string{"bo@b.example"},
			CcAddresses: []string{"cy@c.example"},
		},
		Message: &sesclassic.Message{
			Subject: &sesclassic.Content{Data: aws.String("Hello"), Charset: aws.String("UTF-8")},
			Body: &sesclassic.Body{
				Text: &sesclassic.Content{
					Data: aws.String("plain"),
				},
				Html: &sesclassic.Content{Data: aws.String("<b>html</b>")},
			},
		},
		Tags: []sesclassic.MessageTag{{Name: aws.String("campaign"), Value: aws.String("x")}},
	}
	_, err := v1.SendEmail(ctx, send)
	if errorCode(err) != "MessageRejected" {
		t.Fatalf("unverified sender: %v", err)
	}

	// The identity is verified through API v2; API v1 sends as it.
	if _, err = v2.CreateEmailIdentity(
		ctx,
		&sesv2.CreateEmailIdentityInput{EmailIdentity: aws.String("a.example")},
	); err != nil {
		t.Fatal(err)
	}

	sent, err := v1.SendEmail(ctx, send)
	if err != nil || aws.ToString(sent.MessageId) == "" {
		t.Fatalf("%+v %v", sent, err)
	}

	messages := sesMessages(t, s)
	if len(messages) == 0 || messages[0]["messageId"] != aws.ToString(sent.MessageId) ||
		messages[0]["subject"] != "Hello" || messages[0]["html"] != "<b>html</b>" {
		t.Fatalf("%+v", messages)
	}

	raw := "From: Ana <ana@a.example>\r\nTo: bo@b.example, Cy <cy@c.example>\r\nSubject: Raw hello\r\n" +
		"MIME-Version: 1.0\r\nContent-Type: text/plain\r\n\r\nraw body\r\n"
	rawSent, err := v1.SendRawEmail(ctx, &ses.SendRawEmailInput{RawMessage: &sesclassic.RawMessage{Data: []byte(raw)}})
	if err != nil {
		t.Fatal(err)
	}

	messages = sesMessages(t, s)
	if len(messages) == 0 || messages[0]["messageId"] != aws.ToString(rawSent.MessageId) ||
		messages[0]["subject"] != "Raw hello" || !strings.Contains(messages[0]["from"].(string), "ana@a.example") {
		t.Fatalf("%+v", messages)
	}

	_, err = v1.SendRawEmail(ctx, &ses.SendRawEmailInput{
		Source:     aws.String("stranger@elsewhere.example"),
		RawMessage: &sesclassic.RawMessage{Data: []byte(raw)}, Destinations: []string{"bo@b.example"},
	})
	if errorCode(err) != "MessageRejected" {
		t.Fatalf("unverified raw sender: %v", err)
	}

	// A template created with API v1 is API v2's, and the reverse.
	if _, err = v1.CreateTemplate(ctx, &ses.CreateTemplateInput{Template: &sesclassic.Template{
		TemplateName: aws.String(
			"welcome",
		),
		SubjectPart: aws.String("Hi {{name}}"),
		TextPart:    aws.String("Hello {{name}}"),
	}}); err != nil {
		t.Fatal(err)
	}

	if _, err = v2.CreateEmailTemplate(ctx, &sesv2.CreateEmailTemplateInput{
		TemplateName:    aws.String("bye"),
		TemplateContent: &sestypes.EmailTemplateContent{Subject: aws.String("Bye {{name}}"), Text: aws.String("Bye")},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = v1.CreateTemplate(ctx, &ses.CreateTemplateInput{Template: &sesclassic.Template{
		TemplateName: aws.String("bye"), SubjectPart: aws.String("x"),
	}}); errorCode(err) != "AlreadyExists" {
		t.Fatalf("duplicate template: %v", err)
	}

	if _, err = v1.CreateTemplate(ctx, &ses.CreateTemplateInput{Template: &sesclassic.Template{
		TemplateName: aws.String("broken"), SubjectPart: aws.String("{{#if x}}never closed"),
	}}); errorCode(err) != "InvalidTemplate" {
		t.Fatalf("broken template: %v", err)
	}

	listed, err := v1.ListTemplates(ctx, &ses.ListTemplatesInput{})
	if err != nil || len(listed.TemplatesMetadata) != 2 || listed.TemplatesMetadata[0].CreatedTimestamp == nil {
		t.Fatalf("%+v %v", listed, err)
	}

	got, err := v1.GetTemplate(ctx, &ses.GetTemplateInput{TemplateName: aws.String("bye")})
	if err != nil || aws.ToString(got.Template.SubjectPart) != "Bye {{name}}" {
		t.Fatalf("%+v %v", got, err)
	}

	if _, err = v1.UpdateTemplate(ctx, &ses.UpdateTemplateInput{Template: &sesclassic.Template{
		TemplateName: aws.String("bye"), SubjectPart: aws.String("Goodbye {{name}}"),
	}}); err != nil {
		t.Fatal(err)
	}

	templated, err := v1.SendTemplatedEmail(ctx, &ses.SendTemplatedEmailInput{
		Source: aws.String(
			"ana@a.example",
		),
		Destination:  &sesclassic.Destination{ToAddresses: []string{"bo@b.example"}},
		Template:     aws.String("welcome"),
		TemplateData: aws.String(`{"name":"Bo"}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	messages = sesMessages(t, s)
	if messages[0]["messageId"] != aws.ToString(templated.MessageId) || messages[0]["subject"] != "Hi Bo" ||
		messages[0]["text"] != "Hello Bo" {
		t.Fatalf("%+v", messages[0])
	}

	_, err = v1.SendTemplatedEmail(ctx, &ses.SendTemplatedEmailInput{
		Source: aws.String(
			"ana@a.example",
		),
		Destination:  &sesclassic.Destination{ToAddresses: []string{"bo@b.example"}},
		Template:     aws.String("nope"),
		TemplateData: aws.String(`{}`),
	})
	if errorCode(err) != "TemplateDoesNotExist" {
		t.Fatalf("missing template: %v", err)
	}

	bulk, err := v1.SendBulkTemplatedEmail(ctx, &ses.SendBulkTemplatedEmailInput{
		Source: aws.String(
			"ana@a.example",
		),
		Template:            aws.String("welcome"),
		DefaultTemplateData: aws.String(`{"name":"you"}`),
		Destinations: []sesclassic.BulkEmailDestination{
			{
				Destination:             &sesclassic.Destination{ToAddresses: []string{"bo@b.example"}},
				ReplacementTemplateData: aws.String(`{"name":"Bo"}`),
			},
			{Destination: &sesclassic.Destination{ToAddresses: []string{"cy@c.example"}}},
			{Destination: &sesclassic.Destination{ToAddresses: []string{"not an address"}}},
		},
	})
	if err != nil || len(bulk.Status) != 3 || bulk.Status[0].Status != sesclassic.BulkEmailStatusSuccess ||
		bulk.Status[1].Status != sesclassic.BulkEmailStatusSuccess ||
		bulk.Status[2].Status != sesclassic.BulkEmailStatusInvalidParameterValue {
		t.Fatalf("%+v %v", bulk, err)
	}

	subjects := []string{}
	for _, m := range sesMessages(t, s)[:2] {
		subjects = append(subjects, m["subject"].(string))
	}

	slices.Sort(subjects)
	if !slices.Equal(subjects, []string{"Hi Bo", "Hi you"}) {
		t.Fatalf("%q", subjects)
	}

	if _, err = v1.DeleteTemplate(ctx, &ses.DeleteTemplateInput{TemplateName: aws.String("bye")}); err != nil {
		t.Fatal(err)
	}

	if _, err = v1.DeleteTemplate(ctx, &ses.DeleteTemplateInput{TemplateName: aws.String("bye")}); err != nil {
		t.Fatalf("API v1 deletes a missing template without error: %v", err)
	}

	if _, err = v2.GetEmailTemplate(
		ctx,
		&sesv2.GetEmailTemplateInput{TemplateName: aws.String("bye")},
	); errorCode(
		err,
	) != "NotFoundException" {
		t.Fatalf("deleted template: %v", err)
	}

	// Configuration sets are shared, and a send names one.
	if _, err = v1.CreateConfigurationSet(ctx, &ses.CreateConfigurationSetInput{
		ConfigurationSet: &sesclassic.ConfigurationSet{Name: aws.String("tracked")},
	}); err != nil {
		t.Fatal(err)
	}

	_, err = v1.CreateConfigurationSet(ctx, &ses.CreateConfigurationSetInput{
		ConfigurationSet: &sesclassic.ConfigurationSet{Name: aws.String("tracked")},
	})
	if errorCode(err) != "ConfigurationSetAlreadyExists" {
		t.Fatalf("duplicate set: %v", err)
	}

	if _, err = v2.GetConfigurationSet(
		ctx,
		&sesv2.GetConfigurationSetInput{ConfigurationSetName: aws.String("tracked")},
	); err != nil {
		t.Fatal(err)
	}

	sets, err := v1.ListConfigurationSets(ctx, &ses.ListConfigurationSetsInput{})
	if err != nil || len(sets.ConfigurationSets) != 1 || aws.ToString(sets.ConfigurationSets[0].Name) != "tracked" {
		t.Fatalf("%+v %v", sets, err)
	}

	described, err := v1.DescribeConfigurationSet(
		ctx,
		&ses.DescribeConfigurationSetInput{ConfigurationSetName: aws.String("tracked")},
	)
	if err != nil || aws.ToString(described.ConfigurationSet.Name) != "tracked" {
		t.Fatalf("%+v %v", described, err)
	}

	send.ConfigurationSetName = aws.String("tracked")
	if _, err = v1.SendEmail(ctx, send); err != nil {
		t.Fatal(err)
	}

	send.ConfigurationSetName = aws.String("absent")
	if _, err = v1.SendEmail(ctx, send); errorCode(err) != "ConfigurationSetDoesNotExist" {
		t.Fatalf("missing set: %v", err)
	}

	if _, err = v1.DeleteConfigurationSet(
		ctx,
		&ses.DeleteConfigurationSetInput{ConfigurationSetName: aws.String("tracked")},
	); err != nil {
		t.Fatal(err)
	}

	_, err = v1.DescribeConfigurationSet(ctx, &ses.DescribeConfigurationSetInput{
		ConfigurationSetName: aws.String("tracked"),
	})
	if errorCode(err) != "ConfigurationSetDoesNotExist" {
		t.Fatalf("deleted set: %v", err)
	}

	// Six messages were sent (two plain, one raw, one templated, two bulk) and API v2 counts the same.
	quota, err := v1.GetSendQuota(ctx, &ses.GetSendQuotaInput{})
	if err != nil || quota.SentLast24Hours != 6 || quota.Max24HourSend <= 0 || quota.MaxSendRate <= 0 {
		t.Fatalf("%+v %v", quota, err)
	}

	account, err := v2.GetAccount(ctx, &sesv2.GetAccountInput{})
	if err != nil || account.SendQuota.SentLast24Hours != quota.SentLast24Hours {
		t.Fatalf("%+v %v", account, err)
	}

	stats, err := v1.GetSendStatistics(ctx, &ses.GetSendStatisticsInput{})
	if err != nil || len(stats.SendDataPoints) == 0 {
		t.Fatalf("%+v %v", stats, err)
	}

	total := int64(0)
	for _, p := range stats.SendDataPoints {
		total += p.DeliveryAttempts
	}

	if total != 6 {
		t.Fatalf("statistics count %d messages: %+v", total, stats.SendDataPoints)
	}
}

// TestSESClassicRouting checks that the gateway sends a form request signed
// for "ses" to API v1 while v2's REST calls under the same signing name still
// reach API v2.
func TestSESClassicRouting(t *testing.T) {
	s := server(t)
	const authorization = "AWS4-HMAC-SHA256 Credential=test/20260101/us-east-1/ses/aws4_request, " +
		"SignedHeaders=host, Signature=00"
	do := func(method, path, contentType, body string) (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(context.Background(), method, s.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}

		req.Header.Set("Authorization", authorization)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}

		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}

		return resp.StatusCode, string(data)
	}

	form := url.Values{"Action": {"GetSendQuota"}, "Version": {"2010-12-01"}}.Encode()
	status, body := do(http.MethodPost, "/", "application/x-www-form-urlencoded", form)
	if status != http.StatusOK || !strings.Contains(body, "<GetSendQuotaResult>") ||
		!strings.Contains(body, "Max24HourSend") {
		t.Fatalf("v1 form request: %d %s", status, body)
	}

	status, body = do(http.MethodGet, "/v2/email/account", "", "")
	if status != http.StatusOK || !strings.Contains(body, `"SendQuota"`) {
		t.Fatalf("v2 REST request: %d %s", status, body)
	}

	status, body = do(
		http.MethodPost,
		"/v2/email/identities",
		"application/json",
		`{"EmailIdentity":"routing.example"}`,
	)
	if status != http.StatusOK || !strings.Contains(body, "DOMAIN") {
		t.Fatalf("v2 REST create: %d %s", status, body)
	}

	// An operation v1 does not implement is not found in v1, not sent to REST.
	status, body = do(http.MethodPost, "/", "application/x-www-form-urlencoded",
		url.Values{"Action": {"SetIdentityDkimEnabled"}, "Version": {"2010-12-01"}}.Encode())
	if status == http.StatusOK {
		t.Fatalf("unsupported v1 operation: %d %s", status, body)
	}
}
