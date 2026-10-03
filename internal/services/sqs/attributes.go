package sqs

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/sqs"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const maxReceiveCount = 1000

func invalidRedrive(reason string) error {
	return awserror.New("InvalidAttributeValue", "Invalid value for the parameter RedrivePolicy. Reason: "+reason)
}

// redrive parses a RedrivePolicy. An empty policy removes the dead-letter queue.
func (st *settings) redrive(raw string) error {
	if raw == "" {
		st.DeadLetterTargetArn, st.MaxReceiveCount = "", 0
		return nil
	}

	var policy struct {
		DeadLetterTargetArn string          `json:"deadLetterTargetArn"`
		MaxReceiveCount     json.RawMessage `json:"maxReceiveCount"`
	}

	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&policy); err != nil {
		return invalidRedrive("Redrive policy is not a valid JSON map of deadLetterTargetArn and maxReceiveCount.")
	}

	// SQS accepts maxReceiveCount as a number or a numeric string.
	count, err := strconv.ParseInt(strings.Trim(string(policy.MaxReceiveCount), `"`), 10, 64)
	if err != nil || count < 1 || count > maxReceiveCount {
		return invalidRedrive("maxReceiveCount must be an integer from 1 to 1000.")
	}

	if _, ok := arnKey(policy.DeadLetterTargetArn); !ok {
		return invalidRedrive("deadLetterTargetArn must be an SQS queue ARN.")
	}

	st.DeadLetterTargetArn, st.MaxReceiveCount = policy.DeadLetterTargetArn, count
	return nil
}

// checkTarget requires the dead-letter queue to be another existing queue in
// the same account and region.
func checkTarget(tx storage.ReadTx, key string, st *settings) error {
	if st.DeadLetterTargetArn == "" {
		return nil
	}

	target, _ := arnKey(st.DeadLetterTargetArn)
	scope, _, _ := strings.Cut(key, "/sqs/")
	targetScope, _, _ := strings.Cut(target, "/sqs/")
	if target == key {
		return invalidRedrive("A queue cannot be its own dead-letter queue.")
	}

	if targetScope != scope {
		return invalidRedrive("The dead-letter queue must be in the same account and region.")
	}

	if _, ok := tx.Get(target); !ok {
		return invalidRedrive("Dead letter target does not exist.")
	}

	q, err := load(tx, target)
	if err != nil {
		return err
	}

	if q.Settings.FifoQueue != st.FifoQueue {
		return invalidRedrive("The dead-letter queue must be the same type as the source queue.")
	}

	if !q.Settings.allows(keyARN(key)) {
		return invalidRedrive("The RedriveAllowPolicy of " + st.DeadLetterTargetArn + " does not allow this queue " +
			"to use it as a dead-letter queue.")
	}

	return nil
}

// attributes renders every queue attribute TinyStack supports.
func (q *queue) attributes(key string, now int64) map[string]string {
	var visible, inFlight, delayed int64
	for i := range q.Messages {
		msg := &q.Messages[i]
		switch {
		case msg.VisibleAt <= now:
			visible++
		case msg.ReceiveCount > 0:
			inFlight++
		default:
			delayed++
		}
	}

	st := &q.Settings
	format := func(n int64) string { return strconv.FormatInt(n, 10) }
	out := map[string]string{
		"QueueArn":                              keyARN(key),
		"ApproximateNumberOfMessages":           format(visible),
		"ApproximateNumberOfMessagesNotVisible": format(inFlight),
		"ApproximateNumberOfMessagesDelayed":    format(delayed),
		"CreatedTimestamp":                      format(q.CreatedAt),
		"LastModifiedTimestamp":                 format(q.LastModified),
		"DelaySeconds":                          format(st.DelaySeconds),
		"VisibilityTimeout":                     format(st.VisibilityTimeout),
		"MaximumMessageSize":                    format(st.MaximumMessageSize),
		"MessageRetentionPeriod":                format(st.MessageRetentionPeriod),
		"ReceiveMessageWaitTimeSeconds":         format(st.ReceiveMessageWaitTimeSeconds),
	}

	if st.FifoQueue {
		out[attrFifoQueue] = strconv.FormatBool(st.FifoQueue)
		out[attrContentBasedDeduplication] = strconv.FormatBool(st.ContentBasedDeduplication)
		out[attrDeduplicationScope] = st.DeduplicationScope
		out[attrFifoThroughputLimit] = st.FifoThroughputLimit
	}

	out[attrSqsManagedSseEnabled] = strconv.FormatBool(st.KmsMasterKeyID == "" && !st.SqsManagedSseDisabled)
	if st.KmsMasterKeyID != "" {
		out[attrKmsMasterKeyID] = st.KmsMasterKeyID
		out[attrKmsDataKeyReusePeriod] = format(st.reusePeriod())
	}

	if st.RedriveAllowPolicy != "" {
		out[attrRedriveAllowPolicy] = st.RedriveAllowPolicy
	}

	if st.Policy != "" {
		out[attrPolicy] = st.Policy
	}

	if st.DeadLetterTargetArn != "" {
		policy, _ := json.Marshal(map[string]any{
			"deadLetterTargetArn": st.DeadLetterTargetArn,
			"maxReceiveCount":     st.MaxReceiveCount,
		})
		out[attrRedrivePolicy] = string(policy)
	}

	return out
}

func (s *Service) GetQueueAttributes(
	ctx context.Context,
	in *api.GetQueueAttributesRequest,
) (*api.GetQueueAttributesResult, error) {
	key, err := resolve(ctx, in.QueueUrl)
	if err != nil {
		return nil, err
	}

	var q *queue
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var loadErr error
		q, loadErr = load(tx, key)
		return loadErr
	})
	if err != nil {
		return nil, err
	}

	now := s.now().UnixMilli()
	q.purge(now)
	all := q.attributes(key, now)
	out := &api.GetQueueAttributesResult{}
	for _, name := range in.AttributeNames {
		v, ok := all[name]
		switch {
		case name == "All":
			out.Attributes = all
			return out, nil
		case ok:
			if out.Attributes == nil {
				out.Attributes = map[string]string{}
			}

			out.Attributes[name] = v
		// Known attributes that are unset or do not apply to this queue are omitted.
		case !optionalAttributes[name]:
			return nil, awserror.New("InvalidAttributeName", fmt.Sprintf("Unknown or unsupported attribute %s.", name))
		}
	}

	return out, nil
}

func (s *Service) SetQueueAttributes(
	ctx context.Context,
	in *api.SetQueueAttributesRequest,
) (*api.SetQueueAttributesOutput, error) {
	key, err := resolve(ctx, in.QueueUrl)
	if err != nil {
		return nil, err
	}

	if len(in.Attributes) == 0 {
		return nil, missing("Attribute.1.Name")
	}

	if _, ok := in.Attributes[attrFifoQueue]; ok {
		return nil, awserror.New("InvalidAttributeName", "FifoQueue can only be set when the queue is created.")
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		q, loadErr := load(tx, key)
		if loadErr != nil {
			return loadErr
		}

		st := q.Settings
		if guardErr := st.apply(in.Attributes); guardErr != nil {
			return guardErr
		}

		if guardErr := checkTarget(tx, key, &st); guardErr != nil {
			return guardErr
		}

		q.Settings = st
		q.LastModified = s.now().Unix()
		return save(tx, key, q)
	})
	if err != nil {
		return nil, err
	}
	// New visibility or redrive settings change what a waiting receiver may claim.
	s.notify(key)
	return &api.SetQueueAttributesOutput{}, nil
}
