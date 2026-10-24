package appsync

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/dop251/goja"

	"github.com/amjadjibon/tinystack/internal/aws/execute"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/appsync"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Event subscriptions are transient. Control-plane records survive a persistent-mode restart.
type eventHub struct {
	subscriptions map[*eventSubscription]struct{}
	mu            sync.Mutex
}

type eventSubscription struct {
	conn    *connection
	apiID   string
	channel string
	id      string
}

func (h *eventHub) add(sub *eventSubscription) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subscriptions == nil {
		h.subscriptions = map[*eventSubscription]struct{}{}
	}

	h.subscriptions[sub] = struct{}{}
}

func (h *eventHub) remove(sub *eventSubscription) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subscriptions, sub)
}

func (h *eventHub) broadcast(apiID, channel string, events []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subscriptions {
		prefix, wildcard := strings.CutSuffix(sub.channel, "/*")
		if sub.apiID == apiID && (sub.channel == channel || wildcard && strings.HasPrefix(channel, prefix+"/")) {
			sub.conn.sendEvent(map[string]any{keyMessageType: keyData, "id": sub.id, "event": events})
		}
	}
}

// sendEvent shares the GraphQL connection's bounded queue and lifecycle writer.
func (c *connection) sendEvent(value any) {
	b, err := json.Marshal(value)
	if err != nil {
		return
	}

	select {
	case c.out <- b:
	default:
		c.cancel()
	}
}

func (s *Service) eventAPI(ctx context.Context, id string) (*api.Api, request.Scope, error) {
	var scope request.Scope
	var a *api.Api
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		location, ok, err := storage.Read[location](tx, eventIndexKey(id))
		if err != nil || !ok {
			return err
		}

		scope = request.Scope{AccountID: location.Account, Region: location.Region}
		a, err = loadEventAPI(tx, scope, new(id))
		return err
	})
	return a, scope, err
}

func eventProviders(a *api.Api, modes []api.AuthMode, account string) *graph {
	g := &graph{api: &api.GraphqlApi{ApiId: a.ApiId, Owner: new(account)}}
	for _, p := range a.EventConfig.AuthProviders {
		if !slices.ContainsFunc(modes, func(m api.AuthMode) bool {
			return common.Value(m.AuthType) == common.Value(p.AuthType)
		}) {
			continue
		}

		var pool *api.CognitoUserPoolConfig
		if cfg := p.CognitoConfig; cfg != nil {
			pool = &api.CognitoUserPoolConfig{
				UserPoolId: cfg.UserPoolId, AwsRegion: cfg.AwsRegion,
				AppIdClientRegex: cfg.AppIdClientRegex,
			}
		}

		g.api.AdditionalAuthenticationProviders = append(g.api.AdditionalAuthenticationProviders,
			api.AdditionalAuthenticationProvider{
				AuthenticationType: p.AuthType, UserPoolConfig: pool,
				OpenIDConnectConfig: p.OpenIDConnectConfig, LambdaAuthorizerConfig: p.LambdaAuthorizerConfig,
			})
	}

	// A primary placeholder is never an accepted mode; additional providers carry the selected configuration.
	g.api.AuthenticationType = new("")
	return g
}

func (s *Service) authorizeEvent(ctx context.Context, a *api.Api, scope request.Scope, r *execute.Request,
	modes []api.AuthMode, operation, channel string,
) (*caller, error) {
	g := eventProviders(a, modes, scope.AccountID)
	if key := r.Header.Get("X-Api-Key"); key != "" {
		err := s.store.View(ctx, func(tx storage.ReadTx) error {
			k, ok, err := storage.Read[api.ApiKey](tx, owned(scope, kindAPIKey, *a.ApiId, key))
			g.keyValid = ok && s.now().Unix() < common.Value(k.Expires)
			return err
		})
		if err != nil {
			return nil, err
		}
	}

	return s.authorize(ctx, g, r, &query{eventOperation: operation, eventChannel: channel})
}

func (s *Service) eventNamespace(ctx context.Context, scope request.Scope, id, path string,
	subscribe bool,
) (namespace *api.ChannelNamespace, channelPath string, segments []string, loadErr error) {
	channel, parts, err := eventChannel(path, subscribe)
	if err != nil {
		return nil, "", nil, err
	}

	var n *api.ChannelNamespace
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var readErr error
		n, readErr = loadNamespace(tx, scope, new(id), new(parts[0]))
		return readErr
	})
	return n, channel, parts, err
}

// eventHandler runs the namespace's simple APPSYNC_JS function using the same utils and timeout as resolvers.
func eventHandler(code, name string, event map[string]any) (any, error) {
	if code == "" {
		return event["events"], nil
	}

	var appended []*fieldError
	j, err := newJS(event, &appended)
	if err != nil {
		return nil, err
	}

	defer j.stop()
	module, err := j.eventModule(code)
	if err != nil {
		return nil, err
	}

	value := module.Get(name)
	if goja.IsUndefined(value) {
		return event["events"], nil
	}

	fn, ok := goja.AssertFunction(value)
	if !ok {
		return nil, badRequest("Data source handler objects are not supported.")
	}

	result, err := j.call(fn)
	if err != nil {
		return nil, err
	}

	return j.export(result)
}

// eventModule evaluates the shared module compiler without requiring GraphQL request/response exports.
func (j *jsRun) eventModule(code string) (*goja.Object, error) {
	program, err := compile(code)
	if err != nil {
		return nil, err
	}

	factory, err := j.vm.RunProgram(program)
	if err != nil {
		return nil, j.failure(err)
	}

	build, _ := goja.AssertFunction(factory)
	module, err := build(goja.Undefined(), j.vm.ToValue(func(path string) goja.Value {
		return j.modules.ToObject(j.vm).Get(path)
	}))
	if err != nil {
		return nil, j.failure(err)
	}

	return module.ToObject(j.vm), nil
}

func validateEventHandlers(code string) error {
	var appended []*fieldError
	j, err := newJS(map[string]any{}, &appended)
	if err != nil {
		return err
	}

	defer j.stop()
	module, err := j.eventModule(code)
	if err != nil {
		return err
	}

	count := 0
	for _, name := range []string{"onPublish", "onSubscribe"} {
		value := module.Get(name)
		if goja.IsUndefined(value) {
			continue
		}

		if _, ok := goja.AssertFunction(value); !ok {
			return badRequest("Data source handler objects are not supported.")
		}

		count++
	}

	if count == 0 {
		return badRequest("codeHandlers must export onPublish or onSubscribe.")
	}

	return nil
}

func eventContext(n *api.ChannelNamespace, channel string, parts []string, operation string,
	who *caller, r *execute.Request, events any,
) map[string]any {
	return map[string]any{
		"events": events, "identity": who.identity, "stash": map[string]any{}, keyEventChannel: channel,
		"info": map[string]any{
			keyEventChannel:    map[string]any{"path": channel, "segments": parts},
			"channelNamespace": map[string]any{"name": *n.Name}, keyEventOperation: operation,
		},
		"request": map[string]any{keyEventHeaders: headers(r), "domainName": nil},
	}
}

type eventPublish struct {
	Channel string   `json:"channel"`
	Events  []string `json:"events"`
}

type incomingEvent struct {
	Payload any    `json:"payload"`
	ID      string `json:"id"`
}

func (s *Service) publishEvents(ctx context.Context, a *api.Api, scope request.Scope, r *execute.Request,
	in *eventPublish,
) (map[string]any, error) {
	n, channel, parts, err := s.eventNamespace(ctx, scope, *a.ApiId, in.Channel, false)
	if err != nil {
		return nil, err
	}

	if len(in.Events) < 1 || len(in.Events) > 5 {
		return nil, badRequest("Publish requires between one and five events.")
	}

	modes := n.PublishAuthModes
	if len(modes) == 0 {
		modes = a.EventConfig.DefaultPublishAuthModes
	}

	who, err := s.authorizeEvent(ctx, a, scope, r, modes, "EVENT_PUBLISH", channel)
	if err != nil {
		return nil, err
	}

	events := make([]incomingEvent, 0, len(in.Events))
	indexes := map[string]int{}
	for i, raw := range in.Events {
		var payload any
		if len(raw) > 240*1024 {
			return nil, badRequest("Event payload exceeds 240 KB.")
		}

		if json.Unmarshal([]byte(raw), &payload) != nil {
			return nil, badRequest("Each event must be a stringified JSON value.")
		}

		id := common.RandomID(32)
		indexes[id] = i
		events = append(events, incomingEvent{ID: id, Payload: payload})
	}

	value, err := eventHandler(common.Value(n.CodeHandlers), "onPublish",
		eventContext(n, channel, parts, "PUBLISH", who, r, events))
	if err != nil {
		return nil, err
	}

	b, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}

	var outgoing []struct {
		Error   any             `json:"error"`
		ID      string          `json:"id"`
		Payload json.RawMessage `json:"payload"`
	}

	if json.Unmarshal(b, &outgoing) != nil {
		return nil, badRequest("onPublish must return an array of events or null.")
	}

	successful, failed := []any{}, []any{}
	seen := map[string]bool{}
	broadcast := []string{}
	for _, e := range outgoing {
		if e.ID == "" && e.Payload == nil && e.Error == nil {
			continue
		}

		index, ok := indexes[e.ID]
		if !ok || seen[e.ID] {
			return nil, badRequest("onPublish returned an unknown or duplicate event ID.")
		}

		seen[e.ID] = true
		result := map[string]any{"identifier": e.ID, "index": index}
		if e.Error != nil {
			result["errorCode"], result["errorMessage"] = "CodeError", e.Error
			failed = append(failed, result)
		} else {
			if e.Payload == nil {
				return nil, badRequest("onPublish events require a payload.")
			}

			successful = append(successful, result)
			broadcast = append(broadcast, string(e.Payload))
		}
	}

	if len(broadcast) > 0 {
		s.eventSubscriptions.broadcast(*a.ApiId, channel, broadcast)
	}

	return map[string]any{"successful": successful, "failed": failed}, nil
}

func eventResponse(status int, body any) (*execute.Response, bool, error) {
	b, err := json.Marshal(body)
	return &execute.Response{
		Status: status, Body: b,
		Header: http.Header{"Content-Type": {"application/json"}},
	}, true, err
}

func eventErrors(err error) []map[string]string {
	kind := "BadRequestException"
	if fe, ok := errors.AsType[*fieldError](err); ok {
		kind = fe.Type
	}

	return []map[string]string{{"errorType": kind, "message": err.Error()}}
}

func (s *Service) executeEvent(ctx context.Context, id string, r *execute.Request) (*execute.Response, bool, error) {
	a, scope, err := s.eventAPI(ctx, id)
	if err != nil {
		return nil, true, err
	}

	if a == nil {
		return execute.Message(http.StatusNotFound, "Event API not found"), true, nil
	}

	if r.Method != http.MethodPost {
		return execute.Message(http.StatusMethodNotAllowed, "Use POST /event"), true, nil
	}

	ctx = request.WithMetadata(ctx, request.Metadata{Scope: scope})
	var in eventPublish
	if json.Unmarshal(r.Body, &in) != nil {
		return eventResponse(
			http.StatusBadRequest,
			map[string]any{keyEventErrors: eventErrors(badRequest("Invalid JSON."))},
		)
	}

	out, err := s.publishEvents(ctx, a, scope, r, &in)
	if err != nil {
		status := http.StatusBadRequest
		if _, ok := errors.AsType[*fieldError](err); ok {
			status = http.StatusForbidden
		}

		return eventResponse(status, map[string]any{keyEventErrors: eventErrors(err)})
	}

	return eventResponse(http.StatusOK, out)
}
