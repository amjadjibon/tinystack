package sns

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/sns"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// checkTopicAttribute validates a writable topic attribute. A Policy is stored
// and never evaluated, and delivery status sample rates are stored too; like
// SNS without a feedback role, TinyStack logs nothing for them. Attributes
// that would change delivery fail.
func checkTopicAttribute(name, v string) error {
	switch {
	case name == attrDisplayName:
		if len(v) > 100 {
			return invalid("DisplayName must be at most 100 characters")
		}
	case name == attrFifo || name == attrContentDedup:
		if v != valueTrue && v != valueFalse {
			return invalid("%s must be true or false", name)
		}
	case name == attrPolicy:
		var parsed map[string]json.RawMessage
		if len(v) > maxPolicy || json.Unmarshal([]byte(v), &parsed) != nil || parsed["Statement"] == nil {
			return invalid("Policy Error: the policy must be a JSON object with a Statement")
		}
	case strings.HasSuffix(name, "FeedbackSampleRate"):
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 100 {
			return invalid("%s must be an integer from 0 to 100", name)
		}
	default:
		return invalid("Attribute %s is not supported", name)
	}

	return nil
}

// defaultPolicy is the access policy SNS gives a new topic: only the owning
// account may use it, which matches TinyStack's account scoping.
func defaultPolicy(arn, owner string) string {
	actions := []string{
		"SNS:GetTopicAttributes", "SNS:SetTopicAttributes", "SNS:AddPermission", "SNS:RemovePermission",
		"SNS:DeleteTopic", "SNS:Subscribe", "SNS:ListSubscriptionsByTopic", "SNS:Publish",
	}

	policy := map[string]any{
		"Version": "2008-10-17",
		"Id":      "__default_policy_ID",
		"Statement": []map[string]any{{
			"Sid":       "__default_statement_ID",
			"Effect":    "Allow",
			"Principal": map[string]string{"AWS": "*"},
			"Action":    actions,
			"Resource":  arn,
			"Condition": map[string]any{"StringEquals": map[string]string{"AWS:SourceOwner": owner}},
		}},
	}

	b, _ := json.Marshal(policy)
	return string(b)
}

func (s *Service) GetTopicAttributes(
	ctx context.Context,
	in *api.GetTopicAttributesInput,
) (*api.GetTopicAttributesResponse, error) {
	key, _, err := resolve(ctx, in.TopicArn, false)
	if err != nil {
		return nil, err
	}

	var t *topic
	err = s.store.View(ctx, func(tx storage.ReadTx) (loadErr error) {
		t, loadErr = load(tx, key)
		return loadErr
	})
	if err != nil {
		return nil, err
	}

	arn := keyARN(key)
	owner := strings.Split(arn, ":")[4]
	attributes := map[string]string{
		attrTopicArn:             arn,
		"Owner":                  owner,
		attrPolicy:               defaultPolicy(arn, owner),
		attrDisplayName:          "",
		"SubscriptionsConfirmed": strconv.Itoa(len(t.Subscriptions)),
		"SubscriptionsPending":   "0",
		"SubscriptionsDeleted":   "0",
	}

	for k, v := range t.Attributes {
		attributes[k] = v
	}

	return &api.GetTopicAttributesResponse{Attributes: attributes}, nil
}

func (s *Service) SetTopicAttributes(
	ctx context.Context,
	in *api.SetTopicAttributesInput,
) (*api.SetTopicAttributesOutput, error) {
	key, _, err := resolve(ctx, in.TopicArn, false)
	if err != nil {
		return nil, err
	}

	name, v := value(in.AttributeName), value(in.AttributeValue)
	if guardErr := checkTopicAttribute(name, v); guardErr != nil {
		return nil, guardErr
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		t, loadErr := load(tx, key)
		if loadErr != nil {
			return loadErr
		}

		if guardErr := t.checkSetAttribute(name); guardErr != nil {
			return guardErr
		}

		if t.Attributes == nil {
			t.Attributes = map[string]string{}
		}

		t.Attributes[name] = v
		return save(tx, key, t)
	})
	if err != nil {
		return nil, err
	}

	return &api.SetTopicAttributesOutput{}, nil
}

// findSubscription returns the subscription with id on the topic.
func findSubscription(t *topic, id string) (*subscription, error) {
	for i := range t.Subscriptions {
		if t.Subscriptions[i].ID == id {
			return &t.Subscriptions[i], nil
		}
	}

	return nil, notFound("Subscription")
}

func (s *Service) GetSubscriptionAttributes(
	ctx context.Context,
	in *api.GetSubscriptionAttributesInput,
) (*api.GetSubscriptionAttributesResponse, error) {
	key, id, err := resolve(ctx, in.SubscriptionArn, true)
	if err != nil {
		return nil, err
	}

	var sub *subscription
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		t, loadErr := load(tx, key)
		if loadErr != nil {
			return loadErr
		}

		sub, loadErr = findSubscription(t, id)
		return loadErr
	})
	if err != nil {
		return nil, err
	}

	arn := keyARN(key)
	attributes := map[string]string{
		"SubscriptionArn":              arn + ":" + sub.ID,
		attrTopicArn:                   arn,
		"Owner":                        sub.Owner,
		"Protocol":                     sub.Protocol,
		"Endpoint":                     sub.Endpoint,
		attrRaw:                        strconv.FormatBool(sub.Raw),
		"ConfirmationWasAuthenticated": strconv.FormatBool(!sub.Pending),
		"PendingConfirmation":          strconv.FormatBool(sub.Pending),
	}
	if sub.FilterPolicy != "" {
		attributes[attrFilterPolicy] = sub.FilterPolicy
		attributes[attrFilterPolicyScope] = scopeAttributes
		if sub.FilterPolicyScope != "" {
			attributes[attrFilterPolicyScope] = sub.FilterPolicyScope
		}
	}

	return &api.GetSubscriptionAttributesResponse{Attributes: attributes}, nil
}

func (s *Service) SetSubscriptionAttributes(
	ctx context.Context,
	in *api.SetSubscriptionAttributesInput,
) (*api.SetSubscriptionAttributesOutput, error) {
	key, id, err := resolve(ctx, in.SubscriptionArn, true)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		t, loadErr := load(tx, key)
		if loadErr != nil {
			return loadErr
		}

		sub, findErr := findSubscription(t, id)
		if findErr != nil {
			return findErr
		}

		if setErr := sub.set(value(in.AttributeName), value(in.AttributeValue)); setErr != nil {
			return setErr
		}

		if checkErr := sub.check(); checkErr != nil {
			return checkErr
		}

		return save(tx, key, t)
	})
	if err != nil {
		return nil, err
	}

	return &api.SetSubscriptionAttributesOutput{}, nil
}

// tagMap converts SNS tags to a map, rejecting repeated or missing keys.
func tagMap(tags []api.Tag) (map[string]string, error) {
	out := make(map[string]string, len(tags))
	for _, tag := range tags {
		if tag.Key == nil || tag.Value == nil {
			return nil, invalid("Tags must have a Key and a Value")
		}

		if _, dup := out[*tag.Key]; dup {
			return nil, invalid("Duplicated keys are not allowed.")
		}

		out[*tag.Key] = *tag.Value
	}

	return out, nil
}

func tagError(err error) error {
	if errors.Is(err, tagging.ErrLimit) {
		return awserror.New(
			"TagLimitExceededException",
			"Could not complete request: tag quota of per resource exceeded",
		)
	}

	return invalid("Tags Reason: %v", err)
}

// resourceError reports a missing topic as ResourceNotFoundException, the
// code the tag operations use.
func resourceError(err error) error {
	if e, ok := errors.AsType[*awserror.Error](err); ok && e.Code == "NotFoundException" {
		return awserror.New("ResourceNotFoundException", "Resource does not exist")
	}

	return err
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	key, _, err := resolve(ctx, in.ResourceArn, false)
	if err != nil {
		return nil, resourceError(err)
	}

	add, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		t, loadErr := load(tx, key)
		if loadErr != nil {
			return loadErr
		}

		tags, tagErr := tagging.Merge(t.Tags, add)
		if tagErr != nil {
			return tagError(tagErr)
		}

		t.Tags = tags
		return save(tx, key, t)
	})
	if err != nil {
		return nil, resourceError(err)
	}

	return &api.TagResourceResponse{}, nil
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	key, _, err := resolve(ctx, in.ResourceArn, false)
	if err != nil {
		return nil, resourceError(err)
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		t, loadErr := load(tx, key)
		if loadErr != nil {
			return loadErr
		}

		t.Tags = tagging.Remove(t.Tags, in.TagKeys)
		return save(tx, key, t)
	})
	if err != nil {
		return nil, resourceError(err)
	}

	return &api.UntagResourceResponse{}, nil
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResponse, error) {
	key, _, err := resolve(ctx, in.ResourceArn, false)
	if err != nil {
		return nil, resourceError(err)
	}

	out := &api.ListTagsForResourceResponse{Tags: []api.Tag{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		t, loadErr := load(tx, key)
		if loadErr != nil {
			return loadErr
		}

		for _, k := range slices.Sorted(maps.Keys(t.Tags)) {
			out.Tags = append(out.Tags, api.Tag{Key: new(k), Value: new(t.Tags[k])})
		}

		return nil
	})
	if err != nil {
		return nil, resourceError(err)
	}

	return out, nil
}
