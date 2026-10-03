package sqs

import (
	"encoding/json"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
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
