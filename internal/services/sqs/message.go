package sqs

import (
	"context"
	"crypto/md5" // #nosec G501 -- SQS defines MD5 message checksums; they are not a security control.
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/sqs"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	maxAttributes = 10
	maxDelay      = 900
	maxVisibility = 43200
	maxWait       = 20
	maxReceive    = 10
)

// message times are Unix milliseconds.
type message struct {
	Attributes      map[string]api.MessageAttributeValue `json:",omitempty"`
	ID              string
	Body            string
	BodyMD5         string
	SenderID        string
	Receipt         string
	SentAt          int64
	FirstReceivedAt int64
	VisibleAt       int64
	ReceiveCount    int64
}

var (
	attributeName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,256}$`)
	number        = regexp.MustCompile(`^[+-]?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?$`)
)

// validText applies the SQS message character set.
func validText(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}

	for _, r := range s {
		if r != '\t' && r != '\n' && r != '\r' &&
			(r < 0x20 || (r > 0xD7FF && r < 0xE000) || r == 0xFFFE || r == 0xFFFF) {
			return false
		}
	}

	return true
}

func invalidContents() error {
	return awserror.New(
		"InvalidMessageContents",
		"Invalid characters found. Valid unicode characters are #x9 | #xA | #xD | #x20 to #xD7FF | "+
			"#xE000 to #xFFFD | #x10000 to #x10FFFF",
	)
}

// validAttributeName rejects reserved prefixes and empty dot-separated segments.
func validAttributeName(name string) bool {
	lower := strings.ToLower(name)
	reserved := strings.HasPrefix(lower, "aws.") || strings.HasPrefix(lower, "amazon.")
	dots := strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") || strings.Contains(name, "..")
	return attributeName.MatchString(name) && !reserved && !dots
}

// checkAttributes validates message attributes and returns their size in bytes.
func checkAttributes(attributes map[string]api.MessageAttributeValue) (int64, error) {
	if len(attributes) > maxAttributes {
		return 0, invalid(
			"Number of message attributes [%d] exceeds the allowed maximum [%d].",
			len(attributes),
			maxAttributes,
		)
	}

	var size int64
	for name, a := range attributes {
		if !validAttributeName(name) {
			return 0, invalid("Message attribute name '%s' is invalid.", name)
		}

		dataType := value(a.DataType)
		base, _, _ := strings.Cut(dataType, ".")
		if len(dataType) > 256 || !validText(dataType) {
			return 0, invalid("The type of message (user) attribute '%s' is invalid.", name)
		}

		switch base {
		case "String", "Number":
			if value(a.StringValue) == "" || a.BinaryValue != nil {
				return 0, invalid("Message attribute '%s' must contain a non-empty string value.", name)
			}

			if base == "Number" && !number.MatchString(*a.StringValue) {
				return 0, invalid("Can't cast the value of message (user) attribute '%s' to a number.", name)
			}

			if !validText(*a.StringValue) {
				return 0, invalidContents()
			}

			size += int64(len(*a.StringValue))
		case "Binary":
			if len(a.BinaryValue) == 0 || a.StringValue != nil {
				return 0, invalid("Message attribute '%s' must contain a non-empty binary value.", name)
			}

			size += int64(len(a.BinaryValue))
		default:
			return 0, invalid(
				"The type of message (user) attribute '%s' is invalid. You must use only the following "+
					"supported type prefixes: Binary, Number, String.",
				name,
			)
		}

		size += int64(len(name) + len(dataType))
	}

	return size, nil
}

func md5Hex(b []byte) string {
	sum := md5.Sum(b) // #nosec G401 -- SQS protocol checksum.
	return hex.EncodeToString(sum[:])
}

// attributesMD5 implements the SQS MD5OfMessageAttributes encoding.
func attributesMD5(attributes map[string]api.MessageAttributeValue) string {
	names := make([]string, 0, len(attributes))
	for name := range attributes {
		names = append(names, name)
	}

	sort.Strings(names)
	h := md5.New() // #nosec G401 -- SQS protocol checksum.
	field := func(h hash.Hash, b []byte) {
		_ = binary.Write(h, binary.BigEndian, uint32(len(b))) // #nosec G115 -- attribute sizes are bounded.
		_, _ = h.Write(b)
	}

	for _, name := range names {
		a := attributes[name]
		field(h, []byte(name))
		field(h, []byte(value(a.DataType)))
		if a.StringValue != nil {
			_, _ = h.Write([]byte{1})
			field(h, []byte(*a.StringValue))
		} else {
			_, _ = h.Write([]byte{2})
			field(h, a.BinaryValue)
		}
	}

	return hex.EncodeToString(h.Sum(nil))
}

func messageID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	s := hex.EncodeToString(b[:])
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
}

func receiptHandle(msg *message) string {
	return base64.RawURLEncoding.EncodeToString([]byte(msg.ID + ":" + msg.Receipt))
}

func parseReceipt(handle *string) (id, nonce string, err error) {
	if handle == nil {
		return "", "", missing("ReceiptHandle")
	}

	b, decodeErr := base64.RawURLEncoding.DecodeString(*handle)
	id, nonce, ok := strings.Cut(string(b), ":")
	if decodeErr != nil || !ok || id == "" || nonce == "" {
		return "", "", awserror.New(
			"ReceiptHandleIsInvalid",
			fmt.Sprintf("The input receipt handle %q is not a valid receipt handle.", *handle),
		)
	}

	return id, nonce, nil
}

// purge drops messages past the queue's retention period.
func (q *queue) purge(now int64) {
	kept := q.Messages[:0]
	for _, msg := range q.Messages {
		if msg.SentAt+q.Settings.MessageRetentionPeriod*1000 > now {
			kept = append(kept, msg)
		}
	}

	q.Messages = kept
}

// outgoing is a validated message that the queue's settings have not yet been
// applied to.
type outgoing struct {
	delay *int64
	msg   message
	size  int64
}

// prepare validates a message independently of its queue.
func (s *Service) prepare(
	ctx context.Context,
	body *string,
	delay *int64,
	attributes map[string]api.MessageAttributeValue,
) (*outgoing, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	if value(body) == "" {
		return nil, missing("MessageBody")
	}

	if !validText(*body) {
		return nil, invalidContents()
	}

	if guardErr := between("DelaySeconds", delay, 0, maxDelay); guardErr != nil {
		return nil, guardErr
	}

	size, err := checkAttributes(attributes)
	if err != nil {
		return nil, err
	}

	sender := m.AccessKey
	if sender == "" {
		sender = m.AccountID
	}

	return &outgoing{
		delay: delay,
		size:  int64(len(*body)) + size,
		msg: message{
			ID:         messageID(),
			Body:       *body,
			BodyMD5:    md5Hex([]byte(*body)),
			SenderID:   sender,
			Attributes: attributes,
		},
	}, nil
}

// enqueue applies the queue's size limit and default delay, then appends the
// message. A per-message delay overrides the queue's default delay.
func (q *queue) enqueue(o *outgoing, now int64) error {
	if limit := q.Settings.MaximumMessageSize; o.size > limit {
		return invalid("One or more parameters are invalid. Reason: Message must be shorter than %d bytes.", limit)
	}

	delay := q.Settings.DelaySeconds
	if o.delay != nil {
		delay = *o.delay
	}

	o.msg.SentAt = now
	o.msg.VisibleAt = now + delay*1000
	q.Messages = append(q.Messages, o.msg)
	return nil
}

func (o *outgoing) md5OfAttributes() *string {
	if len(o.msg.Attributes) == 0 {
		return nil
	}

	return ptr(attributesMD5(o.msg.Attributes))
}

func (s *Service) SendMessage(ctx context.Context, in *api.SendMessageRequest) (*api.SendMessageResult, error) {
	key, err := resolve(ctx, in.QueueUrl)
	if err != nil {
		return nil, err
	}

	o, err := s.prepare(ctx, in.MessageBody, in.DelaySeconds, in.MessageAttributes)
	if err != nil {
		return nil, err
	}

	now := s.now().UnixMilli()
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		q, loadErr := load(tx, key)
		if loadErr != nil {
			return loadErr
		}

		q.purge(now)
		if guardErr := q.enqueue(o, now); guardErr != nil {
			return guardErr
		}

		return save(tx, key, q)
	})
	if err != nil {
		return nil, err
	}

	s.notify(key)
	return &api.SendMessageResult{
		MessageId:              ptr(o.msg.ID),
		MD5OfMessageBody:       ptr(o.msg.BodyMD5),
		MD5OfMessageAttributes: o.md5OfAttributes(),
	}, nil
}

type receiveOptions struct {
	visibility *int64
	system     map[string]bool
	filter     []string
	limit      int
}

// claimed is one atomic receive attempt; next is when the earliest in-flight
// message becomes visible again, or zero.
type claimed struct {
	messages []api.Message
	next     int64
	wait     int64
}

// selected applies MessageAttributeNames: All, .*, exact names, and "prefix.*".
func selected(name string, filter []string) bool {
	for _, f := range filter {
		if f == "All" || f == ".*" || f == name ||
			(strings.HasSuffix(f, ".*") && strings.HasPrefix(name, strings.TrimSuffix(f, "*"))) {
			return true
		}
	}

	return false
}

func render(msg *message, opts *receiveOptions) api.Message {
	out := api.Message{
		MessageId:     ptr(msg.ID),
		ReceiptHandle: ptr(receiptHandle(msg)),
		MD5OfBody:     ptr(msg.BodyMD5),
		Body:          ptr(msg.Body),
	}

	system := map[string]string{
		"SenderId":                         msg.SenderID,
		"SentTimestamp":                    strconv.FormatInt(msg.SentAt, 10),
		"ApproximateReceiveCount":          strconv.FormatInt(msg.ReceiveCount, 10),
		"ApproximateFirstReceiveTimestamp": strconv.FormatInt(msg.FirstReceivedAt, 10),
	}

	for name, v := range system {
		if opts.system["All"] || opts.system[name] {
			if out.Attributes == nil {
				out.Attributes = map[string]string{}
			}

			out.Attributes[name] = v
		}
	}

	for name, a := range msg.Attributes {
		if selected(name, opts.filter) {
			if out.MessageAttributes == nil {
				out.MessageAttributes = map[string]api.MessageAttributeValue{}
			}

			out.MessageAttributes[name] = a
		}
	}

	if len(out.MessageAttributes) > 0 {
		out.MD5OfMessageAttributes = ptr(attributesMD5(out.MessageAttributes))
	}

	return out
}

// claim atomically makes up to opts.limit visible messages in flight.
func (s *Service) claim(ctx context.Context, key string, opts *receiveOptions) (claimed, error) {
	var c claimed
	now := s.now().UnixMilli()
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		q, err := load(tx, key)
		if err != nil {
			return err
		}

		q.purge(now)
		c.wait = q.Settings.ReceiveMessageWaitTimeSeconds
		visibility := q.Settings.VisibilityTimeout
		if opts.visibility != nil {
			visibility = *opts.visibility
		}

		for i := range q.Messages {
			msg := &q.Messages[i]
			if msg.VisibleAt > now {
				if c.next == 0 || msg.VisibleAt < c.next {
					c.next = msg.VisibleAt
				}

				continue
			}

			if len(c.messages) == opts.limit {
				continue
			}

			msg.ReceiveCount++
			if msg.FirstReceivedAt == 0 {
				msg.FirstReceivedAt = now
			}

			msg.VisibleAt = now + visibility*1000
			msg.Receipt = rand.Text()
			c.messages = append(c.messages, render(msg, opts))
		}

		return save(tx, key, q)
	})
	return c, err
}

func between(name string, v *int64, lo, hi int64) error {
	if v != nil && (*v < lo || *v > hi) {
		return invalid("Value %d for parameter %s is invalid. Reason: Must be between %d and %d.", *v, name, lo, hi)
	}

	return nil
}

func (s *Service) ReceiveMessage(
	ctx context.Context,
	in *api.ReceiveMessageRequest,
) (*api.ReceiveMessageResult, error) {
	key, err := resolve(ctx, in.QueueUrl)
	if err != nil {
		return nil, err
	}

	for _, check := range []error{
		between("MaxNumberOfMessages", in.MaxNumberOfMessages, 1, maxReceive),
		between("VisibilityTimeout", in.VisibilityTimeout, 0, maxVisibility),
		between("WaitTimeSeconds", in.WaitTimeSeconds, 0, maxWait),
	} {
		if check != nil {
			return nil, check
		}
	}

	opts := &receiveOptions{
		limit:      int(value(in.MaxNumberOfMessages)),
		visibility: in.VisibilityTimeout,
		system:     map[string]bool{},
		filter:     in.MessageAttributeNames,
	}

	if opts.limit == 0 {
		opts.limit = 1
	}

	for _, name := range append(append([]string(nil), in.AttributeNames...), in.MessageSystemAttributeNames...) {
		opts.system[name] = true
	}

	var deadline time.Time
	for {
		ready := s.watch(key)
		c, claimErr := s.claim(ctx, key, opts)
		if claimErr != nil {
			return nil, claimErr
		}

		now := s.now()
		if deadline.IsZero() {
			wait := c.wait
			if in.WaitTimeSeconds != nil {
				wait = *in.WaitTimeSeconds
			}

			deadline = now.Add(time.Duration(wait) * time.Second)
		}

		if len(c.messages) > 0 || !now.Before(deadline) {
			return &api.ReceiveMessageResult{Messages: c.messages}, nil
		}

		delay := deadline.Sub(now)
		if c.next != 0 {
			delay = min(delay, time.UnixMilli(c.next).Sub(now))
		}

		timer := time.NewTimer(delay)
		select {
		case <-ready:
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return &api.ReceiveMessageResult{}, nil
		case <-s.stop:
			timer.Stop()
			return &api.ReceiveMessageResult{}, nil
		}

		timer.Stop()
	}
}

// remove deletes the message a receipt handle refers to. A stale handle
// matches nothing and reports false.
func (q *queue) remove(id, nonce string) bool {
	for i, msg := range q.Messages {
		if msg.ID == id && msg.Receipt == nonce {
			q.Messages = append(q.Messages[:i], q.Messages[i+1:]...)
			return true
		}
	}

	return false
}

func (s *Service) DeleteMessage(ctx context.Context, in *api.DeleteMessageRequest) (*api.DeleteMessageOutput, error) {
	key, err := resolve(ctx, in.QueueUrl)
	if err != nil {
		return nil, err
	}

	id, nonce, err := parseReceipt(in.ReceiptHandle)
	if err != nil {
		return nil, err
	}
	// A stale receipt handle succeeds without deleting, as in SQS.
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		q, loadErr := load(tx, key)
		if loadErr != nil {
			return loadErr
		}

		if !q.remove(id, nonce) {
			return nil
		}

		return save(tx, key, q)
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteMessageOutput{}, nil
}

// changeVisibility makes an in-flight message visible after timeout seconds.
func (q *queue) changeVisibility(id, nonce string, timeout, now int64) error {
	for i := range q.Messages {
		msg := &q.Messages[i]
		if msg.ID != id || msg.Receipt != nonce {
			continue
		}

		if msg.VisibleAt <= now {
			return awserror.New("MessageNotInflight", "Message is not in flight.")
		}

		msg.VisibleAt = now + timeout*1000
		return nil
	}

	return awserror.New("ReceiptHandleIsInvalid", "The receipt handle does not refer to a received message.")
}

func (s *Service) ChangeMessageVisibility(
	ctx context.Context,
	in *api.ChangeMessageVisibilityRequest,
) (*api.ChangeMessageVisibilityOutput, error) {
	key, err := resolve(ctx, in.QueueUrl)
	if err != nil {
		return nil, err
	}

	id, nonce, err := parseReceipt(in.ReceiptHandle)
	if err != nil {
		return nil, err
	}

	if in.VisibilityTimeout == nil {
		return nil, missing("VisibilityTimeout")
	}

	if guardErr := between("VisibilityTimeout", in.VisibilityTimeout, 0, maxVisibility); guardErr != nil {
		return nil, guardErr
	}

	now := s.now().UnixMilli()
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		q, loadErr := load(tx, key)
		if loadErr != nil {
			return loadErr
		}

		q.purge(now)
		if guardErr := q.changeVisibility(id, nonce, *in.VisibilityTimeout, now); guardErr != nil {
			return guardErr
		}

		return save(tx, key, q)
	})
	if err != nil {
		return nil, err
	}

	s.notify(key)
	return &api.ChangeMessageVisibilityOutput{}, nil
}
