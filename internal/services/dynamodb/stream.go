package dynamodb

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodb"
	streamsapi "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodbstreams"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	viewKeysOnly    = "KEYS_ONLY"
	viewNewImage    = "NEW_IMAGE"
	viewOldImage    = "OLD_IMAGE"
	viewNewAndOld   = "NEW_AND_OLD_IMAGES"
	eventInsert     = "INSERT"
	eventModify     = "MODIFY"
	eventRemove     = "REMOVE"
	labelFormat     = "2006-01-02T15:04:05.000"
	streamRetention = 24 * time.Hour
	// maxTrim bounds how many expired records one write removes.
	maxTrim = 16
)

// stream is the record of one stream of a table: the table's current stream
// while it is enabled, then a closed stream that stays readable for 24 hours.
// Its records are "<scope>/dynamodb-record/<table>/<label>/<sequence>", with
// sequence numbers taken from the table's counter, so they are contiguous.
type stream struct {
	Table     string
	Label     string
	ViewType  string
	Shard     string
	KeySchema []api.KeySchemaElement
	// Times are Unix milliseconds; Closed is 0 while the stream is enabled.
	Created int64
	Closed  int64
	// Start is the first sequence number, End the last once closed, and Head
	// the first that has not been trimmed.
	Start int64
	End   int64
	Head  int64
}

func (st *stream) expired(now time.Time) bool {
	return st.Closed != 0 && now.Sub(time.UnixMilli(st.Closed)) > streamRetention
}

// tableScope splits a table's storage key into its scope and name.
func tableScope(key string) (scope request.Scope, name string) {
	account, region, name, _ := storage.Scoped(key, "dynamodb")
	return request.Scope{AccountID: account, Region: region}, name
}

func streamPrefix(scope request.Scope, table string) string {
	return scope.AccountID + "/" + scope.Region + "/dynamodb-stream/" + table + "/"
}

func streamKey(scope request.Scope, table, label string) string {
	return streamPrefix(scope, table) + label
}

// streamRecordPrefix is the prefix of every record of a stream.
func streamRecordPrefix(scope request.Scope, table, label string) string {
	return scope.AccountID + "/" + scope.Region + "/dynamodb-record/" + table + "/" + label + "/"
}

func streamRecordKey(scope request.Scope, table, label string, sequence int64) string {
	return streamRecordPrefix(scope, table, label) + formatSequence(sequence)
}

// formatSequence renders a sequence number as DynamoDB does: 21 digits.
func formatSequence(n int64) string { return fmt.Sprintf("%021d", n) }

func streamARN(scope request.Scope, table, label string) string {
	return "arn:aws:dynamodb:" + scope.Region + ":" + scope.AccountID + ":table/" + table + "/stream/" + label
}

// ParseStreamARN splits a stream ARN
// ("arn:aws:dynamodb:<region>:<account>:table/<table>/stream/<label>").
func ParseStreamARN(arn string) (scope request.Scope, table, label string, ok bool) {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[2] != "dynamodb" || parts[3] == "" || parts[4] == "" {
		return request.Scope{}, "", "", false
	}

	resource, ok := strings.CutPrefix(parts[5], "table/")
	if !ok {
		return request.Scope{}, "", "", false
	}

	table, label, ok = strings.Cut(resource, "/stream/")
	if !ok || !tableName.MatchString(table) || label == "" || strings.Contains(label, "/") {
		return request.Scope{}, "", "", false
	}

	return request.Scope{AccountID: parts[4], Region: parts[3]}, table, label, true
}

func loadStream(tx storage.ReadTx, key string) (*stream, bool, error) {
	data, ok := tx.Get(key)
	if !ok {
		return nil, false, nil
	}

	var st stream
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, false, fmt.Errorf("decode stream: %w", err)
	}

	return &st, true, nil
}

func saveStream(tx storage.WriteTx, scope request.Scope, st *stream) error {
	data, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("encode stream: %w", err)
	}

	tx.Put(streamKey(scope, st.Table, st.Label), data)
	return nil
}

// streamViewType validates a StreamSpecification and returns the view type of
// the stream it enables, or "" when it enables none.
func streamViewType(spec *api.StreamSpecification) (string, error) {
	if spec == nil {
		return "", nil
	}

	if spec.StreamEnabled == nil {
		return "", awserror.Validation("StreamSpecification.StreamEnabled is required")
	}

	viewType := value(spec.StreamViewType)
	if !*spec.StreamEnabled {
		if spec.StreamViewType != nil {
			return "", awserror.Validation("StreamViewType can only be set when StreamEnabled is true")
		}

		return "", nil
	}

	switch viewType {
	case viewKeysOnly, viewNewImage, viewOldImage, viewNewAndOld:
		return viewType, nil
	default:
		return "", awserror.Validation("StreamViewType must be KEYS_ONLY, NEW_IMAGE, OLD_IMAGE, or NEW_AND_OLD_IMAGES")
	}
}

// streamEnabled reports whether the table has an enabled stream.
func streamEnabled(t *table) bool {
	spec := t.Description.StreamSpecification
	return spec != nil && value(spec.StreamEnabled)
}

// openStream gives the table stored at key a new enabled stream, first
// removing its closed streams that have expired.
func openStream(tx storage.WriteTx, key string, t *table, viewType string, now time.Time) error {
	scope, name := tableScope(key)
	for _, k := range tx.Keys(streamPrefix(scope, name)) {
		st, _, err := loadStream(tx, k)
		if err != nil {
			return err
		}

		if st.expired(now) {
			for _, record := range tx.Keys(streamRecordPrefix(scope, name, st.Label)) {
				tx.Delete(record)
			}

			tx.Delete(k)
		}
	}

	// Labels are creation times; a stream opened in the same millisecond as
	// another takes the next free one.
	label := now.UTC().Format(labelFormat)
	for {
		if _, exists := tx.Get(streamKey(scope, name, label)); !exists {
			break
		}

		now = now.Add(time.Millisecond)
		label = now.UTC().Format(labelFormat)
	}

	var suffix [4]byte
	_, _ = rand.Read(suffix[:])
	st := &stream{
		KeySchema: t.Description.KeySchema,
		Table:     name,
		Label:     label,
		ViewType:  viewType,
		Shard:     fmt.Sprintf("shardId-%020d-%s", now.UnixMilli(), hex.EncodeToString(suffix[:])),
		Created:   now.UnixMilli(),
		Start:     t.Sequence + 1,
		Head:      t.Sequence + 1,
	}

	t.Description.StreamSpecification = &api.StreamSpecification{StreamEnabled: new(true), StreamViewType: &viewType}
	t.Description.LatestStreamLabel = new(label)
	t.Description.LatestStreamArn = new(streamARN(scope, name, label))
	return saveStream(tx, scope, st)
}

// closeStream disables the stream of the table stored at key, if it has one.
// The stream stays readable for 24 hours.
func closeStream(tx storage.WriteTx, key string, t *table, now time.Time) error {
	if !streamEnabled(t) {
		return nil
	}

	scope, name := tableScope(key)
	st, found, err := loadStream(tx, streamKey(scope, name, value(t.Description.LatestStreamLabel)))
	if err != nil {
		return err
	}

	t.Description.StreamSpecification = nil
	if !found {
		return nil
	}

	st.Closed, st.End = now.UnixMilli(), t.Sequence
	return saveStream(tx, scope, st)
}

// convert copies v into a T with the same JSON form, such as a DynamoDB item
// into a Streams one.
func convert[T any](v any) (T, error) {
	var out T
	data, err := json.Marshal(v)
	if err != nil {
		return out, err
	}

	err = json.Unmarshal(data, &out)
	return out, err
}

// ttlIdentity marks the stream records of items that TTL deleted.
var ttlIdentity = &streamsapi.Identity{Type: new("Service"), PrincipalId: new("dynamodb.amazonaws.com")}

// appendRecord writes the stream record of an item change (old to it, either
// nil) when the table has an enabled stream and the change alters the item.
// by is the identity of a service that made the change, if any. It then
// trims a few records past the retention period.
func appendRecord(tx storage.WriteTx, key string, t *table, old, it item, by *streamsapi.Identity) error {
	if !streamEnabled(t) || reflect.DeepEqual(old, it) {
		return nil
	}

	scope, name := tableScope(key)
	label := value(t.Description.LatestStreamLabel)
	now := time.Now()
	record, err := newRecord(scope, t, old, it, now)
	if err != nil {
		return err
	}

	t.Sequence++
	record.UserIdentity = by
	record.Dynamodb.SequenceNumber = new(formatSequence(t.Sequence))
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode stream record: %w", err)
	}

	tx.Put(streamRecordKey(scope, name, label, t.Sequence), data)
	st, found, err := loadStream(tx, streamKey(scope, name, label))
	if err != nil || !found {
		return err
	}

	head := st.Head
	for i := 0; i < maxTrim && st.Head < t.Sequence; i++ {
		k := streamRecordKey(scope, name, label, st.Head)
		if created, ok := recordTime(tx, k); ok && now.Sub(created) <= streamRetention {
			break
		}

		tx.Delete(k)
		st.Head++
	}

	if st.Head == head {
		return nil
	}

	return saveStream(tx, scope, st)
}

// newRecord builds the stream record, without its sequence number, of the
// change from old to it in the table's stream view.
func newRecord(scope request.Scope, t *table, old, it item, now time.Time) (*streamsapi.Record, error) {
	event, current := eventModify, it
	switch {
	case old == nil:
		event = eventInsert
	case it == nil:
		event, current = eventRemove, old
	}

	keys := item{}
	for _, k := range t.Description.KeySchema {
		keys[value(k.AttributeName)] = current[value(k.AttributeName)]
	}

	viewType := value(t.Description.StreamSpecification.StreamViewType)
	images := []item{keys}
	var newImage, oldImage item
	if it != nil && (viewType == viewNewImage || viewType == viewNewAndOld) {
		newImage = it
		images = append(images, it)
	}

	if old != nil && (viewType == viewOldImage || viewType == viewNewAndOld) {
		oldImage = old
		images = append(images, old)
	}

	var size int64
	for _, image := range images {
		n, err := itemSize(image)
		if err != nil {
			return nil, err
		}

		size += n
	}

	var id [16]byte
	_, _ = rand.Read(id[:])
	body := &streamsapi.StreamRecord{
		ApproximateCreationDateTime: new(float64(now.Unix())),
		SizeBytes:                   new(size),
		StreamViewType:              new(viewType),
	}

	var err error
	if body.Keys, err = convert[map[string]streamsapi.AttributeValue](keys); err != nil {
		return nil, err
	}

	if newImage != nil {
		if body.NewImage, err = convert[map[string]streamsapi.AttributeValue](newImage); err != nil {
			return nil, err
		}
	}

	if oldImage != nil {
		if body.OldImage, err = convert[map[string]streamsapi.AttributeValue](oldImage); err != nil {
			return nil, err
		}
	}

	return &streamsapi.Record{
		EventID:      new(hex.EncodeToString(id[:])),
		EventName:    new(event),
		EventVersion: new("1.1"),
		EventSource:  new("aws:dynamodb"),
		AwsRegion:    new(scope.Region),
		Dynamodb:     body,
	}, nil
}

// recordTime returns the creation time of the stream record stored at key.
func recordTime(tx storage.ReadTx, key string) (time.Time, bool) {
	record, ok := readRecord(tx, key)
	if !ok || record.Dynamodb == nil {
		return time.Time{}, false
	}

	return time.Unix(int64(value(record.Dynamodb.ApproximateCreationDateTime)), 0), true
}

func readRecord(tx storage.ReadTx, key string) (*streamsapi.Record, bool) {
	data, ok := tx.Get(key)
	if !ok {
		return nil, false
	}

	var record streamsapi.Record
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, false
	}

	return &record, true
}
