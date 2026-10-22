package apigatewayv2

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/execute"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/apigateway/proxy"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	socketIdleTimeout = 10 * time.Minute
	socketLifetime    = 2 * time.Hour
	socketMessage     = "MESSAGE"
)

type connection struct {
	idle         *time.Timer
	socket       execute.Socket
	cancel       context.CancelFunc
	call         *call
	connectedAt  time.Time
	lastActiveAt time.Time
	id           string
}

// ConnectionDetails describes a live WebSocket connection.
type ConnectionDetails struct {
	ConnectedAt  time.Time
	LastActiveAt time.Time
	SourceIP     string
	UserAgent    string
}

func gone() error {
	return &awserror.Error{Code: "GoneException", Message: "Connection no longer exists.", StatusCode: http.StatusGone}
}

// Stop closes all live WebSocket connections and prevents new ones.
func (s *Service) Stop() {
	s.connectionsMu.Lock()
	s.stopped = true
	for id, conn := range s.connections {
		delete(s.connections, id)
		conn.cancel()
	}

	s.connectionsMu.Unlock()
	s.connectionGroup.Wait()
}

// Reset closes connections before the backing store is reset.
func (s *Service) Reset(context.Context) {
	s.connectionsMu.Lock()
	defer s.connectionsMu.Unlock()
	for id, conn := range s.connections {
		delete(s.connections, id)
		conn.cancel()
	}
}

// ManageConnection scopes management calls to the owning account, Region, API and stage.
// POST sends data, GET returns connection details, and DELETE closes the connection.
func (s *Service) ManageConnection(ctx context.Context, scope request.Scope, apiID, stage, id, method string,
	data []byte,
) (*ConnectionDetails, error) {
	s.connectionsMu.Lock()
	conn, ok := s.connections[id]
	if !ok || conn.call.scope != scope || *conn.call.api.ApiId != apiID || *conn.call.stage.StageName != stage {
		s.connectionsMu.Unlock()
		return nil, gone()
	}

	details := &ConnectionDetails{
		ConnectedAt: conn.connectedAt, LastActiveAt: conn.lastActiveAt,
		SourceIP: conn.call.req.SourceIP, UserAgent: conn.call.req.Header.Get("User-Agent"),
	}

	if method == http.MethodDelete {
		delete(s.connections, id)
		conn.cancel()
	}

	s.connectionsMu.Unlock()

	if method == http.MethodPost {
		if len(data) > 128*1024 {
			return nil, &awserror.Error{
				Code:       "PayloadTooLargeException",
				Message:    "Payload exceeds 128 KiB.",
				StatusCode: 413,
			}
		}

		if !utf8.Valid(data) {
			return nil, awserror.New("NotImplemented", "Binary WebSocket payloads are not supported.")
		}

		sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := conn.socket.Send(sendCtx, data); err != nil {
			return nil, gone()
		}
	}

	if method == http.MethodPost {
		s.connectionsMu.Lock()
		if s.connections[id] == conn {
			conn.lastActiveAt = s.now().UTC()
			conn.idle.Reset(socketIdleTimeout)
		}

		s.connectionsMu.Unlock()
	}

	return details, nil
}

// Connect runs a WebSocket API connection until disconnect or expiry.
func (s *Service) Connect(ctx context.Context, id string, r *execute.Request, socket execute.Socket) (bool, error) {
	c := &call{req: r, requestID: newRequestID()}
	found := false
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		loc, ok, err := storage.Read[location](tx, indexKey(id))
		if err != nil || !ok {
			return err
		}

		c.scope = request.Scope{AccountID: loc.Account, Region: loc.Region}
		c.api, err = loadAPI(tx, c.scope, &id)
		if err != nil {
			return err
		}

		if common.Value(c.api.ProtocolType) != protocolWebSocket {
			return nil
		}

		found = true
		return c.resolveStage(tx)
	})
	if err != nil || !found {
		return found, err
	}

	if common.Value(c.api.DisableExecuteApiEndpoint) || c.stage == nil || c.snapshot == nil ||
		(c.path != "/" && (common.Value(c.stage.StageName) != defaultStage || r.Path != "/$default")) {
		return true, &awserror.Error{Code: "NotFoundException", Message: "Not Found", StatusCode: 404}
	}

	connCtx, cancel := context.WithTimeout(ctx, socketLifetime)
	defer cancel()
	now := s.now().UTC()
	conn := &connection{
		socket: socket, cancel: cancel, call: c, id: common.RandomID(16),
		connectedAt: now, lastActiveAt: now,
	}

	response, _ := s.socketEvent(connCtx, conn, "$connect", "CONNECT", nil)
	if response.Status < 200 || response.Status >= 300 {
		return true, &awserror.Error{
			Code:       "ForbiddenException",
			Message:    "Connection rejected",
			StatusCode: response.Status,
		}
	}

	s.connectionsMu.Lock()
	if s.stopped {
		s.connectionsMu.Unlock()
		return true, &awserror.Error{Code: "ServiceUnavailableException", Message: "Shutting down", StatusCode: 503}
	}

	err = socket.Accept("")
	if err == nil {
		socket.SetReadLimit(128 * 1024)
		conn.idle = time.AfterFunc(socketIdleTimeout, cancel)
		s.connections[conn.id] = conn
		s.connectionGroup.Add(1)
	}

	s.connectionsMu.Unlock()
	if err != nil {
		return true, err
	}

	defer func() {
		defer s.connectionGroup.Done()
		defer conn.idle.Stop()
		s.connectionsMu.Lock()
		delete(s.connections, conn.id)
		s.connectionsMu.Unlock()
		disconnectCtx, disconnectCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer disconnectCancel()
		_, _ = s.socketEvent(disconnectCtx, conn, "$disconnect", "DISCONNECT", nil)
	}()
	for {
		message, readErr := socket.Receive(connCtx)
		if readErr != nil || connCtx.Err() != nil {
			return true, nil
		}

		s.connectionsMu.Lock()
		conn.lastActiveAt = s.now().UTC()
		conn.idle.Reset(socketIdleTimeout)
		s.connectionsMu.Unlock()
		response, twoWay := s.socketEvent(connCtx, conn, "", socketMessage, message)
		if twoWay {
			sendCtx, sendCancel := context.WithTimeout(connCtx, 30*time.Second)
			sendErr := socket.Send(sendCtx, response.Body)
			sendCancel()
			if sendErr != nil {
				return true, nil
			}
		}
	}
}

func selectedRoute(expression string, body []byte) string {
	var value any
	if json.Unmarshal(body, &value) != nil {
		return defaultRoute
	}

	for _, field := range strings.Split(strings.TrimPrefix(expression, "$request.body."), ".") {
		object, ok := value.(map[string]any)
		if !ok {
			return defaultRoute
		}

		value = object[field]
	}

	key, ok := value.(string)
	if !ok || key == "" || strings.HasPrefix(key, "$") {
		return defaultRoute
	}

	return key
}

func (s *Service) socketEvent(ctx context.Context, conn *connection, key, eventType string,
	body []byte,
) (*execute.Response, bool) {
	c := *conn.call
	// Each event uses the stage's current deployment, including changes made after connecting.
	if eventType == socketMessage {
		loadErr := s.store.View(ctx, func(tx storage.ReadTx) error {
			var readErr error
			c.api, readErr = loadAPI(tx, c.scope, c.api.ApiId)
			if readErr != nil {
				return readErr
			}

			c.stage, c.snapshot = nil, nil
			return c.resolveStage(tx)
		})
		if loadErr != nil || c.stage == nil || c.snapshot == nil {
			return execute.Message(http.StatusInternalServerError, "Stage is unavailable"), false
		}

		key = selectedRoute(common.Value(c.api.RouteSelectionExpression), body)
	}

	c.requestID = newRequestID()
	c.route = nil
	for i := range c.snapshot.Routes {
		if common.Value(c.snapshot.Routes[i].RouteKey) == key {
			c.route = &c.snapshot.Routes[i]
			break
		}
	}

	if c.route == nil && eventType == socketMessage {
		for i := range c.snapshot.Routes {
			if common.Value(c.snapshot.Routes[i].RouteKey) == defaultRoute {
				c.route = &c.snapshot.Routes[i]
				break
			}
		}
	}

	if c.route == nil {
		return &execute.Response{Status: http.StatusOK}, false
	}

	twoWay := eventType == socketMessage && c.snapshot.Responses[common.Value(c.route.RouteId)]
	integration, ok := c.snapshot.Integrations[integrationID(common.Value(c.route.Target))]
	if !ok {
		return execute.Message(http.StatusInternalServerError, "No integration configured"), twoWay
	}

	if common.Value(integration.IntegrationType) == "MOCK" {
		return &execute.Response{Status: http.StatusOK}, twoWay
	}

	event := map[string]any{
		"requestContext": map[string]any{
			"routeKey": common.Value(c.route.RouteKey), "eventType": eventType, "connectionId": conn.id,
			"connectedAt": conn.connectedAt.UnixMilli(), "requestId": c.requestID, "apiId": *c.api.ApiId,
			"messageDirection": "IN", "requestTime": s.now().UTC().Format("02/Jan/2006:15:04:05 -0700"),
			"stage": *c.stage.StageName, "domainName": c.req.Host, "requestTimeEpoch": s.now().UnixMilli(),
			"identity": map[string]any{"sourceIp": c.req.SourceIP, "userAgent": c.req.Header.Get("User-Agent")},
		},
		"isBase64Encoded": false, "stageVariables": c.stage.StageVariables,
	}

	switch eventType {
	case "CONNECT":
		headers := map[string]string{}
		for name := range c.req.Header {
			headers[strings.ToLower(name)] = c.req.Header.Get(name)
		}

		query := map[string]string{}
		for name := range c.req.Query {
			query[name] = c.req.Query.Get(name)
		}

		event["headers"], event["multiValueHeaders"] = headers, c.req.Header
		event["queryStringParameters"], event["multiValueQueryStringParameters"] = query, c.req.Query
	case socketMessage:
		event["body"] = string(body)
	}

	payload, err := json.Marshal(event)
	if err != nil {
		return execute.Message(500, "Internal Server Error"), twoWay
	}

	uri := proxy.Substitute(common.Value(integration.IntegrationUri), c.stage.StageVariables)
	arn, _ := proxy.LambdaARN(uri)
	err = s.authorizer.Authorize(ctx, catalog.ServiceDelivery{
		Principal: "apigateway.amazonaws.com",
		SourceARN: "arn:aws:execute-api:" + c.scope.Region + ":" + c.scope.AccountID + ":" + *c.api.ApiId + "/" +
			*c.stage.StageName + "/" + *c.route.RouteKey, SourceAccount: c.scope.AccountID, Target: arn,
	})
	if err != nil {
		return execute.Message(500, "Internal Server Error"), twoWay
	}

	out, functionError, err := s.invoke(ctx, &c, arn, payload,
		time.Duration(common.Value(integration.TimeoutInMillis))*time.Millisecond)
	if err != nil {
		return execute.Message(500, "Internal Server Error"), twoWay
	}

	return proxy.ResponseV1(out, functionError, http.StatusInternalServerError), twoWay
}
