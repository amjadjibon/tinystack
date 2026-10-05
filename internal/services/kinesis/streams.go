package kinesis

import (
	"context"
	"math/big"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/kinesis"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) CreateStream(ctx context.Context, in *api.CreateStreamInput) (*api.CreateStreamOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	name := common.Value(in.StreamName)
	if !streamName.MatchString(name) {
		return nil, invalidArgument("StreamName must be 1 to 128 letters, digits, underscores, periods, and hyphens")
	}

	mode := modeProvisioned
	if in.StreamModeDetails != nil {
		mode = common.Value(in.StreamModeDetails.StreamMode)
	}

	count := int(common.Value(in.ShardCount))
	switch {
	case mode != modeProvisioned && mode != modeOnDemand:
		return nil, invalidArgument("StreamMode must be PROVISIONED or ON_DEMAND")
	case mode == modeOnDemand && in.ShardCount != nil:
		return nil, invalidArgument("ShardCount is not used by ON_DEMAND streams")
	case mode == modeOnDemand:
		count = onDemandShards
	case in.ShardCount == nil:
		count = 1
	case count < 1 || count > maxShards:
		return nil, invalidArgument("ShardCount must be between 1 and %d", maxShards)
	}

	if checkErr := checkTags(in.Tags); checkErr != nil {
		return nil, checkErr
	}

	st := &stream{
		Name: name, Mode: mode, Retention: defaultRetention, Created: common.Unix(s.now()), Encryption: "NONE",
		Tags: in.Tags, Metrics: []string{},
	}
	for _, r := range ranges(big.NewInt(0), maxHash, count) {
		st.openShard(r[0], r[1], "", "")
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, found := tx.Get(streamKey(m.Scope, name)); found {
			return awserror.New("ResourceInUseException",
				"Stream "+name+" under account "+m.AccountID+" already exists.")
		}

		return storage.Write(tx, streamKey(m.Scope, name), st)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateStreamOutput{}, nil
}

func (s *Service) DeleteStream(ctx context.Context, in *api.DeleteStreamInput) (*api.DeleteStreamOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	name, err := streamTarget(&m, in.StreamName, in.StreamARN)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		st, loadErr := loadStream(tx, m.Scope, name)
		if loadErr != nil {
			return loadErr
		}

		if len(st.Consumers) > 0 && !common.Value(in.EnforceConsumerDeletion) {
			return awserror.New("ResourceInUseException", "Stream "+name+" has registered consumers; "+
				"set EnforceConsumerDeletion to delete it with them.")
		}

		for _, k := range tx.Keys(prefix(m.Scope, kindRecord) + name + "/") {
			tx.Delete(k)
		}

		tx.Delete(streamKey(m.Scope, name))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteStreamOutput{}, nil
}

// describeShard renders a shard as the API reports it.
func describeShard(sh *shard) api.Shard {
	out := api.Shard{
		ShardId:      new(sh.ID),
		HashKeyRange: &api.HashKeyRange{StartingHashKey: new(sh.StartHash), EndingHashKey: new(sh.EndHash)},
		SequenceNumberRange: &api.SequenceNumberRange{
			StartingSequenceNumber: new(sequence(sh.Index, 0)),
		},
	}
	if sh.Closed {
		out.SequenceNumberRange.EndingSequenceNumber = new(sh.End)
	}

	if sh.Parent != "" {
		out.ParentShardId = new(sh.Parent)
	}

	if sh.AdjacentParent != "" {
		out.AdjacentParentShardId = new(sh.AdjacentParent)
	}

	return out
}

func monitoring(st *stream) []api.EnhancedMetrics {
	return []api.EnhancedMetrics{{ShardLevelMetrics: append([]string{}, st.Metrics...)}}
}

func (s *Service) DescribeStream(ctx context.Context, in *api.DescribeStreamInput) (*api.DescribeStreamOutput, error) {
	limit := 100
	if in.Limit != nil {
		if *in.Limit < 1 || *in.Limit > 10000 {
			return nil, invalidArgument("Limit must be between 1 and 10000")
		}

		limit = int(*in.Limit)
	}

	var out *api.StreamDescription
	err := s.view(ctx, in.StreamName, in.StreamARN, func(_ storage.ReadTx, scope request.Scope, st *stream) error {
		out = &api.StreamDescription{
			StreamName: new(st.Name), StreamARN: new(StreamARN(scope, st.Name)), StreamStatus: new(statusActive),
			StreamModeDetails:    &api.StreamModeDetails{StreamMode: new(st.Mode)},
			RetentionPeriodHours: new(st.Retention), StreamCreationTimestamp: new(st.Created),
			EncryptionType: new(st.Encryption), EnhancedMonitoring: monitoring(st), Shards: []api.Shard{},
			HasMoreShards: new(false),
		}
		if st.KeyID != "" {
			out.KeyId = new(st.KeyID)
		}

		for i := range st.Shards {
			sh := &st.Shards[i]
			if in.ExclusiveStartShardId != nil && sh.ID <= *in.ExclusiveStartShardId {
				continue
			}

			if len(out.Shards) == limit {
				out.HasMoreShards = new(true)
				break
			}

			out.Shards = append(out.Shards, describeShard(sh))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DescribeStreamOutput{StreamDescription: out}, nil
}

func (s *Service) DescribeStreamSummary(
	ctx context.Context,
	in *api.DescribeStreamSummaryInput,
) (*api.DescribeStreamSummaryOutput, error) {
	var out *api.StreamDescriptionSummary
	err := s.view(ctx, in.StreamName, in.StreamARN, func(_ storage.ReadTx, scope request.Scope, st *stream) error {
		out = &api.StreamDescriptionSummary{
			StreamName: new(st.Name), StreamARN: new(StreamARN(scope, st.Name)), StreamStatus: new(statusActive),
			StreamModeDetails:    &api.StreamModeDetails{StreamMode: new(st.Mode)},
			RetentionPeriodHours: new(st.Retention), StreamCreationTimestamp: new(st.Created),
			EncryptionType: new(st.Encryption), EnhancedMonitoring: monitoring(st),
			OpenShardCount: new(int64(len(st.open()))), ConsumerCount: new(int64(len(st.Consumers))),
		}
		if st.KeyID != "" {
			out.KeyId = new(st.KeyID)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DescribeStreamSummaryOutput{StreamDescriptionSummary: out}, nil
}

func (s *Service) ListStreams(ctx context.Context, in *api.ListStreamsInput) (*api.ListStreamsOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	limit := 100
	if in.Limit != nil {
		if *in.Limit < 1 || *in.Limit > 10000 {
			return nil, invalidArgument("Limit must be between 1 and 10000")
		}

		limit = int(*in.Limit)
	}

	after := common.Value(in.ExclusiveStartStreamName)
	if in.NextToken != nil {
		after = *in.NextToken
	}

	out := &api.ListStreamsOutput{
		StreamNames:     []string{},
		StreamSummaries: []api.StreamSummary{},
		HasMoreStreams:  new(false),
	}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys(prefix(m.Scope, kindStream)) {
			st, _, readErr := storage.Read[stream](tx, k)
			if readErr != nil {
				return readErr
			}

			if st.Name <= after {
				continue
			}

			if len(out.StreamNames) == limit {
				out.HasMoreStreams, out.NextToken = new(true), new(out.StreamNames[limit-1])
				break
			}

			out.StreamNames = append(out.StreamNames, st.Name)
			out.StreamSummaries = append(out.StreamSummaries, api.StreamSummary{
				StreamName:   new(st.Name),
				StreamARN:    new(StreamARN(m.Scope, st.Name)),
				StreamStatus: new(statusActive),
				StreamModeDetails: &api.StreamModeDetails{
					StreamMode: new(st.Mode),
				},
				StreamCreationTimestamp: new(st.Created),
			})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// ListShards lists a stream's shards, or those a shard filter selects:
// AFTER_SHARD_ID, AT_LATEST (open shards), and AT_TRIM_HORIZON or
// FROM_TRIM_HORIZON (all shards).
func (s *Service) ListShards(ctx context.Context, in *api.ListShardsInput) (*api.ListShardsOutput, error) {
	limit := 1000
	if in.MaxResults != nil {
		if *in.MaxResults < 1 || *in.MaxResults > 10000 {
			return nil, invalidArgument("MaxResults must be between 1 and 10000")
		}

		limit = int(*in.MaxResults)
	}

	after := common.Value(in.ExclusiveStartShardId)
	openOnly := false
	if f := in.ShardFilter; f != nil {
		switch common.Value(f.Type) {
		case "AFTER_SHARD_ID":
			after = common.Value(f.ShardId)
		case "AT_LATEST":
			openOnly = true
		case "AT_TRIM_HORIZON", "FROM_TRIM_HORIZON":
		default:
			return nil, invalidArgument("TinyStack supports the AFTER_SHARD_ID, AT_LATEST, AT_TRIM_HORIZON, and " +
				"FROM_TRIM_HORIZON shard filters")
		}
	}

	name, arn := in.StreamName, in.StreamARN
	if in.NextToken != nil {
		streamPart, shardPart, ok := strings.Cut(*in.NextToken, "|")
		if !ok {
			return nil, invalidArgument("Invalid NextToken")
		}

		name, arn, after = &streamPart, nil, shardPart
	}

	out := &api.ListShardsOutput{Shards: []api.Shard{}}
	err := s.view(ctx, name, arn, func(_ storage.ReadTx, _ request.Scope, st *stream) error {
		for i := range st.Shards {
			sh := &st.Shards[i]
			if sh.ID <= after || (openOnly && sh.Closed) {
				continue
			}

			if len(out.Shards) == limit {
				out.NextToken = new(st.Name + "|" + *out.Shards[limit-1].ShardId)
				break
			}

			out.Shards = append(out.Shards, describeShard(sh))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) IncreaseStreamRetentionPeriod(
	ctx context.Context,
	in *api.IncreaseStreamRetentionPeriodInput,
) (*api.IncreaseStreamRetentionPeriodOutput, error) {
	err := s.change(ctx, in.StreamName, in.StreamARN, func(_ storage.WriteTx, _ request.Scope, st *stream) error {
		hours := common.Value(in.RetentionPeriodHours)
		if hours < st.Retention || hours > maxRetention {
			return invalidArgument("RetentionPeriodHours must be between the current %d and %d", st.Retention,
				maxRetention)
		}

		st.Retention = hours
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.IncreaseStreamRetentionPeriodOutput{}, nil
}

func (s *Service) DecreaseStreamRetentionPeriod(
	ctx context.Context,
	in *api.DecreaseStreamRetentionPeriodInput,
) (*api.DecreaseStreamRetentionPeriodOutput, error) {
	err := s.change(ctx, in.StreamName, in.StreamARN, func(_ storage.WriteTx, _ request.Scope, st *stream) error {
		hours := common.Value(in.RetentionPeriodHours)
		if hours > st.Retention || hours < defaultRetention {
			return invalidArgument("RetentionPeriodHours must be between %d and the current %d", defaultRetention,
				st.Retention)
		}

		st.Retention = hours
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DecreaseStreamRetentionPeriodOutput{}, nil
}

// reshard closes every open shard and opens count shards over the whole
// hash key space, each the child of the old shards it overlaps.
func reshard(st *stream, count int) {
	old := st.open()
	for _, sh := range old {
		sh.close()
	}

	for _, r := range ranges(big.NewInt(0), maxHash, count) {
		var parents []string
		for _, sh := range old {
			if r[0].Cmp(hashOf(sh.EndHash)) <= 0 && r[1].Cmp(hashOf(sh.StartHash)) >= 0 {
				parents = append(parents, sh.ID)
			}
		}

		parent, adjacent := "", ""
		if len(parents) > 0 {
			parent = parents[0]
		}

		if len(parents) > 1 {
			adjacent = parents[1]
		}

		st.openShard(r[0], r[1], parent, adjacent)
	}
}

func (s *Service) UpdateShardCount(
	ctx context.Context,
	in *api.UpdateShardCountInput,
) (*api.UpdateShardCountOutput, error) {
	out := &api.UpdateShardCountOutput{TargetShardCount: in.TargetShardCount}
	err := s.change(ctx, in.StreamName, in.StreamARN, func(_ storage.WriteTx, scope request.Scope, st *stream) error {
		target, current := int(common.Value(in.TargetShardCount)), len(st.open())
		switch {
		case common.Value(in.ScalingType) != "UNIFORM_SCALING":
			return invalidArgument("ScalingType must be UNIFORM_SCALING")
		case st.Mode == modeOnDemand:
			return invalidArgument("UpdateShardCount is not supported for ON_DEMAND streams")
		case target < 1 || target > maxShards || target > 2*current || 2*target < current:
			return invalidArgument("TargetShardCount must be between half and double the current %d, up to %d",
				current, maxShards)
		}

		out.CurrentShardCount = new(int64(current))
		out.StreamName, out.StreamARN = new(st.Name), new(StreamARN(scope, st.Name))
		if target != current {
			reshard(st, target)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func openShard(st *stream, id *string) (*shard, error) {
	sh := st.shard(common.Value(id))
	switch {
	case sh == nil:
		return nil, awserror.New("ResourceNotFoundException", "Could not find shard "+common.Value(id))
	case sh.Closed:
		return nil, awserror.New("ResourceInUseException", "Shard "+sh.ID+" is closed")
	}

	return sh, nil
}

func (s *Service) SplitShard(ctx context.Context, in *api.SplitShardInput) (*api.SplitShardOutput, error) {
	err := s.change(ctx, in.StreamName, in.StreamARN, func(_ storage.WriteTx, _ request.Scope, st *stream) error {
		sh, err := openShard(st, in.ShardToSplit)
		if err != nil {
			return err
		}

		at, ok := new(big.Int).SetString(common.Value(in.NewStartingHashKey), 10)
		lo, hi := hashOf(sh.StartHash), hashOf(sh.EndHash)
		if !ok || at.Cmp(lo) <= 0 || at.Cmp(hi) > 0 {
			return invalidArgument("NewStartingHashKey must be inside the shard's hash key range, after its start")
		}

		sh.close()
		id := sh.ID
		st.openShard(lo, new(big.Int).Sub(at, big.NewInt(1)), id, "")
		st.openShard(at, hi, id, "")
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.SplitShardOutput{}, nil
}

func (s *Service) MergeShards(ctx context.Context, in *api.MergeShardsInput) (*api.MergeShardsOutput, error) {
	err := s.change(ctx, in.StreamName, in.StreamARN, func(_ storage.WriteTx, _ request.Scope, st *stream) error {
		a, err := openShard(st, in.ShardToMerge)
		if err != nil {
			return err
		}

		b, err := openShard(st, in.AdjacentShardToMerge)
		if err != nil {
			return err
		}

		one := big.NewInt(1)
		switch {
		case new(big.Int).Add(hashOf(a.EndHash), one).Cmp(hashOf(b.StartHash)) == 0:
		case new(big.Int).Add(hashOf(b.EndHash), one).Cmp(hashOf(a.StartHash)) == 0:
		default:
			return invalidArgument("Shards %s and %s are not adjacent", a.ID, b.ID)
		}

		lo, hi := hashOf(a.StartHash), hashOf(b.EndHash)
		if hashOf(b.StartHash).Cmp(lo) < 0 {
			lo, hi = hashOf(b.StartHash), hashOf(a.EndHash)
		}

		a.close()
		b.close()
		st.openShard(lo, hi, a.ID, b.ID)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.MergeShardsOutput{}, nil
}

func (s *Service) StartStreamEncryption(
	ctx context.Context,
	in *api.StartStreamEncryptionInput,
) (*api.StartStreamEncryptionOutput, error) {
	err := s.change(ctx, in.StreamName, in.StreamARN, func(_ storage.WriteTx, _ request.Scope, st *stream) error {
		if common.Value(in.EncryptionType) != encryptionKMS || common.Value(in.KeyId) == "" {
			return invalidArgument("EncryptionType must be KMS, with a KeyId")
		}

		st.Encryption, st.KeyID = encryptionKMS, *in.KeyId
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.StartStreamEncryptionOutput{}, nil
}

func (s *Service) StopStreamEncryption(
	ctx context.Context,
	in *api.StopStreamEncryptionInput,
) (*api.StopStreamEncryptionOutput, error) {
	err := s.change(ctx, in.StreamName, in.StreamARN, func(_ storage.WriteTx, _ request.Scope, st *stream) error {
		if common.Value(in.EncryptionType) != encryptionKMS {
			return invalidArgument("EncryptionType must be KMS")
		}

		st.Encryption, st.KeyID = "NONE", ""
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.StopStreamEncryptionOutput{}, nil
}

var shardMetrics = []string{
	"IncomingBytes", "IncomingRecords", "OutgoingBytes", "OutgoingRecords", "WriteProvisionedThroughputExceeded",
	"ReadProvisionedThroughputExceeded", "IteratorAgeMilliseconds", "ALL",
}

// setMetrics turns shard-level metrics on or off. They are stored; no
// metrics are published.
func (s *Service) setMetrics(ctx context.Context, name, arn *string, metrics []string, enable bool) (
	*api.EnhancedMonitoringOutput, error,
) {
	out := &api.EnhancedMonitoringOutput{}
	err := s.change(ctx, name, arn, func(_ storage.WriteTx, scope request.Scope, st *stream) error {
		for _, metric := range metrics {
			if !slices.Contains(shardMetrics, metric) {
				return invalidArgument("Invalid shard-level metric %s", metric)
			}
		}

		out.CurrentShardLevelMetrics = append([]string{}, st.Metrics...)
		all := slices.Contains(metrics, "ALL")
		if all {
			metrics = shardMetrics[:len(shardMetrics)-1]
		}

		for _, metric := range metrics {
			if enable && !slices.Contains(st.Metrics, metric) {
				st.Metrics = append(st.Metrics, metric)
			}

			if !enable {
				st.Metrics = slices.DeleteFunc(st.Metrics, func(m string) bool { return m == metric })
			}
		}

		out.DesiredShardLevelMetrics = append([]string{}, st.Metrics...)
		out.StreamName, out.StreamARN = new(st.Name), new(StreamARN(scope, st.Name))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) EnableEnhancedMonitoring(
	ctx context.Context,
	in *api.EnableEnhancedMonitoringInput,
) (*api.EnhancedMonitoringOutput, error) {
	return s.setMetrics(ctx, in.StreamName, in.StreamARN, in.ShardLevelMetrics, true)
}

func (s *Service) DisableEnhancedMonitoring(
	ctx context.Context,
	in *api.DisableEnhancedMonitoringInput,
) (*api.EnhancedMonitoringOutput, error) {
	return s.setMetrics(ctx, in.StreamName, in.StreamARN, in.ShardLevelMetrics, false)
}

func (s *Service) UpdateStreamMode(
	ctx context.Context,
	in *api.UpdateStreamModeInput,
) (*api.UpdateStreamModeOutput, error) {
	mode := ""
	if in.StreamModeDetails != nil {
		mode = common.Value(in.StreamModeDetails.StreamMode)
	}

	err := s.change(ctx, nil, in.StreamARN, func(_ storage.WriteTx, _ request.Scope, st *stream) error {
		switch {
		case mode != modeProvisioned && mode != modeOnDemand:
			return invalidArgument("StreamMode must be PROVISIONED or ON_DEMAND")
		case mode == modeOnDemand && st.Mode != modeOnDemand && len(st.open()) < onDemandShards:
			reshard(st, onDemandShards)
		}

		st.Mode = mode
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateStreamModeOutput{}, nil
}

func (s *Service) DescribeLimits(ctx context.Context, _ *api.DescribeLimitsInput) (*api.DescribeLimitsOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeLimitsOutput{
		ShardLimit: new(int64(maxShards)), OpenShardCount: new(int64(0)), OnDemandStreamCount: new(int64(0)),
		OnDemandStreamCountLimit: new(int64(50)),
	}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys(prefix(m.Scope, kindStream)) {
			st, _, readErr := storage.Read[stream](tx, k)
			if readErr != nil {
				return readErr
			}

			*out.OpenShardCount += int64(len(st.open()))
			if st.Mode == modeOnDemand {
				*out.OnDemandStreamCount++
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
