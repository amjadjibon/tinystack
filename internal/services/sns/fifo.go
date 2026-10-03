package sns

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"regexp"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/sns"
)

const (
	attrFifo         = "FifoTopic"
	attrContentDedup = "ContentBasedDeduplication"
	fifoSuffix       = ".fifo"
	valueTrue        = "true"
	valueFalse       = "false"
	// dedupWindow is how long a FIFO topic remembers a deduplication ID.
	dedupWindow = 5 * time.Minute
)

// fifoID matches message group and deduplication IDs.
var fifoID = regexp.MustCompile(`^[\x21-\x7e]{1,128}$`)

// sequenced is a FIFO topic message: its ID, group, deduplication ID, and
// sequence number. A topic keeps each one for dedupWindow, by deduplication
// ID, to answer a repeated publish with the original.
type sequenced struct {
	ID        string
	Group     string
	Dedup     string
	Number    string
	At        int64 // Unix milliseconds
	Duplicate bool  `json:"-"`
}

func (t *topic) fifo() bool { return t.Attributes[attrFifo] == valueTrue }

// topicAttributes validates a new topic's attributes against its name, and
// returns the attributes to store: a FIFO topic's name ends in .fifo, and
// it has ContentBasedDeduplication, false unless set.
func topicAttributes(name string, attributes map[string]string) (map[string]string, error) {
	for k, v := range attributes {
		if err := checkTopicAttribute(k, v); err != nil {
			return nil, err
		}
	}

	fifo := attributes[attrFifo] == valueTrue
	switch {
	case fifo != strings.HasSuffix(name, fifoSuffix):
		return nil, invalid("Fifo Topic names must end with .fifo and Standard Topic cannot end with .fifo")
	case !fifo && attributes[attrContentDedup] != "":
		return nil, invalid("Content based deduplication can only be set for FIFO topics")
	case !fifo:
		return attributes, nil
	}

	stored := maps.Clone(attributes)
	if stored[attrContentDedup] == "" {
		stored[attrContentDedup] = valueFalse
	}

	return stored, nil
}

// checkSetAttribute checks that SetTopicAttributes may change an attribute
// of t: a topic cannot change its type, and only FIFO topics deduplicate.
func (t *topic) checkSetAttribute(name string) error {
	switch {
	case name == attrFifo:
		return invalid("FifoTopic cannot be changed")
	case name == attrContentDedup && !t.fifo():
		return invalid("Content based deduplication can only be set for FIFO topics")
	}

	return nil
}

// checkSubscription checks that t can deliver to a protocol and endpoint:
// FIFO topics deliver only to SQS queues, and FIFO queues subscribe only to
// FIFO topics.
func (t *topic) checkSubscription(protocol, endpoint string) error {
	switch {
	case t.fifo() && protocol != protocolSQS:
		return invalid("Invalid protocol type: %s", protocol)
	case !t.fifo() && protocol == protocolSQS && strings.HasSuffix(endpoint, fifoSuffix):
		return invalid("Endpoint Reason: FIFO SQS Queues can not be subscribed to standard SNS topics")
	}

	return nil
}

// place places a message published to t, returning nil for a standard
// topic. A FIFO topic's message needs a group and a deduplication ID, given
// or, with content-based deduplication, the message's SHA-256; a message
// whose deduplication ID t has seen within dedupWindow is the duplicate of
// the first, which place returns marked Duplicate.
func (t *topic) place(e *api.PublishBatchRequestEntry, id string, now time.Time) (*sequenced, error) {
	if !t.fifo() {
		if e.MessageGroupId != nil || e.MessageDeduplicationId != nil {
			return nil, invalid("MessageGroupId and MessageDeduplicationId are only supported for FIFO topics")
		}

		return nil, nil //nolint:nilnil // a standard topic's message has no place
	}

	group, dedup := value(e.MessageGroupId), value(e.MessageDeduplicationId)
	switch {
	case !fifoID.MatchString(group):
		return nil, invalid("The MessageGroupId parameter is required for FIFO topics")
	case e.MessageDeduplicationId != nil && !fifoID.MatchString(dedup):
		return nil, invalid("MessageDeduplicationId must be 1 to 128 characters from ASCII 33 to 126")
	case e.MessageDeduplicationId == nil && t.Attributes[attrContentDedup] != valueTrue:
		return nil, invalid("The topic should either have ContentBasedDeduplication enabled or " +
			"MessageDeduplicationId provided explicitly")
	case e.MessageDeduplicationId == nil:
		sum := sha256.Sum256([]byte(value(e.Message)))
		dedup = hex.EncodeToString(sum[:])
	}

	maps.DeleteFunc(t.Dedup, func(_ string, s *sequenced) bool {
		return now.Sub(time.UnixMilli(s.At)) >= dedupWindow
	})
	if seen, ok := t.Dedup[dedup]; ok {
		seen.Duplicate = true
		return seen, nil
	}

	if t.Dedup == nil {
		t.Dedup = map[string]*sequenced{}
	}

	t.Sequence++
	s := &sequenced{ID: id, Group: group, Dedup: dedup, Number: fmt.Sprintf("%020d", t.Sequence), At: now.UnixMilli()}
	t.Dedup[dedup] = s
	return s, nil
}
