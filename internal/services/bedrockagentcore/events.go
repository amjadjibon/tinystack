package bedrockagentcore

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/bedrockagentcore"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/bedrockagentcorecontrol"
	"github.com/amjadjibon/tinystack/internal/services/idempotency"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// expireBatch bounds the events one sweep deletes, so a sweep stays a short transaction.
const expireBatch = 1000

// eventRecord is a stored event. Expires is when the sweeper deletes it: its memory's eventExpiryDuration days
// after it was created.
type eventRecord struct {
	Event   api.Event
	Created time.Time
	Expires time.Time
	Token   string
}

func eventPrefix(scope request.Scope, memory string) string {
	return scope.Prefix(serviceName, kindEvent) + memory + "/"
}

// sessionPrefix is the prefix of a session's events; the actor is escaped so that it holds no slash.
func sessionPrefix(scope request.Scope, memory, actor, session string) string {
	return eventPrefix(scope, memory) + url.PathEscape(actor) + "/" + session + "/"
}

func expiryKey(expires time.Time, eventKey string) string {
	return fmt.Sprintf("%s%020d/%s", expiryIndex, expires.UnixMilli(), eventKey)
}

// memoryID returns the ID of the memory that ref names: an ID, or the memory's ARN in the scope.
func memoryID(scope request.Scope, ref string) (string, error) {
	if !strings.HasPrefix(ref, "arn:") {
		return ref, nil
	}

	// arn:aws:bedrock-agentcore:<region>:<account>:memory/<id>
	parts := strings.SplitN(ref, ":", 6)
	if len(parts) == 6 && parts[2] == "bedrock-agentcore" && parts[3] == scope.Region && parts[4] == scope.AccountID {
		if id, ok := strings.CutPrefix(parts[5], "memory/"); ok && id != "" {
			return id, nil
		}
	}

	return "", notFound("Memory %s does not exist", ref)
}

// activeMemory resolves ref to a memory that takes events.
func activeMemory(
	tx storage.ReadTx,
	scope request.Scope,
	ref string,
) (string, bedrockagentcorecontrol.MemoryInfo, error) {
	id, err := memoryID(scope, ref)
	if err != nil {
		return "", bedrockagentcorecontrol.MemoryInfo{}, err
	}

	info, err := bedrockagentcorecontrol.Memory(tx, scope, id)

	return id, info, err
}

func checkPayload(payload []api.PayloadType) error {
	for i, p := range payload {
		set := 0
		for _, present := range []bool{p.Conversational != nil, p.Blob != nil, p.Json != nil} {
			if present {
				set++
			}
		}

		if set != 1 {
			return invalid("payload[%d] must set exactly one of conversational, blob and json", i)
		}
	}

	return nil
}

// branchOf returns the name of the branch the event is on, or "" for the main conversation.
func branchOf(e *api.Event) string {
	if e.Branch == nil {
		return ""
	}

	return common.Value(e.Branch.Name)
}

// checkBranch requires a new branch to fork from an event of the session, and an existing branch to keep the
// root it has. It returns the branch the new event belongs to.
func checkBranch(tx storage.ReadTx, prefix string, b *api.Branch) (*api.Branch, error) {
	events, err := storage.Records[eventRecord](tx, prefix)
	if err != nil {
		return nil, err
	}

	root := common.Value(b.RootEventId)
	for _, e := range events {
		if branchOf(&e.Event) == common.Value(b.Name) {
			if root != "" && root != common.Value(e.Event.Branch.RootEventId) {
				return nil, invalid("branch %s already exists with root event %s", common.Value(b.Name),
					common.Value(e.Event.Branch.RootEventId))
			}

			return common.Clone(e.Event.Branch), nil
		}
	}

	if root == "" {
		return nil, invalid("rootEventId is required to start branch %s", common.Value(b.Name))
	}

	if !slices.ContainsFunc(events, func(e *eventRecord) bool { return common.Value(e.Event.EventId) == root }) {
		return nil, invalid("rootEventId %s is not an event of the session", root)
	}

	return common.Clone(b), nil
}

func (s *Service) CreateEvent(ctx context.Context, in *api.CreateEventInput) (*api.CreateEventOutput, error) {
	if err := checkPayload(in.Payload); err != nil {
		return nil, err
	}

	stamp := common.Value(in.EventTimestamp)
	if stamp < 0 {
		return nil, invalid("eventTimestamp must not be before 1970")
	}

	at := time.UnixMilli(int64(stamp * 1000)).UTC()
	var out *api.CreateEventOutput
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		memory, info, err := activeMemory(tx, scope, common.Value(in.MemoryId))
		if err != nil {
			return err
		}

		token := idempotency.Key(scope, serviceName, "CreateEvent/"+memory, in.ClientToken)
		previous, found, replayErr := idempotency.Replay[*api.CreateEventOutput](tx, token, in,
			invalid("clientToken was used with different parameters"))
		if replayErr != nil || found {
			out = previous
			return replayErr
		}

		session := common.Value(in.SessionId)
		if session == "" {
			session = common.RandomID(32)
		}

		actor := common.Value(in.ActorId)
		var branch *api.Branch
		if in.Branch != nil {
			branch, err = checkBranch(tx, sessionPrefix(scope, memory, actor, session), in.Branch)
			if err != nil {
				return err
			}
		}

		now := s.now().UTC()
		id := fmt.Sprintf("%019d#%s", at.UnixMilli(), common.RandomID(8, "0123456789abcdef"))
		rec := &eventRecord{
			Created: now, Expires: now.Add(time.Duration(info.ExpiryDays) * 24 * time.Hour),
			Event: api.Event{
				ActorId: new(
					actor,
				),
				Branch:         branch,
				EventId:        new(id),
				EventTimestamp: new(stamp),
				MemoryId:       new(memory),
				Metadata:       in.Metadata,
				Payload:        in.Payload,
				SessionId:      new(session),
			},
		}

		key := sessionPrefix(scope, memory, actor, session) + id
		rec.Token = token

		tx.Put(expiryKey(rec.Expires, key), []byte(key))
		out = &api.CreateEventOutput{Event: &rec.Event}

		if err := storage.Write(tx, key, rec); err != nil {
			return err
		}

		return idempotency.Remember(tx, token, in, out)
	})

	return out, err
}

// readEvent returns the event at key, or not found.
func readEvent(tx storage.ReadTx, key, id string) (*eventRecord, error) {
	rec, found, err := storage.Read[eventRecord](tx, key)
	if err != nil {
		return nil, err
	}

	if !found {
		return nil, notFound("Event %s does not exist", id)
	}

	return rec, nil
}

func (s *Service) GetEvent(ctx context.Context, in *api.GetEventInput) (*api.GetEventOutput, error) {
	var out *api.GetEventOutput
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		memory, _, err := activeMemory(tx, scope, common.Value(in.MemoryId))
		if err != nil {
			return err
		}

		rec, err := readEvent(tx, sessionPrefix(scope, memory, common.Value(in.ActorId), common.Value(in.SessionId))+
			common.Value(in.EventId), common.Value(in.EventId))
		if err != nil {
			return err
		}

		out = &api.GetEventOutput{Event: &rec.Event}

		return nil
	})

	return out, err
}

func (s *Service) DeleteEvent(ctx context.Context, in *api.DeleteEventInput) (*api.DeleteEventOutput, error) {
	var out *api.DeleteEventOutput
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		memory, _, err := activeMemory(tx, scope, common.Value(in.MemoryId))
		if err != nil {
			return err
		}

		key := sessionPrefix(scope, memory, common.Value(in.ActorId), common.Value(in.SessionId)) +
			common.Value(in.EventId)
		rec, err := readEvent(tx, key, common.Value(in.EventId))
		if err != nil {
			return err
		}

		dropEvent(tx, key, rec)
		out = &api.DeleteEventOutput{EventId: in.EventId}

		return nil
	})

	return out, err
}

// dropEvent deletes an event with the index entries that point at it.
func dropEvent(tx storage.WriteTx, key string, rec *eventRecord) {
	tx.Delete(expiryKey(rec.Expires, key))
	if rec.Token != "" {
		tx.Delete(rec.Token)
	}

	tx.Delete(key)
}

// onBranch selects the events a branch filter keeps: those on the branch and, with parents, those the branch
// forked from, up to each root event.
func onBranch(events []*eventRecord, name string, parents bool) []*eventRecord {
	var kept []*eventRecord
	included := map[string]bool{}
	// upTo limits a branch's events to those up to its fork point.
	var collect func(branch, upTo string)
	collect = func(branch, upTo string) {
		root := ""
		for _, e := range events {
			id := common.Value(e.Event.EventId)
			if branchOf(&e.Event) != branch || upTo != "" && id > upTo {
				continue
			}

			if !included[id] {
				included[id] = true
				kept = append(kept, e)
			}

			if e.Event.Branch != nil {
				root = common.Value(e.Event.Branch.RootEventId)
			}
		}

		if !parents || root == "" {
			return
		}

		for _, e := range events {
			if common.Value(e.Event.EventId) == root {
				collect(branchOf(&e.Event), root)
			}
		}
	}

	collect(name, "")
	slices.SortFunc(kept, func(a, b *eventRecord) int {
		return strings.Compare(common.Value(a.Event.EventId), common.Value(b.Event.EventId))
	})

	return kept
}

// matches reports whether the event satisfies every metadata filter expression.
func matches(e *api.Event, filters []api.EventMetadataFilterExpression) bool {
	for _, f := range filters {
		if f.Left == nil {
			return false
		}

		value, present := e.Metadata[common.Value(f.Left.MetadataKey)]
		switch common.Value(f.Operator) {
		case "EXISTS":
			if !present {
				return false
			}
		case "NOT_EXISTS":
			if present {
				return false
			}
		default:
			if !present || f.Right == nil || f.Right.MetadataValue == nil ||
				common.Value(value.StringValue) != common.Value(f.Right.MetadataValue.StringValue) {
				return false
			}
		}
	}

	return true
}

// ListEvents lists a session's events from the oldest, as their IDs sort.
func (s *Service) ListEvents(ctx context.Context, in *api.ListEventsInput) (*api.ListEventsOutput, error) {
	var out *api.ListEventsOutput
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		memory, _, err := activeMemory(tx, scope, common.Value(in.MemoryId))
		if err != nil {
			return err
		}

		events, err := storage.Records[eventRecord](tx,
			sessionPrefix(scope, memory, common.Value(in.ActorId), common.Value(in.SessionId)))
		if err != nil {
			return err
		}

		if f := in.Filter; f != nil {
			if f.Branch != nil {
				events = onBranch(events, common.Value(f.Branch.Name), common.Value(f.Branch.IncludeParentBranches))
			}

			events = slices.DeleteFunc(events, func(e *eventRecord) bool { return !matches(&e.Event, f.EventMetadata) })
		}

		page, next, err := common.Page(events, func(e *eventRecord) string { return common.Value(e.Event.EventId) },
			in.NextToken, pageLimit(in.MaxResults), errBadToken)
		if err != nil {
			return err
		}

		out = &api.ListEventsOutput{Events: []api.Event{}, NextToken: next}
		for _, e := range page {
			event := e.Event
			if in.IncludePayloads != nil && !*in.IncludePayloads {
				event.Payload = nil
			}

			out.Events = append(out.Events, event)
		}

		return nil
	})

	return out, err
}

// eventParts splits an event key into its actor and session, given the memory's prefix.
func eventParts(key, prefix string) (actor, session string, ok bool) {
	escaped, rest, ok := strings.Cut(strings.TrimPrefix(key, prefix), "/")
	if !ok {
		return "", "", false
	}

	session, _, ok = strings.Cut(rest, "/")
	actor, err := url.PathUnescape(escaped)

	return actor, session, ok && err == nil
}

func (s *Service) ListActors(ctx context.Context, in *api.ListActorsInput) (*api.ListActorsOutput, error) {
	var out *api.ListActorsOutput
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		memory, _, err := activeMemory(tx, scope, common.Value(in.MemoryId))
		if err != nil {
			return err
		}

		prefix := eventPrefix(scope, memory)
		var actors []string
		for _, key := range tx.Keys(prefix) {
			if actor, _, ok := eventParts(key, prefix); ok && !slices.Contains(actors, actor) {
				actors = append(actors, actor)
			}
		}

		page, next, err := common.Page(
			actors,
			func(a string) string { return a },
			in.NextToken,
			pageLimit(in.MaxResults),
			errBadToken,
		)
		if err != nil {
			return err
		}

		out = &api.ListActorsOutput{ActorSummaries: []api.ActorSummary{}, NextToken: next}
		for _, actor := range page {
			out.ActorSummaries = append(out.ActorSummaries, api.ActorSummary{ActorId: new(actor)})
		}

		return nil
	})

	return out, err
}

// ListSessions lists the sessions of an actor, which exist while they hold events.
func (s *Service) ListSessions(ctx context.Context, in *api.ListSessionsInput) (*api.ListSessionsOutput, error) {
	var out *api.ListSessionsOutput
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		memory, _, err := activeMemory(tx, scope, common.Value(in.MemoryId))
		if err != nil {
			return err
		}

		prefix := eventPrefix(scope, memory)
		actor := common.Value(in.ActorId)
		var sessions []string
		for _, key := range tx.Keys(prefix + url.PathEscape(actor) + "/") {
			if a, session, ok := eventParts(key, prefix); ok && a == actor && !slices.Contains(sessions, session) {
				sessions = append(sessions, session)
			}
		}

		page, next, err := common.Page(sessions, func(id string) string { return id }, in.NextToken,
			pageLimit(in.MaxResults), errBadToken)
		if err != nil {
			return err
		}

		out = &api.ListSessionsOutput{SessionSummaries: []api.SessionSummary{}, NextToken: next}
		for _, session := range page {
			events, e := storage.Records[eventRecord](tx, sessionPrefix(scope, memory, actor, session))
			if e != nil {
				return e
			}

			created := slices.MinFunc(
				events,
				func(a, b *eventRecord) int { return a.Created.Compare(b.Created) },
			).Created
			out.SessionSummaries = append(out.SessionSummaries, api.SessionSummary{
				ActorId: new(actor), CreatedAt: common.SecondsMilli(created), SessionId: new(session),
			})
		}

		return nil
	})

	return out, err
}

// expireEvents deletes the events whose expiry time has passed.
func (s *Service) expireEvents(ctx context.Context) {
	limit := fmt.Sprintf("%s%020d", expiryIndex, s.now().UnixMilli()+1)
	_ = s.store.Update(ctx, func(tx storage.WriteTx) error {
		for i, index := range tx.Keys(expiryIndex) {
			if index >= limit || i >= expireBatch {
				return nil
			}

			target, _ := tx.Get(index)
			rec, found, err := storage.Read[eventRecord](tx, string(target))
			if err != nil {
				return err
			}

			if found {
				dropEvent(tx, string(target), rec)
			}

			tx.Delete(index)
		}

		return nil
	})
}
