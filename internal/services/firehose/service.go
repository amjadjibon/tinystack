// Package firehose implements Amazon Data Firehose delivery streams without
// HTTP dependencies. Records from PutRecord, PutRecordBatch, or a Kinesis
// stream are buffered in the store and delivered as S3 objects, optionally
// transformed by a Lambda function, once a stream's buffering hints are met.
// Resources belong to an account and region:
//
//	<scope>/firehose/stream/<name>
//	<scope>/firehose/buffer/<name>/<zero-padded record number>
//
// The partition-wide aws/firehose/index/<account>/<region>/<name> lists the
// delivery streams the flusher (started by New, stopped by Stop) visits.
// S3 and Lambda are called in-process through a catalog.Caller.
package firehose

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/firehose"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/delivery"
	"github.com/amjadjibon/tinystack/internal/services/kinesis"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	service            = "firehose"
	kindStream         = "stream"
	kindBuffer         = "buffer"
	typeDirectPut      = "DirectPut"
	typeKinesis        = "KinesisStreamAsSource"
	statusActive       = "ACTIVE"
	destinationID      = "destinationId-000000000001"
	defaultSizeMB      = 5
	defaultInterval    = 300
	maxRecord          = 1000 << 10
	maxBatch           = 500
	maxBatchBytes      = 4 << 20
	processorLambda    = "Lambda"
	processorDelimiter = "AppendDelimiterToRecord"
	formatGzip         = "GZIP"
)

var _ api.API = (*Service)(nil)

type Service struct {
	store      storage.Store
	streams    *kinesis.Service
	caller     *catalog.Caller
	deliveries *delivery.Log
	logger     *slog.Logger
	now        func() time.Time
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	stop       sync.Once
}

// New returns a Firehose service whose flusher delivers buffered records and
// reads Kinesis sources from streams in the background.
func New(store storage.Store, streams *kinesis.Service, deliveries *delivery.Log, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{
		store: store, streams: streams, deliveries: deliveries, logger: logger, now: time.Now, ctx: ctx,
		cancel: cancel,
	}
	s.wg.Go(s.flusher)
	return s
}

// UseCaller lets delivery streams write to S3 and invoke Lambda processors.
// Call it before serving requests.
func (s *Service) UseCaller(caller *catalog.Caller) { s.caller = caller }

// Stop ends the flusher and waits for it.
func (s *Service) Stop() {
	s.stop.Do(func() {
		s.cancel()
		s.wg.Wait()
	})
}

// deliveryStream is a delivery stream's record. Its destination is kept as
// an extended S3 destination, whichever kind it was configured as.
type deliveryStream struct {
	Tags        map[string]string
	Source      *api.KinesisStreamSourceConfiguration
	Encryption  *api.DeliveryStreamEncryptionConfiguration
	Destination api.ExtendedS3DestinationConfiguration
	// Cursors are a Kinesis source's last records read, by shard, or "done"
	// for a closed shard read to its end.
	Cursors map[string]string
	Name    string
	Type    string
	// Plain marks a destination configured as S3DestinationConfiguration.
	Plain      bool
	Version    int64
	NextRecord int64
	Created    float64
	Updated    float64
	SourceTime float64 `json:",omitempty"`
}

// buffered is a record waiting for delivery.
type buffered struct {
	ID      string
	Data    []byte
	Arrival float64
}

func identity(ctx context.Context) (request.Metadata, error) {
	m, ok := request.FromContext(ctx)
	if !ok || m.AccountID == "" || m.Region == "" {
		return request.Metadata{}, fmt.Errorf("missing request identity")
	}

	return m, nil
}

func prefix(scope request.Scope, kind string) string {
	return scope.AccountID + "/" + scope.Region + "/" + service + "/" + kind + "/"
}

func streamKey(scope request.Scope, name string) string { return prefix(scope, kindStream) + name }

func bufferPrefix(scope request.Scope, name string) string {
	return prefix(scope, kindBuffer) + name + "/"
}

func indexKey(scope request.Scope, name string) string {
	return "aws/" + service + "/index/" + scope.AccountID + "/" + scope.Region + "/" + name
}

func streamARN(scope request.Scope, name string) string {
	return "arn:aws:firehose:" + scope.Region + ":" + scope.AccountID + ":deliverystream/" + name
}

func value[T any](p *T) (v T) { return common.Value(p) }

func read[T any](tx storage.ReadTx, k string) (v *T, found bool, err error) {
	return storage.Read[T](tx, k)
}

func write(tx storage.WriteTx, k string, v any) error { return storage.Write(tx, k, v) }

func unix(t time.Time) float64 { return common.Unix(t) }

func invalidArgument(format string, args ...any) error {
	return awserror.New("InvalidArgumentException", fmt.Sprintf(format, args...))
}

func notFound(scope request.Scope, name string) error {
	return awserror.New("ResourceNotFoundException",
		fmt.Sprintf("Firehose %s under account %s not found.", name, scope.AccountID))
}

func randomID(n int) string {
	b := make([]byte, n/2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

var (
	streamName = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,64}$`)
	bucketARN  = regexp.MustCompile(`^arn:aws[a-z-]*:s3:::[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	roleARN    = regexp.MustCompile(`^arn:aws[a-z-]*:iam::\d{12}:role/.+$`)
)

// processors are the processor types TinyStack runs.
var processors = []string{processorLambda, processorDelimiter}

// checkDestination validates an S3 destination: its bucket and role,
// buffering, compression, and processing.
func checkDestination(d *api.ExtendedS3DestinationConfiguration) error {
	switch {
	case !bucketARN.MatchString(value(d.BucketARN)):
		return invalidArgument("BucketARN must be an S3 bucket ARN")
	case !roleARN.MatchString(value(d.RoleARN)):
		return invalidArgument("RoleARN must be an IAM role ARN")
	case !slices.Contains([]string{"", "UNCOMPRESSED", formatGzip}, value(d.CompressionFormat)):
		return invalidArgument("TinyStack supports the UNCOMPRESSED and GZIP compression formats")
	case value(d.S3BackupMode) != "" && value(d.S3BackupMode) != "Disabled":
		return invalidArgument("TinyStack does not support S3 backups")
	case value(d.CustomTimeZone) != "" && value(d.CustomTimeZone) != "UTC":
		return invalidArgument("TinyStack supports the UTC time zone only")
	case d.DataFormatConversionConfiguration != nil && value(d.DataFormatConversionConfiguration.Enabled):
		return invalidArgument("TinyStack does not support record format conversion")
	case d.DynamicPartitioningConfiguration != nil && value(d.DynamicPartitioningConfiguration.Enabled):
		return invalidArgument("TinyStack does not support dynamic partitioning")
	}

	if h := d.BufferingHints; h != nil {
		if size := value(h.SizeInMBs); h.SizeInMBs != nil && (size < 1 || size > 128) {
			return invalidArgument("SizeInMBs must be between 1 and 128")
		}

		if interval := value(h.IntervalInSeconds); h.IntervalInSeconds != nil && (interval < 0 || interval > 900) {
			return invalidArgument("IntervalInSeconds must be between 0 and 900")
		}
	}

	if p := d.ProcessingConfiguration; p != nil && value(p.Enabled) {
		for _, proc := range p.Processors {
			if !slices.Contains(processors, value(proc.Type)) {
				return invalidArgument("TinyStack supports the Lambda and AppendDelimiterToRecord processors")
			}

			if value(proc.Type) == processorLambda && parameter(&proc, "LambdaArn") == "" {
				return invalidArgument("A Lambda processor needs a LambdaArn parameter")
			}
		}
	}

	return nil
}

// parameter returns a processor's parameter.
func parameter(p *api.Processor, name string) string {
	for _, param := range p.Parameters {
		if value(param.ParameterName) == name {
			return value(param.ParameterValue)
		}
	}

	return ""
}

// withDefaults fills in a destination's defaults.
func withDefaults(d *api.ExtendedS3DestinationConfiguration) {
	if d.BufferingHints == nil {
		d.BufferingHints = &api.BufferingHints{}
	}

	if d.BufferingHints.SizeInMBs == nil {
		d.BufferingHints.SizeInMBs = new(int64(defaultSizeMB))
	}

	if d.BufferingHints.IntervalInSeconds == nil {
		d.BufferingHints.IntervalInSeconds = new(int64(defaultInterval))
	}

	if d.CompressionFormat == nil {
		d.CompressionFormat = new("UNCOMPRESSED")
	}

	if d.EncryptionConfiguration == nil {
		d.EncryptionConfiguration = &api.EncryptionConfiguration{NoEncryptionConfig: new("NoEncryption")}
	}

	if d.CloudWatchLoggingOptions == nil {
		d.CloudWatchLoggingOptions = &api.CloudWatchLoggingOptions{Enabled: new(false)}
	}

	if d.S3BackupMode == nil {
		d.S3BackupMode = new("Disabled")
	}
}

func tagsOf(tags []api.Tag) map[string]string {
	out := map[string]string{}
	for _, t := range tags {
		out[value(t.Key)] = value(t.Value)
	}

	return out
}

func checkTags(tags map[string]string) error {
	if err := tagging.Check(tags); err != nil {
		return invalidArgument("%s", err.Error())
	}

	return nil
}

func (s *Service) CreateDeliveryStream(
	ctx context.Context,
	in *api.CreateDeliveryStreamInput,
) (*api.CreateDeliveryStreamOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	name := value(in.DeliveryStreamName)
	if !streamName.MatchString(name) {
		return nil, invalidArgument("DeliveryStreamName must be 1 to 64 letters, digits, underscores, periods, " +
			"and hyphens")
	}

	now := unix(s.now())
	st := &deliveryStream{
		Name: name, Type: value(in.DeliveryStreamType), Version: 1, Created: now, Updated: now,
		Tags: tagsOf(in.Tags),
	}
	switch {
	case in.ExtendedS3DestinationConfiguration != nil && in.S3DestinationConfiguration != nil:
		return nil, invalidArgument("Only one destination configuration may be given")
	case in.ExtendedS3DestinationConfiguration != nil:
		st.Destination = *in.ExtendedS3DestinationConfiguration
	case in.S3DestinationConfiguration != nil:
		c := in.S3DestinationConfiguration
		st.Plain, st.Destination = true, api.ExtendedS3DestinationConfiguration{
			RoleARN: c.RoleARN, BucketARN: c.BucketARN, Prefix: c.Prefix, ErrorOutputPrefix: c.ErrorOutputPrefix,
			BufferingHints: c.BufferingHints, CompressionFormat: c.CompressionFormat,
			EncryptionConfiguration: c.EncryptionConfiguration, CloudWatchLoggingOptions: c.CloudWatchLoggingOptions,
		}
	default:
		return nil, invalidArgument("TinyStack supports S3 and extended S3 destinations")
	}

	if checkErr := checkDestination(&st.Destination); checkErr != nil {
		return nil, checkErr
	}

	withDefaults(&st.Destination)
	if checkErr := checkTags(st.Tags); checkErr != nil {
		return nil, checkErr
	}

	switch st.Type {
	case "":
		st.Type = typeDirectPut
	case typeDirectPut:
	case typeKinesis:
		if in.KinesisStreamSourceConfiguration == nil || s.streams == nil {
			return nil, invalidArgument("KinesisStreamAsSource needs a KinesisStreamSourceConfiguration")
		}

		st.Source, st.SourceTime = in.KinesisStreamSourceConfiguration, now
		shards, shardErr := s.streams.Shards(ctx, value(st.Source.KinesisStreamARN))
		if shardErr != nil {
			return nil, invalidArgument("Kinesis stream %s not found", value(st.Source.KinesisStreamARN))
		}

		// Delivery starts with the records the stream gets after now.
		st.Cursors = map[string]string{}
		for _, sh := range shards {
			st.Cursors[sh.ID] = sh.Tip
		}
	default:
		return nil, invalidArgument("TinyStack supports the DirectPut and KinesisStreamAsSource delivery stream types")
	}

	if st.Type == typeDirectPut && in.KinesisStreamSourceConfiguration != nil {
		return nil, invalidArgument("KinesisStreamSourceConfiguration is only for KinesisStreamAsSource")
	}

	if e := in.DeliveryStreamEncryptionConfigurationInput; e != nil {
		if st.Type == typeKinesis {
			return nil, invalidArgument("Encryption is set on the Kinesis stream of a KinesisStreamAsSource stream")
		}

		st.Encryption = &api.DeliveryStreamEncryptionConfiguration{
			KeyType: e.KeyType, KeyARN: e.KeyARN, Status: new("ENABLED"),
		}
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, found := tx.Get(streamKey(m.Scope, name)); found {
			return awserror.New("ResourceInUseException", "Firehose "+name+" under account "+m.AccountID+
				" already exists.")
		}

		tx.Put(indexKey(m.Scope, name), []byte("{}"))
		return write(tx, streamKey(m.Scope, name), st)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateDeliveryStreamOutput{DeliveryStreamARN: new(streamARN(m.Scope, name))}, nil
}

// change runs fn on a delivery stream and stores it.
func (s *Service) change(ctx context.Context, name *string, fn func(st *deliveryStream) error) error {
	m, err := identity(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		st, found, readErr := read[deliveryStream](tx, streamKey(m.Scope, value(name)))
		if readErr != nil || !found {
			if readErr == nil {
				readErr = notFound(m.Scope, value(name))
			}

			return readErr
		}

		if fnErr := fn(st); fnErr != nil {
			return fnErr
		}

		return write(tx, streamKey(m.Scope, st.Name), st)
	})
}

func (s *Service) DeleteDeliveryStream(
	ctx context.Context,
	in *api.DeleteDeliveryStreamInput,
) (*api.DeleteDeliveryStreamOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	name := value(in.DeliveryStreamName)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, found := tx.Get(streamKey(m.Scope, name)); !found {
			return notFound(m.Scope, name)
		}

		for _, k := range tx.Keys(bufferPrefix(m.Scope, name)) {
			tx.Delete(k)
		}

		tx.Delete(streamKey(m.Scope, name))
		tx.Delete(indexKey(m.Scope, name))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteDeliveryStreamOutput{}, nil
}

func describeDestination(st *deliveryStream) api.DestinationDescription {
	d := st.Destination
	out := api.DestinationDescription{
		DestinationId: new(destinationID),
		ExtendedS3DestinationDescription: &api.ExtendedS3DestinationDescription{
			RoleARN: d.RoleARN, BucketARN: d.BucketARN, Prefix: d.Prefix, ErrorOutputPrefix: d.ErrorOutputPrefix,
			BufferingHints: d.BufferingHints, CompressionFormat: d.CompressionFormat,
			EncryptionConfiguration: d.EncryptionConfiguration, CloudWatchLoggingOptions: d.CloudWatchLoggingOptions,
			ProcessingConfiguration: d.ProcessingConfiguration, S3BackupMode: d.S3BackupMode,
			FileExtension: d.FileExtension, CustomTimeZone: d.CustomTimeZone,
			DataFormatConversionConfiguration: d.DataFormatConversionConfiguration,
			DynamicPartitioningConfiguration:  d.DynamicPartitioningConfiguration,
		},
		S3DestinationDescription: &api.S3DestinationDescription{
			RoleARN: d.RoleARN, BucketARN: d.BucketARN, Prefix: d.Prefix, ErrorOutputPrefix: d.ErrorOutputPrefix,
			BufferingHints: d.BufferingHints, CompressionFormat: d.CompressionFormat,
			EncryptionConfiguration: d.EncryptionConfiguration, CloudWatchLoggingOptions: d.CloudWatchLoggingOptions,
		},
	}
	if st.Plain {
		out.ExtendedS3DestinationDescription = nil
	}

	return out
}

func (s *Service) DescribeDeliveryStream(
	ctx context.Context,
	in *api.DescribeDeliveryStreamInput,
) (*api.DescribeDeliveryStreamOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	var st *deliveryStream
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var found bool
		var readErr error
		st, found, readErr = read[deliveryStream](tx, streamKey(m.Scope, value(in.DeliveryStreamName)))
		if readErr == nil && !found {
			readErr = notFound(m.Scope, value(in.DeliveryStreamName))
		}

		return readErr
	})
	if err != nil {
		return nil, err
	}

	d := &api.DeliveryStreamDescription{
		DeliveryStreamName: new(st.Name), DeliveryStreamARN: new(streamARN(m.Scope, st.Name)),
		DeliveryStreamStatus: new(statusActive), DeliveryStreamType: new(st.Type),
		VersionId: new(strconv.FormatInt(st.Version, 10)), CreateTimestamp: new(st.Created),
		LastUpdateTimestamp: new(st.Updated), HasMoreDestinations: new(false),
		Destinations:                          []api.DestinationDescription{describeDestination(st)},
		DeliveryStreamEncryptionConfiguration: st.Encryption,
	}
	if d.DeliveryStreamEncryptionConfiguration == nil {
		d.DeliveryStreamEncryptionConfiguration = &api.DeliveryStreamEncryptionConfiguration{Status: new("DISABLED")}
	}

	if st.Source != nil {
		d.Source = &api.SourceDescription{KinesisStreamSourceDescription: &api.KinesisStreamSourceDescription{
			KinesisStreamARN: st.Source.KinesisStreamARN, RoleARN: st.Source.RoleARN,
			DeliveryStartTimestamp: new(st.SourceTime),
		}}
	}

	return &api.DescribeDeliveryStreamOutput{DeliveryStreamDescription: d}, nil
}

func (s *Service) ListDeliveryStreams(
	ctx context.Context,
	in *api.ListDeliveryStreamsInput,
) (*api.ListDeliveryStreamsOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	limit := 10
	if in.Limit != nil {
		if *in.Limit < 1 || *in.Limit > 10000 {
			return nil, invalidArgument("Limit must be between 1 and 10000")
		}

		limit = int(*in.Limit)
	}

	out := &api.ListDeliveryStreamsOutput{DeliveryStreamNames: []string{}, HasMoreDeliveryStreams: new(false)}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys(prefix(m.Scope, kindStream)) {
			st, _, readErr := read[deliveryStream](tx, k)
			if readErr != nil {
				return readErr
			}

			if st.Name <= value(in.ExclusiveStartDeliveryStreamName) ||
				(in.DeliveryStreamType != nil && st.Type != *in.DeliveryStreamType) {
				continue
			}

			if len(out.DeliveryStreamNames) == limit {
				out.HasMoreDeliveryStreams = new(true)
				break
			}

			out.DeliveryStreamNames = append(out.DeliveryStreamNames, st.Name)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) UpdateDestination(
	ctx context.Context,
	in *api.UpdateDestinationInput,
) (*api.UpdateDestinationOutput, error) {
	err := s.change(ctx, in.DeliveryStreamName, func(st *deliveryStream) error {
		switch {
		case value(in.CurrentDeliveryStreamVersionId) != strconv.FormatInt(st.Version, 10):
			return awserror.New("ConcurrentModificationException",
				"CurrentDeliveryStreamVersionId does not match the delivery stream's version")
		case value(in.DestinationId) != destinationID:
			return invalidArgument("Invalid DestinationId %s", value(in.DestinationId))
		}

		d := st.Destination
		var update api.ExtendedS3DestinationUpdate
		switch {
		case in.ExtendedS3DestinationUpdate != nil:
			update = *in.ExtendedS3DestinationUpdate
		case in.S3DestinationUpdate != nil:
			u := in.S3DestinationUpdate
			update = api.ExtendedS3DestinationUpdate{
				RoleARN:                  u.RoleARN,
				BucketARN:                u.BucketARN,
				Prefix:                   u.Prefix,
				ErrorOutputPrefix:        u.ErrorOutputPrefix,
				BufferingHints:           u.BufferingHints,
				CompressionFormat:        u.CompressionFormat,
				EncryptionConfiguration:  u.EncryptionConfiguration,
				CloudWatchLoggingOptions: u.CloudWatchLoggingOptions,
			}
		default:
			return invalidArgument("TinyStack supports S3 and extended S3 destination updates")
		}

		d.RoleARN, d.BucketARN = orElse(update.RoleARN, d.RoleARN), orElse(update.BucketARN, d.BucketARN)
		d.Prefix, d.ErrorOutputPrefix = orElse(
			update.Prefix,
			d.Prefix,
		), orElse(
			update.ErrorOutputPrefix,
			d.ErrorOutputPrefix,
		)
		d.CompressionFormat = orElse(update.CompressionFormat, d.CompressionFormat)
		d.S3BackupMode, d.FileExtension = orElse(update.S3BackupMode, d.S3BackupMode), orElse(update.FileExtension,
			d.FileExtension)
		d.CustomTimeZone, d.BufferingHints = orElse(
			update.CustomTimeZone,
			d.CustomTimeZone,
		), orElse(
			update.BufferingHints,
			d.BufferingHints,
		)
		d.EncryptionConfiguration = orElse(update.EncryptionConfiguration, d.EncryptionConfiguration)
		d.CloudWatchLoggingOptions = orElse(update.CloudWatchLoggingOptions, d.CloudWatchLoggingOptions)
		d.ProcessingConfiguration = orElse(update.ProcessingConfiguration, d.ProcessingConfiguration)
		d.DataFormatConversionConfiguration = orElse(update.DataFormatConversionConfiguration,
			d.DataFormatConversionConfiguration)
		d.DynamicPartitioningConfiguration = orElse(update.DynamicPartitioningConfiguration,
			d.DynamicPartitioningConfiguration)
		if err := checkDestination(&d); err != nil {
			return err
		}

		withDefaults(&d)
		st.Destination, st.Plain = d, st.Plain && in.ExtendedS3DestinationUpdate == nil
		st.Version++
		st.Updated = unix(s.now())
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateDestinationOutput{}, nil
}

func (s *Service) StartDeliveryStreamEncryption(
	ctx context.Context,
	in *api.StartDeliveryStreamEncryptionInput,
) (*api.StartDeliveryStreamEncryptionOutput, error) {
	err := s.change(ctx, in.DeliveryStreamName, func(st *deliveryStream) error {
		if st.Type == typeKinesis {
			return invalidArgument("Encryption is set on the Kinesis stream of a KinesisStreamAsSource stream")
		}

		e := in.DeliveryStreamEncryptionConfigurationInput
		keyType := "AWS_OWNED_CMK"
		if e != nil && e.KeyType != nil {
			keyType = *e.KeyType
		}

		st.Encryption = &api.DeliveryStreamEncryptionConfiguration{KeyType: new(keyType), Status: new("ENABLED")}
		if e != nil {
			st.Encryption.KeyARN = e.KeyARN
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.StartDeliveryStreamEncryptionOutput{}, nil
}

func (s *Service) StopDeliveryStreamEncryption(
	ctx context.Context,
	in *api.StopDeliveryStreamEncryptionInput,
) (*api.StopDeliveryStreamEncryptionOutput, error) {
	err := s.change(ctx, in.DeliveryStreamName, func(st *deliveryStream) error {
		st.Encryption = nil
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.StopDeliveryStreamEncryptionOutput{}, nil
}

func (s *Service) TagDeliveryStream(
	ctx context.Context,
	in *api.TagDeliveryStreamInput,
) (*api.TagDeliveryStreamOutput, error) {
	err := s.change(ctx, in.DeliveryStreamName, func(st *deliveryStream) error {
		merged := map[string]string{}
		for k, v := range st.Tags {
			merged[k] = v
		}

		for k, v := range tagsOf(in.Tags) {
			merged[k] = v
		}

		st.Tags = merged
		return checkTags(merged)
	})
	if err != nil {
		return nil, err
	}

	return &api.TagDeliveryStreamOutput{}, nil
}

func (s *Service) UntagDeliveryStream(
	ctx context.Context,
	in *api.UntagDeliveryStreamInput,
) (*api.UntagDeliveryStreamOutput, error) {
	err := s.change(ctx, in.DeliveryStreamName, func(st *deliveryStream) error {
		st.Tags = tagging.Remove(st.Tags, in.TagKeys)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagDeliveryStreamOutput{}, nil
}

func (s *Service) ListTagsForDeliveryStream(
	ctx context.Context,
	in *api.ListTagsForDeliveryStreamInput,
) (*api.ListTagsForDeliveryStreamOutput, error) {
	limit := 50
	if in.Limit != nil {
		if *in.Limit < 1 || *in.Limit > 50 {
			return nil, invalidArgument("Limit must be between 1 and 50")
		}

		limit = int(*in.Limit)
	}

	out := &api.ListTagsForDeliveryStreamOutput{Tags: []api.Tag{}, HasMoreTags: new(false)}
	err := s.change(ctx, in.DeliveryStreamName, func(st *deliveryStream) error {
		keys := make([]string, 0, len(st.Tags))
		for k := range st.Tags {
			keys = append(keys, k)
		}

		slices.Sort(keys)
		for _, k := range keys {
			if k <= value(in.ExclusiveStartTagKey) {
				continue
			}

			if len(out.Tags) == limit {
				out.HasMoreTags = new(true)
				break
			}

			out.Tags = append(out.Tags, api.Tag{Key: new(k), Value: new(st.Tags[k])})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// buffer appends records to a delivery stream's buffer, returning their IDs.
func (s *Service) buffer(tx storage.WriteTx, scope request.Scope, st *deliveryStream, records [][]byte) (
	[]string, error,
) {
	now := unix(s.now())
	ids := make([]string, len(records))
	for i, data := range records {
		st.NextRecord++
		ids[i] = randomID(56)
		key := fmt.Sprintf("%s%020d", bufferPrefix(scope, st.Name), st.NextRecord)
		if err := write(tx, key, buffered{Data: data, ID: ids[i], Arrival: now}); err != nil {
			return nil, err
		}
	}

	return ids, write(tx, streamKey(scope, st.Name), st)
}

// put buffers records written to a DirectPut delivery stream.
func (s *Service) put(ctx context.Context, name *string, records [][]byte) (ids []string, encrypted bool, err error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, false, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		st, found, readErr := read[deliveryStream](tx, streamKey(m.Scope, value(name)))
		switch {
		case readErr != nil:
			return readErr
		case !found:
			return notFound(m.Scope, value(name))
		case st.Type != typeDirectPut:
			return invalidArgument("This operation is not permitted on KinesisStreamAsSource delivery stream type.")
		}

		encrypted = st.Encryption != nil
		ids, readErr = s.buffer(tx, m.Scope, st, records)
		return readErr
	})

	return ids, encrypted, err
}

func (s *Service) PutRecord(ctx context.Context, in *api.PutRecordInput) (*api.PutRecordOutput, error) {
	if in.Record == nil || len(in.Record.Data) > maxRecord {
		return nil, invalidArgument("A record must be at most 1000 KiB")
	}

	ids, encrypted, err := s.put(ctx, in.DeliveryStreamName, [][]byte{in.Record.Data})
	if err != nil {
		return nil, err
	}

	return &api.PutRecordOutput{RecordId: new(ids[0]), Encrypted: new(encrypted)}, nil
}

func (s *Service) PutRecordBatch(ctx context.Context, in *api.PutRecordBatchInput) (*api.PutRecordBatchOutput, error) {
	if n := len(in.Records); n < 1 || n > maxBatch {
		return nil, invalidArgument("Records must hold 1 to %d records", maxBatch)
	}

	size := 0
	records := make([][]byte, len(in.Records))
	for i, r := range in.Records {
		if len(r.Data) > maxRecord {
			return nil, invalidArgument("A record must be at most 1000 KiB")
		}

		size += len(r.Data)
		records[i] = r.Data
	}

	if size > maxBatchBytes {
		return nil, invalidArgument("A PutRecordBatch request must be at most 4 MiB")
	}

	ids, encrypted, err := s.put(ctx, in.DeliveryStreamName, records)
	if err != nil {
		return nil, err
	}

	out := &api.PutRecordBatchOutput{FailedPutCount: new(int64(0)), Encrypted: new(encrypted)}
	for _, id := range ids {
		out.RequestResponses = append(out.RequestResponses, api.PutRecordBatchResponseEntry{RecordId: new(id)})
	}

	return out, nil
}

func orElse[T any](p, fallback *T) *T { return common.OrElse(p, fallback) }

// bucketName is the bucket an S3 bucket ARN names.
func bucketName(arn string) string {
	return arn[strings.LastIndex(arn, ":")+1:]
}
