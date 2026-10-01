package sqs

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
)

// High-throughput FIFO settings and their defaults.
const (
	scopeQueue        = "queue"
	scopeMessageGroup = "messageGroup"
	limitPerQueue     = "perQueue"
	limitPerGroup     = "perMessageGroupId"
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

	// With the messageGroup scope, a deduplication ID is unique within its
	// message group.
	dedupKey := o.msg.DedupID
	if q.Settings.DeduplicationScope == scopeMessageGroup {
		dedupKey = o.msg.GroupID + "\x00" + o.msg.DedupID
	}

	if d, ok := q.Dedup[dedupKey]; ok {
		o.msg.ID, o.msg.Sequence = d.MessageID, d.Sequence
		return true, nil
	}

	q.Sequence++
	o.msg.Sequence = fmt.Sprintf("%020d", q.Sequence)
	if q.Dedup == nil {
		q.Dedup = map[string]dedup{}
	}

	q.Dedup[dedupKey] = dedup{MessageID: o.msg.ID, Sequence: o.msg.Sequence, Expires: now + dedupWindow}
	return false, nil
}

func (o *outgoing) sequenceNumber() *string {
	if o.msg.Sequence == "" {
		return nil
	}

	return new(o.msg.Sequence)
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

// throughput sets DeduplicationScope or FifoThroughputLimit. Throughput is
// never limited, so the settings only choose the deduplication scope.
func (st *settings) throughput(name, raw string) error {
	allowed := []string{scopeQueue, scopeMessageGroup}
	field := &st.DeduplicationScope
	if name == attrFifoThroughputLimit {
		allowed, field = []string{limitPerQueue, limitPerGroup}, &st.FifoThroughputLimit
	}

	if !slices.Contains(allowed, raw) {
		return awserror.New("InvalidAttributeValue", fmt.Sprintf("Invalid value for the parameter %s: must be "+
			"%s or %s.", name, allowed[0], allowed[1]))
	}

	*field = raw
	return nil
}

// checkThroughput fills in the FIFO throughput defaults and checks that the
// settings fit together: only FIFO queues have them, and a per-group
// throughput limit needs the messageGroup deduplication scope.
func (st *settings) checkThroughput() error {
	switch {
	case !st.FifoQueue && (st.DeduplicationScope != "" || st.FifoThroughputLimit != ""):
		return awserror.New("InvalidAttributeName", "DeduplicationScope and FifoThroughputLimit are only "+
			"supported for FIFO queues.")
	case !st.FifoQueue:
		return nil
	}

	if st.DeduplicationScope == "" {
		st.DeduplicationScope = scopeQueue
	}

	if st.FifoThroughputLimit == "" {
		st.FifoThroughputLimit = limitPerQueue
	}

	if st.FifoThroughputLimit == limitPerGroup && st.DeduplicationScope != scopeMessageGroup {
		return awserror.New("InvalidAttributeValue", "The perMessageGroupId FifoThroughputLimit requires the "+
			"messageGroup DeduplicationScope.")
	}

	return nil
}

// attempt is a FIFO receive that a retry with the same ReceiveRequestAttemptId
// returns again within the deduplication window: the receipt handles it
// returned.
type attempt struct {
	Receipts []string
	Expires  int64
}

// forgetAttempts drops the receive attempts whose window has passed.
func (q *queue) forgetAttempts(now int64) {
	for id, a := range q.Attempts {
		if a.Expires <= now {
			delete(q.Attempts, id)
		}
	}
}

// retry returns the messages a receive attempt returned, with their visibility
// timeouts restarted, if they are all still in flight under the same receipt
// handles.
func (q *queue) retry(id string, now, visibility int64) ([]*message, bool) {
	a, ok := q.Attempts[id]
	if !ok {
		return nil, false
	}

	out := make([]*message, 0, len(a.Receipts))
	for _, receipt := range a.Receipts {
		i := slices.IndexFunc(q.Messages, func(m message) bool { return m.Receipt == receipt })
		if i < 0 || q.Messages[i].VisibleAt <= now {
			return nil, false
		}

		out = append(out, &q.Messages[i])
	}

	for _, msg := range out {
		msg.VisibleAt = now + visibility*1000
	}

	return out, true
}

// remember records a FIFO receive's receipt handles under its attempt ID.
func (q *queue) remember(id string, receipts []string, now int64) {
	if q.Attempts == nil {
		q.Attempts = map[string]attempt{}
	}

	q.Attempts[id] = attempt{Receipts: receipts, Expires: now + dedupWindow}
}
