package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/amjadjibon/tinystack/internal/config"
)

func TestEventBridgeResourcePolicies(t *testing.T) {
	for _, tc := range []struct {
		bus    string
		cross  bool
		verify bool
	}{
		{"shared", false, false},
		{"shared", true, false},
		{"shared", false, true},
		{"shared", true, true},
		{"default", false, false},
		{"default", true, false},
		{"default", false, true},
		{"default", true, true},
	} {
		t.Run(fmt.Sprintf("cross=%t/verify=%t/bus=%s", tc.cross, tc.verify, tc.bus), func(t *testing.T) {
			cfg := testConfig()
			cfg.Identity.Accounts["test"] = cfg.Defaults.Account
			cfg.Identity.Accounts["other"] = "222222222222"
			cfg.Identity.Authorization = config.AuthorizationEnforce
			if tc.verify {
				cfg.Identity.Mode = config.IdentityVerify
				cfg.Identity.SecretKeys = map[string]string{"test": "test", "other": "test"}
			}

			s := serverConfig(t, cfg, &fakeRuntime{})
			ctx := context.Background()
			key := authorizationUser(t, s)
			owner := "test"
			if tc.cross {
				owner = "other"
			}

			root := eventsClient(s, owner)
			if tc.bus != "default" {
				_, err := root.CreateEventBus(ctx, &eventbridge.CreateEventBusInput{Name: &tc.bus})
				mustAuthorize(t, err)
			}

			described, err := root.DescribeEventBus(ctx, &eventbridge.DescribeEventBusInput{Name: &tc.bus})
			mustAuthorize(t, err)
			arn := *described.Arn
			user := eventbridge.New(eventbridge.Options{
				Region:       "us-east-1",
				BaseEndpoint: new(s.URL),
				Credentials:  authorizationProvider(key),
				Retryer:      aws.NopRetryer{},
			})
			call := func() error {
				out, callErr := user.DescribeEventBus(ctx, &eventbridge.DescribeEventBusInput{Name: &arn})
				if callErr == nil && *out.Arn != arn {
					t.Fatalf("wrong owner: %s", *out.Arn)
				}

				return callErr
			}

			put := func(effect string) {
				t.Helper()
				doc := fmt.Sprintf(
					`{"Statement":[{"Effect":%q,"Principal":"*","Action":"events:DescribeEventBus","Resource":%q}]}`,
					effect,
					arn,
				)
				_, putErr := root.PutPermission(ctx, &eventbridge.PutPermissionInput{EventBusName: &arn, Policy: &doc})
				mustAuthorize(t, putErr)
			}

			wantCode(t, call(), "AccessDeniedException")
			put("Allow")
			if tc.cross {
				wantCode(t, call(), "AccessDeniedException")
			} else {
				mustAuthorize(t, call())
			}

			authorizationPolicy(
				t,
				s,
				fmt.Sprintf(`{"Statement":{"Effect":"Allow","Action":"events:DescribeEventBus","Resource":%q}}`, arn),
			)
			mustAuthorize(t, call())
			put("Deny")
			wantCode(t, call(), "AccessDeniedException")
			_, err = root.DescribeEventBus(ctx, &eventbridge.DescribeEventBusInput{Name: &arn})
			wantCode(t, err, "AccessDeniedException")
		})
	}
}

func TestEventBridgeCrossAccountPublication(t *testing.T) {
	cfg := testConfig()
	cfg.Identity.Accounts["test"] = cfg.Defaults.Account
	cfg.Identity.Accounts["other"] = "222222222222"
	cfg.Identity.Authorization = config.AuthorizationEnforce
	s := serverConfig(t, cfg, &fakeRuntime{})
	ctx := context.Background()
	root := eventsClient(s, "other")
	q := sqsClient(s, "other")
	queueURL, queue := queueARN(t, q, "received")
	created, err := root.CreateEventBus(ctx, &eventbridge.CreateEventBusInput{Name: new("shared")})
	mustAuthorize(t, err)
	arn := *created.EventBusArn
	_, err = root.PutRule(ctx, &eventbridge.PutRuleInput{
		Name: new("orders"), EventBusName: &arn, EventPattern: new(orderPattern),
	})
	mustAuthorize(t, err)
	_, err = root.PutTargets(ctx, &eventbridge.PutTargetsInput{
		Rule: new("orders"), EventBusName: &arn, Targets: []ebtypes.Target{{Id: new("queue"), Arn: &queue}},
	})
	mustAuthorize(t, err)
	_, err = root.CreateArchive(
		ctx,
		&eventbridge.CreateArchiveInput{ArchiveName: new("received"), EventSourceArn: &arn},
	)
	mustAuthorize(t, err)
	key := authorizationUser(t, s)
	user := eventbridge.New(eventbridge.Options{
		Region:       "us-east-1",
		BaseEndpoint: new(s.URL),
		Credentials:  authorizationProvider(key),
		Retryer:      aws.NopRetryer{},
	})
	authorizationPolicy(
		t,
		s,
		fmt.Sprintf(`{"Statement":{"Effect":"Allow","Action":"events:PutEvents","Resource":%q}}`, arn),
	)
	entry := ebtypes.PutEventsRequestEntry{
		EventBusName: &arn, Source: new("shop"), DetailType: new("order"), Detail: new(`{"id":1}`),
	}

	_, err = user.PutEvents(ctx, &eventbridge.PutEventsInput{Entries: []ebtypes.PutEventsRequestEntry{entry}})
	wantCode(t, err, "AccessDeniedException")
	doc := fmt.Sprintf(
		`{"Statement":[{"Effect":"Allow",
		"Principal":{"AWS":%q},
		"Action":"events:PutEvents",
		"Resource":%q,
		"Condition":{"StringEquals":{"events:source":"shop",
		"events:detail-type":"order"},
		"Bool":{"events:eventBusInvocation":"false"}}}]}`,
		cfg.Defaults.Account,
		arn,
	)
	_, err = root.PutPermission(ctx, &eventbridge.PutPermissionInput{EventBusName: &arn, Policy: &doc})
	mustAuthorize(t, err)
	out, err := user.PutEvents(ctx, &eventbridge.PutEventsInput{Entries: []ebtypes.PutEventsRequestEntry{entry}})
	mustAuthorize(t, err)
	if out.FailedEntryCount != 0 || len(out.Entries) != 1 || out.Entries[0].EventId == nil {
		t.Fatalf("event was not accepted: %+v", out)
	}

	var envelope struct{ Account string }
	mustAuthorize(t, json.Unmarshal([]byte(receiveBody(t, q, queueURL)), &envelope))
	if envelope.Account != cfg.Defaults.Account {
		t.Fatalf("sender account lost: %s", envelope.Account)
	}

	// Every entry must satisfy the policy before any delivery or archive write.
	bad := entry
	bad.Source = new("untrusted")
	_, err = user.PutEvents(ctx, &eventbridge.PutEventsInput{Entries: []ebtypes.PutEventsRequestEntry{entry, bad}})
	wantCode(t, err, "AccessDeniedException")
	bad = entry
	bad.DetailType = new("other")
	_, err = user.PutEvents(ctx, &eventbridge.PutEventsInput{Entries: []ebtypes.PutEventsRequestEntry{bad}})
	wantCode(t, err, "AccessDeniedException")
	messages, err := q.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: &queueURL})
	mustAuthorize(t, err)
	if len(messages.Messages) != 0 {
		t.Fatal("denied batch delivered an event")
	}

	archived, err := root.DescribeArchive(ctx, &eventbridge.DescribeArchiveInput{ArchiveName: new("received")})
	mustAuthorize(t, err)
	if archived.EventCount != 1 {
		t.Fatalf("owner archive count: %d", archived.EventCount)
	}
}

func TestEventBridgeRuleCreatorPolicy(t *testing.T) {
	cfg := testConfig()
	cfg.Identity.Accounts["test"] = cfg.Defaults.Account
	cfg.Identity.Accounts["other"] = "222222222222"
	cfg.Identity.Authorization = config.AuthorizationEnforce
	s := serverConfig(t, cfg, &fakeRuntime{})
	ctx := context.Background()
	owner := eventsClient(s, "other")
	created, err := owner.CreateEventBus(ctx, &eventbridge.CreateEventBusInput{Name: new("shared")})
	mustAuthorize(t, err)
	arn := *created.EventBusArn
	rules := "arn:aws:events:us-east-1:222222222222:rule/shared/*"
	key := authorizationUser(t, s)
	user := eventbridge.New(eventbridge.Options{
		Region:       "us-east-1",
		BaseEndpoint: new(s.URL),
		Credentials:  authorizationProvider(key),
		Retryer:      aws.NopRetryer{},
	})
	authorizationPolicy(t, s, fmt.Sprintf(`{"Statement":{"Effect":"Allow","Action":"events:*","Resource":%q}}`, rules))
	doc := fmt.Sprintf(
		`{"Statement":[{"Effect":"Allow",
		"Principal":{"AWS":%q},
		"Action":"events:*",
		"Resource":%q,
		"Condition":{"StringEqualsIfExists":{"events:creatorAccount":%q}}}]}`,
		cfg.Defaults.Account,
		rules,
		cfg.Defaults.Account,
	)
	_, err = owner.PutPermission(ctx, &eventbridge.PutPermissionInput{EventBusName: &arn, Policy: &doc})
	mustAuthorize(t, err)
	out, err := user.PutRule(ctx, &eventbridge.PutRuleInput{
		Name: new("mine"), EventBusName: &arn, EventPattern: new(orderPattern),
	})
	mustAuthorize(t, err)
	if *out.RuleArn != "arn:aws:events:us-east-1:222222222222:rule/shared/mine" {
		t.Fatalf("wrong rule owner: %s", *out.RuleArn)
	}

	// An owner's update must not transfer the original creator's permissions.
	_, err = owner.PutRule(ctx, &eventbridge.PutRuleInput{
		Name: new("mine"), EventBusName: &arn, EventPattern: new(orderPattern),
	})
	mustAuthorize(t, err)
	described, err := user.DescribeRule(ctx, &eventbridge.DescribeRuleInput{Name: new("mine"), EventBusName: &arn})
	mustAuthorize(t, err)
	if aws.ToString(described.CreatedBy) != cfg.Defaults.Account {
		t.Fatalf("creator changed: %+v", described)
	}

	_, err = user.DisableRule(ctx, &eventbridge.DisableRuleInput{Name: new("mine"), EventBusName: &arn})
	mustAuthorize(t, err)
	_, err = user.EnableRule(ctx, &eventbridge.EnableRuleInput{Name: new("mine"), EventBusName: &arn})
	mustAuthorize(t, err)
	_, err = user.TagResource(ctx, &eventbridge.TagResourceInput{
		ResourceARN: out.RuleArn, Tags: []ebtypes.Tag{{Key: new("team"), Value: new("apps")}},
	})
	mustAuthorize(t, err)
	tags, err := user.ListTagsForResource(ctx, &eventbridge.ListTagsForResourceInput{ResourceARN: out.RuleArn})
	mustAuthorize(t, err)
	if len(tags.Tags) != 1 || aws.ToString(tags.Tags[0].Value) != "apps" {
		t.Fatalf("tags not saved to owner's rule: %+v", tags)
	}

	_, err = user.UntagResource(
		ctx,
		&eventbridge.UntagResourceInput{ResourceARN: out.RuleArn, TagKeys: []string{"team"}},
	)
	mustAuthorize(t, err)
	_, queue := queueARN(t, sqsClient(s, "other"), "creator-target")
	attached, err := user.PutTargets(ctx, &eventbridge.PutTargetsInput{
		Rule: new("mine"), EventBusName: &arn, Targets: []ebtypes.Target{{Id: new("q"), Arn: &queue}},
	})
	mustAuthorize(t, err)
	if attached.FailedEntryCount != 0 {
		t.Fatalf("cross-account target rejected: %+v", attached)
	}

	listed, err := user.ListTargetsByRule(
		ctx,
		&eventbridge.ListTargetsByRuleInput{Rule: new("mine"), EventBusName: &arn},
	)
	mustAuthorize(t, err)
	if len(listed.Targets) != 1 || aws.ToString(listed.Targets[0].Arn) != queue {
		t.Fatalf("target saved in wrong scope: %+v", listed)
	}

	removed, err := user.RemoveTargets(ctx, &eventbridge.RemoveTargetsInput{
		Rule: new("mine"), EventBusName: &arn, Ids: []string{"q"},
	})
	mustAuthorize(t, err)
	if removed.FailedEntryCount != 0 {
		t.Fatalf("target not removed: %+v", removed)
	}

	_, err = owner.PutRule(ctx, &eventbridge.PutRuleInput{
		Name: new("owners"), EventBusName: &arn, EventPattern: new(orderPattern),
	})
	mustAuthorize(t, err)
	_, err = user.DisableRule(ctx, &eventbridge.DisableRuleInput{Name: new("owners"), EventBusName: &arn})
	wantCode(t, err, "AccessDeniedException")
	_, err = user.DeleteRule(ctx, &eventbridge.DeleteRuleInput{Name: new("mine"), EventBusName: &arn})
	mustAuthorize(t, err)
	_, err = owner.DescribeRule(ctx, &eventbridge.DescribeRuleInput{Name: new("mine"), EventBusName: &arn})
	wantCode(t, err, "ResourceNotFoundException")
}

func TestEventBridgePolicyValidation(t *testing.T) {
	s := server(t)
	c := eventsClient(s, "test")
	ctx := context.Background()
	for _, statement := range []string{
		`{"Effect":"Allow","NotPrincipal":"*","Action":"events:PutEvents","Resource":"*"}`,
		`{"Effect":"Permit","Principal":"*","Action":"events:PutEvents","Resource":"*"}`,
		`{"Effect":"Allow",
		"Principal":"*",
		"Action":"events:PutEvents",
		"Resource":"*",
		"Condition":{"StringEquals":{"events:unknown":"x"}}}`,
		`{"Effect":"Allow",
		"Principal":"*",
		"Action":"events:PutEvents",
		"Resource":"*",
		"Condition":{"Unknown":{"events:source":"x"}}}`,
	} {
		_, err := c.PutPermission(
			ctx,
			&eventbridge.PutPermissionInput{Policy: new(`{"Statement":[` + statement + `]}`)},
		)
		wantCode(t, err, "ValidationException")
	}

	_, err := c.PutPermission(ctx, &eventbridge.PutPermissionInput{
		Action: new("events:PutEvents"), Principal: new("*"), StatementId: new("org"),
		Condition: &ebtypes.Condition{Type: new("StringEquals"), Key: new("aws:PrincipalOrgID"), Value: new("o-1")},
	})
	wantCode(t, err, "ValidationException")
	// The evaluator accepts a single Statement object; statement management must too.
	doc := `{"Id":"bus-policy",
		"Statement":{"Sid":"first",
		"Effect":"Allow",
		"Principal":"*",
		"Action":"events:PutEvents",
		"Resource":"*"}}`
	_, err = c.PutPermission(ctx, &eventbridge.PutPermissionInput{Policy: &doc})
	mustAuthorize(t, err)
	_, err = c.PutPermission(ctx, &eventbridge.PutPermissionInput{
		Action: new("events:PutEvents"), Principal: new("*"), StatementId: new("second"),
	})
	mustAuthorize(t, err)
	_, err = c.RemovePermission(ctx, &eventbridge.RemovePermissionInput{StatementId: new("first")})
	mustAuthorize(t, err)
	out, err := c.DescribeEventBus(ctx, &eventbridge.DescribeEventBusInput{})
	mustAuthorize(t, err)
	var policy struct {
		ID        string `json:"Id"`
		Statement []struct{ Sid string }
	}

	mustAuthorize(t, json.Unmarshal([]byte(aws.ToString(out.Policy)), &policy))
	if policy.ID != "bus-policy" || len(policy.Statement) != 1 || policy.Statement[0].Sid != "second" {
		t.Fatalf("policy lost statements or Id: %s", aws.ToString(out.Policy))
	}
}

func TestEventBridgeRuleConditionContext(t *testing.T) {
	s := authorizationServer(t, config.AuthorizationEnforce, true)
	ctx := context.Background()
	key := authorizationUser(t, s)
	user := eventbridge.New(eventbridge.Options{
		Region:       "us-east-1",
		BaseEndpoint: new(s.URL),
		Credentials:  authorizationProvider(key),
		Retryer:      aws.NopRetryer{},
	})
	_, queue := queueARN(t, sqsClient(s, "test"), "allowed")
	doc := fmt.Sprintf(
		`{"Statement":[{"Effect":"Allow",
		"Action":"events:PutRule",
		"Resource":"*",
		"Condition":{"ForAllValues:StringEquals":{"events:source":"shop",
		"events:detail-type":"order"},
		"Null":{"events:source":"false",
		"events:detail-type":"false"}}},{"Effect":"Allow",
		"Action":"events:PutTargets",
		"Resource":"*",
		"Condition":{"ForAllValues:ArnEquals":{"events:TargetArn":%q},
		"Null":{"events:TargetArn":"false"}}}]}`,
		queue,
	)
	authorizationPolicy(t, s, doc)
	_, err := user.PutRule(ctx, &eventbridge.PutRuleInput{Name: new("allowed"), EventPattern: new(orderPattern)})
	mustAuthorize(t, err)
	for _, pattern := range []string{
		`{"detail-type":["order"]}`,
		`{"source":["shop","untrusted"],"detail-type":["order"]}`,
		`{"source":[{"prefix":"shop"}],"detail-type":["order"]}`,
		`{"$or":[{"source":["shop"]},{"source":["untrusted"]}],"detail-type":["order"]}`,
	} {
		_, err = user.PutRule(ctx, &eventbridge.PutRuleInput{Name: new("bad"), EventPattern: &pattern})
		wantCode(t, err, "AccessDeniedException")
	}

	out, err := user.PutTargets(ctx, &eventbridge.PutTargetsInput{
		Rule: new("allowed"), Targets: []ebtypes.Target{{Id: new("q"), Arn: &queue}},
	})
	mustAuthorize(t, err)
	if out.FailedEntryCount != 0 {
		t.Fatalf("target rejected: %+v", out)
	}

	_, err = user.PutTargets(ctx, &eventbridge.PutTargetsInput{
		Rule: new("allowed"), Targets: []ebtypes.Target{
			{Id: new("q"), Arn: &queue}, {Id: new("other"), Arn: new(queue + "-other")},
		},
	})
	wantCode(t, err, "AccessDeniedException")
}

func TestEventBridgeBatchBusOwners(t *testing.T) {
	cfg := testConfig()
	cfg.Identity.Accounts["test"] = cfg.Defaults.Account
	cfg.Identity.Accounts["other"] = "222222222222"
	cfg.Identity.Authorization = config.AuthorizationEnforce
	s := serverConfig(t, cfg, &fakeRuntime{})
	ctx := context.Background()
	var entries []ebtypes.PutEventsRequestEntry
	type destination struct {
		bus   *eventbridge.Client
		queue *sqs.Client
		url   string
	}

	var received []destination
	for _, owner := range []string{"other", "test"} {
		c := eventsClient(s, owner)
		q := sqsClient(s, owner)
		url, queue := queueARN(t, q, "shared")
		created, err := c.CreateEventBus(ctx, &eventbridge.CreateEventBusInput{Name: new("shared")})
		mustAuthorize(t, err)
		_, err = c.PutPermission(ctx, &eventbridge.PutPermissionInput{
			EventBusName: created.EventBusArn, Action: new("events:PutEvents"),
			Principal: &cfg.Defaults.Account, StatementId: new("publisher"),
		})
		mustAuthorize(t, err)
		_, err = c.PutRule(ctx, &eventbridge.PutRuleInput{
			Name: new("shared"), EventBusName: new("shared"), EventPattern: new(orderPattern),
		})
		mustAuthorize(t, err)
		_, err = c.PutTargets(ctx, &eventbridge.PutTargetsInput{
			Rule: new("shared"), EventBusName: new("shared"), Targets: []ebtypes.Target{{Id: new("q"), Arn: &queue}},
		})
		mustAuthorize(t, err)
		_, err = c.CreateArchive(ctx, &eventbridge.CreateArchiveInput{
			ArchiveName: new("shared"), EventSourceArn: created.EventBusArn,
		})
		mustAuthorize(t, err)
		entries = append(entries, ebtypes.PutEventsRequestEntry{
			EventBusName: created.EventBusArn, Source: new("shop"), DetailType: new("order"), Detail: new(`{}`),
		})
		received = append(received, destination{bus: c, queue: q, url: url})
	}

	out, err := eventsClient(s, "test").PutEvents(ctx, &eventbridge.PutEventsInput{Entries: entries})
	mustAuthorize(t, err)
	if out.FailedEntryCount != 0 || len(out.Entries) != 2 {
		t.Fatalf("mixed-owner batch rejected: %+v", out)
	}

	for _, target := range received {
		var envelope struct{ Account string }
		mustAuthorize(t, json.Unmarshal([]byte(receiveBody(t, target.queue, target.url)), &envelope))
		if envelope.Account != cfg.Defaults.Account {
			t.Fatalf("wrong sender: %+v", envelope)
		}

		archive, describeErr := target.bus.DescribeArchive(
			ctx,
			&eventbridge.DescribeArchiveInput{ArchiveName: new("shared")},
		)
		mustAuthorize(t, describeErr)
		if archive.EventCount != 1 {
			t.Fatalf("wrong archive owner: %+v", archive)
		}
	}
}

func TestEventBridgeOperatorPatternsWithoutConditions(t *testing.T) {
	s := authorizationServer(t, config.AuthorizationEnforce, true)
	ctx := context.Background()
	key := authorizationUser(t, s)
	user := eventbridge.New(eventbridge.Options{
		Region:       "us-east-1",
		BaseEndpoint: new(s.URL),
		Credentials:  authorizationProvider(key),
		Retryer:      aws.NopRetryer{},
	})
	authorizationPolicy(t, s, `{"Statement":{"Effect":"Allow","Action":"events:*","Resource":"*"}}`)
	for _, pattern := range []string{
		`{"source":[{"prefix":"aws."}]}`,
		`{"$or":[{"source":["shop"]},{"detail-type":["order"]}]}`,
	} {
		_, err := user.PutRule(ctx, &eventbridge.PutRuleInput{Name: new("operators"), EventPattern: &pattern})
		mustAuthorize(t, err)
	}
}
