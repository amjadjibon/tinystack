package integration_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/firehose"
	fhtypes "github.com/aws/aws-sdk-go-v2/service/firehose/types"
	"github.com/aws/aws-sdk-go-v2/service/kinesis"
	ktypes "github.com/aws/aws-sdk-go-v2/service/kinesis/types"

	"github.com/amjadjibon/tinystack/internal/config"
)

func TestKinesisResourcePolicies(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := kinesisClient(s)
	_, err := c.CreateStream(ctx, &kinesis.CreateStreamInput{StreamName: new("policies"), ShardCount: new(int32(1))})
	mustAuthorize(t, err)
	desc, err := c.DescribeStreamSummary(ctx, &kinesis.DescribeStreamSummaryInput{StreamName: new("policies")})
	mustAuthorize(t, err)
	arn := *desc.StreamDescriptionSummary.StreamARN
	consumer, err := c.RegisterStreamConsumer(
		ctx,
		&kinesis.RegisterStreamConsumerInput{StreamARN: &arn, ConsumerName: new("reader")},
	)
	mustAuthorize(t, err)
	for _, resource := range []string{arn, *consumer.Consumer.ConsumerARN} {
		action := "kinesis:PutRecord"
		if resource != arn {
			action = "kinesis:DescribeStreamConsumer"
		}

		doc := fmt.Sprintf(
			`{"Statement":{"Effect":"Allow","Principal":{"AWS":"222222222222"},"Action":%q,"Resource":%q}}`,
			action,
			resource,
		)
		get := func(want string) {
			t.Helper()
			out, getErr := c.GetResourcePolicy(ctx, &kinesis.GetResourcePolicyInput{ResourceARN: &resource})
			mustAuthorize(t, getErr)
			if *out.Policy != want {
				t.Fatalf("policy = %s, want %s", *out.Policy, want)
			}
		}

		get("{}")
		for _, document := range []string{doc, strings.Replace(doc, "Allow", "Deny", 1)} {
			_, err = c.PutResourcePolicy(
				ctx,
				&kinesis.PutResourcePolicyInput{ResourceARN: &resource, Policy: &document},
			)
			mustAuthorize(t, err)
			get(document)
		}

		_, err = c.DeleteResourcePolicy(ctx, &kinesis.DeleteResourcePolicyInput{ResourceARN: &resource})
		mustAuthorize(t, err)
		get("{}")
		for _, invalid := range []string{
			strings.Repeat(" ", 20481), "{", strings.Replace(doc, `{"AWS":"222222222222"}`, `"*"`, 1),
			strings.Replace(doc, `"222222222222"`, `"*"`, 1),
			strings.Replace(doc, action, "kinesis:*", 1), strings.Replace(doc, action, "kinesis:DeleteStream", 1),
			strings.Replace(doc, `"AWS":"222222222222"`, `"Service":"lambda.amazonaws.com"`, 1),
			strings.Replace(doc, `"AWS":"222222222222"`, `"Federated":"example.com"`, 1),
			strings.Replace(doc, `"AWS":"222222222222"`, `"CanonicalUser":"abc"`, 1),
		} {
			_, err = c.PutResourcePolicy(ctx, &kinesis.PutResourcePolicyInput{ResourceARN: &resource, Policy: &invalid})
			wantCode(t, err, "InvalidArgumentException")
		}

		_, err = c.PutResourcePolicy(ctx, &kinesis.PutResourcePolicyInput{ResourceARN: &resource, Policy: &doc})
		mustAuthorize(t, err)
	}

	missing := arn + "missing"
	_, err = c.GetResourcePolicy(ctx, &kinesis.GetResourcePolicyInput{ResourceARN: &missing})
	wantCode(t, err, "ResourceNotFoundException")
	_, err = c.GetResourcePolicy(ctx, &kinesis.GetResourcePolicyInput{ResourceARN: &arn, StreamId: new("reserved")})
	wantCode(t, err, "ValidationException")
	_, err = c.DeregisterStreamConsumer(
		ctx,
		&kinesis.DeregisterStreamConsumerInput{ConsumerARN: consumer.Consumer.ConsumerARN},
	)
	mustAuthorize(t, err)
	_, err = c.GetResourcePolicy(ctx, &kinesis.GetResourcePolicyInput{ResourceARN: consumer.Consumer.ConsumerARN})
	wantCode(t, err, "ResourceNotFoundException")

	replacement, err := c.RegisterStreamConsumer(
		ctx,
		&kinesis.RegisterStreamConsumerInput{StreamARN: &arn, ConsumerName: new("reader")},
	)
	mustAuthorize(t, err)
	replacementPolicy, err := c.GetResourcePolicy(
		ctx,
		&kinesis.GetResourcePolicyInput{ResourceARN: replacement.Consumer.ConsumerARN},
	)
	mustAuthorize(t, err)
	if *replacementPolicy.Policy != "{}" {
		t.Fatal("replacement consumer retained policy")
	}

	_, err = c.DeregisterStreamConsumer(
		ctx,
		&kinesis.DeregisterStreamConsumerInput{ConsumerARN: replacement.Consumer.ConsumerARN},
	)
	mustAuthorize(t, err)
	_, err = c.DeleteStream(ctx, &kinesis.DeleteStreamInput{StreamARN: &arn})
	mustAuthorize(t, err)
	_, err = c.CreateStream(ctx, &kinesis.CreateStreamInput{StreamName: new("policies"), ShardCount: new(int32(1))})
	mustAuthorize(t, err)
	out, err := c.GetResourcePolicy(ctx, &kinesis.GetResourcePolicyInput{ResourceARN: &arn})
	mustAuthorize(t, err)
	if *out.Policy != "{}" {
		t.Fatal("recreated stream retained policy")
	}
}

func TestKinesisPolicyAuthorization(t *testing.T) {
	for _, cross := range []bool{false, true} {
		t.Run(fmt.Sprintf("cross=%t", cross), func(t *testing.T) {
			ctx := context.Background()
			cfg := testConfig()
			cfg.Identity.Accounts["test"] = cfg.Defaults.Account
			cfg.Identity.Accounts["other"] = "222222222222"
			cfg.Identity.Authorization = config.AuthorizationEnforce
			s := serverConfig(t, cfg, &fakeRuntime{})
			ownerKey := "test"
			if cross {
				ownerKey = "other"
			}

			client := func(key string) *kinesis.Client {
				return kinesis.New(
					kinesis.Options{
						Region:       "us-east-1",
						BaseEndpoint: new(s.URL),
						Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
							return aws.Credentials{AccessKeyID: key, SecretAccessKey: "test"}, nil
						}),
						Retryer: aws.NopRetryer{},
					},
				)
			}

			root := client(ownerKey)
			user := kinesis.New(
				kinesis.Options{
					Region:       "us-east-1",
					BaseEndpoint: new(s.URL),
					Credentials:  authorizationProvider(authorizationUser(t, s)),
					Retryer:      aws.NopRetryer{},
				},
			)
			_, err := root.CreateStream(
				ctx,
				&kinesis.CreateStreamInput{StreamName: new("shared"), ShardCount: new(int32(1))},
			)
			mustAuthorize(t, err)
			summary, err := root.DescribeStreamSummary(
				ctx,
				&kinesis.DescribeStreamSummaryInput{StreamName: new("shared")},
			)
			mustAuthorize(t, err)
			arn := *summary.StreamDescriptionSummary.StreamARN
			consumer, err := root.RegisterStreamConsumer(
				ctx,
				&kinesis.RegisterStreamConsumerInput{StreamARN: &arn, ConsumerName: new("reader")},
			)
			mustAuthorize(t, err)
			put := func(resource, effect string) {
				t.Helper()
				actions := `["kinesis:DescribeStreamSummary","kinesis:ListShards","kinesis:PutRecord",` +
					`"kinesis:PutRecords","kinesis:GetShardIterator","kinesis:GetRecords"]`
				if resource != arn {
					actions = `"kinesis:DescribeStreamConsumer"`
				}

				doc := fmt.Sprintf(
					`{"Statement":{"Effect":%q,"Principal":{"AWS":%q},"Action":%s,"Resource":%q}}`,
					effect,
					"arn:aws:iam::"+cfg.Defaults.Account+":user/apps/alice",
					actions,
					resource,
				)
				_, putErr := root.PutResourcePolicy(
					ctx,
					&kinesis.PutResourcePolicyInput{ResourceARN: &resource, Policy: &doc},
				)
				mustAuthorize(t, putErr)
			}

			ownerIterator, err := root.GetShardIterator(ctx, &kinesis.GetShardIteratorInput{
				StreamARN: &arn, ShardId: new("shardId-000000000000"),
				ShardIteratorType: ktypes.ShardIteratorTypeTrimHorizon,
			})
			mustAuthorize(t, err)
			calls := []func() error{
				func() error {
					_, e := user.GetRecords(ctx, &kinesis.GetRecordsInput{ShardIterator: ownerIterator.ShardIterator})
					return e
				},
				func() error {
					_, e := user.DescribeStreamSummary(ctx, &kinesis.DescribeStreamSummaryInput{StreamARN: &arn})
					return e
				},
				func() error { _, e := user.ListShards(ctx, &kinesis.ListShardsInput{StreamARN: &arn}); return e },
				func() error {
					_, e := user.PutRecord(
						ctx,
						&kinesis.PutRecordInput{StreamARN: &arn, Data: []byte("x"), PartitionKey: new("p")},
					)
					return e
				},
				func() error {
					_, e := user.PutRecords(
						ctx,
						&kinesis.PutRecordsInput{
							StreamARN: &arn,
							Records:   []ktypes.PutRecordsRequestEntry{{Data: []byte("x"), PartitionKey: new("p")}},
						},
					)
					return e
				},
				func() error {
					it, e := user.GetShardIterator(
						ctx,
						&kinesis.GetShardIteratorInput{
							StreamARN:         &arn,
							ShardId:           new("shardId-000000000000"),
							ShardIteratorType: ktypes.ShardIteratorTypeTrimHorizon,
						},
					)
					if e != nil {
						return e
					}

					_, e = user.GetRecords(
						ctx,
						&kinesis.GetRecordsInput{ShardIterator: it.ShardIterator, StreamARN: &arn},
					)
					return e
				},
				func() error {
					_, e := user.DescribeStreamConsumer(
						ctx,
						&kinesis.DescribeStreamConsumerInput{ConsumerARN: consumer.Consumer.ConsumerARN},
					)
					return e
				},
			}

			for _, call := range calls {
				wantCode(t, call(), "AccessDeniedException")
			}

			authorizationPolicy(t, s, `{"Statement":{"Effect":"Allow","Action":"kinesis:*","Resource":"*"}}`)
			for _, call := range calls {
				if cross {
					wantCode(t, call(), "AccessDeniedException")
				} else {
					mustAuthorize(t, call())
				}
			}

			authorizationPolicy(t, s, `{"Statement":{"Effect":"Allow","Action":"s3:ListAllMyBuckets","Resource":"*"}}`)
			put(arn, "Allow")

			_, err = user.DescribeStreamConsumer(
				ctx,
				&kinesis.DescribeStreamConsumerInput{ConsumerARN: consumer.Consumer.ConsumerARN},
			)
			wantCode(t, err, "AccessDeniedException")
			put(*consumer.Consumer.ConsumerARN, "Allow")
			for _, call := range calls {
				if cross {
					wantCode(t, call(), "AccessDeniedException")
				} else {
					mustAuthorize(t, call())
				}
			}

			authorizationPolicy(t, s, `{"Statement":{"Effect":"Allow","Action":"kinesis:*","Resource":"*"}}`)
			for _, call := range calls {
				mustAuthorize(t, call())
			}

			if cross {
				_, err = user.GetResourcePolicy(ctx, &kinesis.GetResourcePolicyInput{ResourceARN: &arn})
				wantCode(t, err, "AccessDeniedException")
				_, err = user.DeleteResourcePolicy(ctx, &kinesis.DeleteResourcePolicyInput{ResourceARN: &arn})
				wantCode(t, err, "AccessDeniedException")
				_, err = user.PutResourcePolicy(
					ctx,
					&kinesis.PutResourcePolicyInput{ResourceARN: &arn, Policy: new(`{}`)},
				)
				wantCode(t, err, "AccessDeniedException")
			}

			authorizationPolicy(t, s, `{"Statement":{"Effect":"Deny","Action":"kinesis:*","Resource":"*"}}`)
			for _, call := range calls {
				wantCode(t, call(), "AccessDeniedException")
			}

			authorizationPolicy(t, s, `{"Statement":{"Effect":"Allow","Action":"kinesis:*","Resource":"*"}}`)
			put(arn, "Deny")
			put(*consumer.Consumer.ConsumerARN, "Deny")
			for _, call := range calls {
				wantCode(t, call(), "AccessDeniedException")
			}
		})
	}
}

func TestKinesisCrossAccountFirehosePolicy(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig()
	cfg.Identity.Accounts["test"] = cfg.Defaults.Account
	cfg.Identity.Accounts["other"] = "222222222222"
	cfg.Identity.Authorization = config.AuthorizationEnforce
	s := serverConfig(t, cfg, &fakeRuntime{})
	owner := kinesis.New(kinesis.Options{
		Region: "us-east-1", BaseEndpoint: new(s.URL), Retryer: aws.NopRetryer{},
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "other", SecretAccessKey: "test"}, nil
		}),
	})
	_, err := owner.CreateStream(ctx, &kinesis.CreateStreamInput{StreamName: new("source"), ShardCount: new(int32(1))})
	mustAuthorize(t, err)
	summary, err := owner.DescribeStreamSummary(ctx, &kinesis.DescribeStreamSummaryInput{StreamName: new("source")})
	mustAuthorize(t, err)
	arn := *summary.StreamDescriptionSummary.StreamARN
	trust := `{"Statement":{"Effect":"Allow","Principal":{"Service":"firehose.amazonaws.com"},"Action":"sts:AssumeRole"}}`
	role := createDeliveryRole(
		t,
		s,
		"reader",
		trust,
		`{"Statement":{"Effect":"Allow","Action":"kinesis:*","Resource":"*"}}`,
	)
	fh := firehoseClient(s)
	create := func() error {
		_, e := fh.CreateDeliveryStream(ctx, &firehose.CreateDeliveryStreamInput{
			DeliveryStreamName: new(
				"shared-source",
			),
			DeliveryStreamType: fhtypes.DeliveryStreamTypeKinesisStreamAsSource,
			KinesisStreamSourceConfiguration: &fhtypes.KinesisStreamSourceConfiguration{
				KinesisStreamARN: &arn,
				RoleARN:          &role,
			},
			ExtendedS3DestinationConfiguration: &fhtypes.ExtendedS3DestinationConfiguration{
				BucketARN: new("arn:aws:s3:::lake"),
				RoleARN:   &role,
			},
		})
		return e
	}

	wantCode(t, create(), "AccessDeniedException")
	document := fmt.Sprintf(`{"Statement":{"Effect":"Allow","Principal":{"AWS":%q},`+
		`"Action":["kinesis:DescribeStream","kinesis:ListShards","kinesis:GetShardIterator","kinesis:GetRecords"],`+
		`"Resource":%q}}`, role, arn)
	_, err = owner.PutResourcePolicy(ctx, &kinesis.PutResourcePolicyInput{ResourceARN: &arn, Policy: &document})
	mustAuthorize(t, err)
	mustAuthorize(t, create())
}
