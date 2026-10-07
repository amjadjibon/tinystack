package iot

import (
	"context"
	"net/url"
	"strings"
	"time"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/packets"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/iot"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// customAuthorize decides an anonymous connection's CONNECT with the custom
// authorizer it names (in its MQTT username's query, or its WebSocket
// request's query or headers) or the default one. Without an authorizer,
// plain MQTT and unsigned WebSocket connections are let in outside
// identity.mode verify, and TLS connections without a certificate never.
func (h *hook) customAuthorize(cl *mqtt.Client, c *connection, pk *packets.Packet) bool {
	username := string(pk.Connect.Username)
	_, rawParams, _ := strings.Cut(username, "?")
	params, _ := url.ParseQuery(rawParams)
	lookup := func(key string) string {
		if v := params.Get(key); v != "" {
			return v
		}

		if v := c.query.Get(key); v != "" {
			return v
		}

		return c.header.Get(key)
	}

	s := h.service
	ctx := context.Background()
	var a *authorizer
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		var err error
		if name := lookup(authorizerNameKey); name != "" {
			a, err = loadAuthorizer(tx, c.scope, name)
		} else {
			a, err = defaultAuthorizer(tx, c.scope)
		}

		return err
	})
	switch {
	case err != nil:
		s.logger.Debug("iot custom authorizer unavailable", "error", err)
		return false
	case a == nil:
		return !s.verify && cl.Net.Listener != listenerTLS
	case a.Status != statusActive:
		return false
	}

	r := &authRequest{
		signature: lookup(authorizerSigKey), serverName: c.serverName,
		mqtt: &api.MqttContext{ClientId: new(cl.ID)},
	}
	if a.TokenKey != "" {
		r.token = lookup(a.TokenKey)
	}

	if pk.Connect.UsernameFlag {
		r.mqtt.Username = new(username)
	}

	if pk.Connect.PasswordFlag {
		r.mqtt.Password = pk.Connect.Password
	}

	if c.header != nil {
		r.headers, r.query = map[string]string{}, "?"+c.query.Encode()
		for k, v := range c.header {
			r.headers[k] = v[0]
		}
	}

	result, err := s.invokeAuthorizer(ctx, c.scope, a, r)
	if err != nil || !result.authenticated {
		s.logger.Debug("iot custom authorizer refused a connection", "authorizer", a.Name, "error", err)
		return false
	}

	c.custom, c.principal, c.caller = result.policies, result.principal, result.principal
	if result.disconnectAt > 0 {
		b := h.broker
		c.expiry = time.AfterFunc(time.Duration(result.disconnectAt)*time.Second, func() {
			_ = b.server.DisconnectClient(cl, packets.ErrMaxConnectTime)
		})
	}

	return true
}
