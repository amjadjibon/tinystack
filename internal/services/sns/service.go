// Package sns implements standard SNS topics with SQS, Lambda, HTTP, HTTPS,
// and email subscriptions. Deliveries call the SQS and Lambda services
// in-process, post to HTTP endpoints through the shared outbound proxy, and
// record email in the delivery log.
package sns

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"maps"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/sns"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/delivery"
	"github.com/amjadjibon/tinystack/internal/services/awsmetrics"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/services/lambda"
	"github.com/amjadjibon/tinystack/internal/services/sqs"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	pageSize        = 100
	protocolSQS     = "sqs"
	protocolLambda  = "lambda"
	arnScheme       = "arn"
	attrDisplayName = "DisplayName"
	attrRaw         = "RawMessageDelivery"
	attrTopicArn    = "TopicArn"
	attrPolicy      = "Policy"
	// maxPolicy is the largest topic access policy SNS accepts.
	maxPolicy = 30 * 1024
)

var _ api.API = (*Service)(nil)

type Service struct {
	store      storage.Store
	queues     *sqs.Service
	functions  *lambda.Service
	deliveries *delivery.Log
	now        func() time.Time
	// authorizer checks deliveries to queues and functions against their resource policies; see UseServiceAuthorizer.
	authorizer catalog.ServiceAuthorizer
	// bg runs HTTP deliveries until Stop.
	bg      *background.Group
	metrics awsmetrics.Meter
}

// New returns SNS. Call Stop to end HTTP deliveries in flight.
func New(store storage.Store, queues *sqs.Service, functions *lambda.Service, deliveries *delivery.Log) *Service {
	return &Service{
		store: store, queues: queues, functions: functions, deliveries: deliveries, now: time.Now,
		bg: background.New(),
	}
}

// Stop cancels HTTP deliveries in flight and waits for them to end.
func (s *Service) Stop() { s.bg.Stop() }

// UseServiceAuthorizer makes deliveries to SQS queues and Lambda functions require the target's resource policy to
// allow sns.amazonaws.com. Call it before serving requests.
func (s *Service) UseServiceAuthorizer(authorizer catalog.ServiceAuthorizer) {
	s.authorizer = authorizer
}

// subscription is a topic's subscription. SQS and Lambda subscriptions are
// confirmed immediately, as AWS does for resources in the subscriber's
// account; HTTP, HTTPS, and email ones are Pending until ConfirmSubscription
// presents their Token. FilterPolicyScope is empty for the default,
// MessageAttributes.
type subscription struct {
	ID                string
	Protocol          string
	Endpoint          string
	Owner             string
	Token             string `json:",omitempty"`
	FilterPolicy      string `json:",omitempty"`
	FilterPolicyScope string `json:",omitempty"`
	Raw               bool   `json:",omitempty"`
	Pending           bool   `json:",omitempty"`
}

// arn is the subscription's ARN as list operations report it.
func (sub *subscription) arn(topicARN string) string {
	if sub.Pending {
		return "PendingConfirmation"
	}

	return topicARN + ":" + sub.ID
}

type topic struct {
	Attributes map[string]string `json:",omitempty"`
	Tags       map[string]string `json:",omitempty"`
	// Dedup holds a FIFO topic's recent messages by deduplication ID.
	Dedup         map[string]*sequenced `json:",omitempty"`
	Subscriptions []subscription
	CreatedAt     int64
	// Sequence is the last sequence number a FIFO topic gave a message.
	Sequence uint64 `json:",omitempty"`
}

var (
	topicName = regexp.MustCompile(`^(?:[A-Za-z0-9_-]{1,256}|[A-Za-z0-9_-]{1,251}\.fifo)$`)
	accountID = regexp.MustCompile(`^\d{12}$`)
)

func invalid(format string, args ...any) error {
	return awserror.New("InvalidParameterException", "Invalid parameter: "+fmt.Sprintf(format, args...))
}

func notFound(what string) error {
	return awserror.New("NotFoundException", what+" does not exist")
}

//nolint:gocritic // Metadata is an immutable request snapshot.
func topicKey(m request.Metadata, name string) string {
	return m.AccountID + "/" + m.Region + "/sns/" + name
}

// keyARN is the ARN of the topic stored under key.
func keyARN(key string) string {
	parts := strings.SplitN(key, "/", 4)
	return "arn:aws:sns:" + parts[1] + ":" + parts[0] + ":" + parts[3]
}

// parseARN splits a topic ARN, or a subscription ARN when sub is true.
func parseARN(arn *string, sub bool) ([]string, error) {
	name, want := attrTopicArn, 6
	if sub {
		name, want = "SubscriptionArn", 7
	}

	parts := strings.Split(common.Value(arn), ":")
	if len(parts) != want || parts[0] != arnScheme || parts[2] != "sns" || parts[3] == "" ||
		!accountID.MatchString(parts[4]) || !topicName.MatchString(parts[5]) || (sub && parts[6] == "") {
		return nil, invalid("%s", name)
	}

	return parts, nil
}

// resolve maps a topic ARN, or a subscription ARN when sub is true, to the
// topic's storage key and the subscription ID. Cross-account resolution is
// available on the gateway resource authorization path; regions stay isolated.
func resolve(ctx context.Context, arn *string, sub bool) (key, id string, err error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return "", "", err
	}

	parts, err := parseARN(arn, sub)
	if err != nil {
		return "", "", err
	}

	if parts[3] != m.Region || (parts[4] != m.AccountID && !m.ResourceAuthorization) {
		return "", "", notFound("Topic")
	}

	if sub {
		id = parts[6]
	}

	m.AccountID = parts[4]
	return topicKey(m, parts[5]), id, nil
}

// topicKeyOf maps a topic ARN to its storage key in any account and region.
func topicKeyOf(arn *string) (string, error) {
	parts, err := parseARN(arn, false)
	if err != nil {
		return "", err
	}

	return parts[4] + "/" + parts[3] + "/sns/" + parts[5], nil
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	s := hex.EncodeToString(b[:])
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
}

func load(tx storage.ReadTx, key string) (*topic, error) {
	t, found, err := storage.Read[topic](tx, key)
	if err != nil {
		return nil, fmt.Errorf("decode topic: %w", err)
	}
	if !found {
		return nil, notFound("Topic")
	}
	return t, nil
}

// page returns the sorted names after the NextToken and the token for the next page.
func page(names []string, token *string) (selected []string, next *string, err error) {
	sort.Strings(names)
	return common.Page(names, func(n string) string { return n }, token, pageSize, invalid("NextToken"))
}

func (s *Service) CreateTopic(ctx context.Context, in *api.CreateTopicInput) (*api.CreateTopicResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	name := common.Value(in.Name)
	if !topicName.MatchString(name) {
		return nil, invalid("Topic Name must be 1 to 256 alphanumeric characters, hyphens, or underscores, " +
			"ending in .fifo for a FIFO topic")
	}

	attributes, err := topicAttributes(name, in.Attributes)
	if err != nil {
		return nil, err
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	if tagErr := tagging.Check(tags); tagErr != nil {
		return nil, tagError(tagErr)
	}

	key := topicKey(m, name)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, exists := tx.Get(key); !exists {
			t := &topic{Attributes: attributes, Tags: tags, CreatedAt: s.now().Unix()}
			return storage.Write(tx, key, t)
		}

		t, loadErr := load(tx, key)
		if loadErr != nil {
			return loadErr
		}

		if t.Attributes[attrDisplayName] != in.Attributes[attrDisplayName] {
			return invalid("Topic already exists with different attributes")
		}

		for k, v := range in.Attributes {
			if t.Attributes[k] != v {
				return invalid("Topic already exists with different attributes")
			}
		}

		if len(tags) > 0 && !maps.Equal(t.Tags, tags) {
			return invalid("Tags Reason: Topic already exists with different tags")
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateTopicResponse{TopicArn: new(keyARN(key))}, nil
}

// DeleteTopic removes a topic and its subscriptions. Deleting a missing topic succeeds.
func (s *Service) DeleteTopic(ctx context.Context, in *api.DeleteTopicInput) (*api.DeleteTopicOutput, error) {
	key, _, err := resolve(ctx, in.TopicArn, false)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		tx.Delete(key)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteTopicOutput{}, nil
}

// topicKeys lists the caller's topic keys.
func (s *Service) topicKeys(ctx context.Context, tx storage.ReadTx) ([]string, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	return tx.Keys(topicKey(m, "")), nil
}

func (s *Service) ListTopics(ctx context.Context, in *api.ListTopicsInput) (*api.ListTopicsResponse, error) {
	var arns []string
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		keys, err := s.topicKeys(ctx, tx)
		for _, k := range keys {
			arns = append(arns, keyARN(k))
		}

		return err
	})
	if err != nil {
		return nil, err
	}

	arns, next, err := page(arns, in.NextToken)
	if err != nil {
		return nil, err
	}

	out := &api.ListTopicsResponse{NextToken: next}
	for _, arn := range arns {
		out.Topics = append(out.Topics, api.Topic{TopicArn: new(arn)})
	}

	return out, nil
}

func (s *Service) Subscribe(ctx context.Context, in *api.SubscribeInput) (*api.SubscribeResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	key, _, err := resolve(ctx, in.TopicArn, false)
	if err != nil {
		return nil, err
	}

	protocol, endpoint := common.Value(in.Protocol), common.Value(in.Endpoint)
	if guardErr := checkSubscriber(protocol, endpoint, m.AccountID); guardErr != nil {
		return nil, guardErr
	}

	sub := subscription{ID: newID(), Protocol: protocol, Endpoint: endpoint, Owner: m.AccountID}
	if confirmable(protocol) {
		sub.Pending, sub.Token = true, newToken()
	}

	for k, v := range in.Attributes {
		if guardErr := sub.set(k, v); guardErr != nil {
			return nil, guardErr
		}
	}

	if guardErr := sub.check(); guardErr != nil {
		return nil, guardErr
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		t, loadErr := load(tx, key)
		if loadErr != nil {
			return loadErr
		}

		if guardErr := t.checkSubscription(sub.Protocol, sub.Endpoint); guardErr != nil {
			return guardErr
		}

		for _, existing := range t.Subscriptions {
			if existing.Protocol == sub.Protocol && existing.Endpoint == sub.Endpoint {
				if existing.Raw != sub.Raw || existing.FilterPolicy != sub.FilterPolicy ||
					existing.FilterPolicyScope != sub.FilterPolicyScope {
					return invalid("Subscription already exists with different attributes")
				}

				sub = existing
				return nil
			}
		}

		t.Subscriptions = append(t.Subscriptions, sub)
		return storage.Write(tx, key, t)
	})
	if err != nil {
		return nil, err
	}

	// A pending subscription's confirmation request goes out again each time
	// it is subscribed.
	if sub.Pending {
		if confirmErr := s.requestConfirmation(m.Endpoint, keyARN(key), &sub); confirmErr != nil {
			return nil, confirmErr
		}

		if !common.Value(in.ReturnSubscriptionArn) {
			return &api.SubscribeResponse{SubscriptionArn: new(pendingARN)}, nil
		}
	}

	return &api.SubscribeResponse{SubscriptionArn: new(keyARN(key) + ":" + sub.ID)}, nil
}

// checkSubscriber accepts SQS queues and Lambda functions in the subscriber's
// account, HTTP/HTTPS URLs and email addresses. In-process deliveries bypass
// destination resource policy enforcement.
func checkSubscriber(protocol, endpoint, account string) error {
	parts := strings.Split(endpoint, ":")
	switch protocol {
	case protocolSQS:
		if len(parts) != 6 || parts[0] != arnScheme || parts[2] != protocolSQS || parts[3] == "" || parts[5] == "" {
			return invalid("SQS endpoint ARN")
		}
	case protocolLambda:
		if len(parts) < 7 || len(parts) > 8 || parts[0] != arnScheme || parts[2] != protocolLambda ||
			parts[3] == "" || parts[5] != "function" || parts[6] == "" {
			return invalid("Lambda endpoint ARN")
		}
	case protocolHTTP, protocolHTTPS, protocolEmail, protocolEmailJSON:
		return checkEndpoint(protocol, endpoint)
	case protocolSMS:
		return checkPhoneNumber(endpoint)
	default:
		return invalid("Protocol %q is not supported; only sqs, lambda, http, https, email, email-json, and "+
			"sms subscriptions are", protocol)
	}

	if parts[4] != account {
		return invalid("The endpoint must be in the subscriber's account")
	}

	return nil
}

func (s *Service) Unsubscribe(ctx context.Context, in *api.UnsubscribeInput) (*api.UnsubscribeOutput, error) {
	key, id, err := resolve(ctx, in.SubscriptionArn, true)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		t, loadErr := load(tx, key)
		if loadErr != nil {
			return loadErr
		}

		for i := range t.Subscriptions {
			if t.Subscriptions[i].ID == id {
				t.Subscriptions = append(t.Subscriptions[:i], t.Subscriptions[i+1:]...)
				return storage.Write(tx, key, t)
			}
		}

		return notFound("Subscription")
	})
	if err != nil {
		return nil, err
	}

	return &api.UnsubscribeOutput{}, nil
}

// subscriptions renders the subscriptions of the topics stored under keys,
// keyed by subscription ARN.
func subscriptions(tx storage.ReadTx, keys []string) (map[string]api.Subscription, error) {
	out := map[string]api.Subscription{}
	for _, key := range keys {
		t, err := load(tx, key)
		if err != nil {
			return nil, err
		}

		arn := keyARN(key)
		for _, sub := range t.Subscriptions {
			out[arn+":"+sub.ID] = api.Subscription{
				SubscriptionArn: new(sub.arn(arn)),
				Owner:           new(sub.Owner),
				Protocol:        new(sub.Protocol),
				Endpoint:        new(sub.Endpoint),
				TopicArn:        new(arn),
			}
		}
	}

	return out, nil
}

// listSubscriptions pages through the subscriptions of the topics that keys selects.
func (s *Service) listSubscriptions(
	ctx context.Context,
	token *string,
	keys func(tx storage.ReadTx) ([]string, error),
) ([]api.Subscription, *string, error) {
	var all map[string]api.Subscription
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		selected, err := keys(tx)
		if err != nil {
			return err
		}

		all, err = subscriptions(tx, selected)
		return err
	})
	if err != nil {
		return nil, nil, err
	}

	arns := make([]string, 0, len(all))
	for arn := range all {
		arns = append(arns, arn)
	}

	arns, next, err := page(arns, token)
	if err != nil {
		return nil, nil, err
	}

	out := make([]api.Subscription, 0, len(arns))
	for _, arn := range arns {
		out = append(out, all[arn])
	}

	return out, next, nil
}

func (s *Service) ListSubscriptions(
	ctx context.Context,
	in *api.ListSubscriptionsInput,
) (*api.ListSubscriptionsResponse, error) {
	subs, next, err := s.listSubscriptions(ctx, in.NextToken, func(tx storage.ReadTx) ([]string, error) {
		return s.topicKeys(ctx, tx)
	})
	if err != nil {
		return nil, err
	}

	return &api.ListSubscriptionsResponse{Subscriptions: subs, NextToken: next}, nil
}

func (s *Service) ListSubscriptionsByTopic(
	ctx context.Context,
	in *api.ListSubscriptionsByTopicInput,
) (*api.ListSubscriptionsByTopicResponse, error) {
	key, _, err := resolve(ctx, in.TopicArn, false)
	if err != nil {
		return nil, err
	}

	subs, next, err := s.listSubscriptions(ctx, in.NextToken, func(tx storage.ReadTx) ([]string, error) {
		if _, ok := tx.Get(key); !ok {
			return nil, notFound("Topic")
		}

		return []string{key}, nil
	})
	if err != nil {
		return nil, err
	}

	return &api.ListSubscriptionsByTopicResponse{Subscriptions: subs, NextToken: next}, nil
}
