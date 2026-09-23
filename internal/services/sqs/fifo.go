package sqs

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
)

// dedupWindow is the FIFO deduplication interval in milliseconds.
const dedupWindow = 5 * 60 * 1000

// fifoID matches message group and deduplication IDs: 1 to 128 printable,
// non-space ASCII characters.
var fifoID = regexp.MustCompile(`^[\x21-\x7E]{1,128}$`)

// dedup is a FIFO send remembered for the deduplication window.
type dedup struct {
	MessageID string
	Sequence  string
	Expires   int64
}

// sequence applies the FIFO rules to o and assigns its sequence number. It
// reports a duplicate send inside the deduplication window, in which case o
// takes the original message's ID and sequence number and is not enqueued.
func (q *queue) sequence(o *outgoing, now int64) (bool, error) {
	if o.msg.GroupID == "" {
		return false, missing("MessageGroupId")
	}

	if o.delay != nil {
		return false, invalid("DelaySeconds is not supported per message on FIFO queues; set it on the queue.")
	}

	if o.msg.DedupID == "" {
		if !q.Settings.ContentBasedDeduplication {
			return false, invalid(
				"The queue should either have ContentBasedDeduplication enabled or MessageDeduplicationId " +
					"provided explicitly",
			)
		}

		sum := sha256.Sum256([]byte(o.msg.Body))
		o.msg.DedupID = hex.EncodeToString(sum[:])
	}

	if d, ok := q.Dedup[o.msg.DedupID]; ok {
		o.msg.ID, o.msg.Sequence = d.MessageID, d.Sequence
		return true, nil
	}

	q.Sequence++
	o.msg.Sequence = fmt.Sprintf("%020d", q.Sequence)
	if q.Dedup == nil {
		q.Dedup = map[string]dedup{}
	}

	q.Dedup[o.msg.DedupID] = dedup{MessageID: o.msg.ID, Sequence: o.msg.Sequence, Expires: now + dedupWindow}
	return false, nil
}

func (o *outgoing) sequenceNumber() *string {
	if o.msg.Sequence == "" {
		return nil
	}

	return ptr(o.msg.Sequence)
}

// inFlightGroups returns the FIFO message groups that have a message in flight.
// No other message from such a group may be received until it is deleted or
// becomes visible again.
func (q *queue) inFlightGroups(now int64) map[string]bool {
	groups := map[string]bool{}
	for i := range q.Messages {
		msg := &q.Messages[i]
		if msg.GroupID != "" && msg.ReceiveCount > 0 && msg.VisibleAt > now {
			groups[msg.GroupID] = true
		}
	}

	return groups
}
