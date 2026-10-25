package integration_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	data "github.com/aws/aws-sdk-go-v2/service/bedrockagentcore"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore/document"
	datatypes "github.com/aws/aws-sdk-go-v2/service/bedrockagentcore/types"
	control "github.com/aws/aws-sdk-go-v2/service/bedrockagentcorecontrol"
	controltypes "github.com/aws/aws-sdk-go-v2/service/bedrockagentcorecontrol/types"

	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func agentCoreData(s *httptest.Server) *data.Client {
	return data.New(data.Options{
		Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test"), Retryer: aws.NopRetryer{},
	})
}

// agentCoreMemory makes an ACTIVE memory that keeps events for days.
func agentCoreMemory(t *testing.T, c *control.Client, name string, days int32) *controltypes.Memory {
	t.Helper()
	made, err := c.CreateMemory(context.Background(), &control.CreateMemoryInput{
		Name: new(name), EventExpiryDuration: new(days),
	})
	if err != nil {
		t.Fatal(err)
	}

	return wantAgentMemory(t, c, made.Memory.Id, controltypes.MemoryStatusActive)
}

func text(role datatypes.Role, content string) datatypes.PayloadType {
	return &datatypes.PayloadTypeMemberConversational{Value: datatypes.Conversational{
		Role: role, Content: &datatypes.ContentMemberText{Value: content},
	}}
}

func metadata(pairs ...string) map[string]datatypes.MetadataValue {
	out := map[string]datatypes.MetadataValue{}
	for i := 0; i < len(pairs); i += 2 {
		out[pairs[i]] = &datatypes.MetadataValueMemberStringValue{Value: pairs[i+1]}
	}

	return out
}

func createEvent(
	t *testing.T,
	c *data.Client,
	memoryID, actor, session string,
	at time.Time,
	in *data.CreateEventInput,
) *datatypes.Event {
	t.Helper()
	in.MemoryId, in.ActorId, in.SessionId, in.EventTimestamp = &memoryID, &actor, &session, &at
	out, err := c.CreateEvent(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}

	return out.Event
}

func eventIDs(events []datatypes.Event) []string {
	var ids []string
	for _, e := range events {
		ids = append(ids, *e.EventId)
	}

	return ids
}

func TestAgentCoreEvents(t *testing.T) {
	t.Parallel()
	s := server(t)
	ctx := context.Background()
	admin := agentCoreControl(s, "us-east-1")
	c := agentCoreData(s)
	memoryRecord := agentCoreMemory(t, admin, "events_memory", 7)
	id := *memoryRecord.Id
	start := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	at := func(n int) time.Time { return start.Add(time.Duration(n) * time.Second) }

	first := createEvent(t, c, id, "alice", "chat", at(1), &data.CreateEventInput{
		Payload:  []datatypes.PayloadType{text(datatypes.RoleUser, "hello"), text(datatypes.RoleAssistant, "hi")},
		Metadata: metadata("topic", "greeting", "lang", "en"),
	})
	if *first.MemoryId != id || *first.ActorId != "alice" || *first.SessionId != "chat" || len(first.Payload) != 2 ||
		!strings.Contains(*first.EventId, "#") || !first.EventTimestamp.Equal(at(1)) {
		t.Fatalf("event: %+v", first)
	}

	second := createEvent(t, c, id, "alice", "chat", at(2), &data.CreateEventInput{
		Payload: []datatypes.PayloadType{&datatypes.PayloadTypeMemberJson{
			Value: datatypes.MemoryJsonData{Content: document.NewLazyDocument(map[string]any{"tool": "search"})},
		}},
		Metadata: metadata("topic", "search"),
	})
	// An event whose timestamp is earlier sorts first: events are ordered by when they happened.
	earliest := createEvent(t, c, id, "alice", "chat", at(0), &data.CreateEventInput{
		Payload: []datatypes.PayloadType{text(datatypes.RoleOther, "joined")},
	})
	branched := createEvent(t, c, id, "alice", "chat", at(3), &data.CreateEventInput{
		Payload: []datatypes.PayloadType{text(datatypes.RoleUser, "what if")},
		Branch:  &datatypes.Branch{Name: new("alternative"), RootEventId: first.EventId},
	})
	if *branched.Branch.RootEventId != *first.EventId {
		t.Fatalf("branch: %+v", branched.Branch)
	}

	followUp := createEvent(t, c, id, "alice", "chat", at(4), &data.CreateEventInput{
		Payload: []datatypes.PayloadType{text(datatypes.RoleAssistant, "then")},
		Branch:  &datatypes.Branch{Name: new("alternative")},
	})
	if *followUp.Branch.RootEventId != *first.EventId {
		t.Fatalf("existing branch keeps its root: %+v", followUp.Branch)
	}

	got, err := c.GetEvent(ctx, &data.GetEventInput{
		MemoryId: &id, ActorId: new("alice"), SessionId: new("chat"), EventId: second.EventId,
	})
	if err != nil || len(got.Event.Payload) != 1 || got.Event.Metadata["topic"] == nil {
		t.Fatalf("get: %+v %v", got, err)
	}

	list := func(in *data.ListEventsInput) *data.ListEventsOutput {
		t.Helper()
		in.MemoryId, in.ActorId, in.SessionId = &id, new("alice"), new("chat")
		out, e := c.ListEvents(ctx, in)
		if e != nil {
			t.Fatal(e)
		}

		return out
	}

	all := list(&data.ListEventsInput{})
	want := []string{*earliest.EventId, *first.EventId, *second.EventId, *branched.EventId, *followUp.EventId}
	if ids := eventIDs(all.Events); strings.Join(ids, ",") != strings.Join(want, ",") || all.NextToken != nil {
		t.Fatalf("list: %v, want %v", ids, want)
	}

	bare := list(&data.ListEventsInput{IncludePayloads: new(false)})
	if len(bare.Events) != 5 || len(bare.Events[0].Payload) != 0 {
		t.Fatalf("without payloads: %+v", bare.Events[0])
	}

	var paged []string
	var token *string
	for range 4 {
		page := list(&data.ListEventsInput{MaxResults: new(int32(2)), NextToken: token})
		paged = append(paged, eventIDs(page.Events)...)
		if token = page.NextToken; token == nil {
			break
		}
	}

	if strings.Join(paged, ",") != strings.Join(want, ",") {
		t.Fatalf("paged %v, want %v", paged, want)
	}

	equals := list(
		&data.ListEventsInput{Filter: &datatypes.FilterInput{EventMetadata: []datatypes.EventMetadataFilterExpression{
			{
				Left:     &datatypes.LeftExpressionMemberMetadataKey{Value: "topic"},
				Operator: datatypes.OperatorTypeEqualsTo,
				Right: &datatypes.RightExpressionMemberMetadataValue{
					Value: &datatypes.MetadataValueMemberStringValue{Value: "search"},
				},
			},
		}}},
	)
	exists := list(
		&data.ListEventsInput{Filter: &datatypes.FilterInput{EventMetadata: []datatypes.EventMetadataFilterExpression{{
			Left: &datatypes.LeftExpressionMemberMetadataKey{Value: "lang"}, Operator: datatypes.OperatorTypeExists,
		}}}},
	)
	missing := list(
		&data.ListEventsInput{Filter: &datatypes.FilterInput{EventMetadata: []datatypes.EventMetadataFilterExpression{{
			Left: &datatypes.LeftExpressionMemberMetadataKey{Value: "topic"}, Operator: datatypes.OperatorTypeNotExists,
		}}}},
	)
	if len(equals.Events) != 1 || *equals.Events[0].EventId != *second.EventId ||
		len(exists.Events) != 1 || *exists.Events[0].EventId != *first.EventId || len(missing.Events) != 3 {
		t.Fatalf(
			"metadata filters: %v %v %v",
			eventIDs(equals.Events),
			eventIDs(exists.Events),
			eventIDs(missing.Events),
		)
	}

	own := list(&data.ListEventsInput{Filter: &datatypes.FilterInput{
		Branch: &datatypes.BranchFilter{Name: new("alternative"), IncludeParentBranches: new(false)},
	}})
	withParents := list(&data.ListEventsInput{Filter: &datatypes.FilterInput{
		Branch: &datatypes.BranchFilter{Name: new("alternative"), IncludeParentBranches: new(true)},
	}})
	if strings.Join(eventIDs(own.Events), ",") != *branched.EventId+","+*followUp.EventId ||
		strings.Join(eventIDs(withParents.Events), ",") != strings.Join([]string{
			*earliest.EventId, *first.EventId, *branched.EventId, *followUp.EventId,
		}, ",") {
		t.Fatalf("branch filters: %v %v", eventIDs(own.Events), eventIDs(withParents.Events))
	}

	// A client token makes the retried request return the event it made.
	token1 := new("event-token-1")
	tokenEvent := createEvent(t, c, id, "bob", "chat", at(5), &data.CreateEventInput{
		Payload: []datatypes.PayloadType{text(datatypes.RoleUser, "once")}, ClientToken: token1,
	})
	retried := createEvent(t, c, id, "bob", "chat", at(5), &data.CreateEventInput{
		Payload: []datatypes.PayloadType{text(datatypes.RoleUser, "once")}, ClientToken: token1,
	})
	bobs, err := c.ListEvents(ctx, &data.ListEventsInput{MemoryId: &id, ActorId: new("bob"), SessionId: new("chat")})
	if err != nil || *retried.EventId != *tokenEvent.EventId || len(bobs.Events) != 1 {
		t.Fatalf("token: %+v %+v %v", retried, bobs, err)
	}

	// Actors with a slash in their ID are kept apart from the path that stores them.
	createEvent(t, c, id, "team/carol:1", "standup", at(6), &data.CreateEventInput{
		Payload: []datatypes.PayloadType{text(datatypes.RoleUser, "morning")},
	})
	createEvent(t, c, id, "alice", "second", at(7), &data.CreateEventInput{
		Payload: []datatypes.PayloadType{text(datatypes.RoleUser, "again")},
	})
	actors, err := c.ListActors(ctx, &data.ListActorsInput{MemoryId: &id, MaxResults: new(int32(2))})
	if err != nil || len(actors.ActorSummaries) != 2 || actors.NextToken == nil ||
		*actors.ActorSummaries[0].ActorId != "alice" || *actors.ActorSummaries[1].ActorId != "bob" {
		t.Fatalf("actors: %+v %v", actors, err)
	}

	actors, err = c.ListActors(ctx, &data.ListActorsInput{MemoryId: &id, NextToken: actors.NextToken})
	if err != nil || len(actors.ActorSummaries) != 1 || *actors.ActorSummaries[0].ActorId != "team/carol:1" {
		t.Fatalf("actors rest: %+v %v", actors, err)
	}

	sessions, err := c.ListSessions(
		ctx,
		&data.ListSessionsInput{MemoryId: &id, ActorId: new("alice"), MaxResults: new(int32(1))},
	)
	if err != nil || len(sessions.SessionSummaries) != 1 || *sessions.SessionSummaries[0].SessionId != "chat" ||
		sessions.NextToken == nil || sessions.SessionSummaries[0].CreatedAt == nil {
		t.Fatalf("sessions: %+v %v", sessions, err)
	}

	sessions, err = c.ListSessions(
		ctx,
		&data.ListSessionsInput{MemoryId: &id, ActorId: new("alice"), NextToken: sessions.NextToken},
	)
	if err != nil || len(sessions.SessionSummaries) != 1 || *sessions.SessionSummaries[0].SessionId != "second" {
		t.Fatalf("sessions rest: %+v %v", sessions, err)
	}

	carol, err := c.ListSessions(ctx, &data.ListSessionsInput{MemoryId: &id, ActorId: new("team/carol:1")})
	if err != nil || len(carol.SessionSummaries) != 1 || *carol.SessionSummaries[0].ActorId != "team/carol:1" {
		t.Fatalf("slash actor: %+v %v", carol, err)
	}

	deleted, err := c.DeleteEvent(ctx, &data.DeleteEventInput{
		MemoryId: &id, ActorId: new("alice"), SessionId: new("chat"), EventId: second.EventId,
	})
	if err != nil || *deleted.EventId != *second.EventId {
		t.Fatalf("delete: %+v %v", deleted, err)
	}

	_, err = c.GetEvent(ctx, &data.GetEventInput{
		MemoryId: &id, ActorId: new("alice"), SessionId: new("chat"), EventId: second.EventId,
	})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	_, err = c.DeleteEvent(ctx, &data.DeleteEventInput{
		MemoryId: &id, ActorId: new("alice"), SessionId: new("chat"), EventId: second.EventId,
	})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	if len(list(&data.ListEventsInput{}).Events) != 4 {
		t.Fatalf("after delete: %v", eventIDs(list(&data.ListEventsInput{}).Events))
	}

	// The memory can be addressed by its ARN.
	arnList, err := c.ListEvents(ctx, &data.ListEventsInput{
		MemoryId: memoryRecord.Arn, ActorId: new("bob"), SessionId: new("chat"),
	})
	if err != nil || len(arnList.Events) != 1 {
		t.Fatalf("memory ARN: %+v %v", arnList, err)
	}
}

func TestAgentCoreEventValidation(t *testing.T) {
	t.Parallel()
	s := server(t)
	ctx := context.Background()
	admin := agentCoreControl(s, "us-east-1")
	c := agentCoreData(s)
	memoryRecord := agentCoreMemory(t, admin, "checked_memory", 7)
	id := *memoryRecord.Id
	now := time.Now()
	event := func() *data.CreateEventInput {
		return &data.CreateEventInput{
			MemoryId: &id, ActorId: new("alice"), SessionId: new("chat"), EventTimestamp: &now,
			Payload: []datatypes.PayloadType{text(datatypes.RoleUser, "hi")},
		}
	}

	for name, tc := range map[string]struct {
		mutate func(*data.CreateEventInput)
		code   string
	}{
		"missing memory": {
			func(in *data.CreateEventInput) { in.MemoryId = new("missing-0123456789") }, "ResourceNotFoundException",
		},
		"foreign ARN": {func(in *data.CreateEventInput) {
			in.MemoryId = new("arn:aws:bedrock-agentcore:us-west-2:000000000000:memory/" + id)
		}, "ResourceNotFoundException"},
		"actor pattern": {func(in *data.CreateEventInput) { in.ActorId = new("-bad") }, "ValidationException"},
		"branch without root": {
			func(in *data.CreateEventInput) { in.Branch = &datatypes.Branch{Name: new("fork")} }, "ValidationException",
		},
		"branch unknown root": {func(in *data.CreateEventInput) {
			in.Branch = &datatypes.Branch{Name: new("fork"), RootEventId: new("1#abc")}
		}, "ValidationException"},
		"skip extraction ok": {func(in *data.CreateEventInput) { in.ExtractionMode = datatypes.ExtractionModeSkip }, ""},
	} {
		in := event()
		tc.mutate(in)
		_, err := c.CreateEvent(ctx, in)
		if tc.code == "" {
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}

			continue
		}

		if err == nil {
			t.Errorf("%s: accepted", name)

			continue
		}

		wantAccessAnalyzerError(t, err, tc.code)
	}

	// A memory that is not ACTIVE takes no events.
	young, err := admin.CreateMemory(
		ctx,
		&control.CreateMemoryInput{Name: new("young_memory"), EventExpiryDuration: new(int32(7))},
	)
	if err != nil {
		t.Fatal(err)
	}

	in := event()
	in.MemoryId = young.Memory.Id
	_, err = c.CreateEvent(ctx, in)
	wantAccessAnalyzerError(t, err, "ValidationException")
	_, err = c.ListActors(ctx, &data.ListActorsInput{MemoryId: new("missing-0123456789")})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	_, err = c.ListEvents(
		ctx,
		&data.ListEventsInput{MemoryId: &id, ActorId: new("alice"), SessionId: new("chat"), NextToken: new("!")},
	)
	wantAccessAnalyzerError(t, err, "ValidationException")
}

// eventKeys returns the keys of the stored events.
func eventKeys(t *testing.T, store storage.Store) []string {
	t.Helper()
	var keys []string
	if err := store.View(context.Background(), func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			if strings.Contains(key, "/bedrockagentcore/event/") ||
				strings.HasPrefix(key, "aws/bedrockagentcore/expiry/") ||
				strings.Contains(key, "/bedrockagentcore/event-token/") {
				keys = append(keys, key)
			}
		}

		return nil
	}); err != nil {
		t.Fatal(err)
	}

	return keys
}

func TestAgentCoreEventExpiryAndMemoryDeletion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := &memory.Store{}
	s, stop := serverStore(t, testConfig(), &fakeRuntime{}, store)
	t.Cleanup(stop)
	admin := agentCoreControl(s, "us-east-1")
	c := agentCoreData(s)
	kept := agentCoreMemory(t, admin, "kept_memory", 3)
	doomed := agentCoreMemory(t, admin, "doomed_memory", 3)
	now := time.Now()
	old := createEvent(t, c, *kept.Id, "alice", "chat", now, &data.CreateEventInput{
		Payload: []datatypes.PayloadType{text(datatypes.RoleUser, "old")}, ClientToken: new("expiring"),
	})
	fresh := createEvent(t, c, *kept.Id, "alice", "chat", now.Add(time.Second), &data.CreateEventInput{
		Payload: []datatypes.PayloadType{text(datatypes.RoleUser, "fresh")},
	})
	createEvent(t, c, *doomed.Id, "alice", "chat", now, &data.CreateEventInput{
		Payload: []datatypes.PayloadType{text(datatypes.RoleUser, "doomed")}, ClientToken: new("doomed"),
	})

	// An event lives eventExpiryDuration days from its creation: three days, which the test cannot wait for, so
	// it moves the old event's expiry into the past, as the clock passing it would.
	if err := store.Update(ctx, func(tx storage.WriteTx) error {
		for _, index := range tx.Keys("aws/bedrockagentcore/expiry/") {
			key, _ := tx.Get(index)
			if !strings.Contains(string(key), *old.EventId) {
				continue
			}

			record, _ := tx.Get(string(key))
			var fields map[string]any
			if err := json.Unmarshal(record, &fields); err != nil {
				return err
			}

			expired := time.Now().Add(-time.Minute).UTC()
			fields["Expires"] = expired
			encoded, err := json.Marshal(fields)
			if err != nil {
				return err
			}

			tx.Delete(index)
			tx.Put(string(key), encoded)
			tx.Put(
				"aws/bedrockagentcore/expiry/"+leftPad(strconv.FormatInt(expired.UnixMilli(), 10))+"/"+string(key),
				key,
			)
		}

		return nil
	}); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "the old event to expire", func() bool {
		_, err := c.GetEvent(ctx, &data.GetEventInput{
			MemoryId: kept.Id, ActorId: new("alice"), SessionId: new("chat"), EventId: old.EventId,
		})

		return err != nil
	})
	listed, err := c.ListEvents(
		ctx,
		&data.ListEventsInput{MemoryId: kept.Id, ActorId: new("alice"), SessionId: new("chat")},
	)
	if err != nil || len(listed.Events) != 1 || *listed.Events[0].EventId != *fresh.EventId {
		t.Fatalf("after expiry: %+v %v", listed, err)
	}

	// The token of an expired event is free again.
	again := createEvent(t, c, *kept.Id, "alice", "chat", now, &data.CreateEventInput{
		Payload: []datatypes.PayloadType{text(datatypes.RoleUser, "again")}, ClientToken: new("expiring"),
	})
	if *again.EventId == *old.EventId {
		t.Fatal("an expired event's token still points at it")
	}

	// Deleting a memory deletes its events with the index entries that point at them.
	before := len(eventKeys(t, store))
	if _, err = admin.DeleteMemory(ctx, &control.DeleteMemoryInput{MemoryId: doomed.Id}); err != nil {
		t.Fatal(err)
	}

	wantAgentMemory(t, admin, doomed.Id, "")
	for _, key := range eventKeys(t, store) {
		if strings.Contains(key, *doomed.Id) || strings.Contains(key, "doomed") {
			t.Fatalf("event state of the deleted memory remains: %s", key)
		}
	}

	if after := len(eventKeys(t, store)); after >= before || after == 0 {
		t.Fatalf("event keys %d before, %d after", before, after)
	}

	_, err = c.ListActors(ctx, &data.ListActorsInput{MemoryId: doomed.Id})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
}

func leftPad(s string) string { return strings.Repeat("0", 20-len(s)) + s }

func TestAgentCoreEventsSQLiteRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	admin := agentCoreControl(s, "us-east-1")
	kept := agentCoreMemory(t, admin, "durable_memory", 7)
	c := agentCoreData(s)
	event := createEvent(t, c, *kept.Id, "alice", "chat", time.Now(), &data.CreateEventInput{
		Payload: []datatypes.PayloadType{text(datatypes.RoleUser, "remember")}, ClientToken: new("durable"),
	})

	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	defer stop()

	c = agentCoreData(s)
	got, err := c.GetEvent(ctx, &data.GetEventInput{
		MemoryId: kept.Id, ActorId: new("alice"), SessionId: new("chat"), EventId: event.EventId,
	})
	if err != nil || len(got.Event.Payload) != 1 {
		t.Fatalf("restarted event: %+v %v", got, err)
	}

	retried := createEvent(t, c, *kept.Id, "alice", "chat", time.Now(), &data.CreateEventInput{
		Payload: []datatypes.PayloadType{text(datatypes.RoleUser, "remember")}, ClientToken: new("durable"),
	})
	if *retried.EventId != *event.EventId {
		t.Fatalf("token after restart: %s, want %s", *retried.EventId, *event.EventId)
	}
}

func TestAgentCoreRecordsModeRuntimes(t *testing.T) {
	t.Parallel()
	s := server(t)
	ctx := context.Background()
	admin := agentCoreControl(s, "us-east-1")
	c := agentCoreData(s)
	role := agentCoreRole(t, s, "agent")
	made, err := admin.CreateAgentRuntime(ctx, agentRuntimeInput("records_agent", role))
	if err != nil {
		t.Fatal(err)
	}

	wantAgentRuntime(t, admin, made.AgentRuntimeId, controltypes.AgentRuntimeStatusReady)
	// Records mode keeps the runtime a record: invoking it fails with the validation error that says so, as
	// CodeBuild's StartBuild does, rather than returning an answer no agent gave.
	_, err = c.InvokeAgentRuntime(ctx, &data.InvokeAgentRuntimeInput{
		AgentRuntimeArn: made.AgentRuntimeArn, Payload: []byte(`{"prompt":"hi"}`),
	})
	wantAccessAnalyzerError(t, err, "ValidationException")
	if err == nil || !strings.Contains(err.Error(), "bedrockagentcore.runtimes: docker") {
		t.Fatalf("records mode error: %v", err)
	}

	// No container ever ran, so no session exists to stop; an unknown runtime is not found either.
	_, err = c.StopRuntimeSession(ctx, &data.StopRuntimeSessionInput{
		AgentRuntimeArn: made.AgentRuntimeArn, RuntimeSessionId: new(strings.Repeat("s", 40)),
	})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	_, err = c.StopRuntimeSession(ctx, &data.StopRuntimeSessionInput{
		AgentRuntimeArn:  new("arn:aws:bedrock-agentcore:us-east-1:000000000000:runtime/missing-0123456789"),
		RuntimeSessionId: new(strings.Repeat("s", 40)),
	})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
}
