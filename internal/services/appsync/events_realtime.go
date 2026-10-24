package appsync

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/execute"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
)

var eventOperationID = regexp.MustCompile(`^[a-zA-Z0-9_+\-]{1,128}$`)

func (s *Service) connectEvent(
	ctx context.Context,
	id string,
	r *execute.Request,
	socket execute.Socket,
) (bool, error) {
	a, scope, err := s.eventAPI(ctx, id)
	if err != nil || a == nil {
		return false, err
	}

	if acceptErr := socket.Accept("aws-appsync-event-ws"); acceptErr != nil {
		return true, acceptErr
	}

	ctx, cancel := context.WithTimeout(request.WithMetadata(ctx, request.Metadata{Scope: scope}), 24*time.Hour)
	defer cancel()
	c := &connection{
		service: s, request: r, socket: socket, scope: scope, apiID: id,
		out: make(chan []byte, outbox), cancel: cancel,
	}

	header, err := connectionHeader(r)
	if err == nil && header.Get("Host") != a.Dns["HTTP"] {
		err = errUnauthorized
	}

	if err == nil {
		authRequest := c.eventRequest(ctx, header, []byte("{}"))
		_, err = s.authorizeEvent(ctx, a, scope, authRequest, a.EventConfig.ConnectionAuthModes, "EVENT_CONNECT", "")
	}

	if err != nil {
		b, marshalErr := json.Marshal(
			map[string]any{keyMessageType: "connection_error", keyEventErrors: eventErrors(err)},
		)
		if marshalErr != nil {
			return true, marshalErr
		}

		return true, socket.Send(ctx, b)
	}

	done := make(chan struct{})
	if !s.bg.Go(func(stop context.Context) {
		defer close(done)
		c.write(ctx, stop)
	}) {
		return true, nil
	}

	subs := map[string]*eventSubscription{}
	defer func() {
		for _, sub := range subs {
			s.eventSubscriptions.remove(sub)
		}

		cancel()
		<-done
	}()

	fail := func(id, kind string, err error) {
		c.sendEvent(map[string]any{keyMessageType: kind, "id": id, keyEventErrors: eventErrors(err)})
	}

	c.sendEvent(map[string]any{keyMessageType: "ka"})
	for {
		b, err := socket.Receive(ctx)
		if err != nil {
			return true, nil //nolint:nilerr // A closed connection has no service error.
		}

		var m struct {
			Authorization map[string]string `json:"authorization"`
			ID            string            `json:"id"`
			Type          string            `json:"type"`
			Channel       string            `json:"channel"`
			Events        []string          `json:"events"`
		}

		if json.Unmarshal(b, &m) != nil {
			fail("", messageError, badRequest("Invalid JSON message."))
			continue
		}

		if m.Type == "connection_init" {
			c.sendEvent(
				map[string]any{
					keyMessageType:        "connection_ack",
					"connectionTimeoutMs": connectionTimeout.Milliseconds(),
				},
			)
			continue
		}

		if !eventOperationID.MatchString(m.ID) {
			fail(m.ID, m.Type+"_error", badRequest("Invalid operation ID."))
			continue
		}

		// Configuration updates take effect on the next operation, including on existing connections.
		current, _, err := s.eventAPI(ctx, id)
		if err != nil || current == nil {
			fail(m.ID, m.Type+"_error", notFound("Event API no longer exists."))
			continue
		}

		switch m.Type {
		case "subscribe":
			if subs[m.ID] != nil {
				fail(m.ID, "subscribe_error", badRequest("Duplicate subscription ID."))
				continue
			}

			ns, channel, parts, err := s.eventNamespace(ctx, scope, id, m.Channel, true)
			if err != nil {
				fail(m.ID, "subscribe_error", err)
				continue
			}

			modes := ns.SubscribeAuthModes
			if len(modes) == 0 {
				modes = current.EventConfig.DefaultSubscribeAuthModes
			}

			body, err := json.Marshal(map[string]string{keyEventChannel: m.Channel})
			if err != nil {
				return true, err
			}

			req := c.eventRequest(ctx, toHeader(m.Authorization), body)
			who, err := s.authorizeEvent(ctx, current, scope, req, modes, "EVENT_SUBSCRIBE", channel)
			if err != nil {
				fail(m.ID, "subscribe_error", err)
				continue
			}

			if _, err := eventHandler(common.Value(ns.CodeHandlers), "onSubscribe",
				eventContext(ns, channel, parts, "SUBSCRIBE", who, req, nil)); err != nil {
				fail(m.ID, "subscribe_error", err)
				continue
			}

			sub := &eventSubscription{conn: c, apiID: id, channel: channel, id: m.ID}
			subs[m.ID] = sub
			s.eventSubscriptions.add(sub)
			c.sendEvent(map[string]any{keyMessageType: "subscribe_success", "id": m.ID})
		case "unsubscribe":
			sub := subs[m.ID]
			if sub == nil {
				fail(m.ID, "unsubscribe_error", badRequest("Unknown subscription ID."))
				continue
			}

			s.eventSubscriptions.remove(sub)
			delete(subs, m.ID)
			c.sendEvent(map[string]any{keyMessageType: "unsubscribe_success", "id": m.ID})
		case "publish":
			in := &eventPublish{Channel: m.Channel, Events: m.Events}
			body, err := json.Marshal(in)
			if err != nil {
				return true, err
			}

			req := c.eventRequest(ctx, toHeader(m.Authorization), body)
			out, err := s.publishEvents(ctx, current, scope, req, in)
			if err != nil {
				fail(m.ID, "publish_error", err)
				continue
			}

			out[keyMessageType], out["id"] = "publish_success", m.ID
			c.sendEvent(out)
		default:
			fail(m.ID, messageError, badRequest("Unknown message type %s.", m.Type))
		}
	}
}

func (c *connection) eventRequest(ctx context.Context, header http.Header, body []byte) *execute.Request {
	r := &execute.Request{
		Time: c.service.now().UTC(), Header: header, Method: http.MethodPost,
		Path: "/event", Body: body, Host: header.Get("Host"), Endpoint: c.request.Endpoint,
		SourceIP: c.request.SourceIP, Protocol: c.request.Protocol,
	}

	if strings.HasPrefix(header.Get("Authorization"), "AWS4-") {
		r.Caller = c.socket.Caller(ctx, header, "/event", body)
	}

	return r
}
