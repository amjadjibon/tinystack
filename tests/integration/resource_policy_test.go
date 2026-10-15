package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/gateway"
)

// Both services exercise the gateway with SDK requests; the other account owns
// the resource while alice's identity policies remain in the default account.
func TestResourcePolicies(t *testing.T) {
	for _, service := range []string{"s3", "sqs"} {
		for _, cross := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/cross=%t", service, cross), func(t *testing.T) {
				cfg := testConfig()
				cfg.Identity.Accounts["test"] = cfg.Defaults.Account
				cfg.Identity.Accounts["other"] = "222222222222"
				cfg.Identity.Authorization = config.AuthorizationEnforce
				s := serverConfig(t, cfg, &fakeRuntime{})
				ctx := context.Background()
				key := authorizationUser(t, s)
				owner := "test"
				if cross {
					owner = "other"
				}

				arn := "arn:aws:s3:::resource-policy"
				action := "s3:GetObject"
				var putPolicy func(string) error
				var call func() error
				if service == "s3" {
					root := s3Client(s, owner, "us-east-1")
					_, err := root.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("resource-policy")})
					mustAuthorize(t, err)
					_, err = root.PutPublicAccessBlock(
						ctx,
						&s3.PutPublicAccessBlockInput{
							Bucket: new("resource-policy"),
							PublicAccessBlockConfiguration: &s3types.PublicAccessBlockConfiguration{
								BlockPublicPolicy: new(false),
							},
						},
					)
					mustAuthorize(t, err)
					_, err = root.PutObject(ctx, &s3.PutObjectInput{Bucket: new("resource-policy"), Key: new("key")})
					mustAuthorize(t, err)
					putPolicy = func(doc string) error {
						_, err := root.PutBucketPolicy(
							ctx,
							&s3.PutBucketPolicyInput{Bucket: new("resource-policy"), Policy: &doc},
						)
						return err
					}

					call = func() error {
						out, err := authorizationS3(
							s,
							key,
						).GetObject(ctx, &s3.GetObjectInput{Bucket: new("resource-policy"), Key: new("key")})
						if err == nil {
							out.Body.Close()
						}

						return err
					}

					arn += "/*"
				} else {
					root := sqsClient(s, owner)
					q, err := root.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: new("resource-policy")})
					mustAuthorize(t, err)
					account := cfg.Defaults.Account
					if cross {
						account = "222222222222"
					}

					arn = "arn:aws:sqs:us-east-1:" + account + ":resource-policy"
					action = "sqs:SendMessage"
					putPolicy = func(doc string) error {
						_, err := root.SetQueueAttributes(
							ctx,
							&sqs.SetQueueAttributesInput{
								QueueUrl:   q.QueueUrl,
								Attributes: map[string]string{"Policy": doc},
							},
						)
						return err
					}

					call = func() error {
						_, err := sqsClient(
							s,
							aws.ToString(key.AccessKeyId),
						).SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: q.QueueUrl, MessageBody: new("hello")})
						return err
					}
				}

				denied := func() {
					t.Helper()
					err := call()
					if service == "s3" {
						wantCode(t, err, "AccessDenied")
					} else {
						wantCode(t, err, "AccessDeniedException")
					}
				}

				denied()
				identity := fmt.Sprintf(`{"Statement":{"Effect":"Allow","Action":%q,"Resource":%q}}`, action, arn)
				authorizationPolicy(t, s, identity)
				if cross {
					denied()
				} else {
					mustAuthorize(t, call())
				}

				resource := func(effect, principal string) string {
					return fmt.Sprintf(
						`{"Statement":{"Effect":%q,"Principal":{"AWS":%q},"Action":%q,"Resource":%q}}`,
						effect,
						principal,
						action,
						arn,
					)
				}

				mustAuthorize(t, putPolicy(resource("Allow", "arn:aws:iam::"+cfg.Defaults.Account+":user/apps/alice")))
				mustAuthorize(t, call())
				authorizationPolicy(t, s, `{"Statement":{"Effect":"Allow","Action":"iam:GetUser","Resource":"*"}}`)
				if cross {
					denied()
				} else {
					mustAuthorize(t, call())
				}

				authorizationPolicy(t, s, identity)
				mustAuthorize(t, putPolicy(resource("Deny", "*")))
				denied()
				mustAuthorize(t, putPolicy(resource("Allow", "*")))
				authorizationPolicy(
					t,
					s,
					fmt.Sprintf(
						`{"Statement":[{"Effect":"Allow","Action":"*","Resource":"*"},{"Effect":"Deny","Action":%q,"Resource":%q}]}`,
						action,
						arn,
					),
				)
				denied()
				mustAuthorize(t, putPolicy(resource("Allow", "arn:aws:iam::999999999999:root")))
				authorizationPolicy(t, s, `{"Statement":{"Effect":"Allow","Action":"iam:GetUser","Resource":"*"}}`)
				denied()
				bad := fmt.Sprintf(
					`{"Statement":{"Effect":"Allow","NotPrincipal":"*","Action":%q,"Resource":%q}}`,
					action,
					arn,
				)
				if err := putPolicy(bad); err == nil {
					t.Fatal("NotPrincipal accepted")
				}
			})
		}
	}
}

func TestResourcePolicySoftAndParse(t *testing.T) {
	for _, mode := range []string{config.AuthorizationSoft, config.AuthorizationOff} {
		t.Run(mode, func(t *testing.T) {
			s := authorizationServer(t, mode, false)
			ctx := context.Background()
			root := s3Client(s, "test", "us-east-1")
			_, err := root.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("policy-mode")})
			mustAuthorize(t, err)
			_, err = root.PutBucketPolicy(
				ctx,
				&s3.PutBucketPolicyInput{
					Bucket: new("policy-mode"),
					Policy: new(
						`{"Statement":{"Effect":"Deny","Principal":"*","Action":"s3:PutObject","Resource":"*"}}`,
					),
				},
			)
			mustAuthorize(t, err)
			key := authorizationUser(t, s)
			_, err = authorizationS3(
				s,
				key,
			).PutObject(ctx, &s3.PutObjectInput{Bucket: new("policy-mode"), Key: new("key")})
			mustAuthorize(t, err)
			response, err := http.Get(s.URL + "/_tinystack/requests")
			mustAuthorize(t, err)
			defer response.Body.Close()
			var records struct {
				Requests []gateway.Record `json:"requests"`
			}

			mustAuthorize(t, json.NewDecoder(response.Body).Decode(&records))
			found := false
			for _, record := range records.Requests {
				if record.Operation != "PutObject" {
					continue
				}

				found = true
				if mode == config.AuthorizationOff {
					if record.Authorization != nil {
						t.Fatal("parse/off enforced policy")
					}

					continue
				}

				if record.Authorization == nil || record.Authorization.Allowed ||
					len(record.Authorization.Checks) == 0 ||
					record.Authorization.Checks[0].ResourcePolicy == nil ||
					!record.Authorization.Checks[0].ResourcePolicy.ExplicitDeny {
					t.Fatalf("missing resource denial: %+v", record.Authorization)
				}
			}

			if !found {
				t.Fatal("missing request")
			}
		})
	}
}

// Owning-account root can recover a denied bucket policy, but its object access
// is still denied. Policies can be fetched and deleted after denying everything.
func TestResourcePolicyRootRecovery(t *testing.T) {
	s := authorizationServer(t, config.AuthorizationEnforce, true)
	ctx := context.Background()
	root := s3Client(s, "test", "us-east-1")
	_, err := root.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("root-policy")})
	mustAuthorize(t, err)
	_, err = root.PutBucketPolicy(
		ctx,
		&s3.PutBucketPolicyInput{
			Bucket: new("root-policy"),
			Policy: new(`{"Statement":{"Effect":"Deny","Principal":"*","Action":"*","Resource":"*"}}`),
		},
	)
	mustAuthorize(t, err)
	_, err = root.PutObject(ctx, &s3.PutObjectInput{Bucket: new("root-policy"), Key: new("key")})
	wantCode(t, err, "AccessDenied")
	_, err = root.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{Bucket: new("root-policy")})
	mustAuthorize(t, err)
	_, err = root.DeleteBucketPolicy(ctx, &s3.DeleteBucketPolicyInput{Bucket: new("root-policy")})
	mustAuthorize(t, err)
}

func TestResourcePolicyQueueRootRecovery(t *testing.T) {
	s := authorizationServer(t, config.AuthorizationEnforce, true)
	ctx := context.Background()
	root := sqsClient(s, "test")
	queue, err := root.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: new("root-policy")})
	mustAuthorize(t, err)
	_, err = root.SetQueueAttributes(ctx, &sqs.SetQueueAttributesInput{
		QueueUrl: queue.QueueUrl,
		Attributes: map[string]string{
			"Policy": `{"Statement":{"Effect":"Deny","Principal":"*","Action":"*","Resource":"*"}}`,
		},
	})
	mustAuthorize(t, err)
	_, err = root.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: queue.QueueUrl, MessageBody: new("denied")})
	wantCode(t, err, "AccessDeniedException")
	_, err = root.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       queue.QueueUrl,
		AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNamePolicy},
	})
	mustAuthorize(t, err)
	_, err = root.SetQueueAttributes(ctx, &sqs.SetQueueAttributesInput{
		QueueUrl:   queue.QueueUrl,
		Attributes: map[string]string{"Policy": ""},
	})
	mustAuthorize(t, err)
	_, err = root.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: queue.QueueUrl, MessageBody: new("allowed")})
	mustAuthorize(t, err)
}
