package delivery_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/amjadjibon/tinystack/internal/delivery"
)

func TestMessagesHandler(t *testing.T) {
	log := delivery.New()

	// 1. Add non-outbound delivery
	ruleARN := "arn:aws:events:us-east-1:123456789012:rule/my-rule"
	log.Add(delivery.ViaEventBridge, ruleARN, "target-queue", []byte(`{"event":"test"}`), nil)

	// 2. Add SES email
	sesJSON := `{"messageId":"ses-msg-123","from":"sender@example.com","subject":"Welcome",` +
		`"text":"Hello world!","to":["alice@example.com","bob@example.com"]}`
	sesSource := "arn:aws:ses:us-east-1:123456789012:identity/sender@example.com"
	log.Add(delivery.ViaSES, sesSource, "alice@example.com, bob@example.com", []byte(sesJSON), nil)

	// 3. Add SNS SMS
	snsSource := "arn:aws:sns:us-west-2:123456789012:topic-name"
	log.Add(delivery.ViaSNS, snsSource, "+15555550100", []byte("SMS code 4567"), nil)

	// 4. Add Cognito email code
	cognitoEmailJSON := `{"userPoolId":"us-east-1_PoolId","username":"charlie",` +
		`"destination":"charlie@example.com","subject":"Your verification code",` +
		`"message":"Your code is 987654","code":"987654"}`
	cognitoSource := "arn:aws:cognito-idp:us-east-1:123456789012:userpool/us-east-1_PoolId"
	log.Add(delivery.ViaCognito, cognitoSource, "email:charlie@example.com", []byte(cognitoEmailJSON), nil)

	// 5. Add Cognito SMS code
	cognitoSMSJSON := `{"userPoolId":"us-east-1_PoolId","username":"dave",` +
		`"destination":"+15555550200","message":"Your code is 112233","code":"112233"}`
	log.Add(delivery.ViaCognito, cognitoSource, "sms:+15555550200", []byte(cognitoSMSJSON), nil)

	h := delivery.MessagesHandler(log)

	t.Run("GET list all messages newest first", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/_tinystack/messages", http.NoBody)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}

		var resp struct {
			Messages []delivery.Message `json:"messages"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}

		// 4 outbound messages (EventBridge ignored)
		if len(resp.Messages) != 4 {
			t.Fatalf("expected 4 messages, got %d", len(resp.Messages))
		}

		// Newest first: dave (cognito sms), charlie (cognito email), sns sms, ses email
		first := resp.Messages[0]
		if first.Channel != "sms" || first.To != "+15555550200" || first.Service != "cognito-idp" {
			t.Fatalf("unexpected first message: %+v", first)
		}
	})

	t.Run("filter by channel=email", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/_tinystack/messages?channel=email", http.NoBody)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		var resp struct {
			Messages []delivery.Message `json:"messages"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)

		if len(resp.Messages) != 2 {
			t.Fatalf("expected 2 email messages, got %d", len(resp.Messages))
		}
	})

	t.Run("filter by channel=sms", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/_tinystack/messages?channel=sms", http.NoBody)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		var resp struct {
			Messages []delivery.Message `json:"messages"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)

		if len(resp.Messages) != 2 {
			t.Fatalf("expected 2 sms messages, got %d", len(resp.Messages))
		}
	})

	t.Run("filter by to", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/_tinystack/messages?to=alice@example.com", http.NoBody)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		var resp struct {
			Messages []delivery.Message `json:"messages"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)

		if len(resp.Messages) != 1 || resp.Messages[0].ID != "ses-msg-123" {
			t.Fatalf("expected 1 match for alice, got %v", resp.Messages)
		}
	})

	t.Run("filter by service", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/_tinystack/messages?service=ses", http.NoBody)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		var resp struct {
			Messages []delivery.Message `json:"messages"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)

		if len(resp.Messages) != 1 || resp.Messages[0].Service != "ses" {
			t.Fatalf("expected 1 match for ses, got %v", resp.Messages)
		}
	})

	t.Run("filter by since RFC 3339", func(t *testing.T) {
		since := time.Now().Add(-1 * time.Hour).Format(time.RFC3339)
		req := httptest.NewRequest(http.MethodGet, "/_tinystack/messages?since="+since, http.NoBody)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}

		future := time.Now().Add(1 * time.Hour).Format(time.RFC3339)
		req2 := httptest.NewRequest(http.MethodGet, "/_tinystack/messages?since="+future, http.NoBody)
		rec2 := httptest.NewRecorder()
		h.ServeHTTP(rec2, req2)

		var resp struct {
			Messages []delivery.Message `json:"messages"`
		}
		_ = json.Unmarshal(rec2.Body.Bytes(), &resp)
		if len(resp.Messages) != 0 {
			t.Fatalf("expected 0 messages for future since, got %d", len(resp.Messages))
		}
	})

	t.Run("unknown query parameter returns 400", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/_tinystack/messages?foo=bar", http.NoBody)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("invalid since returns 400", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/_tinystack/messages?since=yesterday", http.NoBody)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("DELETE clears only outbound messages", func(t *testing.T) {
		delReq := httptest.NewRequest(http.MethodDelete, "/_tinystack/messages", http.NoBody)
		delRec := httptest.NewRecorder()
		h.ServeHTTP(delRec, delReq)

		if delRec.Code != http.StatusNoContent {
			t.Fatalf("delete status = %d, want 204", delRec.Code)
		}

		// GET messages should now be empty
		getReq := httptest.NewRequest(http.MethodGet, "/_tinystack/messages", http.NoBody)
		getRec := httptest.NewRecorder()
		h.ServeHTTP(getRec, getReq)

		var resp struct {
			Messages []delivery.Message `json:"messages"`
		}
		_ = json.Unmarshal(getRec.Body.Bytes(), &resp)
		if len(resp.Messages) != 0 {
			t.Fatalf("expected 0 messages after delete, got %d", len(resp.Messages))
		}

		// Non-outbound delivery (EventBridge) should still be in the delivery log!
		entries := log.List()
		if len(entries) != 1 || entries[0].Via != delivery.ViaEventBridge {
			t.Fatalf("expected 1 non-outbound record preserved, got %v", entries)
		}
	})
}

func TestLocalStackAliases(t *testing.T) {
	log := delivery.New()

	sesJSON := `{"messageId":"ses-target-999","from":"source@example.com","subject":"Hello",` +
		`"text":"body text","html":"<p>body html</p>","to":["user@example.com"]}`
	log.Add(
		delivery.ViaSES,
		"arn:aws:ses:us-east-1:123456789012:identity/source@example.com",
		"user@example.com",
		[]byte(sesJSON),
		nil,
	)

	log.Add(delivery.ViaSNS, "", "+15555550199", []byte("SNS message body"), nil)

	t.Run("GET /_aws/ses format and filters", func(t *testing.T) {
		h := delivery.SESMessagesHandler(log)

		// List all
		req := httptest.NewRequest(http.MethodGet, "/_aws/ses", http.NoBody)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}

		var resp struct {
			Messages []delivery.LocalStackSESMessage `json:"messages"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}

		if len(resp.Messages) != 1 {
			t.Fatalf("expected 1 ses message, got %d", len(resp.Messages))
		}

		msg := resp.Messages[0]
		if msg.Id != "ses-target-999" || msg.Source != "source@example.com" || msg.Subject != "Hello" {
			t.Fatalf("unexpected message fields: %+v", msg)
		}
		if msg.Body.TextPart != "body text" || msg.Body.HTMLPart != "<p>body html</p>" {
			t.Fatalf("unexpected body: %+v", msg.Body)
		}
		if len(msg.Destination["ToAddresses"]) != 1 || msg.Destination["ToAddresses"][0] != "user@example.com" {
			t.Fatalf("unexpected destination: %+v", msg.Destination)
		}

		// Filter by id
		reqID := httptest.NewRequest(http.MethodGet, "/_aws/ses?id=ses-target-999", http.NoBody)
		recID := httptest.NewRecorder()
		h.ServeHTTP(recID, reqID)
		var respID struct {
			Messages []delivery.LocalStackSESMessage `json:"messages"`
		}
		_ = json.Unmarshal(recID.Body.Bytes(), &respID)
		if len(respID.Messages) != 1 {
			t.Fatalf("filter by id failed")
		}

		// Filter by non-existent id
		reqNone := httptest.NewRequest(http.MethodGet, "/_aws/ses?id=other", http.NoBody)
		recNone := httptest.NewRecorder()
		h.ServeHTTP(recNone, reqNone)
		var respNone struct {
			Messages []delivery.LocalStackSESMessage `json:"messages"`
		}
		_ = json.Unmarshal(recNone.Body.Bytes(), &respNone)
		if len(respNone.Messages) != 0 {
			t.Fatalf("filter by non-existent id should return 0")
		}

		// Filter by email
		reqEmail := httptest.NewRequest(http.MethodGet, "/_aws/ses?email=user@example.com", http.NoBody)
		recEmail := httptest.NewRecorder()
		h.ServeHTTP(recEmail, reqEmail)
		var respEmail struct {
			Messages []delivery.LocalStackSESMessage `json:"messages"`
		}
		_ = json.Unmarshal(recEmail.Body.Bytes(), &respEmail)
		if len(respEmail.Messages) != 1 {
			t.Fatalf("filter by email failed")
		}
	})

	t.Run("GET /_aws/sns/sms-messages format and filters", func(t *testing.T) {
		h := delivery.SMSMessagesHandler(log)

		req := httptest.NewRequest(http.MethodGet, "/_aws/sns/sms-messages", http.NoBody)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}

		var resp struct {
			SMSMessages map[string][]delivery.LocalStackSMSItem `json:"sms_messages"`
			Region      string                                  `json:"region"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}

		list, ok := resp.SMSMessages["+15555550199"]
		if !ok || len(list) != 1 {
			t.Fatalf("expected message for +15555550199, got %v", resp.SMSMessages)
		}

		if list[0].Message != "SNS message body" {
			t.Fatalf("unexpected message: %q", list[0].Message)
		}

		// Filter by phoneNumber
		reqPhone := httptest.NewRequest(http.MethodGet, "/_aws/sns/sms-messages?phoneNumber=+15555550199", http.NoBody)
		recPhone := httptest.NewRecorder()
		h.ServeHTTP(recPhone, reqPhone)
		var respPhone struct {
			SMSMessages map[string][]delivery.LocalStackSMSItem `json:"sms_messages"`
		}
		_ = json.Unmarshal(recPhone.Body.Bytes(), &respPhone)
		if len(respPhone.SMSMessages["+15555550199"]) != 1 {
			t.Fatalf("filter by phoneNumber failed")
		}

		// Filter by non-matching phoneNumber
		reqOther := httptest.NewRequest(http.MethodGet, "/_aws/sns/sms-messages?phoneNumber=+10000000000", http.NoBody)
		recOther := httptest.NewRecorder()
		h.ServeHTTP(recOther, reqOther)
		var respOther struct {
			SMSMessages map[string][]delivery.LocalStackSMSItem `json:"sms_messages"`
		}
		_ = json.Unmarshal(recOther.Body.Bytes(), &respOther)
		if len(respOther.SMSMessages) != 0 {
			t.Fatalf("expected 0 messages for other phone, got %d", len(respOther.SMSMessages))
		}
	})
}
