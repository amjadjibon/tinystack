package iot

import (
	"cmp"
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"net/url"
	"slices"
	"strings"
	"time"

	mqtt "github.com/mochi-mqtt/server/v2"
	mqttstorage "github.com/mochi-mqtt/server/v2/hooks/storage"
	"github.com/mochi-mqtt/server/v2/packets"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	kindRetained = "retained"
	// eventsPrefix starts the lifecycle event topics, which only IoT Core
	// publishes.
	eventsPrefix = "$aws/events/"
)

// hook is how a broker asks TinyStack who may connect, publish, subscribe,
// and receive, and how it reports sessions and retained messages.
type hook struct {
	mqtt.HookBase
	service *Service
	broker  *broker
}

func (h *hook) ID() string { return "tinystack-iot" }

func (h *hook) Provides(b byte) bool {
	return slices.Contains([]byte{
		mqtt.OnConnectAuthenticate, mqtt.OnACLCheck, mqtt.OnSessionEstablished, mqtt.OnDisconnect,
		mqtt.OnSubscribe, mqtt.OnSubscribed, mqtt.OnUnsubscribed, mqtt.OnRetainMessage, mqtt.StoredRetainedMessages,
		mqtt.OnPublish, mqtt.OnPublished,
	}, b)
}

// connectionOf returns a client's connection, or nil for the broker's own
// inline clients.
func connectionOf(cl *mqtt.Client) *connection {
	c, _ := cl.Net.Conn.(*connection)
	return c
}

//nolint:gocritic // mochi's Hook interface passes packets by value.
func (h *hook) OnConnectAuthenticate(cl *mqtt.Client, pk packets.Packet) bool {
	c := connectionOf(cl)
	if c == nil || len(cl.ID) > maxClientID || c.anonymous && !h.customAuthorize(cl, c, &pk) {
		return false
	}

	docs, vars, err := h.policies(context.Background(), c, cl.ID)
	c.thing = vars[varThingName]
	return err == nil && h.allowed(c, docs, vars, "iot:Connect", "client/"+cl.ID)
}

const (
	varClientID   = "iot:ClientId"
	varThingName  = "iot:Connection.Thing.ThingName"
	varIsAttached = "iot:Connection.Thing.IsAttached"
	varThingType  = "iot:Connection.Thing.ThingTypeName"
	varAttribute  = "iot:Connection.Thing.Attributes["
	varCognitoSub = "cognito-identity.amazonaws.com:sub"
)

// policies returns the IoT policies that govern a connection, with what the
// policy variables are for it. No policies means IAM alone governs it, as for
// every connection outside identity.mode verify and for Cognito callers with
// no attached IoT policy, which TinyStack allows (AWS would also evaluate
// IAM). A device certificate in verify mode has only its IoT policies, so it
// is allowed nothing without one. A client ID that names a thing attached to
// the principal is that thing's connection.
func (h *hook) policies(ctx context.Context, c *connection, clientID string) (
	[]*policyDocument, map[string]string, error,
) {
	vars := map[string]string{varClientID: clientID, varIsAttached: "false"}
	if c.custom != nil {
		// A custom authorizer's policies govern its connections in every
		// identity mode.
		return c.custom, vars, nil
	}

	if c.principal == "" {
		return nil, vars, nil
	}

	var docs []*policyDocument
	if c.cert != nil {
		certificateVariables(vars, c.caller, c.cert)
		if h.service.verify {
			docs = []*policyDocument{}
		}
	} else {
		vars[varCognitoSub] = c.principal
	}

	err := h.service.store.View(ctx, func(tx storage.ReadTx) error {
		p, err := loadPrincipal(tx, c.scope, c.principal)
		if err != nil {
			return err
		}

		if slices.Contains(p.Things, clientID) {
			t, thingErr := loadThing(tx, c.scope, clientID)
			if thingErr != nil {
				return thingErr
			}

			vars[varThingName], vars[varIsAttached] = t.Name, "true"
			if t.Type != "" {
				vars[varThingType] = t.Type
			}

			for k, v := range t.Attributes {
				vars[varAttribute+k+"]"] = v
			}
		}

		if !h.service.verify {
			return nil
		}

		names, err := attachedPolicies(tx, c.scope, c.principal, true)
		if err != nil {
			return err
		}

		for _, name := range names {
			pol, err := loadPolicy(tx, c.scope, name)
			if err != nil {
				return err
			}

			doc, err := parsePolicy(pol.defaultVersion().Document)
			if err != nil {
				return err
			}

			docs = append(docs, doc)
		}

		return nil
	})
	if err != nil {
		h.service.logger.Warn("iot policies unreadable", "principal", c.principal, "error", err)
	}

	return docs, vars, err
}

// certificateVariables sets the iot:Certificate.* policy variables of a
// client certificate: its ID and the first value of each subject and issuer
// attribute it has.
func certificateVariables(vars map[string]string, id string, cert *x509.Certificate) {
	vars["iot:Certificate.Id"] = id
	for part, name := range map[string]*pkix.Name{"Subject": &cert.Subject, "Issuer": &cert.Issuer} {
		for attr, values := range map[string][]string{
			"CommonName": {name.CommonName}, "SerialNumber": {name.SerialNumber}, "Country": name.Country,
			"Organization": name.Organization, "OrganizationalUnit": name.OrganizationalUnit,
			"State": name.Province,
		} {
			if len(values) > 0 && values[0] != "" {
				vars["iot:Certificate."+part+"."+attr] = values[0]
			}
		}
	}
}

// allowed reports whether docs allow action on the resource of the
// connection's account and Region, where no documents allow everything.
func (h *hook) allowed(c *connection, docs []*policyDocument, vars map[string]string, action, resource string) bool {
	if docs == nil {
		return true
	}

	return allows(docs, &attempt{
		vars:     vars,
		action:   action,
		resource: "arn:aws:iot:" + c.scope.Region + ":" + c.scope.AccountID + ":" + resource,
	})
}

// authorize reports whether a connection's current policies allow action on
// resource.
func (h *hook) authorize(ctx context.Context, c *connection, clientID, action, resource string) bool {
	docs, vars, err := h.policies(ctx, c, clientID)
	return err == nil && h.allowed(c, docs, vars, action, resource)
}

func (h *hook) OnACLCheck(cl *mqtt.Client, topic string, write bool) bool {
	c := connectionOf(cl)
	if c == nil {
		return true
	}

	if write {
		return publishable(topic) && h.authorize(context.Background(), c, cl.ID, "iot:Publish", "topic/"+topic)
	}

	// A SUBSCRIBE's filters were authorized by OnSubscribe; anything else is
	// a message being delivered.
	c.mu.Lock()
	allowed, subscribing := c.subscribing[topic]
	c.mu.Unlock()
	if subscribing {
		return allowed
	}

	return h.authorize(context.Background(), c, cl.ID, "iot:Receive", "topic/"+topic)
}

//nolint:gocritic // mochi's Hook interface passes packets by value.
func (h *hook) OnSubscribe(cl *mqtt.Client, pk packets.Packet) packets.Packet {
	c := connectionOf(cl)
	if c == nil {
		return pk
	}

	decided := map[string]bool{}
	for _, f := range pk.Filters {
		decided[f.Filter] = subscribable(f.Filter) && h.authorize(context.Background(), c, cl.ID, "iot:Subscribe",
			"topicfilter/"+f.Filter)
	}

	c.mu.Lock()
	c.subscribing = decided
	c.mu.Unlock()
	return pk
}

//nolint:gocritic // mochi's Hook interface passes packets by value.
func (h *hook) OnSubscribed(cl *mqtt.Client, pk packets.Packet, codes []byte) {
	c := connectionOf(cl)
	if c == nil {
		return
	}

	c.mu.Lock()
	c.subscribing = nil
	c.mu.Unlock()
	var topics []string
	for i, f := range pk.Filters {
		if i < len(codes) && codes[i] < packets.ErrUnspecifiedError.Code {
			topics = append(topics, f.Filter)
		}
	}

	if len(topics) > 0 {
		h.event(cl, c, "subscriptions/subscribed", map[string]any{"topics": topics})
	}
}

//nolint:gocritic // mochi's Hook interface passes packets by value.
func (h *hook) OnUnsubscribed(cl *mqtt.Client, pk packets.Packet) {
	if c := connectionOf(cl); c != nil {
		topics := make([]string, 0, len(pk.Filters))
		for _, f := range pk.Filters {
			topics = append(topics, f.Filter)
		}

		h.event(cl, c, "subscriptions/unsubscribed", map[string]any{"topics": topics})
	}
}

//nolint:gocritic // mochi's Hook interface passes packets by value.
func (h *hook) OnSessionEstablished(cl *mqtt.Client, _ packets.Packet) {
	c := connectionOf(cl)
	if c == nil {
		return
	}

	b := h.broker
	b.mu.Lock()
	info := b.connections[cl.ID]
	if info == nil {
		info = &connectionInfo{}
		b.connections[cl.ID] = info
	}

	info.connected, info.disconnected, info.reason, info.thing = h.service.now(), time.Time{}, "", c.thing
	info.version++
	version := info.version
	b.mu.Unlock()
	h.event(cl, c, "presence/connected", map[string]any{"ipAddress": host(cl.Net.Remote), "versionNumber": version})
}

func (h *hook) OnDisconnect(cl *mqtt.Client, err error, expire bool) {
	c := connectionOf(cl)
	if c == nil {
		return
	}

	if c.expiry != nil {
		c.expiry.Stop()
	}

	b := h.broker
	b.mu.Lock()
	info := b.connections[cl.ID]
	reason, version := "CONNECTION_LOST", int64(0)
	if err == nil {
		reason = "CLIENT_INITIATED_DISCONNECT"
	}

	if info != nil {
		reason = cmp.Or(info.reason, reason)
		info.disconnected, info.reason = h.service.now(), reason
		version = info.version
		if expire {
			delete(b.connections, cl.ID)
		}
	}
	b.mu.Unlock()
	h.event(cl, c, "presence/disconnected", map[string]any{
		"clientInitiatedDisconnect": reason == "CLIENT_INITIATED_DISCONNECT", "disconnectReason": reason,
		"versionNumber": version,
	})
}

// event publishes a lifecycle event about a client, with IoT Core's common
// fields, to $aws/events/<kind>/<client ID>.
func (h *hook) event(cl *mqtt.Client, c *connection, kind string, fields map[string]any) {
	if h.broker.closing.Load() {
		return
	}

	fields[keyClientID] = cl.ID
	fields["timestamp"] = h.service.now().UnixMilli()
	fields["eventType"] = kind[strings.LastIndex(kind, "/")+1:]
	fields["sessionIdentifier"] = c.session
	fields["principalIdentifier"] = c.caller
	payload, err := json.Marshal(fields)
	if err == nil {
		err = h.broker.server.Publish(eventsPrefix+kind+"/"+cl.ID, payload, false, 0)
	}

	if err != nil {
		h.service.logger.Warn("iot lifecycle event failed", "client", cl.ID, "error", err)
	}
}

// retained is a retained message's record.
type retained struct {
	Topic    string
	Payload  []byte
	Qos      byte
	Modified int64 // Unix milliseconds
}

func retainedKey(scope request.Scope, topic string) string {
	return scope.Key(service, kindRetained, url.PathEscape(topic))
}

// OnRetainMessage keeps the broker's retained messages in the store, where
// the data plane reads them and a persistent store keeps them across restarts.
//
//nolint:gocritic // mochi's Hook interface passes packets by value.
func (h *hook) OnRetainMessage(_ *mqtt.Client, pk packets.Packet, r int64) {
	if r == 0 {
		return
	}

	key := retainedKey(h.broker.scope, pk.TopicName)
	err := h.service.store.Update(context.Background(), func(tx storage.WriteTx) error {
		if r < 0 {
			tx.Delete(key)
			return nil
		}

		return storage.Write(tx, key, &retained{
			Topic: pk.TopicName, Payload: pk.Payload, Qos: pk.FixedHeader.Qos, Modified: h.service.now().UnixMilli(),
		})
	})
	if err != nil {
		h.service.logger.Warn("iot retained message not stored", keyTopic, pk.TopicName, "error", err)
	}
}

// StoredRetainedMessages loads the scope's retained messages into its broker
// when it starts.
func (h *hook) StoredRetainedMessages() ([]mqttstorage.Message, error) {
	var out []mqttstorage.Message
	err := h.service.store.View(context.Background(), func(tx storage.ReadTx) error {
		records, err := storage.Records[retained](tx, h.broker.scope.Prefix(service, kindRetained))
		for _, r := range records {
			out = append(out, mqttstorage.Message{
				TopicName: r.Topic, Payload: r.Payload, Created: r.Modified / 1000,
				FixedHeader: packets.FixedHeader{Type: packets.Publish, Retain: true, Qos: r.Qos},
			})
		}

		return err
	})

	return out, err
}

// publishable reports whether a client may publish to topic: within IoT
// Core's limits, and not a reserved topic other than Basic Ingest's and the
// shadow requests (lifecycle events and shadow responses are IoT Core's own).
func publishable(topic string) bool {
	return withinLimits(topic) && (!strings.HasPrefix(topic, "$") || consumed(topic))
}

// consumed reports whether a topic is one IoT Core consumes instead of
// delivering: Basic Ingest's, and the shadow, jobs, and fleet provisioning
// requests.
func consumed(topic string) bool {
	if rule, ok := strings.CutPrefix(topic, ingestPrefix); ok {
		name, _, _ := strings.Cut(rule, "/")
		return ruleNamePattern.MatchString(name)
	}

	if _, _, ok := shadowRequest(topic); ok {
		return true
	}

	if _, _, _, ok := jobsRequest(topic); ok {
		return true
	}

	_, ok := provisioningRequest(topic)
	return ok
}

// OnPublish keeps Basic Ingest messages and shadow requests from
// subscribers; OnPublished still sees them.
//
//nolint:gocritic // mochi's Hook interface passes packets by value.
func (h *hook) OnPublish(_ *mqtt.Client, pk packets.Packet) (packets.Packet, error) {
	if consumed(pk.TopicName) {
		return pk, packets.CodeSuccessIgnore
	}

	return pk, nil
}

// OnPublished runs the rules a message triggers and answers shadow
// requests.
//
//nolint:gocritic // mochi's Hook interface passes packets by value.
func (h *hook) OnPublished(cl *mqtt.Client, pk packets.Packet) {
	if h.broker.closing.Load() {
		return
	}

	m := &message{scope: h.broker.scope, topic: pk.TopicName, origin: cl.ID, payload: pk.Payload}
	if c := connectionOf(cl); c != nil {
		m.clientID, m.principal = cl.ID, c.caller
	}

	if rest, ok := strings.CutPrefix(pk.TopicName, ingestPrefix); ok {
		m.ingest, m.topic, _ = strings.Cut(rest, "/")
	} else if _, _, ok := shadowRequest(pk.TopicName); ok {
		h.service.bg.Go(func(ctx context.Context) {
			h.service.serveShadow(ctx, h.broker.scope, pk.TopicName, pk.Payload)
		})
	} else if _, _, _, ok := jobsRequest(pk.TopicName); ok {
		h.service.bg.Go(func(ctx context.Context) {
			h.service.serveJobs(ctx, h.broker.scope, pk.TopicName, pk.Payload)
		})
	} else if _, ok := provisioningRequest(pk.TopicName); ok {
		claim := ""
		if c := connectionOf(cl); c != nil && c.cert != nil {
			claim = c.caller
		}

		h.service.bg.Go(func(ctx context.Context) {
			h.service.serveFleet(ctx, h.broker.scope, m.clientID, claim, pk.TopicName, pk.Payload)
		})
	}

	h.service.runRules(m)
}

// subscribable reports whether a client may subscribe to filter: within IoT
// Core's limits, and not a $SYS topic, which IoT Core does not have.
func subscribable(filter string) bool {
	return withinLimits(filter) && !strings.HasPrefix(filter, "$SYS")
}

// withinLimits checks a topic's length and its slashes, not counting those
// of a reserved topic's prefix.
func withinLimits(topic string) bool {
	if len(topic) > maxTopic {
		return false
	}

	slashes := strings.Count(topic, "/")
	if strings.HasPrefix(topic, "$aws/") {
		slashes -= 3
	}

	return slashes <= maxSlashes
}

// host returns the host of an address, or the address if it has no port.
func host(addr string) string {
	if i := strings.LastIndex(addr, ":"); i > 0 && !strings.HasSuffix(addr, "]") {
		return strings.Trim(addr[:i], "[]")
	}

	return addr
}
