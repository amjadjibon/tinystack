// Package sns implements standard SNS topics with SQS and Lambda subscriptions,
// without HTTP dependencies. Deliveries call the SQS and Lambda services
// in-process.
package sns

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/sns"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/delivery"
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
)

var _ api.API = (*Service)(nil)

type Service struct {
	store      storage.Store
	queues     *sqs.Service
	functions  *lambda.Service
	deliveries *delivery.Log
	now        func() time.Time
}

func New(store storage.Store, queues *sqs.Service, functions *lambda.Service, deliveries *delivery.Log) *Service {
	return &Service{store: store, queues: queues, functions: functions, deliveries: deliveries, now: time.Now}
}

// subscription is a confirmed SQS or Lambda subscription. These endpoints are
// confirmed immediately, as AWS does for resources in the subscriber's account.
type subscription struct {
	ID       string
	Protocol string
	Endpoint string
	Owner    string
	Raw      bool `json:",omitempty"`
}

type topic struct {
	Attributes    map[string]string `json:",omitempty"`
	Tags          map[string]string `json:",omitempty"`
	Subscriptions []subscription
	CreatedAt     int64
}

var (
	topicName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)
	accountID = regexp.MustCompile(`^\d{12}$`)
)

func ptr[T any](v T) *T { return &v }

func value[T any](p *T) (v T) {
	if p != nil {
		return *p
	}

	return v
}

func invalid(format string, args ...any) error {
	return awserror.New("InvalidParameterException", "Invalid parameter: "+fmt.Sprintf(format, args...))
}

func notFound(what string) error {
	return awserror.New("NotFoundException", what+" does not exist")
}

func identity(ctx context.Context) (request.Metadata, error) {
	m, ok := request.FromContext(ctx)
	if !ok || m.AccountID == "" || m.Region == "" {
		return m, fmt.Errorf("missing request identity")
	}

	return m, nil
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

// resolve maps a topic ARN, or a subscription ARN when sub is true, to the
// topic's storage key and the subscription ID. Topics outside the caller's
// account and region do not exist for the caller.
func resolve(ctx context.Context, arn *string, sub bool) (key, id string, err error) {
	m, err := identity(ctx)
	if err != nil {
		return "", "", err
	}

	name, want := attrTopicArn, 6
	if sub {
		name, want = "SubscriptionArn", 7
	}

	parts := strings.Split(value(arn), ":")
	if len(parts) != want || parts[0] != arnScheme || parts[2] != "sns" || parts[3] == "" ||
		!accountID.MatchString(parts[4]) || !topicName.MatchString(parts[5]) || (sub && parts[6] == "") {
		return "", "", invalid("%s", name)
	}

	if parts[3] != m.Region || parts[4] != m.AccountID {
		return "", "", notFound("Topic")
	}

	if sub {
		id = parts[6]
	}

	return topicKey(m, parts[5]), id, nil
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
	data, ok := tx.Get(key)
	if !ok {
		return nil, notFound("Topic")
	}

	var t topic
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("decode topic: %w", err)
	}

	return &t, nil
}

func save(tx storage.WriteTx, key string, t *topic) error {
	data, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("encode topic: %w", err)
	}

	tx.Put(key, data)
	return nil
}

// page returns the sorted names after the NextToken and the token for the next page.
func page(names []string, token *string) (selected []string, next *string, err error) {
	after := ""
	if token != nil {
		b, decodeErr := base64.RawURLEncoding.DecodeString(*token)
		if decodeErr != nil || len(b) == 0 {
			return nil, nil, invalid("NextToken")
		}

		after = string(b)
	}

	sort.Strings(names)
	start := sort.SearchStrings(names, after)
	if start < len(names) && names[start] == after {
		start++
	}

	names = names[start:]
	if len(names) <= pageSize {
		return names, nil, nil
	}

	names = names[:pageSize]
	return names, ptr(base64.RawURLEncoding.EncodeToString([]byte(names[pageSize-1]))), nil
}

func (s *Service) CreateTopic(ctx context.Context, in *api.CreateTopicInput) (*api.CreateTopicResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	name := value(in.Name)
	if !topicName.MatchString(name) {
		return nil, invalid(
			"Topic Name must be 1 to 256 alphanumeric characters, hyphens, or underscores; FIFO topics are not supported",
		)
	}

	for k, v := range in.Attributes {
		if guardErr := checkTopicAttribute(k, v); guardErr != nil {
			return nil, guardErr
		}
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
			t := &topic{Attributes: in.Attributes, Tags: tags, CreatedAt: s.now().Unix()}
			return save(tx, key, t)
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

	return &api.CreateTopicResponse{TopicArn: ptr(keyARN(key))}, nil
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
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	prefix := topicKey(m, "")
	var keys []string
	for _, k := range tx.Keys() {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}

	return keys, nil
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
		out.Topics = append(out.Topics, api.Topic{TopicArn: ptr(arn)})
	}

	return out, nil
}

func parseSubscriptionAttributes(attributes map[string]string) (bool, error) {
	raw := false
	for k, v := range attributes {
		if k != attrRaw {
			return false, invalid("Attribute %s is not supported", k)
		}

		if v != "true" && v != "false" {
			return false, invalid("RawMessageDelivery must be true or false")
		}

		raw = v == "true"
	}

	return raw, nil
}

func (s *Service) Subscribe(ctx context.Context, in *api.SubscribeInput) (*api.SubscribeResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	key, _, err := resolve(ctx, in.TopicArn, false)
	if err != nil {
		return nil, err
	}

	protocol, endpoint := value(in.Protocol), value(in.Endpoint)
	parts := strings.Split(endpoint, ":")
	switch protocol {
	case protocolSQS:
		if len(parts) != 6 || parts[0] != arnScheme || parts[2] != protocolSQS || parts[3] == "" || parts[5] == "" {
			return nil, invalid("SQS endpoint ARN")
		}
	case protocolLambda:
		if len(parts) < 7 || len(parts) > 8 || parts[0] != arnScheme || parts[2] != protocolLambda ||
			parts[3] == "" || parts[5] != "function" || parts[6] == "" {
			return nil, invalid("Lambda endpoint ARN")
		}
	default:
		return nil, invalid("Protocol %q is not supported; only sqs and lambda subscriptions are", protocol)
	}
	// Cross-account delivery needs a resource policy, which TinyStack does not model.
	if parts[4] != m.AccountID {
		return nil, invalid("The endpoint must be in the subscriber's account")
	}

	raw, err := parseSubscriptionAttributes(in.Attributes)
	if err != nil {
		return nil, err
	}

	if raw && protocol == protocolLambda {
		return nil, invalid("Delivery protocol [lambda] does not support raw message delivery.")
	}

	sub := subscription{ID: newID(), Protocol: protocol, Endpoint: endpoint, Owner: m.AccountID, Raw: raw}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		t, loadErr := load(tx, key)
		if loadErr != nil {
			return loadErr
		}

		for _, existing := range t.Subscriptions {
			if existing.Protocol == sub.Protocol && existing.Endpoint == sub.Endpoint {
				if existing.Raw != sub.Raw {
					return invalid("Subscription already exists with different attributes")
				}

				sub = existing
				return nil
			}
		}

		t.Subscriptions = append(t.Subscriptions, sub)
		return save(tx, key, t)
	})
	if err != nil {
		return nil, err
	}

	return &api.SubscribeResponse{SubscriptionArn: ptr(keyARN(key) + ":" + sub.ID)}, nil
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
				return save(tx, key, t)
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
				SubscriptionArn: ptr(arn + ":" + sub.ID),
				Owner:           ptr(sub.Owner),
				Protocol:        ptr(sub.Protocol),
				Endpoint:        ptr(sub.Endpoint),
				TopicArn:        ptr(arn),
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
