package dynamodb

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodb"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// view loads the table named name and passes it to fn.
func (s *Service) view(ctx context.Context, name *string, fn func(*table)) error {
	key, err := tableKey(ctx, name)
	if err != nil {
		return err
	}

	return s.store.View(ctx, func(tx storage.ReadTx) error {
		t, loadErr := load(tx, key)
		if loadErr != nil {
			return loadErr
		}

		fn(t)
		return nil
	})
}

// DescribeContinuousBackups reports a table's continuous backups, which are
// always on, and its point-in-time recovery.
func (s *Service) DescribeContinuousBackups(
	ctx context.Context,
	in *api.DescribeContinuousBackupsInput,
) (*api.DescribeContinuousBackupsOutput, error) {
	out := &api.DescribeContinuousBackupsOutput{}
	err := s.view(ctx, in.TableName, func(t *table) { out.ContinuousBackupsDescription = continuousBackups(t) })
	if err != nil {
		return nil, err
	}

	return out, nil
}

// UpdateTable enables or disables the table's stream; it changes nothing else.
// maxGlobalIndexes is DynamoDB's default limit of global secondary indexes.
const maxGlobalIndexes = 20

// UpdateTable changes a table's stream, billing mode, provisioned throughput,
// and global secondary indexes. Indexes are derived from the items at read
// time, so a new index is ACTIVE and complete at once.
func (s *Service) UpdateTable(ctx context.Context, in *api.UpdateTableInput) (*api.UpdateTableOutput, error) {
	key, err := tableKey(ctx, in.TableName)
	if err != nil {
		return nil, err
	}

	if in.StreamSpecification == nil && in.BillingMode == nil && in.ProvisionedThroughput == nil &&
		len(in.GlobalSecondaryIndexUpdates) == 0 {
		return nil, awserror.Validation("At least one of ProvisionedThroughput, BillingMode, UpdateStreamEnabled, " +
			"GlobalSecondaryIndexUpdates or SSESpecification or ReplicaUpdates is required")
	}

	var viewType string
	if in.StreamSpecification != nil {
		if viewType, err = streamViewType(in.StreamSpecification); err != nil {
			return nil, err
		}
	}

	out := &api.UpdateTableOutput{}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		t, loadErr := load(tx, key)
		if loadErr != nil {
			return loadErr
		}

		if in.StreamSpecification != nil {
			if streamErr := updateStream(tx, key, t, viewType); streamErr != nil {
				return streamErr
			}
		}

		if capacityErr := updateCapacity(t, in); capacityErr != nil {
			return capacityErr
		}

		out.TableDescription = &t.Description
		return save(tx, key, t)
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// updateStream enables a stream of viewType, or disables the stream when
// viewType is empty.
func updateStream(tx storage.WriteTx, key string, t *table, viewType string) error {
	switch now := time.Now(); {
	case viewType != "" && streamEnabled(t):
		return awserror.Validation("Table already has an enabled stream: TableName: %s",
			common.Value(t.Description.TableName))
	case viewType == "" && !streamEnabled(t):
		return awserror.Validation("Table does not have an enabled stream: TableName: %s",
			common.Value(t.Description.TableName))
	case viewType != "":
		return openStream(tx, key, t, viewType, now)
	default:
		return closeStream(tx, key, t, now)
	}
}

// billingMode checks a billing mode.
func billingMode(mode string) (string, error) {
	if mode != billingOnDemand && mode != billingProvisioned {
		return "", awserror.Validation("Invalid BillingMode")
	}

	return mode, nil
}

// throughputFor describes the capacity of a table or an index in mode: none
// on demand, and positive read and write capacity when provisioned.
func throughputFor(mode string, p *api.ProvisionedThroughput) (*api.ProvisionedThroughputDescription, error) {
	out := &api.ProvisionedThroughputDescription{
		ReadCapacityUnits:      new(int64(0)),
		WriteCapacityUnits:     new(int64(0)),
		NumberOfDecreasesToday: new(int64(0)),
	}

	if mode == billingOnDemand {
		if p != nil {
			return nil, awserror.Validation("ProvisionedThroughput is not valid for PAY_PER_REQUEST")
		}

		return out, nil
	}

	if p == nil || common.Value(p.ReadCapacityUnits) < 1 || common.Value(p.WriteCapacityUnits) < 1 {
		return nil, awserror.Validation("Positive provisioned read and write capacity required")
	}

	out.ReadCapacityUnits = p.ReadCapacityUnits
	out.WriteCapacityUnits = p.WriteCapacityUnits
	return out, nil
}

// updateCapacity applies UpdateTable's billing mode, throughput, attribute
// definitions, and global secondary index updates to t. Switching to
// PROVISIONED needs throughput for the table and an Update for each index.
func updateCapacity(t *table, in *api.UpdateTableInput) error {
	d := &t.Description
	mode := common.Value(common.Value(d.BillingModeSummary).BillingMode)
	if mode == "" {
		mode = billingProvisioned
	}

	switched := false
	if in.BillingMode != nil {
		next, err := billingMode(*in.BillingMode)
		if err != nil {
			return err
		}

		switched, mode = next != mode, next
	}

	if in.ProvisionedThroughput != nil || switched {
		pt, err := throughputFor(mode, in.ProvisionedThroughput)
		if err != nil {
			return err
		}

		d.ProvisionedThroughput = pt
		d.BillingModeSummary = &api.BillingModeSummary{BillingMode: &mode}
	}

	if switched && mode == billingOnDemand {
		for i := range d.GlobalSecondaryIndexes {
			d.GlobalSecondaryIndexes[i].ProvisionedThroughput, _ = throughputFor(mode, nil)
		}
	}

	types := map[string]string{}
	for _, a := range d.AttributeDefinitions {
		types[common.Value(a.AttributeName)] = common.Value(a.AttributeType)
	}

	definitions := d.AttributeDefinitions
	for _, a := range in.AttributeDefinitions {
		name, kind := common.Value(a.AttributeName), common.Value(a.AttributeType)
		switch {
		case name == "" || (kind != "S" && kind != "N" && kind != "B"):
			return awserror.Validation("Invalid attribute definition")
		case types[name] == "":
			types[name] = kind
			definitions = append(definitions, a)
		case types[name] != kind:
			return awserror.Validation("Cannot change the type of attribute %s", name)
		}
	}

	updated, err := updateGlobalIndexes(t, in.GlobalSecondaryIndexUpdates, types, mode)
	if err != nil {
		return err
	}

	if switched && mode == billingProvisioned {
		for _, g := range d.GlobalSecondaryIndexes {
			if !updated[common.Value(g.IndexName)] {
				return awserror.Validation("ProvisionedThroughput must be specified for index: %s",
					common.Value(g.IndexName))
			}
		}
	}

	// Only the attributes a key schema uses stay defined.
	used := map[string]bool{}
	for _, ix := range append(secondaryIndexes(t), &index{schema: d.KeySchema}) {
		for _, k := range ix.schema {
			used[common.Value(k.AttributeName)] = true
		}
	}

	d.AttributeDefinitions = slices.DeleteFunc(slices.Clone(definitions), func(a api.AttributeDefinition) bool {
		return !used[common.Value(a.AttributeName)]
	})

	return nil
}

// updateGlobalIndexes applies Update, Create, and Delete actions to t's
// global secondary indexes, at most one of them a Create or Delete, and
// returns the indexes whose throughput an Update or Create set.
func updateGlobalIndexes(
	t *table,
	updates []api.GlobalSecondaryIndexUpdate,
	types map[string]string,
	mode string,
) (map[string]bool, error) {
	d := &t.Description
	updated, changes := map[string]bool{}, 0
	find := func(name *string) (int, error) {
		for i, g := range d.GlobalSecondaryIndexes {
			if common.Value(g.IndexName) == common.Value(name) {
				return i, nil
			}
		}

		return 0, awserror.New("ResourceNotFoundException", "Requested resource not found: Index: "+common.Value(name))
	}

	for _, u := range updates {
		actions := 0
		for _, set := range []bool{u.Update != nil, u.Create != nil, u.Delete != nil} {
			if set {
				actions++
			}
		}

		if actions != 1 {
			return nil, awserror.Validation(
				"Each GlobalSecondaryIndexUpdate must have one of Update, Create, or Delete",
			)
		}

		if u.Update == nil {
			changes++
			if changes > 1 {
				return nil, awserror.New("LimitExceededException", "Subscriber limit exceeded: Only 1 online index "+
					"can be created or deleted simultaneously per table")
			}
		}

		switch {
		case u.Update != nil:
			i, err := find(u.Update.IndexName)
			if err != nil {
				return nil, err
			}

			if updated[common.Value(u.Update.IndexName)] {
				return nil, awserror.Validation("Only one update per index: %s", common.Value(u.Update.IndexName))
			}

			pt, err := throughputFor(mode, u.Update.ProvisionedThroughput)
			if err != nil {
				return nil, err
			}

			d.GlobalSecondaryIndexes[i].ProvisionedThroughput = pt
			updated[common.Value(u.Update.IndexName)] = true
		case u.Create != nil:
			c := u.Create
			if err := checkIndexName(c.IndexName); err != nil {
				return nil, err
			}

			for _, ix := range secondaryIndexes(t) {
				if ix.name == *c.IndexName {
					return nil, awserror.Validation("Attempting to create an index which already exists: %s",
						*c.IndexName)
				}
			}

			if len(d.GlobalSecondaryIndexes) == maxGlobalIndexes {
				return nil, awserror.New("LimitExceededException", "A table can have at most 20 global "+
					"secondary indexes")
			}

			g, err := describeGlobal(&api.GlobalSecondaryIndex{
				IndexName: c.IndexName, KeySchema: c.KeySchema, Projection: c.Projection,
				ProvisionedThroughput: c.ProvisionedThroughput,
			}, types, map[string]bool{}, common.Value(d.TableArn), mode)
			if err != nil {
				return nil, err
			}

			d.GlobalSecondaryIndexes = append(d.GlobalSecondaryIndexes, g)
			updated[*c.IndexName] = true
		default:
			i, err := find(u.Delete.IndexName)
			if err != nil {
				return nil, err
			}

			d.GlobalSecondaryIndexes = slices.Delete(d.GlobalSecondaryIndexes, i, i+1)
		}
	}

	return updated, nil
}

// tagMap converts DynamoDB tags to a map, rejecting repeated keys.
func tagMap(tags []api.Tag) (map[string]string, error) {
	out := make(map[string]string, len(tags))
	for _, tag := range tags {
		if tag.Key == nil || tag.Value == nil {
			return nil, awserror.Validation("Tags must have a Key and a Value")
		}

		if _, dup := out[*tag.Key]; dup {
			return nil, awserror.Validation("Duplicate tag keys found: %s", *tag.Key)
		}

		out[*tag.Key] = *tag.Value
	}

	if err := tagging.Check(out); err != nil {
		return nil, awserror.Validation("Invalid tags: %v", err)
	}

	return out, nil
}

// arnName returns the table name in a table ARN of the caller's account and region.
func arnName(ctx context.Context, arn *string) (*string, error) {
	scope, name, ok := strings.Cut(common.Value(arn), ":table/")
	parts := strings.Split(scope, ":")
	if !ok || len(parts) != 5 || parts[0] != "arn" || parts[2] != "dynamodb" {
		return nil, awserror.Validation("Invalid TableArn: %s", common.Value(arn))
	}

	key, err := tableKey(ctx, &name)
	if err != nil {
		return nil, err
	}

	if !strings.HasPrefix(key, parts[4]+"/"+parts[3]+"/") {
		return nil, awserror.New("ResourceNotFoundException", "Requested resource not found: "+common.Value(arn))
	}

	return &name, nil
}

// retag loads the table in arn, applies fn to its tags, and saves it.
func (s *Service) retag(
	ctx context.Context,
	arn *string,
	fn func(map[string]string) (map[string]string, error),
) error {
	name, err := arnName(ctx, arn)
	if err != nil {
		return err
	}

	key, err := tableKey(ctx, name)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		t, loadErr := load(tx, key)
		if loadErr != nil {
			return loadErr
		}

		tags, tagErr := fn(t.Tags)
		if tagErr != nil {
			return tagErr
		}

		t.Tags = tags
		return save(tx, key, t)
	})
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceInput) (*api.TagResourceOutput, error) {
	add, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	err = s.retag(ctx, in.ResourceArn, func(tags map[string]string) (map[string]string, error) {
		merged, mergeErr := tagging.Merge(tags, add)
		if mergeErr != nil {
			return nil, awserror.Validation("Invalid tags: %v", mergeErr)
		}

		return merged, nil
	})
	if err != nil {
		return nil, err
	}

	return &api.TagResourceOutput{}, nil
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceInput) (*api.UntagResourceOutput, error) {
	err := s.retag(ctx, in.ResourceArn, func(tags map[string]string) (map[string]string, error) {
		return tagging.Remove(tags, in.TagKeys), nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagResourceOutput{}, nil
}

// ListTagsOfResource returns every tag in one page; a table has at most 50.
func (s *Service) ListTagsOfResource(
	ctx context.Context,
	in *api.ListTagsOfResourceInput,
) (*api.ListTagsOfResourceOutput, error) {
	if in.NextToken != nil {
		return nil, awserror.Validation("Invalid NextToken")
	}

	name, err := arnName(ctx, in.ResourceArn)
	if err != nil {
		return nil, err
	}

	out := &api.ListTagsOfResourceOutput{}
	err = s.view(ctx, name, func(t *table) {
		out.Tags = tagging.List(t.Tags, func(k, v string) api.Tag { return api.Tag{Key: &k, Value: &v} })
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
