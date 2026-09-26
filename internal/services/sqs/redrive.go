package sqs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/sqs"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	attrRedriveAllowPolicy = "RedriveAllowPolicy"
	permissionAllowAll     = "allowAll"
	permissionDenyAll      = "denyAll"
	permissionByQueue      = "byQueue"
	maxAllowedSources      = 10
)

// allowPolicy is a RedriveAllowPolicy: which source queues may use a queue as
// their dead-letter queue.
type allowPolicy struct {
	RedrivePermission string   `json:"redrivePermission"`
	SourceQueueArns   []string `json:"sourceQueueArns,omitempty"`
}

func invalidAllowPolicy(reason string) error {
	return awserror.New("InvalidAttributeValue",
		"Invalid value for the parameter RedriveAllowPolicy. Reason: "+reason)
}

// allowRedrive parses a RedriveAllowPolicy and stores it in a canonical form,
// so settings stay comparable; an empty policy allows every source.
func (st *settings) allowRedrive(raw string) error {
	if raw == "" {
		st.RedriveAllowPolicy = ""
		return nil
	}

	var p allowPolicy
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return invalidAllowPolicy("Redrive allow policy is not a valid JSON map of redrivePermission and " +
			"sourceQueueArns.")
	}

	switch p.RedrivePermission {
	case permissionAllowAll, permissionDenyAll:
		if len(p.SourceQueueArns) > 0 {
			return invalidAllowPolicy("sourceQueueArns can only be given with the byQueue redrivePermission.")
		}
	case permissionByQueue:
		if len(p.SourceQueueArns) == 0 || len(p.SourceQueueArns) > maxAllowedSources {
			return invalidAllowPolicy("byQueue needs 1 to 10 sourceQueueArns.")
		}

		for _, arn := range p.SourceQueueArns {
			if _, ok := arnKey(arn); !ok {
				return invalidAllowPolicy(arn + " is not an SQS queue ARN.")
			}
		}
	default:
		return invalidAllowPolicy("redrivePermission must be allowAll, denyAll, or byQueue.")
	}

	data, err := json.Marshal(p)
	if err != nil {
		return err
	}

	st.RedriveAllowPolicy = string(data)
	return nil
}

// allows reports whether the queue's RedriveAllowPolicy lets the queue with
// ARN source use it as a dead-letter queue.
func (st *settings) allows(source string) bool {
	if st.RedriveAllowPolicy == "" {
		return true
	}

	var p allowPolicy
	_ = json.Unmarshal([]byte(st.RedriveAllowPolicy), &p)
	switch p.RedrivePermission {
	case permissionAllowAll:
		return true
	case permissionByQueue:
		return slices.Contains(p.SourceQueueArns, source)
	default:
		return false
	}
}

// deadLetterSources returns the names of the queues in the scope of key whose
// redrive policy targets the queue stored at key, in name order.
func deadLetterSources(tx storage.ReadTx, key string) ([]string, error) {
	scope, _, _ := strings.Cut(key, "/sqs/")
	prefix := scope + "/sqs/"
	arn := keyARN(key)
	var names []string
	for _, k := range tx.Keys(prefix) {
		q, err := load(tx, k)
		if err != nil {
			return nil, err
		}

		if q.Settings.DeadLetterTargetArn == arn {
			names = append(names, strings.TrimPrefix(k, prefix))
		}
	}

	return names, nil
}

// ListDeadLetterSourceQueues lists the queues whose redrive policy names the
// queue as their dead-letter queue.
func (s *Service) ListDeadLetterSourceQueues(
	ctx context.Context,
	in *api.ListDeadLetterSourceQueuesRequest,
) (*api.ListDeadLetterSourceQueuesResult, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	key, err := resolve(ctx, in.QueueUrl)
	if err != nil {
		return nil, err
	}

	limit := int64(listLimit)
	if in.MaxResults != nil {
		if limit = *in.MaxResults; limit < 1 || limit > listLimit {
			return nil, invalid("Value for parameter MaxResults is invalid. Reason: Must be between 1 and 1000.")
		}
	}

	after := ""
	if in.NextToken != nil {
		b, decodeErr := base64.RawURLEncoding.DecodeString(*in.NextToken)
		if decodeErr != nil || len(b) == 0 {
			return nil, invalid("Invalid NextToken value.")
		}

		after = string(b)
	}

	var names []string
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if _, loadErr := load(tx, key); loadErr != nil {
			return loadErr
		}

		var sourcesErr error
		names, sourcesErr = deadLetterSources(tx, key)
		return sourcesErr
	})
	if err != nil {
		return nil, err
	}

	names = slices.DeleteFunc(names, func(name string) bool { return name <= after })
	out := &api.ListDeadLetterSourceQueuesResult{QueueUrls: []string{}}
	if int64(len(names)) > limit {
		names = names[:limit]
		if in.MaxResults != nil {
			out.NextToken = new(base64.RawURLEncoding.EncodeToString([]byte(names[len(names)-1])))
		}
	}

	for _, name := range names {
		out.QueueUrls = append(out.QueueUrls, queueURL(m, name))
	}

	return out, nil
}
