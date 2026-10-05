package logs

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/logs"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const orderByLastEventTime = "LastEventTime"

// stream is a log stream record.
type stream struct {
	First         *int64 `json:",omitempty"` // earliest event timestamp
	Last          *int64 `json:",omitempty"` // latest event timestamp
	LastIngestion *int64 `json:",omitempty"`
	Name          string
	Created       int64
	// Seq counts the events the stream has ingested; it orders events with
	// equal timestamps.
	Seq int64
}

func streamPrefix(scope request.Scope, group string) string {
	return scope.Prefix("logs", "stream") + encode(group) + "/"
}

func streamKey(scope request.Scope, group, name string) string {
	return streamPrefix(scope, group) + name
}

func checkStreamName(name *string) error {
	if n := common.Value(name); n == "" || len(n) > 512 || strings.ContainsAny(n, ":*") {
		return invalid("Log stream name must be 1 to 512 characters without ':' or '*'")
	}

	return nil
}

func streamNotFound() error { return notFound("The specified log stream does not exist.") }

func loadStream(tx storage.ReadTx, scope request.Scope, group, name string) (*stream, error) {
	st, found, err := storage.Read[stream](tx, streamKey(scope, group, name))
	if err != nil {
		return nil, err
	}

	if !found {
		return nil, streamNotFound()
	}

	return st, nil
}

func (st *stream) describe(scope request.Scope, group string) api.LogStream {
	return api.LogStream{
		LogStreamName:       new(st.Name),
		CreationTime:        new(st.Created),
		FirstEventTimestamp: st.First,
		LastEventTimestamp:  st.Last,
		LastIngestionTime:   st.LastIngestion,
		UploadSequenceToken: new(strconv.FormatInt(st.Seq, 10)),
		Arn:                 new(groupARN(scope, group) + ":log-stream:" + st.Name),
		StoredBytes:         new(int64(0)),
	}
}

func (s *Service) CreateLogStream(
	ctx context.Context,
	in *api.CreateLogStreamRequest,
) (*api.CreateLogStreamOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if guardErr := checkGroupName(in.LogGroupName); guardErr != nil {
		return nil, guardErr
	}

	if guardErr := checkStreamName(in.LogStreamName); guardErr != nil {
		return nil, guardErr
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, loadErr := loadGroup(tx, scope, *in.LogGroupName); loadErr != nil {
			return loadErr
		}

		key := streamKey(scope, *in.LogGroupName, *in.LogStreamName)
		if _, exists := tx.Get(key); exists {
			return awserror.New("ResourceAlreadyExistsException", "The specified log stream already exists")
		}

		return storage.Write(tx, key, &stream{Name: *in.LogStreamName, Created: s.now().UnixMilli()})
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateLogStreamOutput{}, nil
}

// DeleteLogStream deletes the stream and its events.
func (s *Service) DeleteLogStream(
	ctx context.Context,
	in *api.DeleteLogStreamRequest,
) (*api.DeleteLogStreamOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if guardErr := checkGroupName(in.LogGroupName); guardErr != nil {
		return nil, guardErr
	}

	if guardErr := checkStreamName(in.LogStreamName); guardErr != nil {
		return nil, guardErr
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		g, loadErr := loadGroup(tx, scope, *in.LogGroupName)
		if loadErr != nil {
			return loadErr
		}

		if _, loadErr = loadStream(tx, scope, g.Name, *in.LogStreamName); loadErr != nil {
			return loadErr
		}

		for _, key := range tx.Keys(eventPrefix(scope, g.Name, *in.LogStreamName)) {
			e, _, readErr := storage.Read[event](tx, key)
			if readErr != nil {
				return readErr
			}

			g.StoredBytes -= int64(len(e.Message))
			tx.Delete(key)
		}

		tx.Delete(streamKey(scope, g.Name, *in.LogStreamName))
		return storage.Write(tx, groupKey(scope, g.Name), g)
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteLogStreamOutput{}, nil
}

// DescribeLogStreams lists a group's streams by name, optionally filtered by a
// name prefix, or by the time of their latest event.
func (s *Service) DescribeLogStreams(
	ctx context.Context,
	in *api.DescribeLogStreamsRequest,
) (*api.DescribeLogStreamsResponse, error) {
	byTime := false
	switch common.Value(in.OrderBy) {
	case "", "LogStreamName":
	case orderByLastEventTime:
		if in.LogStreamNamePrefix != nil {
			return nil, invalid("Cannot order by LastEventTime with a logStreamNamePrefix.")
		}

		byTime = true
	default:
		return nil, invalid("orderBy must be LogStreamName or LastEventTime")
	}

	limit, err := describeLimit(in.Limit)
	if err != nil {
		return nil, err
	}

	after, err := decodeToken(in.NextToken)
	if err != nil {
		return nil, err
	}

	descending := common.Value(in.Descending)
	out := &api.DescribeLogStreamsResponse{LogStreams: []api.LogStream{}}
	err = s.view(ctx, in.LogGroupName, in.LogGroupIdentifier,
		func(tx storage.ReadTx, scope request.Scope, g *group) error {
			type entry struct {
				st  *stream
				key string
			}

			var entries []entry
			for _, key := range tx.Keys(streamKey(scope, g.Name, common.Value(in.LogStreamNamePrefix))) {
				st, _, readErr := storage.Read[stream](tx, key)
				if readErr != nil {
					return readErr
				}

				sortKey := st.Name
				if byTime {
					sortKey = fmt.Sprintf("%020d/%s", common.Value(st.Last), st.Name)
				}

				entries = append(entries, entry{st: st, key: sortKey})
			}

			slices.SortFunc(entries, func(a, b entry) int { return strings.Compare(a.key, b.key) })
			if descending {
				slices.Reverse(entries)
			}

			entries = slices.DeleteFunc(entries, func(e entry) bool {
				return after != "" && ((!descending && e.key <= after) || (descending && e.key >= after))
			})
			if len(entries) > limit {
				entries = entries[:limit]
				out.NextToken = encodeToken(entries[limit-1].key)
			}

			for _, e := range entries {
				out.LogStreams = append(out.LogStreams, e.st.describe(scope, g.Name))
			}

			return nil
		})
	if err != nil {
		return nil, err
	}

	return out, nil
}
