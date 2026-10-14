package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/amjadjibon/tinystack/internal/config"
)

const (
	keyAccountA = "123456789012"
	keyAccountB = "210987654321"
)

func TestTwelveDigitKeyIsAccount(t *testing.T) {
	s := server(t)
	ctx := context.Background()

	for _, account := range []string{keyAccountA, keyAccountB} {
		identity, err := stsClient(s, account, "test").GetCallerIdentity(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}

		if aws.ToString(identity.Account) != account || !strings.Contains(aws.ToString(identity.Arn), account) {
			t.Fatalf("%s: got %s %s", account, aws.ToString(identity.Account), aws.ToString(identity.Arn))
		}
	}

	var urls []string
	for _, account := range []string{keyAccountA, keyAccountB} {
		queue, err := sqsClient(s, account).CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String("jobs")})
		if err != nil {
			t.Fatal(err)
		}

		if !strings.Contains(aws.ToString(queue.QueueUrl), "/"+account+"/") {
			t.Fatalf("queue URL %s lacks account %s", aws.ToString(queue.QueueUrl), account)
		}

		urls = append(urls, aws.ToString(queue.QueueUrl))
		if _, err = s3Client(s, account, "us-east-1").CreateBucket(
			ctx, &s3.CreateBucketInput{Bucket: aws.String("assets-" + account)},
		); err != nil {
			t.Fatalf("each account creates its own bucket: %v", err)
		}
	}

	if urls[0] == urls[1] {
		t.Fatal("accounts shared a queue")
	}

	buckets, err := s3Client(s, keyAccountA, "us-east-1").ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil || len(buckets.Buckets) != 1 {
		t.Fatalf("account A sees %v, %v", buckets, err)
	}

	// An unrelated key stays in the default account.
	identity, err := stsClient(s, "test", "test").GetCallerIdentity(ctx, nil)
	if err != nil || aws.ToString(identity.Account) != "000000000000" {
		t.Fatalf("default account: %v %v", identity, err)
	}

	// Eleven and thirteen digits, or a stray letter, are not accounts.
	for _, notAccount := range []string{"12345678901", "1234567890123", "12345678901a"} {
		identity, err = stsClient(s, notAccount, "test").GetCallerIdentity(ctx, nil)
		if err != nil || aws.ToString(identity.Account) != "000000000000" {
			t.Fatalf("%s: %v %v", notAccount, identity, err)
		}
	}
}

func TestConfiguredAccountBeatsTwelveDigitKey(t *testing.T) {
	cfg := testConfig()
	cfg.Identity.Accounts[keyAccountA] = "999999999999"
	s := serverConfig(t, cfg, &fakeRuntime{})

	identity, err := stsClient(s, keyAccountA, "test").GetCallerIdentity(context.Background(), nil)
	if err != nil || aws.ToString(identity.Account) != "999999999999" {
		t.Fatalf("mapping must win: %v %v", identity, err)
	}
}

func TestTwelveDigitKeyStillNeedsSecretWhenVerifying(t *testing.T) {
	cfg := testConfig()
	cfg.Identity.Mode = config.IdentityVerify
	cfg.Identity.SecretKeys = map[string]string{"test": "test", keyAccountA: "s3cret"}
	s := serverConfig(t, cfg, &fakeRuntime{})
	ctx := context.Background()

	identity, err := stsClient(s, keyAccountA, "s3cret").GetCallerIdentity(ctx, nil)
	if err != nil || aws.ToString(identity.Account) != keyAccountA {
		t.Fatalf("configured 12-digit key: %v %v", identity, err)
	}

	if _, err = stsClient(s, keyAccountA, "wrong").GetCallerIdentity(ctx, nil); err == nil {
		t.Fatal("wrong secret accepted")
	}

	// A 12-digit key with no configured secret is no bypass.
	if _, err = stsClient(s, keyAccountB, "anything").GetCallerIdentity(ctx, nil); err == nil {
		t.Fatal("unconfigured 12-digit key accepted in verify mode")
	}
}
