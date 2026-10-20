package firehose

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"uuid"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/delivery"
	"github.com/amjadjibon/tinystack/internal/services/kinesis"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	shardDone     = "done"
	sourceBatch   = 500
	megabyte      = 1 << 20
	indexPrefix   = "aws/" + service + "/index/"
	defaultErrors = "processing-failed"
)

// flushInterval is how often the flusher visits the delivery streams.
var flushInterval = 200 * time.Millisecond

type target struct {
	scope request.Scope
	name  string
}

func (s *Service) visit(ctx context.Context) {
	var targets []target
	_ = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys(indexPrefix) {
			parts := strings.SplitN(strings.TrimPrefix(k, indexPrefix), "/", 3)
			if len(parts) == 3 {
				targets = append(targets, target{request.Scope{AccountID: parts[0], Region: parts[1]}, parts[2]})
			}
		}

		return nil
	})

	for _, t := range targets {
		if ctx.Err() != nil {
			return
		}

		if err := s.pull(ctx, t); err != nil {
			s.logger.Warn("reading a firehose source failed", "stream", t.name, "error", err)
		}

		if err := s.flush(ctx, t); err != nil {
			s.logger.Warn("firehose delivery failed", "stream", t.name, "error", err)
		}
	}
}

// pull buffers a Kinesis source's new records, parents' before children's.
func (s *Service) pull(ctx context.Context, t target) error {
	var st *deliveryStream
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		var readErr error
		st, _, readErr = storage.Read[deliveryStream](tx, streamKey(t.scope, t.name))
		return readErr
	})
	if err != nil || st == nil || st.Type != typeKinesis || s.streams == nil {
		return err
	}

	arn := common.Value(st.Source.KinesisStreamARN)
	shards, err := s.streams.Shards(ctx, arn)
	if err != nil {
		return err
	}

	known := map[string]bool{}
	for _, sh := range shards {
		known[sh.ID] = true
	}

	cursors := map[string]string{}
	var records [][]byte
	for _, sh := range shards {
		cursor := st.Cursors[sh.ID]
		if cursor == shardDone || !parentsDone(&sh, st.Cursors, known) {
			continue
		}

		got, done, readErr := s.streams.Records(ctx, arn, sh.ID, cursor, sourceBatch)
		if readErr != nil {
			return readErr
		}

		for _, r := range got {
			records = append(records, r.Data)
			cursors[sh.ID] = r.SequenceNumber
		}

		if len(got) == 0 && done {
			cursors[sh.ID] = shardDone
		}
	}

	if len(cursors) == 0 {
		return nil
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		current, found, readErr := storage.Read[deliveryStream](tx, streamKey(t.scope, t.name))
		if readErr != nil || !found {
			return readErr
		}

		if current.Cursors == nil {
			current.Cursors = map[string]string{}
		}

		for id, c := range cursors {
			current.Cursors[id] = c
		}

		_, bufferErr := s.buffer(tx, t.scope, current, records)
		return bufferErr
	})
}

func parentsDone(sh *kinesis.Shard, cursors map[string]string, known map[string]bool) bool {
	for _, parent := range []string{sh.Parent, sh.AdjacentParent} {
		if parent != "" && known[parent] && cursors[parent] != shardDone {
			return false
		}
	}

	return true
}

// flush delivers a delivery stream's buffer once it holds its buffering
// size or its oldest record has waited its buffering interval. Records stay
// buffered until their delivery succeeds.
func (s *Service) flush(ctx context.Context, t target) error {
	var st *deliveryStream
	var keys []string
	var records []buffered
	size := 0
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		var readErr error
		if st, _, readErr = storage.Read[deliveryStream](tx, streamKey(t.scope, t.name)); readErr != nil || st == nil {
			return readErr
		}

		limit := int(common.Value(st.Destination.BufferingHints.SizeInMBs)) * megabyte
		for _, k := range tx.Keys(bufferPrefix(t.scope, t.name)) {
			r, _, readErr := storage.Read[buffered](tx, k)
			if readErr != nil {
				return readErr
			}

			if size >= limit {
				break
			}

			keys, records, size = append(keys, k), append(records, *r), size+len(r.Data)
		}

		return nil
	})
	if err != nil || st == nil || len(records) == 0 {
		return err
	}

	interval := time.Duration(common.Value(st.Destination.BufferingHints.IntervalInSeconds)) * time.Second
	oldest := time.UnixMilli(int64(records[0].Arrival * 1000))
	if size < int(common.Value(st.Destination.BufferingHints.SizeInMBs))*megabyte && s.now().Sub(oldest) < interval {
		return nil
	}

	if deliverErr := s.deliver(ctx, t.scope, st, records); deliverErr != nil {
		return deliverErr
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, k := range keys {
			tx.Delete(k)
		}

		return nil
	})
}

// failedRecord is a record a processor failed, as Firehose writes it to the
// error output.
type failedRecord struct {
	ErrorCode              string `json:"errorCode"`
	ErrorMessage           string `json:"errorMessage"`
	LambdaArn              string `json:"lambdaArn,omitempty"`
	RawData                []byte `json:"rawData"`
	AttemptsMade           int    `json:"attemptsMade"`
	ArrivalTimestamp       int64  `json:"arrivalTimestamp"`
	AttemptEndingTimestamp int64  `json:"attemptEndingTimestamp"`
}

// deliver processes records and writes them, and the records processing
// failed, to the destination bucket.
func (s *Service) deliver(ctx context.Context, scope request.Scope, st *deliveryStream, records []buffered) error {
	d := &st.Destination
	var failed []failedRecord
	if p := d.ProcessingConfiguration; p != nil && common.Value(p.Enabled) {
		for i := range p.Processors {
			proc := &p.Processors[i]
			switch common.Value(proc.Type) {
			case processorLambda:
				var lost []failedRecord
				records, lost = s.transform(ctx, scope, st, parameter(proc, "LambdaArn"), records)
				failed = append(failed, lost...)
			case processorDelimiter:
				delimiter := parameter(proc, "Delimiter")
				if delimiter == "" {
					delimiter = "\n"
				}

				for j := range records {
					records[j].Data = append(records[j].Data, []byte(delimiter)...)
				}
			}
		}
	}

	now := s.now().UTC()
	if len(records) > 0 {
		var body bytes.Buffer
		for _, r := range records {
			body.Write(r.Data)
		}

		key := expand(common.Value(d.Prefix), now, "") + objectName(st, now)
		if err := s.putObject(ctx, scope, st, key, body.Bytes()); err != nil {
			return err
		}
	}

	if len(failed) == 0 {
		return nil
	}

	var lines bytes.Buffer
	for _, f := range failed {
		line, _ := json.Marshal(f)
		lines.Write(append(line, '\n'))
	}

	errorPrefix := common.Value(d.ErrorOutputPrefix)
	if errorPrefix == "" {
		errorPrefix = common.Value(d.Prefix) + defaultErrors + "/"
	}

	return s.putObject(ctx, scope, st, expand(errorPrefix, now, defaultErrors)+objectName(st, now), lines.Bytes())
}

// call runs an operation of another service under the destination's role.
func (s *Service) call(
	ctx context.Context,
	scope request.Scope,
	st *deliveryStream,
	svc, op string,
	params map[string]any,
) (map[string]any, error) {
	return s.authorizer.Call(ctx, s.caller, catalog.ServiceDelivery{
		Principal:     "firehose.amazonaws.com",
		SourceARN:     streamARN(scope, st.Name),
		SourceAccount: scope.AccountID,
		Role: &catalog.DeliveryRole{
			ARN: common.Value(st.Destination.RoleARN), Session: st.Name, Service: svc, Operation: op, Params: params,
		},
	})
}

// putObject writes an object, compressed as the destination asks, to the
// destination bucket.
func (s *Service) putObject(
	ctx context.Context,
	scope request.Scope,
	st *deliveryStream,
	key string,
	data []byte,
) error {
	if s.caller == nil {
		return errors.New("S3 is not available")
	}

	d := &st.Destination
	if common.Value(d.CompressionFormat) == formatGzip {
		var compressed bytes.Buffer
		w := gzip.NewWriter(&compressed)
		if _, err := w.Write(data); err != nil {
			return err
		}

		if err := w.Close(); err != nil {
			return err
		}

		data = compressed.Bytes()
	}

	bucket := bucketName(common.Value(d.BucketARN))
	ctx = request.WithMetadata(ctx, request.Metadata{Scope: scope, Service: "s3", RequestID: randomID(32)})
	_, err := s.call(ctx, scope, st, "s3", "PutObject", map[string]any{
		"Bucket": bucket, "Key": key, "Body": string(data),
	})
	if s.deliveries != nil {
		s.deliveries.Add(delivery.ViaFirehose, streamARN(scope, st.Name), "s3://"+bucket+"/"+key, data, err)
	}

	return err
}

// objectName is a delivered object's name: the stream, its version, the
// time, and a random ID, with the destination's file extension.
func objectName(st *deliveryStream, now time.Time) string {
	ext := common.Value(st.Destination.FileExtension)
	if ext == "" && common.Value(st.Destination.CompressionFormat) == formatGzip {
		ext = ".gz"
	}

	return fmt.Sprintf(
		"%s-%d-%s-%s%s",
		st.Name,
		st.Version,
		now.Format("2006-01-02-15-04-05"),
		uuid.NewV4().String(),
		ext,
	)
}

var expression = regexp.MustCompile(`!\{([a-z]+):([^}]*)\}`)

// javaTime maps the Java date and time pattern letters Firehose prefixes use
// to Go's layout.
var javaTime = strings.NewReplacer("yyyy", "2006", "MM", "01", "dd", "02", "HH", "15", "mm", "04", "ss", "05")

// expand evaluates a prefix's !{timestamp:...} and !{firehose:...}
// expressions. A prefix without a timestamp expression gets the default
// yyyy/MM/dd/HH/ time path.
func expand(prefix string, now time.Time, errorType string) string {
	timed := false
	out := expression.ReplaceAllStringFunc(prefix, func(e string) string {
		m := expression.FindStringSubmatch(e)
		switch {
		case m[1] == "timestamp":
			timed = true
			return now.Format(javaTime.Replace(m[2]))
		case m[1] == "firehose" && m[2] == "random-string":
			return randomID(12)
		case m[1] == "firehose" && m[2] == "error-output-type":
			return errorType
		}

		return e
	})
	if !timed {
		out += now.Format("2006/01/02/15/")
	}

	return out
}

// transform runs a Lambda processor on records and returns those it kept,
// with their new data, and those it failed.
func (s *Service) transform(
	ctx context.Context,
	scope request.Scope,
	st *deliveryStream,
	arn string,
	records []buffered,
) (
	kept []buffered, failed []failedRecord,
) {
	type eventRecord struct {
		RecordID                    string `json:"recordId"`
		Data                        []byte `json:"data"`
		ApproximateArrivalTimestamp int64  `json:"approximateArrivalTimestamp"`
	}

	event := map[string]any{
		"invocationId": uuid.NewV4().String(), "deliveryStreamArn": streamARN(scope, st.Name), "region": scope.Region,
	}
	list := make([]eventRecord, len(records))
	for i, r := range records {
		list[i] = eventRecord{RecordID: r.ID, Data: r.Data, ApproximateArrivalTimestamp: int64(r.Arrival * 1000)}
	}

	event["records"] = list
	fail := func(code, message string) ([]buffered, []failedRecord) {
		now := s.now().UnixMilli()
		for _, r := range records {
			failed = append(failed, failedRecord{
				AttemptsMade: 1, ArrivalTimestamp: int64(r.Arrival * 1000), ErrorCode: code, ErrorMessage: message,
				AttemptEndingTimestamp: now, RawData: r.Data, LambdaArn: arn,
			})
		}

		return nil, failed
	}

	ctx = request.WithMetadata(context.WithoutCancel(ctx), request.Metadata{Scope: scope, Service: "lambda"})
	out, err := s.call(ctx, scope, st, "lambda", "Invoke", map[string]any{"FunctionName": arn, "Payload": event})
	if err != nil {
		return fail("Lambda.InvokeAccessDenied", err.Error())
	}

	payload, _ := out["Payload"].(string)
	if out["FunctionError"] != nil {
		return fail("Lambda.FunctionError", payload)
	}

	var response struct {
		Records []struct {
			RecordID string `json:"recordId"`
			Result   string `json:"result"`
			Data     string `json:"data"`
		} `json:"records"`
	}
	if json.Unmarshal([]byte(payload), &response) != nil {
		return fail("Lambda.InvalidResponse", "The Lambda function returned an invalid response")
	}

	results := map[string]int{}
	for i, r := range response.Records {
		results[r.RecordID] = i
	}

	now := s.now().UnixMilli()
	for _, r := range records {
		i, ok := results[r.ID]
		if !ok {
			failed = append(failed, failedRecord{
				AttemptsMade: 1, ArrivalTimestamp: int64(r.Arrival * 1000), ErrorCode: "Lambda.MissingRecordId",
				ErrorMessage: "The function returned no result for the record", AttemptEndingTimestamp: now,
				RawData: r.Data, LambdaArn: arn,
			})
			continue
		}

		result := response.Records[i]
		data, decodeErr := base64.StdEncoding.DecodeString(result.Data)
		switch {
		case result.Result == "Dropped":
		case result.Result == "Ok" && decodeErr == nil:
			kept = append(kept, buffered{Data: data, ID: r.ID, Arrival: r.Arrival})
		default:
			failed = append(failed, failedRecord{
				AttemptsMade: 1, ArrivalTimestamp: int64(r.Arrival * 1000), ErrorCode: "Lambda.ProcessingFailedStatus",
				ErrorMessage: "The function marked the record ProcessingFailed", AttemptEndingTimestamp: now,
				RawData: r.Data, LambdaArn: arn,
			})
		}
	}

	return kept, failed
}
