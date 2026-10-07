package iot

import (
	"context"
	"encoding/base64"
	"maps"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/packets"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/iotdataplane"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// maxDataPage is the most items a data plane list returns at once.
const maxDataPage = 200

// DataPlane serves the IoT data plane API from the service's brokers and
// store.
type DataPlane struct{ s *Service }

var _ api.API = (*DataPlane)(nil)

// DataPlane returns the service's data plane API.
func (s *Service) DataPlane() *DataPlane { return &DataPlane{s: s} }

// Publish publishes a message as the data plane does, with its MQTT 5
// properties.
func (d *DataPlane) Publish(ctx context.Context, in *api.PublishRequest) (*api.PublishOutput, error) {
	topic := common.Value(in.Topic)
	switch {
	case !withinLimits(topic) || topic == "":
		return nil, invalid("The topic %q is too long or has too many levels.", topic)
	case !publishable(topic):
		return nil, invalid("Publishing to the reserved topic %s is not supported.", topic)
	case common.Value(in.Qos) < 0 || common.Value(in.Qos) > 1:
		return nil, invalid("The QoS must be 0 or 1.")
	case len(in.Payload) > maxPayload:
		return nil, invalid("The payload is larger than %d bytes.", maxPayload)
	}

	var qos byte
	if common.Value(in.Qos) == 1 {
		qos = 1
	}

	pk := packets.Packet{
		FixedHeader: packets.FixedHeader{Type: packets.Publish, Qos: qos, Retain: common.Value(in.Retain)},
		TopicName:   topic,
		Payload:     in.Payload,
		// The broker never acknowledges its own messages, but a QoS 1
		// message needs a packet ID.
		PacketID: uint16(qos),
		Properties: packets.Properties{
			ContentType: common.Value(in.ContentType), ResponseTopic: common.Value(in.ResponseTopic),
		},
	}
	if err := publishProperties(&pk.Properties, in); err != nil {
		return nil, err
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	b, err := d.s.broker(scope)
	if err != nil {
		return nil, err
	}

	if injectErr := b.server.InjectPacket(b.data, pk); injectErr != nil {
		return nil, injectErr
	}

	return &api.PublishOutput{}, nil
}

// publishProperties sets the MQTT 5 properties a request carries in its
// headers and query.
func publishProperties(p *packets.Properties, in *api.PublishRequest) error {
	switch common.Value(in.PayloadFormatIndicator) {
	case "":
	case "UNSPECIFIED_BYTES":
		p.PayloadFormatFlag = true
	case "UTF8_DATA":
		p.PayloadFormat, p.PayloadFormatFlag = 1, true
	default:
		return invalid("The payload format indicator must be UNSPECIFIED_BYTES or UTF8_DATA.")
	}

	if in.CorrelationData != nil {
		data, err := base64.StdEncoding.DecodeString(*in.CorrelationData)
		if err != nil {
			return invalid("The correlation data must be base64.")
		}

		p.CorrelationData = data
	}

	if expiry := common.Value(in.MessageExpiry); expiry != 0 {
		if expiry < 0 || expiry > 7*24*3600 {
			return invalid("The message expiry must be between 0 and 604800 seconds.")
		}

		p.MessageExpiryInterval = uint32(expiry)
	}

	return nil
}

func (d *DataPlane) GetRetainedMessage(
	ctx context.Context,
	in *api.GetRetainedMessageRequest,
) (*api.GetRetainedMessageResponse, error) {
	var r *retained
	err := d.s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		var found bool
		var err error
		r, found, err = storage.Read[retained](tx, retainedKey(scope, common.Value(in.Topic)))
		if err == nil && !found {
			err = notFound("No retained message exists for topic %s.", common.Value(in.Topic))
		}

		return err
	})
	if err != nil {
		return nil, err
	}

	return &api.GetRetainedMessageResponse{
		Topic: new(r.Topic), Payload: r.Payload, Qos: new(int64(r.Qos)), LastModifiedTime: new(r.Modified),
	}, nil
}

func (d *DataPlane) ListRetainedMessages(
	ctx context.Context,
	in *api.ListRetainedMessagesRequest,
) (*api.ListRetainedMessagesResponse, error) {
	var out []api.RetainedMessageSummary
	err := d.s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		records, err := storage.Records[retained](tx, scope.Prefix(service, kindRetained))
		for _, r := range records {
			out = append(out, api.RetainedMessageSummary{
				Topic: new(r.Topic), PayloadSize: new(int64(len(r.Payload))), Qos: new(int64(r.Qos)),
				LastModifiedTime: new(r.Modified),
			})
		}

		return err
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := dataPage(out, func(r api.RetainedMessageSummary) string { return *r.Topic }, in.NextToken,
		in.MaxResults)
	if err != nil {
		return nil, err
	}

	return &api.ListRetainedMessagesResponse{RetainedTopics: selected, NextToken: next}, nil
}

// dataPage pages a data plane list, whose pages hold up to 200 items.
func dataPage[T any](items []T, id func(T) string, token *string, size *int64) (
	selected []T, next *string, err error,
) {
	n := int(common.Value(size))
	switch {
	case size == nil:
		n = maxDataPage
	case n < 1 || n > maxDataPage:
		return nil, nil, invalid("maxResults must be between 1 and %d.", maxDataPage)
	}

	return common.Page(items, id, token, n, invalid("The pagination token is invalid."))
}

// client returns the broker and session of a client ID in the caller's
// scope: a connected client or a disconnected persistent session.
func (d *DataPlane) client(ctx context.Context, id *string) (*broker, *mqtt.Client, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, nil, err
	}

	if b := d.s.existing(scope); b != nil {
		if cl, ok := b.server.Clients.Get(common.Value(id)); ok && !cl.Net.Inline {
			return b, cl, nil
		}
	}

	return nil, nil, notFound("No connection or session exists for client %s.", common.Value(id))
}

func (d *DataPlane) GetConnection(
	ctx context.Context,
	in *api.GetConnectionRequest,
) (*api.GetConnectionResponse, error) {
	b, cl, err := d.client(ctx, in.ClientId)
	if err != nil {
		return nil, err
	}

	connected := !cl.Closed()
	v5 := cl.Properties.ProtocolVersion == 5
	out := &api.GetConnectionResponse{
		ClientId: new(cl.ID), Connected: new(connected), KeepAliveDuration: new(int64(cl.State.Keepalive)),
		CleanSession: new((v5 && cl.Properties.Props.SessionExpiryInterval == 0) || (!v5 && cl.Properties.Clean)),
	}
	if v5 {
		out.SessionExpiry = new(int64(cl.Properties.Props.SessionExpiryInterval))
	}

	b.mu.Lock()
	if info := b.connections[cl.ID]; info != nil {
		out.ConnectedSince = new(info.connected.UnixMilli())
		if info.thing != "" {
			out.ThingName = new(info.thing)
		}

		if !connected {
			out.DisconnectedSince, out.DisconnectReason = new(info.disconnected.UnixMilli()), new(info.reason)
		}
	}
	b.mu.Unlock()
	if common.Value(in.IncludeSocketInformation) {
		out.SourceIp, out.SourcePort = splitAddress(cl.Net.Remote)
		if cl.Net.Conn != nil {
			out.TargetIp, out.TargetPort = splitAddress(cl.Net.Conn.LocalAddr().String())
		}
	}

	return out, nil
}

// splitAddress splits an address into its host and port.
func splitAddress(addr string) (ip *string, port *int64) {
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		return new(addr), nil
	}

	n, err := strconv.ParseInt(p, 10, 64)
	if err != nil {
		return new(h), nil
	}

	return new(h), new(n)
}

// DeleteConnection disconnects a client and, with cleanSession, ends its
// session; preventWillMessage drops its will first.
func (d *DataPlane) DeleteConnection(
	ctx context.Context,
	in *api.DeleteConnectionRequest,
) (*api.DeleteConnectionOutput, error) {
	b, cl, err := d.client(ctx, in.ClientId)
	if err != nil {
		return nil, err
	}

	if common.Value(in.PreventWillMessage) {
		atomic.StoreUint32(&cl.Properties.Will.Flag, 0)
	}

	b.mu.Lock()
	if info := b.connections[cl.ID]; info != nil {
		info.reason = "API_INITIATED_DISCONNECT"
	}
	b.mu.Unlock()
	if !cl.Closed() {
		_ = b.server.DisconnectClient(cl, packets.ErrAdministrativeAction)
	}

	if common.Value(in.CleanSession) {
		cl.ClearInflights()
		b.server.UnsubscribeClient(cl)
		b.server.Clients.Delete(cl.ID)
		b.mu.Lock()
		delete(b.connections, cl.ID)
		b.mu.Unlock()
	}

	return &api.DeleteConnectionOutput{}, nil
}

func (d *DataPlane) ListSubscriptions(
	ctx context.Context,
	in *api.ListSubscriptionsRequest,
) (*api.ListSubscriptionsResponse, error) {
	_, cl, err := d.client(ctx, in.ClientId)
	if err != nil {
		return nil, err
	}

	subs := cl.State.Subscriptions.GetAll()
	out := make([]api.SubscriptionSummary, 0, len(subs))
	for _, filter := range slices.Sorted(maps.Keys(subs)) {
		out = append(out, api.SubscriptionSummary{TopicFilter: new(filter), Qos: new(int64(subs[filter].Qos))})
	}

	selected, next, err := dataPage(out, func(s api.SubscriptionSummary) string { return *s.TopicFilter },
		in.NextToken, in.MaxResults)
	if err != nil {
		return nil, err
	}

	return &api.ListSubscriptionsResponse{Subscriptions: selected, NextToken: next}, nil
}

// dataPlaneError converts a rejected shadow request to the data plane's error.
func dataPlaneError(f *shadowError) error {
	code := "InvalidRequestException"
	switch f.code {
	case http.StatusNotFound:
		code = "ResourceNotFoundException"
	case http.StatusConflict:
		code = "ConflictException"
	case http.StatusRequestEntityTooLarge:
		code = "RequestEntityTooLargeException"
	case http.StatusInternalServerError:
		code = "InternalFailureException"
	}

	return awserror.New(code, f.message)
}

// shadow resolves the caller's scope and a request's shadow.
func shadow(ctx context.Context, thing, name *string) (request.Scope, shadowRef, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return request.Scope{}, shadowRef{}, err
	}

	ref, err := checkShadow(common.Value(thing), common.Value(name))
	if err != nil {
		return request.Scope{}, shadowRef{}, invalid("%s.", err)
	}

	return scope, ref, nil
}

func (d *DataPlane) GetThingShadow(
	ctx context.Context,
	in *api.GetThingShadowRequest,
) (*api.GetThingShadowResponse, error) {
	scope, ref, err := shadow(ctx, in.ThingName, in.ShadowName)
	if err != nil {
		return nil, err
	}

	doc, failure := d.s.getShadow(ctx, scope, ref, "")
	if failure != nil {
		return nil, dataPlaneError(failure)
	}

	return &api.GetThingShadowResponse{Payload: encodeResult(doc)}, nil
}

func (d *DataPlane) UpdateThingShadow(
	ctx context.Context,
	in *api.UpdateThingShadowRequest,
) (*api.UpdateThingShadowResponse, error) {
	scope, ref, err := shadow(ctx, in.ThingName, in.ShadowName)
	if err != nil {
		return nil, err
	}

	accepted, notes, failure := d.s.updateShadow(ctx, scope, ref, in.Payload)
	if failure != nil {
		return nil, dataPlaneError(failure)
	}

	d.s.notify(scope, notes)
	return &api.UpdateThingShadowResponse{Payload: encodeResult(accepted)}, nil
}

func (d *DataPlane) DeleteThingShadow(
	ctx context.Context,
	in *api.DeleteThingShadowRequest,
) (*api.DeleteThingShadowResponse, error) {
	scope, ref, err := shadow(ctx, in.ThingName, in.ShadowName)
	if err != nil {
		return nil, err
	}

	accepted, notes, failure := d.s.deleteShadow(ctx, scope, ref, nil)
	if failure != nil {
		return nil, dataPlaneError(failure)
	}

	d.s.notify(scope, notes)
	return &api.DeleteThingShadowResponse{Payload: encodeResult(accepted)}, nil
}

// maxShadowPage is the most names a ListNamedShadowsForThing page holds.
const maxShadowPage = 100

func (d *DataPlane) ListNamedShadowsForThing(
	ctx context.Context,
	in *api.ListNamedShadowsForThingRequest,
) (*api.ListNamedShadowsForThingResponse, error) {
	scope, ref, err := shadow(ctx, in.ThingName, nil)
	if err != nil {
		return nil, err
	}

	n := int(common.Value(in.PageSize))
	switch {
	case in.PageSize == nil:
		n = maxShadowPage
	case n < 1 || n > maxShadowPage:
		return nil, invalid("pageSize must be between 1 and %d.", maxShadowPage)
	}

	var names []string
	prefix := scope.Prefix(service, kindNamedShadow) + ref.thing + "/"
	err = d.s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys(prefix) {
			r, _, readErr := storage.Read[shadowRecord](tx, key)
			if readErr != nil {
				return readErr
			}

			if !r.Deleted {
				names = append(names, strings.TrimPrefix(key, prefix))
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := common.Page(names, func(n string) string { return n }, in.NextToken, n,
		invalid("The pagination token is invalid."))
	if err != nil {
		return nil, err
	}

	if selected == nil {
		selected = []string{}
	}

	return &api.ListNamedShadowsForThingResponse{
		Results: selected, NextToken: next, Timestamp: new(d.s.now().Unix()),
	}, nil
}
