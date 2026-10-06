package appsync

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/vektah/gqlparser/v2/ast"

	"github.com/amjadjibon/tinystack/internal/aws/execute"
	"github.com/amjadjibon/tinystack/internal/aws/request"
)

const (
	// graphqlPath is the path of an API's GraphQL endpoint.
	graphqlPath = "/graphql"
	// messageError is the type of the real-time protocol's error messages.
	messageError = "error"
	// subprotocol is the WebSocket subprotocol of AppSync's real-time
	// endpoint.
	subprotocol = "graphql-ws"
	// connectionTimeout is how long a client waits for a keep-alive before
	// it reconnects, which connection_ack tells it.
	connectionTimeout = 5 * time.Minute
	// keepAlive is how often a connection sends "ka".
	keepAlive = time.Minute
	// outbox is how many messages a connection queues for a slow client
	// before it closes.
	outbox = 256
)

// message is a message of the real-time protocol.
type message struct {
	Payload any    `json:"payload,omitempty"`
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
}

// errorsPayload is the payload of an error message.
func errorsPayload(errs ...*gqlError) any {
	return map[string]any{"errors": errs}
}

// connection is one client's real-time connection to an API.
type connection struct {
	service *Service
	request *execute.Request
	out     chan []byte
	cancel  context.CancelFunc
	// subscriptions are the connection's by the IDs its client gave them.
	subscriptions map[string]*subscription
	socket        execute.Socket
	scope         request.Scope
	apiID         string
	ready         bool
}

// subscription is a started subscription operation.
type subscription struct {
	conn     *connection
	schema   *ast.Schema
	doc      *ast.QueryDocument
	op       *ast.OperationDefinition
	vars     map[string]any
	allowed  func(parent *ast.Definition, f *ast.FieldDefinition) bool
	args     map[string]any
	filter   any
	id       string
	mutation []string
}

// hub holds every API's subscriptions.
type hub struct {
	byAPI map[string]map[*subscription]struct{}
	mu    sync.Mutex
}

func (h *hub) add(apiID string, sub *subscription) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.byAPI == nil {
		h.byAPI = map[string]map[*subscription]struct{}{}
	}

	if h.byAPI[apiID] == nil {
		h.byAPI[apiID] = map[*subscription]struct{}{}
	}

	h.byAPI[apiID][sub] = struct{}{}
}

func (h *hub) remove(apiID string, sub *subscription) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.byAPI[apiID], sub)
	if len(h.byAPI[apiID]) == 0 {
		delete(h.byAPI, apiID)
	}
}

func (h *hub) list(apiID string) []*subscription {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*subscription, 0, len(h.byAPI[apiID]))
	for sub := range h.byAPI[apiID] {
		out = append(out, sub)
	}

	return out
}

// Connect serves a real-time connection: GET /graphql on the real-time host,
// or /graphql/realtime on the GraphQL endpoint, upgraded to a WebSocket.
func (s *Service) Connect(ctx context.Context, id string, r *execute.Request, socket execute.Socket) (bool, error) {
	scope, err := s.locate(ctx, id)
	if err != nil || scope.AccountID == "" || (r.Path != graphqlPath && r.Path != graphqlPath+"/realtime") {
		return false, err
	}

	if acceptErr := socket.Accept(subprotocol); acceptErr != nil {
		return true, acceptErr
	}

	ctx, cancel := context.WithCancel(request.WithMetadata(ctx, request.Metadata{Scope: scope}))
	defer cancel()
	c := &connection{
		service: s, socket: socket, request: r, scope: scope, apiID: id, out: make(chan []byte, outbox),
		cancel: cancel, subscriptions: map[string]*subscription{},
	}
	defer func() {
		for _, sub := range c.subscriptions {
			s.subscriptions.remove(id, sub)
		}
	}()

	done := make(chan struct{})
	if !s.bg.Go(func(stop context.Context) {
		defer close(done)
		c.write(ctx, stop)
	}) {
		return true, nil
	}

	defer func() {
		cancel()
		<-done
	}()

	return true, c.read(ctx)
}

// write sends queued messages and keep-alives until ctx or stop ends.
func (c *connection) write(ctx, stop context.Context) {
	t := time.NewTicker(keepAlive)
	defer t.Stop()
	for {
		var b []byte
		select {
		case <-ctx.Done():
			return
		case <-stop.Done():
			c.cancel()
			return
		case b = <-c.out:
		case <-t.C:
			b = []byte(`{"type":"ka"}`)
		}

		if c.socket.Send(ctx, b) != nil {
			c.cancel()
			return
		}
	}
}

// send queues m, closing the connection when its client is too slow to read
// what was queued before.
func (c *connection) send(m message) {
	b, err := json.Marshal(m)
	if err != nil {
		return
	}

	select {
	case c.out <- b:
	default:
		c.cancel()
	}
}

// read handles the client's messages until it closes the connection.
func (c *connection) read(ctx context.Context) error {
	for {
		b, err := c.socket.Receive(ctx)
		if err != nil {
			return nil //nolint:nilerr // The client closed the connection, or it ended.
		}

		var m struct {
			ID      string          `json:"id"`
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(b, &m) != nil {
			c.send(message{Type: messageError, Payload: errorsPayload(&gqlError{
				ErrorType: new("MalformedMessage"), Message: "The message is not JSON.",
			})})
			continue
		}

		switch {
		case m.Type == "connection_init":
			if initErr := c.init(ctx); initErr != nil {
				return initErr
			}

			if !c.ready {
				return nil
			}
		case !c.ready:
			c.send(message{ID: m.ID, Type: messageError, Payload: errorsPayload(&gqlError{
				ErrorType: new("UnsupportedOperation"), Message: "connection_init must come first.",
			})})
		case m.Type == "start":
			if startErr := c.start(ctx, m.ID, m.Payload); startErr != nil {
				return startErr
			}
		case m.Type == "stop":
			if sub := c.subscriptions[m.ID]; sub != nil {
				c.service.subscriptions.remove(c.apiID, sub)
				delete(c.subscriptions, m.ID)
			}

			c.send(message{ID: m.ID, Type: "complete"})
		default:
			c.send(message{ID: m.ID, Type: messageError, Payload: errorsPayload(&gqlError{
				ErrorType: new("UnsupportedOperation"), Message: fmt.Sprintf("Unknown message type %q.", m.Type),
			})})
		}
	}
}

// init authorizes the connection with the headers its client sent as the
// header query parameter or a header- subprotocol, as a request to
// /graphql/connect whose body is {}.
func (c *connection) init(ctx context.Context) error {
	header, err := connectionHeader(c.request)
	if err == nil {
		_, _, err = c.authorize(ctx, header, "/graphql/connect", "{}", &query{})
	}

	fe, ok := errors.AsType[*fieldError](err)
	if err != nil && !ok {
		return err
	}

	if ok {
		// The error goes out before the connection closes, so it is not
		// queued.
		b, marshalErr := json.Marshal(message{Type: "connection_error", Payload: errorsPayload(&gqlError{
			ErrorType: new(fe.Type), Message: fe.Message,
		})})
		if marshalErr != nil {
			return marshalErr
		}

		_ = c.socket.Send(ctx, b)
		return nil
	}

	c.ready = true
	c.send(message{Type: "connection_ack", Payload: map[string]any{
		"connectionTimeoutMs": connectionTimeout.Milliseconds(),
	}})
	c.send(message{Type: "ka"})
	return nil
}

// connectionHeader decodes the headers a client authorizes its connection
// with: base64 JSON in the header query parameter, or base64url JSON in a
// header-<value> subprotocol.
func connectionHeader(r *execute.Request) (http.Header, error) {
	encoded, decode := r.Query.Get("header"), base64.StdEncoding.DecodeString
	if encoded == "" {
		for p := range strings.SplitSeq(r.Header.Get("Sec-WebSocket-Protocol"), ",") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(p), "header-"); ok {
				encoded, decode = strings.TrimRight(v, "="), base64.RawURLEncoding.DecodeString
			}
		}
	}

	b, err := decode(encoded)
	var values map[string]string
	if err != nil || json.Unmarshal(b, &values) != nil {
		return nil, &fieldError{
			Type: "UnauthorizedException", Message: "The connection's header is not base64-encoded JSON headers.",
		}
	}

	return toHeader(values), nil
}

func toHeader(values map[string]string) http.Header {
	header := http.Header{}
	for k, v := range values {
		header.Set(k, v)
	}

	return header
}

// authorize authorizes a request the client describes, a POST of body to
// path with header, returning the API it reads and its caller.
func (c *connection) authorize(ctx context.Context, header http.Header, path, body string, q *query) (
	*graph, *caller, error,
) {
	g, err := c.service.load(ctx, c.scope, c.apiID, header.Get("X-Api-Key"))
	if err != nil {
		return nil, nil, err
	}

	r := &execute.Request{
		Time: c.service.now().UTC(), Header: header, Method: http.MethodPost, Path: path, Host: header.Get("Host"),
		Endpoint: c.request.Endpoint, SourceIP: c.request.SourceIP, Protocol: c.request.Protocol, Body: []byte(body),
	}
	if strings.HasPrefix(header.Get("Authorization"), "AWS4-") {
		r.Caller = c.socket.Caller(ctx, header, path, r.Body)
	}

	who, err := c.service.authorize(ctx, g, r, q)
	return g, who, err
}

// start starts a subscription: its payload's data is the GraphQL request,
// which extensions.authorization authorizes as a POST to /graphql.
func (c *connection) start(ctx context.Context, id string, raw json.RawMessage) error {
	fail := func(errs ...*gqlError) error {
		c.send(message{ID: id, Type: messageError, Payload: errorsPayload(errs...)})
		return nil
	}
	var p struct {
		Extensions struct {
			Authorization map[string]string `json:"authorization"`
		} `json:"extensions"`
		Data string `json:"data"`
	}
	var q query
	if json.Unmarshal(raw, &p) != nil || json.Unmarshal([]byte(p.Data), &q) != nil || q.Query == "" {
		return fail(&gqlError{
			ErrorType: new("MalformedHttpRequestException"), Message: "The payload's data is not a GraphQL request.",
		})
	}

	header := toHeader(p.Extensions.Authorization)
	g, who, err := c.authorize(ctx, header, graphqlPath, p.Data, &q)
	if fe, ok := errors.AsType[*fieldError](err); ok {
		return fail(&gqlError{ErrorType: new(fe.Type), Message: fe.Message})
	}

	if err != nil {
		return err
	}

	if g.schema == nil || g.schema.Subscription == nil {
		return fail(&gqlError{ErrorType: new("ValidationError"), Message: "The API's schema has no subscriptions."})
	}

	r := &execute.Request{Header: header, Method: http.MethodPost, Path: graphqlPath, SourceIP: c.request.SourceIP}
	e, errs := c.service.prepare(g, who, r, &q)
	if errs != nil {
		return fail(errs...)
	}

	if e.op.Operation != ast.Subscription {
		return fail(&gqlError{
			ErrorType: new("UnsupportedOperation"), Message: "The real-time endpoint runs subscriptions only.",
		})
	}

	root := g.schema.Subscription
	groups := e.collect(root, e.op.SelectionSet, nil)
	if len(groups) != 1 || root.Fields.ForName(groups[0].fields[0].Name) == nil {
		return fail(&gqlError{
			ErrorType: new("ValidationError"), Message: "A subscription selects exactly one field of Subscription.",
		})
	}

	f := groups[0].fields[0]
	def := root.Fields.ForName(f.Name)
	if !e.allowed(root, def) {
		return fail(&gqlError{ErrorType: new(typeUnauthorized), Message: fmt.Sprintf(
			"Not Authorized to access %s on type %s", f.Name, root.Name)})
	}

	sub := &subscription{
		conn: c, schema: g.schema, doc: e.doc, op: e.op, vars: e.vars, allowed: e.allowed, id: id,
		args: map[string]any{}, mutation: subscribedMutations(def),
	}
	for k, v := range f.ArgumentMap(e.vars) {
		if v != nil {
			sub.args[k] = normalize(v)
		}
	}

	// The field's resolver runs once, now: it may refuse the subscription or
	// set an enhanced filter.
	s := &session{service: c.service, graph: g, caller: who, request: r, vars: e.vars}
	_, _, err = s.resolve(ctx, &resolution{
		args: f.ArgumentMap(e.vars), parent: root, def: def, group: groups[0], path: []any{groups[0].key},
	})
	if fe, ok := errors.AsType[*fieldError](err); ok {
		return fail(&gqlError{ErrorType: new(fe.Type), Message: fe.Message, Path: []any{groups[0].key}})
	}

	if err != nil {
		return err
	}

	if s.filter != nil {
		if filterErr := checkFilter(s.filter); filterErr != nil {
			return fail(&gqlError{ErrorType: new(errorCode), Message: filterErr.Error()})
		}

		sub.filter = normalize(s.filter)
	}

	if old := c.subscriptions[id]; old != nil {
		c.service.subscriptions.remove(c.apiID, old)
	}

	c.subscriptions[id] = sub
	c.service.subscriptions.add(c.apiID, sub)
	c.send(message{ID: id, Type: "start_ack"})
	return nil
}

// subscribedMutations are the mutations a subscription field's
// @aws_subscribe names.
func subscribedMutations(def *ast.FieldDefinition) []string {
	d := def.Directives.ForName("aws_subscribe")
	if d == nil {
		return nil
	}

	var out []string
	if list := d.Arguments.ForName("mutations"); list != nil {
		for _, m := range list.Value.Children {
			out = append(out, m.Value.Raw)
		}
	}

	return out
}

// publish sends each root field of a mutation's data to the subscriptions
// that subscribe to it.
func (s *Service) publish(ctx context.Context, apiID string, data any) {
	root, ok := data.(object)
	if !ok {
		return
	}

	subs := s.subscriptions.list(apiID)
	for _, e := range root {
		if e.value == nil || len(subs) == 0 {
			continue
		}

		value := normalize(plainOutput(e.value))
		for _, sub := range subs {
			if slices.Contains(sub.mutation, e.name) && sub.matches(value) {
				sub.deliver(ctx, value)
			}
		}
	}
}

// matches reports whether a mutation's value passes the subscription's
// arguments, each of which must equal the value's field of the same name,
// and its enhanced filter.
func (sub *subscription) matches(value any) bool {
	m, _ := value.(map[string]any)
	for k, want := range sub.args {
		if !reflect.DeepEqual(m[k], want) {
			return false
		}
	}

	return sub.filter == nil || filterMatches(sub.filter, m)
}

// deliver sends value to the subscription, selected as its selection set
// asks.
func (sub *subscription) deliver(ctx context.Context, value any) {
	e := &executor{
		schema: sub.schema, doc: sub.doc, op: sub.op, vars: sub.vars, allowed: sub.allowed,
		resolve: func(_ context.Context, r *resolution) (any, bool, error) {
			return value, r.parent == sub.schema.Subscription, nil
		},
	}
	data, errs := e.run(ctx)
	sub.conn.send(message{ID: sub.id, Type: keyData, Payload: result{data, errs}})
}

// plainOutput converts response objects into maps keyed by field name, as
// subscriptions read a mutation's data whatever its aliases.
func plainOutput(v any) any {
	switch t := v.(type) {
	case object:
		out := make(map[string]any, len(t))
		for _, e := range t {
			out[e.name] = plainOutput(e.value)
		}

		return out
	case []any:
		out := make([]any, 0, len(t))
		for _, item := range t {
			out = append(out, plainOutput(item))
		}

		return out
	}

	return v
}

// normalize gives a value JSON's Go types, so values from arguments,
// responses, and code compare equal.
func normalize(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}

	var out any
	if json.Unmarshal(b, &out) != nil {
		return v
	}

	return out
}
