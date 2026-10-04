package sns

import (
	"context"
	"regexp"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/sns"
	"github.com/amjadjibon/tinystack/internal/services/policydoc"
	"github.com/amjadjibon/tinystack/internal/storage"
)

var (
	// label is a permission label, the statement ID AddPermission writes.
	label = regexp.MustCompile(`^[A-Za-z0-9_-]{1,80}$`)
	// sharedActions are the actions AddPermission can share.
	sharedActions = []string{
		"Publish", "Subscribe", "Receive", "GetTopicAttributes", "SetTopicAttributes", "AddPermission",
		"RemovePermission", "DeleteTopic", "ListSubscriptionsByTopic",
	}
)

// AddPermission adds a statement to the topic's policy that lets accounts
// take actions on the topic.
func (s *Service) AddPermission(ctx context.Context, in *api.AddPermissionInput) (*api.AddPermissionOutput, error) {
	key, _, err := resolve(ctx, in.TopicArn, false)
	if err != nil {
		return nil, err
	}

	name := value(in.Label)
	switch {
	case !label.MatchString(name):
		return nil, invalid("Label")
	case len(in.AWSAccountId) == 0:
		return nil, invalid("AWSAccountId")
	case len(in.ActionName) == 0:
		return nil, invalid("ActionName")
	}

	principals := make([]string, len(in.AWSAccountId))
	for i, id := range in.AWSAccountId {
		if !accountID.MatchString(id) {
			return nil, invalid("AWSAccountId")
		}

		principals[i] = "arn:aws:iam::" + id + ":root"
	}

	actions := make([]string, len(in.ActionName))
	for i, a := range in.ActionName {
		if !slices.Contains(sharedActions, a) {
			return nil, invalid("Policy statement action out of service scope!")
		}

		actions[i] = "SNS:" + a
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		t, loadErr := load(tx, key)
		if loadErr != nil {
			return loadErr
		}

		policy, allowErr := policydoc.Allow(t.policy(key), "__default_policy_ID", name, principals, actions,
			keyARN(key))
		if allowErr != nil {
			return invalid("Statement already exists")
		}

		if len(policy) > maxPolicy {
			return invalid("Policy Error: the policy would be longer than 30720 characters")
		}

		if t.Attributes == nil {
			t.Attributes = map[string]string{}
		}

		t.Attributes[attrPolicy] = policy
		return save(tx, key, t)
	})
	if err != nil {
		return nil, err
	}

	return &api.AddPermissionOutput{}, nil
}

// RemovePermission removes the statement AddPermission added with a label.
func (s *Service) RemovePermission(
	ctx context.Context,
	in *api.RemovePermissionInput,
) (*api.RemovePermissionOutput, error) {
	key, _, err := resolve(ctx, in.TopicArn, false)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		t, loadErr := load(tx, key)
		if loadErr != nil {
			return loadErr
		}

		policy, removeErr := policydoc.Remove(t.policy(key), value(in.Label))
		if removeErr != nil {
			return invalid("Label")
		}

		if t.Attributes == nil {
			t.Attributes = map[string]string{}
		}

		t.Attributes[attrPolicy] = policy
		return save(tx, key, t)
	})
	if err != nil {
		return nil, err
	}

	return &api.RemovePermissionOutput{}, nil
}

// policy returns the topic's access policy: the one set, or the default.
func (t *topic) policy(key string) string {
	if policy, ok := t.Attributes[attrPolicy]; ok {
		return policy
	}

	arn := keyARN(key)
	return defaultPolicy(arn, strings.Split(arn, ":")[4])
}
