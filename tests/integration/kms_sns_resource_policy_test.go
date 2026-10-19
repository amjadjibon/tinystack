package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/gateway"
)

func TestKMSResourcePolicies(t *testing.T) {
	for _, cross := range []bool{false, true} {
		t.Run(fmt.Sprintf("cross=%t", cross), func(t *testing.T) {
			cfg := testConfig()
			cfg.Identity.Accounts["test"] = cfg.Defaults.Account
			cfg.Identity.Accounts["other"] = "222222222222"
			cfg.Identity.Authorization = config.AuthorizationEnforce
			s := serverConfig(t, cfg, &fakeRuntime{})
			ctx := context.Background()
			issued := authorizationUser(t, s)
			owner := "test"
			if cross {
				owner = "other"
			}

			root := kmsClient(s, owner, "us-east-1")
			key := createKey(t, root, "policy")
			_, err := root.CreateAlias(
				ctx,
				&kms.CreateAliasInput{AliasName: new("alias/policy"), TargetKeyId: key.KeyId},
			)
			mustAuthorize(t, err)
			user := kmsClient(s, aws.ToString(issued.AccessKeyId), "us-east-1")
			identity := fmt.Sprintf(`{"Statement":{"Effect":"Allow","Action":"kms:Encrypt","Resource":%q}}`, *key.Arn)
			call := func() error {
				out, callErr := user.Encrypt(ctx, &kms.EncryptInput{KeyId: key.Arn, Plaintext: []byte("secret")})
				if callErr == nil && aws.ToString(out.KeyId) != *key.Arn {
					t.Fatalf("resolved key = %v", out.KeyId)
				}

				return callErr
			}

			put := func(statement string) {
				t.Helper()
				// Retain the owner's policy management permission throughout this test.
				doc := fmt.Sprintf(`{"Statement":[{"Effect":"Allow","Principal":{"AWS":%q},`+
					`"Action":"kms:PutKeyPolicy","Resource":"*"},%s]}`,
					"arn:aws:iam::"+aws.ToString(key.AWSAccountId)+":root", statement)
				_, putErr := root.PutKeyPolicy(ctx, &kms.PutKeyPolicyInput{KeyId: key.KeyId, Policy: &doc})
				mustAuthorize(t, putErr)
			}

			statement := func(effect, principal string) string {
				return fmt.Sprintf(`{"Effect":%q,"Principal":{"AWS":%q},"Action":"kms:Encrypt","Resource":"*"}`,
					effect, principal)
			}

			wantCode(t, call(), "AccessDeniedException")
			authorizationPolicy(t, s, identity)
			if cross {
				wantCode(t, call(), "AccessDeniedException")
			} else {
				mustAuthorize(t, call()) // default key policy enables IAM
			}

			put(statement("Allow", "arn:aws:iam::333333333333:root"))
			wantCode(t, call(), "AccessDeniedException") // IAM alone cannot authorize a KMS key
			put(statement("Allow", "arn:aws:iam::"+cfg.Defaults.Account+":root"))
			mustAuthorize(t, call())
			refs := []string{"arn:aws:kms:us-east-1:" + *key.AWSAccountId + ":alias/policy"}
			if !cross {
				refs = append(refs, *key.KeyId, "alias/policy")
			}

			for _, ref := range refs {
				out, aliasErr := user.Encrypt(ctx, &kms.EncryptInput{KeyId: &ref, Plaintext: []byte("alias")})
				mustAuthorize(t, aliasErr)
				if aws.ToString(out.KeyId) != *key.Arn {
					t.Fatalf("alias resolved to %v", out.KeyId)
				}
			}

			authorizationPolicy(t, s, `{"Statement":{"Effect":"Allow","Action":"iam:GetUser","Resource":"*"}}`)
			wantCode(t, call(), "AccessDeniedException") // account delegation still requires IAM
			put(statement("Allow", "arn:aws:iam::"+cfg.Defaults.Account+":user/apps/alice"))
			if cross {
				wantCode(t, call(), "AccessDeniedException")
			} else {
				mustAuthorize(t, call()) // same-account direct key grant needs no IAM Allow
			}

			authorizationPolicy(t, s, identity)
			mustAuthorize(t, call())
			put(statement("Deny", cfg.Defaults.Account))
			wantCode(t, call(), "AccessDeniedException")
			put(statement("Allow", "*"))
			authorizationPolicy(t, s, `{"Statement":{"Effect":"Deny","Action":"kms:Encrypt","Resource":"*"}}`)
			wantCode(t, call(), "AccessDeniedException")

			resp, err := http.Get(s.URL + "/_tinystack/requests")
			mustAuthorize(t, err)
			defer resp.Body.Close()
			var records struct {
				Requests []gateway.Record `json:"requests"`
			}

			mustAuthorize(t, json.NewDecoder(resp.Body).Decode(&records))
			found := false
			for _, record := range records.Requests {
				if record.Service == "kms" && record.Operation == "Encrypt" && record.Authorization != nil {
					for _, check := range record.Authorization.Checks {
						found = found || (check.ResourcePolicy != nil && check.Resource == *key.Arn &&
							check.CrossAccount == cross)
					}
				}
			}

			if !found {
				t.Fatal("missing KMS resource policy inspector decision")
			}
		})
	}
}

func TestKMSPolicyLockout(t *testing.T) {
	cfg := testConfig()
	cfg.Identity.Accounts["test"] = cfg.Defaults.Account
	cfg.Identity.Authorization = config.AuthorizationEnforce
	s := serverConfig(t, cfg, &fakeRuntime{})
	ctx := context.Background()
	root := kmsClient(s, "test", "us-east-1")
	key := createKey(t, root, "lockout")
	locked := `{"Statement":{"Effect":"Allow","Principal":{"AWS":"333333333333"},"Action":"kms:*","Resource":"*"}}`
	_, err := root.CreateKey(ctx, &kms.CreateKeyInput{Policy: &locked})
	wantCode(t, err, "MalformedPolicyDocumentException")
	unmanageable, err := root.CreateKey(ctx, &kms.CreateKeyInput{Policy: &locked, BypassPolicyLockoutSafetyCheck: true})
	mustAuthorize(t, err)
	_, err = root.DescribeKey(ctx, &kms.DescribeKeyInput{KeyId: unmanageable.KeyMetadata.KeyId})
	wantCode(t, err, "AccessDeniedException")
	_, err = root.PutKeyPolicy(ctx, &kms.PutKeyPolicyInput{KeyId: key.KeyId, Policy: &locked})
	wantCode(t, err, "MalformedPolicyDocumentException")
	_, err = root.PutKeyPolicy(ctx, &kms.PutKeyPolicyInput{
		KeyId: key.KeyId, Policy: &locked, BypassPolicyLockoutSafetyCheck: true,
	})
	mustAuthorize(t, err)
	_, err = root.GetKeyPolicy(ctx, &kms.GetKeyPolicyInput{KeyId: key.KeyId})
	wantCode(t, err, "AccessDeniedException")
	_, err = root.ListKeyPolicies(ctx, &kms.ListKeyPoliciesInput{KeyId: key.KeyId})
	wantCode(t, err, "AccessDeniedException")
	_, err = root.PutKeyPolicy(ctx, &kms.PutKeyPolicyInput{
		KeyId: key.KeyId, Policy: &locked, BypassPolicyLockoutSafetyCheck: true,
	})
	wantCode(t, err, "AccessDeniedException") // bypass does not bypass current authorization

	issued := authorizationUser(t, s)
	user := kmsClient(s, aws.ToString(issued.AccessKeyId), "us-east-1")
	authorizationPolicy(t, s, `{"Statement":{"Effect":"Allow","Action":"kms:CreateKey","Resource":"*"}}`)
	_, err = user.CreateKey(ctx, &kms.CreateKeyInput{})
	wantCode(t, err, "MalformedPolicyDocumentException") // IAM delegation lacks PutKeyPolicy
	authorizationPolicy(
		t,
		s,
		`{"Statement":{"Effect":"Allow","Action":["kms:CreateKey","kms:PutKeyPolicy"],"Resource":"*"}}`,
	)
	created, err := user.CreateKey(ctx, &kms.CreateKeyInput{})
	mustAuthorize(t, err)
	_, err = user.PutKeyPolicy(ctx, &kms.PutKeyPolicyInput{KeyId: created.KeyMetadata.KeyId, Policy: &locked})
	wantCode(t, err, "MalformedPolicyDocumentException")
}

func TestSNSResourcePolicies(t *testing.T) {
	for _, cross := range []bool{false, true} {
		t.Run(fmt.Sprintf("cross=%t", cross), func(t *testing.T) {
			cfg := testConfig()
			cfg.Identity.Accounts["test"] = cfg.Defaults.Account
			cfg.Identity.Accounts["other"] = "222222222222"
			cfg.Identity.Authorization = config.AuthorizationEnforce
			s := serverConfig(t, cfg, &fakeRuntime{})
			ctx := context.Background()
			issued := authorizationUser(t, s)
			owner := "test"
			if cross {
				owner = "other"
			}

			root := snsClient(s, owner)
			topic, err := root.CreateTopic(ctx, &sns.CreateTopicInput{Name: new("resource-policy")})
			mustAuthorize(t, err)
			user := snsClient(s, aws.ToString(issued.AccessKeyId))
			call := func() error {
				_, callErr := user.Publish(ctx, &sns.PublishInput{TopicArn: topic.TopicArn, Message: new("hello")})
				return callErr
			}

			if cross {
				wantCode(t, call(), "AccessDenied")
			} else {
				mustAuthorize(t, call()) // default topic policy permits its SourceOwner
			}

			put := func(effect, principal string) {
				t.Helper()
				doc := fmt.Sprintf(`{"Statement":{"Effect":%q,"Principal":{"AWS":%q},"Action":"sns:*","Resource":"*"}}`,
					effect, principal)
				_, putErr := root.SetTopicAttributes(ctx, &sns.SetTopicAttributesInput{
					TopicArn: topic.TopicArn, AttributeName: new("Policy"), AttributeValue: &doc,
				})
				mustAuthorize(t, putErr)
			}

			put("Allow", "333333333333")
			wantCode(t, call(), "AccessDenied")
			authorizationPolicy(t, s, fmt.Sprintf(
				`{"Statement":{"Effect":"Allow","Action":"sns:Publish","Resource":%q}}`, *topic.TopicArn))
			if cross {
				wantCode(t, call(), "AccessDenied")
			} else {
				mustAuthorize(t, call())
			}

			put("Allow", "arn:aws:iam::"+cfg.Defaults.Account+":user/apps/alice")
			mustAuthorize(t, call())
			authorizationPolicy(t, s, `{"Statement":{"Effect":"Allow","Action":"iam:GetUser","Resource":"*"}}`)
			if cross {
				wantCode(t, call(), "AccessDenied")
			} else {
				mustAuthorize(t, call())
			}

			put("Deny", "*")
			wantCode(t, call(), "AccessDenied")
			_, err = root.Publish(ctx, &sns.PublishInput{TopicArn: topic.TopicArn, Message: new("denied")})
			wantCode(t, err, "AccessDenied")
			_, err = root.GetTopicAttributes(ctx, &sns.GetTopicAttributesInput{TopicArn: topic.TopicArn})
			mustAuthorize(t, err) // owning root recovery survives an explicit Deny
			put("Allow", "*")
		})
	}
}

func TestKMSAndSNSPolicyValidation(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	keys := kmsClient(s, "test", "us-east-1")
	key := createKey(t, keys, "validation")
	topics := snsClient(s, "test")
	topic, err := topics.CreateTopic(ctx, &sns.CreateTopicInput{Name: new("validation")})
	mustAuthorize(t, err)
	for _, doc := range []string{
		`{"Statement":{"Effect":"Allow","NotPrincipal":{"AWS":"*"},"Action":"*","Resource":"*"}}`,
		`{"Statement":{"Effect":"Allow","Principal":"*","Action":"*","Resource":"*",` +
			`"Condition":{"StringEquals":{"kms:Unknown":"x"}}}}`,
	} {
		_, err = keys.CreateKey(ctx, &kms.CreateKeyInput{Policy: &doc, BypassPolicyLockoutSafetyCheck: true})
		wantCode(t, err, "MalformedPolicyDocumentException")
		_, err = keys.PutKeyPolicy(ctx, &kms.PutKeyPolicyInput{
			KeyId: key.KeyId, Policy: &doc, BypassPolicyLockoutSafetyCheck: true,
		})
		wantCode(t, err, "MalformedPolicyDocumentException")
		_, err = topics.CreateTopic(ctx, &sns.CreateTopicInput{
			Name: new("invalid"), Attributes: map[string]string{"Policy": doc},
		})
		wantCode(t, err, "InvalidParameter")
		_, err = topics.SetTopicAttributes(ctx, &sns.SetTopicAttributesInput{
			TopicArn: topic.TopicArn, AttributeName: new("Policy"), AttributeValue: &doc,
		})
		wantCode(t, err, "InvalidParameter")
	}
}

func TestKMSAndSNSResourcePolicyModes(t *testing.T) {
	for _, mode := range []string{config.AuthorizationOff, config.AuthorizationSoft, config.AuthorizationEnforce} {
		t.Run(mode, func(t *testing.T) {
			cfg := testConfig()
			cfg.Identity.Accounts["test"] = cfg.Defaults.Account
			cfg.Identity.Authorization = mode
			s := serverConfig(t, cfg, &fakeRuntime{})
			ctx := context.Background()
			keys := kmsClient(s, "test", "us-east-1")
			key := createKey(t, keys, "modes")
			deny := `{"Statement":{"Effect":"Deny","Principal":"*","Action":"*","Resource":"*"}}`
			_, err := keys.PutKeyPolicy(ctx, &kms.PutKeyPolicyInput{KeyId: key.KeyId, Policy: &deny})
			wantCode(t, err, "MalformedPolicyDocumentException")
			_, err = keys.PutKeyPolicy(ctx, &kms.PutKeyPolicyInput{
				KeyId: key.KeyId, Policy: &deny, BypassPolicyLockoutSafetyCheck: true,
			})
			mustAuthorize(t, err)
			topics := snsClient(s, "test")
			topic, err := topics.CreateTopic(ctx, &sns.CreateTopicInput{
				Name: new("modes"), Attributes: map[string]string{"Policy": deny},
			})
			mustAuthorize(t, err)
			issued := authorizationUser(t, s)
			_, kmsErr := kmsClient(s, *issued.AccessKeyId, "us-east-1").Encrypt(ctx, &kms.EncryptInput{
				KeyId: key.KeyId, Plaintext: []byte("hello"),
			})
			_, snsErr := snsClient(s, *issued.AccessKeyId).Publish(ctx, &sns.PublishInput{
				TopicArn: topic.TopicArn, Message: new("hello"),
			})
			if mode == config.AuthorizationEnforce {
				wantCode(t, kmsErr, "AccessDeniedException")
				wantCode(t, snsErr, "AccessDenied")
			} else {
				mustAuthorize(t, kmsErr)
				mustAuthorize(t, snsErr)
			}

			response, err := http.Get(s.URL + "/_tinystack/requests")
			mustAuthorize(t, err)
			defer response.Body.Close()
			var records struct {
				Requests []gateway.Record `json:"requests"`
			}

			mustAuthorize(t, json.NewDecoder(response.Body).Decode(&records))
			found := 0
			for _, record := range records.Requests {
				if record.Operation != "Encrypt" && record.Operation != "Publish" {
					continue
				}

				found++
				if mode == config.AuthorizationOff {
					if record.Authorization != nil {
						t.Fatal("off mode evaluated permissions")
					}
				} else if record.Authorization == nil || record.Authorization.Allowed ||
					len(record.Authorization.Checks) != 1 ||
					record.Authorization.Checks[0].ResourcePolicy == nil ||
					!record.Authorization.Checks[0].ResourcePolicy.ExplicitDeny {
					t.Fatalf("missing denial: %+v", record.Authorization)
				}
			}

			if found != 2 {
				t.Fatalf("found %d policy decisions", found)
			}

			if mode == config.AuthorizationEnforce {
				// Unresolved parse-mode credentials retain their gateway bypass.
				_, err = kmsClient(s, "unresolved", "us-east-1").Encrypt(ctx, &kms.EncryptInput{
					KeyId: key.KeyId, Plaintext: []byte("parse"),
				})
				mustAuthorize(t, err)
				_, err = snsClient(s, "unresolved").Publish(ctx, &sns.PublishInput{
					TopicArn: topic.TopicArn, Message: new("parse"),
				})
				mustAuthorize(t, err)
			}
		})
	}
}

func TestSNSResourcePolicyDeliveryEnforced(t *testing.T) {
	cfg := testConfig()
	cfg.Identity.Accounts["test"] = cfg.Defaults.Account
	cfg.Identity.Authorization = config.AuthorizationEnforce
	runtime := &fakeRuntime{calls: make(chan invocation, 10)}
	s := serverConfig(t, cfg, runtime)
	ctx := context.Background()
	topics := snsClient(s, "test")
	topic, err := topics.CreateTopic(ctx, &sns.CreateTopicInput{Name: new("deliveries")})
	mustAuthorize(t, err)
	queues := sqsClient(s, "test")
	queueURL, queueARN := queueARN(t, queues, "deliveries")
	_, err = topics.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: topic.TopicArn, Protocol: new("sqs"), Endpoint: &queueARN,
		Attributes: map[string]string{"RawMessageDelivery": "true"},
	})
	mustAuthorize(t, err)
	function := createFunction(t, lambdaClient(s, "test"), "delivery-policy", "app.echo")
	_, err = topics.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: topic.TopicArn, Protocol: new("lambda"), Endpoint: function.FunctionArn,
	})
	mustAuthorize(t, err)
	allowService(t, s, "delivery-policy", "sns.amazonaws.com", *topic.TopicArn)
	// Services deliver by their own principals: the policies deny every caller but them.
	_, err = topics.SetTopicAttributes(ctx, &sns.SetTopicAttributesInput{
		TopicArn: topic.TopicArn, AttributeName: new("Policy"),
		AttributeValue: new(`{"Statement":[
			{"Effect":"Allow","Principal":{"Service":["s3.amazonaws.com","events.amazonaws.com"]},
			"Action":"sns:Publish","Resource":"*"},
			{"Effect":"Deny","Principal":"*","Action":"sns:Publish","Resource":"*",
			"Condition":{"Null":{"aws:PrincipalServiceName":"true"}}}]}`),
	})
	mustAuthorize(t, err)
	_, err = queues.SetQueueAttributes(ctx, &sqs.SetQueueAttributesInput{
		QueueUrl: &queueURL, Attributes: map[string]string{
			"Policy": `{"Statement":[
				{"Effect":"Allow","Principal":{"Service":"sns.amazonaws.com"},"Action":"sqs:SendMessage","Resource":"*"},
				{"Effect":"Deny","Principal":"*","Action":"sqs:SendMessage","Resource":"*",
				"Condition":{"Null":{"aws:PrincipalServiceName":"true"}}}]}`,
		},
	})
	mustAuthorize(t, err)
	_, err = topics.Publish(ctx, &sns.PublishInput{TopicArn: topic.TopicArn, Message: new("external")})
	wantCode(t, err, "AccessDenied")

	buckets := s3Client(s, "test", "us-east-1")
	_, err = buckets.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("policy-deliveries")})
	mustAuthorize(t, err)
	_, err = buckets.PutBucketNotificationConfiguration(ctx, &s3.PutBucketNotificationConfigurationInput{
		Bucket: new("policy-deliveries"), NotificationConfiguration: &s3types.NotificationConfiguration{
			TopicConfigurations: []s3types.TopicConfiguration{{
				TopicArn: topic.TopicArn, Events: []s3types.Event{"s3:ObjectCreated:*"},
			}},
		},
	})
	mustAuthorize(t, err)
	// Saving notifications emits a test event; consume it before the actual event.
	if body := receiveOne(t, queues, queueURL); !strings.Contains(body, "s3:TestEvent") {
		t.Fatalf("test event = %q", body)
	}

	_, err = buckets.PutObject(ctx, &s3.PutObjectInput{
		Bucket: new("policy-deliveries"), Key: new("object"), Body: bytes.NewReader([]byte("data")),
	})
	mustAuthorize(t, err)
	if body := receiveOne(t, queues, queueURL); !strings.Contains(body, "ObjectCreated:Put") {
		t.Fatalf("S3 delivery = %q", body)
	}

	events := eventsClient(s, "test")
	putRule(t, events, "policy-deliveries", `{"source":["policy.delivery"]}`)
	_, err = events.PutTargets(ctx, &eventbridge.PutTargetsInput{
		Rule: new("policy-deliveries"), Targets: []ebtypes.Target{{Id: new("topic"), Arn: topic.TopicArn}},
	})
	mustAuthorize(t, err)
	_, err = events.PutEvents(ctx, &eventbridge.PutEventsInput{Entries: []ebtypes.PutEventsRequestEntry{{
		Source: new("policy.delivery"), DetailType: new("test"), Detail: new(`{"ok":true}`),
	}}})
	mustAuthorize(t, err)
	if body := receiveOne(t, queues, queueURL); !strings.Contains(body, "policy.delivery") {
		t.Fatalf("EventBridge delivery = %q", body)
	}

	// All three topic messages (test event, S3 event, EventBridge event) fan out to Lambda too.
	for range 3 {
		select {
		case call := <-runtime.calls:
			if call.fn.Name != "delivery-policy" || !strings.Contains(string(call.payload), "aws:sns") {
				t.Fatalf("Lambda delivery = %s", call.payload)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("SNS did not deliver to Lambda")
		}
	}
}
