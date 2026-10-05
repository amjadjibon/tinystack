package eventbridge

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/eventbridge"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/pattern"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Replays send an archive's events from a time range to the rules of the
// archive's bus again, with a "replay-name" field. StartReplay delivers them
// before it returns, so a replay is COMPLETED as soon as it is described.
// Each replay is "<account>/<region>/eventbridge/replay/<name>".

const (
	replayStarting  = "STARTING"
	replayCompleted = "COMPLETED"
)

var replayName = regexp.MustCompile(`^[.\-_A-Za-z0-9]{1,64}$`)

// replay is a replay's record. Times are Unix milliseconds.
type replay struct {
	Name         string
	Description  string `json:",omitempty"`
	Archive      string
	Bus          string
	State        string
	Filters      []string `json:",omitempty"`
	From         int64
	To           int64
	Started      int64
	Ended        int64 `json:",omitempty"`
	LastReplayed int64 `json:",omitempty"`
}

func replayPrefix(scope request.Scope) string {
	return scope.AccountID + "/" + scope.Region + "/eventbridge/replay/"
}

func replayARN(scope request.Scope, name string) string {
	return "arn:aws:events:" + scope.Region + ":" + scope.AccountID + ":replay/" + name
}

func replayNotFound(name string) error {
	return awserror.New("ResourceNotFoundException", "Replay "+name+" does not exist.")
}

func millis(seconds *float64) int64 {
	return int64(common.Value(seconds) * 1000)
}

func seconds(ms int64) *float64 {
	if ms == 0 {
		return nil
	}

	return new(float64(ms) / 1000)
}

// StartReplay replays the archive's events from EventStartTime up to
// EventEndTime to the rules of the archive's bus, or to the rules of it that
// FilterArns names.
func (s *Service) StartReplay(ctx context.Context, in *api.StartReplayRequest) (*api.StartReplayResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	r, err := s.newReplay(m.Scope, in)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		a, loadErr := loadArchive(tx, m.Scope, r.Archive)
		if loadErr != nil {
			return loadErr
		}

		if a.Bus != r.Bus {
			return invalid("The destination of a replay must be the event bus of its archive.")
		}

		for _, arn := range r.Filters {
			if !strings.HasPrefix(arn, ruleARN(m.Scope, r.Bus, "")) {
				return invalid("FilterArns must name rules on the event bus %s.", r.Bus)
			}
		}

		if _, exists := tx.Get(replayPrefix(m.Scope) + r.Name); exists {
			return awserror.New("ResourceAlreadyExistsException", "Replay "+r.Name+" already exists.")
		}

		return storage.Write(tx, replayPrefix(m.Scope)+r.Name, r)
	})
	if err != nil {
		return nil, err
	}

	if runErr := s.run(ctx, m.Scope, r); runErr != nil {
		return nil, runErr
	}

	return &api.StartReplayResponse{
		ReplayArn: new(replayARN(m.Scope, r.Name)), State: new(replayStarting), StateReason: new(""),
		ReplayStartTime: seconds(r.Started),
	}, nil
}

// newReplay validates a StartReplay request.
func (s *Service) newReplay(scope request.Scope, in *api.StartReplayRequest) (*replay, error) {
	name := common.Value(in.ReplayName)
	switch {
	case !replayName.MatchString(name):
		return nil, invalid("Replay name must be 1 to 64 characters from [.-_A-Za-z0-9].")
	case len(common.Value(in.Description)) > 512:
		return nil, invalid("Description must be at most 512 characters.")
	case in.EventStartTime == nil || in.EventEndTime == nil || in.Destination == nil:
		return nil, invalid("EventStartTime, EventEndTime, and Destination are required.")
	case millis(in.EventStartTime) >= millis(in.EventEndTime):
		return nil, invalid("EventStartTime must be before EventEndTime.")
	}

	archive, ok := strings.CutPrefix(common.Value(in.EventSourceArn), archiveARN(scope, ""))
	if !ok {
		return nil, archiveNotFound(common.Value(in.EventSourceArn))
	}

	bus, ok := strings.CutPrefix(common.Value(in.Destination.Arn), busARN(scope, ""))
	if !ok {
		return nil, busNotFound(common.Value(in.Destination.Arn))
	}

	return &replay{
		Name: name, Description: common.Value(in.Description), Archive: archive, Bus: bus, State: replayStarting,
		Filters: in.Destination.FilterArns, From: millis(in.EventStartTime), To: millis(in.EventEndTime),
		Started: s.now().UnixMilli(),
	}, nil
}

// run delivers a replay's events and records that it completed.
func (s *Service) run(ctx context.Context, scope request.Scope, r *replay) error {
	rules, err := s.enabledRules(ctx, scope, &r.Bus)
	if err != nil {
		return err
	}

	if len(r.Filters) > 0 {
		rules = slices.DeleteFunc(rules, func(c compiledRule) bool { return !slices.Contains(r.Filters, c.arn) })
	}

	var keys []string
	var events [][]byte
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		from := archivedKey(scope, r.Archive, time.UnixMilli(r.From), "")
		to := archivedKey(scope, r.Archive, time.UnixMilli(r.To), "")
		for _, k := range tx.Keys(archivedPrefix(scope, r.Archive)) {
			if k >= from && k < to {
				data, _ := tx.Get(k)
				keys, events = append(keys, k), append(events, data)
			}
		}

		return nil
	})
	if err != nil {
		return err
	}

	for i, data := range events {
		var e event
		if decodeErr := json.Unmarshal(data, &e); decodeErr != nil {
			return decodeErr
		}

		e.ReplayName = r.Name
		encoded, encodeErr := json.Marshal(&e)
		if encodeErr != nil {
			return encodeErr
		}

		leafValues, _ := pattern.Flatten(encoded)
		for j := range rules {
			if rules[j].pattern.Matches(leafValues) {
				for k := range rules[j].targets {
					s.deliver(ctx, scope, &rules[j], &rules[j].targets[k], &e, encoded)
				}
			}
		}

		at := strings.TrimPrefix(keys[i], archivedPrefix(scope, r.Archive))
		r.LastReplayed, _ = strconv.ParseInt(at[:20], 10, 64)
	}

	r.State, r.Ended = replayCompleted, s.now().UnixMilli()
	return s.store.Update(context.WithoutCancel(ctx), func(tx storage.WriteTx) error {
		return storage.Write(tx, replayPrefix(scope)+r.Name, r)
	})
}

func (s *Service) loadReplay(ctx context.Context, name string) (request.Scope, *replay, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return request.Scope{}, nil, err
	}

	var r *replay
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var found bool
		var loadErr error
		if r, found, loadErr = storage.Read[replay](tx, replayPrefix(m.Scope)+name); loadErr == nil && !found {
			return replayNotFound(name)
		}

		return loadErr
	})
	return m.Scope, r, err
}

// list renders a replay as ListReplays does.
func (r *replay) list(scope request.Scope) api.Replay {
	return api.Replay{
		ReplayName: new(r.Name), EventSourceArn: new(archiveARN(scope, r.Archive)), State: new(r.State),
		StateReason: new(""), EventStartTime: seconds(r.From), EventEndTime: seconds(r.To),
		EventLastReplayedTime: seconds(r.LastReplayed), ReplayStartTime: seconds(r.Started),
		ReplayEndTime: seconds(r.Ended),
	}
}

func (s *Service) DescribeReplay(
	ctx context.Context,
	in *api.DescribeReplayRequest,
) (*api.DescribeReplayResponse, error) {
	scope, r, err := s.loadReplay(ctx, common.Value(in.ReplayName))
	if err != nil {
		return nil, err
	}

	l := r.list(scope)
	out := &api.DescribeReplayResponse{
		ReplayName: l.ReplayName, ReplayArn: new(replayARN(scope, r.Name)), State: l.State,
		StateReason: l.StateReason, EventSourceArn: l.EventSourceArn,
		Destination:    &api.ReplayDestination{Arn: new(busARN(scope, r.Bus)), FilterArns: r.Filters},
		EventStartTime: l.EventStartTime, EventEndTime: l.EventEndTime,
		EventLastReplayedTime: l.EventLastReplayedTime, ReplayStartTime: l.ReplayStartTime,
		ReplayEndTime: l.ReplayEndTime,
	}
	if r.Description != "" {
		out.Description = new(r.Description)
	}

	return out, nil
}

// ListReplays lists the caller's replays in name order, by name prefix,
// state, or archive.
func (s *Service) ListReplays(ctx context.Context, in *api.ListReplaysRequest) (*api.ListReplaysResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	after, size, err := page(in.NextToken, in.Limit)
	if err != nil {
		return nil, err
	}

	out := &api.ListReplaysResponse{Replays: []api.Replay{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys(replayPrefix(m.Scope) + common.Value(in.NamePrefix)) {
			r, _, loadErr := storage.Read[replay](tx, k)
			if loadErr != nil {
				return loadErr
			}

			l := r.list(m.Scope)
			switch {
			case r.Name <= after:
			case in.EventSourceArn != nil && *in.EventSourceArn != *l.EventSourceArn:
			case in.State != nil && *in.State != r.State:
			case len(out.Replays) == size:
				out.NextToken = nextToken(*out.Replays[size-1].ReplayName)
				return nil
			default:
				out.Replays = append(out.Replays, l)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// CancelReplay cancels a replay that is still starting or running, which a
// TinyStack replay never is once StartReplay returns.
func (s *Service) CancelReplay(ctx context.Context, in *api.CancelReplayRequest) (*api.CancelReplayResponse, error) {
	_, r, err := s.loadReplay(ctx, common.Value(in.ReplayName))
	if err != nil {
		return nil, err
	}

	return nil, awserror.New("IllegalStatusException",
		"Replay "+r.Name+" is "+r.State+" and cannot be cancelled.")
}
