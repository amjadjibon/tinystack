package sqs

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/sqs"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/policydoc"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// attrPolicy is the queue's access policy, stored as sent and never
// evaluated: TinyStack scopes queues to their account instead.
const (
	attrPolicy = "Policy"
	maxPolicy  = 20 * 1024
)

// policy checks a Policy attribute, a JSON object with a Statement; an empty
// one removes the policy.
func (st *settings) policy(v string) error {
	if v == "" {
		st.Policy = ""
		return nil
	}

	var parsed map[string]json.RawMessage
	if len(v) > maxPolicy || json.Unmarshal([]byte(v), &parsed) != nil || parsed["Statement"] == nil {
		return awserror.New("InvalidAttributeValue", "Invalid value for the parameter Policy.")
	}

	st.Policy = v
	return nil
}

// label is a permission label, the statement ID AddPermission writes.
var label = regexp.MustCompile(`^[A-Za-z0-9_-]{1,80}$`)

// sharedActions are the actions AddPermission can share.
var sharedActions = []string{
	"*", "SendMessage", "ReceiveMessage", "DeleteMessage", "ChangeMessageVisibility", "GetQueueAttributes",
	"GetQueueUrl", "ListDeadLetterSourceQueues", "PurgeQueue", "ListQueueTags",
}

// AddPermission adds a statement to the queue's policy that lets accounts
// take actions on the queue.
func (s *Service) AddPermission(ctx context.Context, in *api.AddPermissionRequest) (*api.AddPermissionOutput, error) {
	key, err := resolve(ctx, in.QueueUrl)
	if err != nil {
		return nil, err
	}

	name := common.Value(in.Label)
	switch {
	case !label.MatchString(name):
		return nil, invalid("Value %s for parameter Label is invalid. Reason: Must be 1 to 80 characters from "+
			"[A-Za-z0-9_-].", name)
	case len(in.AWSAccountIds) == 0:
		return nil, missing("AWSAccountId")
	case len(in.Actions) == 0:
		return nil, missing("ActionName")
	}

	principals := make([]string, len(in.AWSAccountIds))
	for i, id := range in.AWSAccountIds {
		if !accountID.MatchString(id) {
			return nil, invalid("Value %s for parameter AWSAccountId is invalid.", id)
		}

		principals[i] = "arn:aws:iam::" + id + ":root"
	}

	actions := make([]string, len(in.Actions))
	for i, a := range in.Actions {
		if !slices.Contains(sharedActions, a) {
			return nil, invalid("Value SQS:%s for parameter ActionName is invalid. Reason: Only the queue owner "+
				"is allowed to invoke this action.", a)
		}

		actions[i] = "SQS:" + a
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		q, loadErr := load(tx, key)
		if loadErr != nil {
			return loadErr
		}

		arn := keyARN(key)
		policy, allowErr := policydoc.Allow(q.Settings.Policy, arn+"/SQSDefaultPolicy", name, principals, actions, arn)
		if errors.Is(allowErr, policydoc.ErrExists) {
			return invalid("Value %s for parameter Label is invalid. Reason: Already exists.", name)
		}

		if len(policy) > maxPolicy {
			return awserror.New("OverLimit", "The queue's policy would be longer than 20480 characters.")
		}

		q.Settings.Policy = policy
		return storage.Write(tx, key, q)
	})
	if err != nil {
		return nil, err
	}

	return &api.AddPermissionOutput{}, nil
}

// RemovePermission removes the statement AddPermission added with a label.
func (s *Service) RemovePermission(
	ctx context.Context,
	in *api.RemovePermissionRequest,
) (*api.RemovePermissionOutput, error) {
	key, err := resolve(ctx, in.QueueUrl)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		q, loadErr := load(tx, key)
		if loadErr != nil {
			return loadErr
		}

		policy, removeErr := policydoc.Remove(q.Settings.Policy, common.Value(in.Label))
		if removeErr != nil {
			return invalid("Value %s for parameter Label is invalid. Reason: can't find label on existing policy.",
				common.Value(in.Label))
		}

		q.Settings.Policy = policy
		return storage.Write(tx, key, q)
	})
	if err != nil {
		return nil, err
	}

	return &api.RemovePermissionOutput{}, nil
}
