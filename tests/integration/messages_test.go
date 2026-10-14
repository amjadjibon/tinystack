package integration_test

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	cip "github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider"
	ciptypes "github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider/types"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	sestypes "github.com/aws/aws-sdk-go-v2/service/sesv2/types"
	"github.com/aws/aws-sdk-go-v2/service/sns"

	"github.com/amjadjibon/tinystack/internal/delivery"
)

func TestMessagesInspection(t *testing.T) {
	s := server(t)
	ctx := context.Background()

	// 1. Send SES email
	ses := sesClient(s)
	_, err := ses.CreateEmailIdentity(ctx, &sesv2.CreateEmailIdentityInput{
		EmailIdentity: aws.String("sender@example.com"),
	})
	if err != nil {
		t.Fatal(err)
	}

	sesSent, err := ses.SendEmail(ctx, &sesv2.SendEmailInput{
		FromEmailAddress: aws.String("sender@example.com"),
		Destination: &sestypes.Destination{
			ToAddresses: []string{"recipient@example.com"},
		},
		Content: &sestypes.EmailContent{
			Simple: &sestypes.Message{
				Subject: &sestypes.Content{Data: aws.String("Order Confirmation")},
				Body: &sestypes.Body{
					Text: &sestypes.Content{Data: aws.String("Your order has been placed.")},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sesMsgID := aws.ToString(sesSent.MessageId)

	// 2. Send SNS SMS
	topics := snsClient(s, "test")
	_, err = topics.Publish(ctx, &sns.PublishInput{
		PhoneNumber: aws.String("+15555550123"),
		Message:     aws.String("Your security code is 445566"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// 3. Trigger Cognito verification code
	admin := cognitoClient(s, "test", "us-east-1")
	pool := createPool(t, admin, "messages-pool", &cip.CreateUserPoolInput{
		AutoVerifiedAttributes: []ciptypes.VerifiedAttributeType{ciptypes.VerifiedAttributeTypeEmail},
	})
	clientRes, err := admin.CreateUserPoolClient(ctx, &cip.CreateUserPoolClientInput{
		UserPoolId: pool.Id, ClientName: aws.String("client-app"),
	})
	if err != nil {
		t.Fatal(err)
	}

	users := cognitoClient(s, "test", "us-east-1")
	_, err = users.SignUp(ctx, &cip.SignUpInput{
		ClientId: clientRes.UserPoolClient.ClientId,
		Username: aws.String("cognito-user@example.com"),
		Password: aws.String("P@ssw0rd123!"),
		UserAttributes: []ciptypes.AttributeType{
			{Name: aws.String("email"), Value: aws.String("cognito-user@example.com")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// 4. Test GET /_tinystack/messages
	t.Run("list all messages", func(t *testing.T) {
		var resp struct {
			Messages []delivery.Message `json:"messages"`
		}
		status := internal(t, s, http.MethodGet, "/_tinystack/messages", &resp)
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}

		if len(resp.Messages) != 3 {
			t.Fatalf("expected 3 messages, got %d: %+v", len(resp.Messages), resp.Messages)
		}

		// Newest first: Cognito sign-up, SNS SMS, SES email
		first := resp.Messages[0]
		if first.Service != "cognito-idp" || first.To != "cognito-user@example.com" {
			t.Fatalf("expected newest to be cognito, got %+v", first)
		}
	})

	t.Run("filter channel=sms", func(t *testing.T) {
		var resp struct {
			Messages []delivery.Message `json:"messages"`
		}
		status := internal(t, s, http.MethodGet, "/_tinystack/messages?channel=sms", &resp)
		if status != http.StatusOK {
			t.Fatalf("status = %d", status)
		}

		if len(resp.Messages) != 1 || resp.Messages[0].To != "+15555550123" {
			t.Fatalf("expected 1 sms message, got %+v", resp.Messages)
		}
	})

	t.Run("filter service=ses", func(t *testing.T) {
		var resp struct {
			Messages []delivery.Message `json:"messages"`
		}
		status := internal(t, s, http.MethodGet, "/_tinystack/messages?service=ses", &resp)
		if status != http.StatusOK {
			t.Fatalf("status = %d", status)
		}

		if len(resp.Messages) != 1 || resp.Messages[0].ID != sesMsgID {
			t.Fatalf("expected 1 ses message with ID %s, got %+v", sesMsgID, resp.Messages)
		}
	})

	t.Run("filter to recipient", func(t *testing.T) {
		var resp struct {
			Messages []delivery.Message `json:"messages"`
		}
		status := internal(t, s, http.MethodGet, "/_tinystack/messages?to=recipient@example.com", &resp)
		if status != http.StatusOK {
			t.Fatalf("status = %d", status)
		}

		if len(resp.Messages) != 1 || resp.Messages[0].Subject != "Order Confirmation" {
			t.Fatalf("expected 1 order confirmation, got %+v", resp.Messages)
		}
	})

	t.Run("filter since parameter", func(t *testing.T) {
		since := url.QueryEscape(time.Now().Add(-5 * time.Minute).Format(time.RFC3339))
		var resp struct {
			Messages []delivery.Message `json:"messages"`
		}
		status := internal(t, s, http.MethodGet, "/_tinystack/messages?since="+since, &resp)
		if status != http.StatusOK {
			t.Fatalf("status = %d", status)
		}
		if len(resp.Messages) != 3 {
			t.Fatalf("expected 3 messages, got %d", len(resp.Messages))
		}

		future := url.QueryEscape(time.Now().Add(5 * time.Minute).Format(time.RFC3339))
		status = internal(t, s, http.MethodGet, "/_tinystack/messages?since="+future, &resp)
		if status != http.StatusOK {
			t.Fatalf("status = %d", status)
		}
		if len(resp.Messages) != 0 {
			t.Fatalf("expected 0 messages for future since, got %d", len(resp.Messages))
		}
	})

	t.Run("unknown query parameter returns 400", func(t *testing.T) {
		status := internal(t, s, http.MethodGet, "/_tinystack/messages?bad_param=true", nil)
		if status != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", status)
		}
	})

	// 5. Test LocalStack aliases
	t.Run("GET /_aws/ses", func(t *testing.T) {
		var resp struct {
			Messages []delivery.LocalStackSESMessage `json:"messages"`
		}
		status := internal(t, s, http.MethodGet, "/_aws/ses", &resp)
		if status != http.StatusOK {
			t.Fatalf("status = %d", status)
		}

		if len(resp.Messages) != 1 {
			t.Fatalf("expected 1 ses message, got %d", len(resp.Messages))
		}

		msg := resp.Messages[0]
		if msg.Id != sesMsgID || msg.Source != "sender@example.com" || msg.Subject != "Order Confirmation" {
			t.Fatalf("unexpected ses message fields: %+v", msg)
		}

		// Filter by ?id=
		var respID struct {
			Messages []delivery.LocalStackSESMessage `json:"messages"`
		}
		status = internal(t, s, http.MethodGet, "/_aws/ses?id="+sesMsgID, &respID)
		if status != http.StatusOK || len(respID.Messages) != 1 {
			t.Fatalf("filter by id failed: status %d, len %d", status, len(respID.Messages))
		}

		// Filter by ?email=
		var respEmail struct {
			Messages []delivery.LocalStackSESMessage `json:"messages"`
		}
		status = internal(t, s, http.MethodGet, "/_aws/ses?email=recipient@example.com", &respEmail)
		if status != http.StatusOK || len(respEmail.Messages) != 1 {
			t.Fatalf("filter by email failed: status %d, len %d", status, len(respEmail.Messages))
		}
	})

	t.Run("GET /_aws/sns/sms-messages", func(t *testing.T) {
		var resp struct {
			SMSMessages map[string][]delivery.LocalStackSMSItem `json:"sms_messages"`
			Region      string                                  `json:"region"`
		}
		status := internal(t, s, http.MethodGet, "/_aws/sns/sms-messages", &resp)
		if status != http.StatusOK {
			t.Fatalf("status = %d", status)
		}

		list, ok := resp.SMSMessages["+15555550123"]
		if !ok || len(list) != 1 {
			t.Fatalf("expected message for +15555550123, got %+v", resp.SMSMessages)
		}

		if list[0].Message != "Your security code is 445566" {
			t.Fatalf("unexpected message content: %q", list[0].Message)
		}

		// Filter by ?phoneNumber=
		var respPhone struct {
			SMSMessages map[string][]delivery.LocalStackSMSItem `json:"sms_messages"`
		}
		status = internal(
			t,
			s,
			http.MethodGet,
			"/_aws/sns/sms-messages?phoneNumber="+url.QueryEscape("+15555550123"),
			&respPhone,
		)
		if status != http.StatusOK || len(respPhone.SMSMessages["+15555550123"]) != 1 {
			t.Fatalf("filter by phone failed: status %d, len %d", status, len(respPhone.SMSMessages))
		}
	})

	// 6. Test DELETE /_tinystack/messages
	t.Run("DELETE /_tinystack/messages", func(t *testing.T) {
		status := internal(t, s, http.MethodDelete, "/_tinystack/messages", nil)
		if status != http.StatusNoContent {
			t.Fatalf("DELETE status = %d, want 204", status)
		}

		var resp struct {
			Messages []delivery.Message `json:"messages"`
		}
		status = internal(t, s, http.MethodGet, "/_tinystack/messages", &resp)
		if status != http.StatusOK || len(resp.Messages) != 0 {
			t.Fatalf("messages after delete: status %d, len %d", status, len(resp.Messages))
		}
	})
}
