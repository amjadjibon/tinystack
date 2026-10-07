package iot

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	kindShadow      = "shadow"
	kindNamedShadow = "named-shadow"
	// shadowsPrefix starts every shadow topic.
	shadowsPrefix = "$aws/things/"
	// maxShadowState is the most bytes a shadow's desired and reported
	// sections may hold together.
	maxShadowState = 8 * 1024
	// maxShadowDepth is how deeply a shadow's state may nest.
	maxShadowDepth = 6
	// maxClientToken is the longest client token a request may carry.
	maxClientToken = 64
	// The shadow sections and document members.
	sectionDesired  = "desired"
	sectionReported = "reported"
	keyState        = "state"
	keyVersion      = "version"
	keyMetadata     = "metadata"
	keyMessage      = "message"
	keyTimestamp    = "timestamp"
	// The shadow request operations.
	opGet    = "get"
	opUpdate = "update"
	opDelete = "delete"
)

var shadowNamePattern = regexp.MustCompile(`^[a-zA-Z0-9:_-]{1,64}$`)

// shadowRef names a thing's classic shadow (name "") or one of its named
// shadows.
type shadowRef struct {
	thing string
	name  string
}

func (r shadowRef) key(scope request.Scope) string {
	if r.name == "" {
		return scope.Key(service, kindShadow, r.thing)
	}

	return scope.Key(service, kindNamedShadow, r.thing+"/"+r.name)
}

// topic is the shadow's topic prefix, $aws/things/<thing>/shadow[/name/<name>].
func (r shadowRef) topic() string {
	t := shadowsPrefix + r.thing + "/shadow"
	if r.name != "" {
		t += "/name/" + r.name
	}

	return t
}

// display names the shadow in error messages, as IoT does (thing~name).
func (r shadowRef) display() string {
	if r.name == "" {
		return r.thing
	}

	return r.thing + "~" + r.name
}

func checkShadow(thing, name string) (shadowRef, error) {
	if !thingNamePattern.MatchString(thing) {
		return shadowRef{}, errors.New("the thing name is invalid")
	}

	if name != "" && !shadowNamePattern.MatchString(name) {
		return shadowRef{}, errors.New("the shadow name is invalid")
	}

	return shadowRef{thing: thing, name: name}, nil
}

// shadowRequest parses a shadow request topic:
// $aws/things/<thing>/shadow[/name/<name>]/{get,update,delete}.
func shadowRequest(topic string) (ref shadowRef, op string, ok bool) {
	parts := strings.Split(topic, "/")
	if len(parts) < 5 || parts[0] != "$aws" || parts[1] != "things" || parts[3] != "shadow" {
		return shadowRef{}, "", false
	}

	name := ""
	switch {
	case len(parts) == 5:
	case len(parts) == 7 && parts[4] == "name":
		name = parts[5]
	default:
		return shadowRef{}, "", false
	}

	op = parts[len(parts)-1]
	if op != opGet && op != opUpdate && op != opDelete {
		return shadowRef{}, "", false
	}

	ref, err := checkShadow(parts[2], name)
	return ref, op, err == nil
}

// shadowRecord is a shadow's record. A deleted shadow keeps its record, so
// its version carries on when it is created again.
type shadowRecord struct {
	// Desired, Reported, and Metadata hold JSON objects.
	Desired   json.RawMessage
	Reported  json.RawMessage
	Metadata  json.RawMessage
	Version   int64
	Timestamp int64
	Deleted   bool
}

// shadowError is a rejected shadow request: an HTTP status code and the
// message IoT reports.
type shadowError struct {
	message string
	code    int
}

func (e *shadowError) Error() string { return e.message }

func shadowFailure(code int, message string) *shadowError {
	return &shadowError{code: code, message: message}
}

// notification is a message a shadow change publishes once it commits.
type notification struct {
	payload map[string]any
	topic   string
}

// shadowState is a shadow's sections, decoded.
type shadowState struct {
	desired, reported, metadata map[string]any
	version, timestamp          int64
}

func (r *shadowRecord) state() *shadowState {
	return &shadowState{
		desired: decodeObject(r.Desired), reported: decodeObject(r.Reported), metadata: decodeObject(r.Metadata),
		version: r.Version, timestamp: r.Timestamp,
	}
}

func decodeObject(raw json.RawMessage) map[string]any {
	if m, ok := decodePayload(raw).(map[string]any); ok {
		return m
	}

	return map[string]any{}
}

// document renders a shadow as GetThingShadow and get/accepted do.
func (st *shadowState) document(withDelta bool) map[string]any {
	state, meta := map[string]any{}, map[string]any{}
	for _, section := range []struct {
		value map[string]any
		name  string
	}{{st.desired, sectionDesired}, {st.reported, sectionReported}} {
		if len(section.value) > 0 {
			state[section.name] = section.value
			if m, ok := st.metadata[section.name]; ok {
				meta[section.name] = m
			}
		}
	}

	if withDelta {
		if d := delta(st.desired, st.reported); len(d) > 0 {
			state["delta"] = d
		}
	}

	return map[string]any{keyState: state, keyMetadata: meta, keyVersion: st.version, keyTimestamp: st.timestamp}
}

// delta returns the desired fields whose reported value differs.
func delta(desired, reported map[string]any) map[string]any {
	out := map[string]any{}
	for k, d := range desired {
		r, found := reported[k]
		dm, dok := d.(map[string]any)
		rm, rok := r.(map[string]any)
		switch {
		case dok && rok:
			if sub := delta(dm, rm); len(sub) > 0 {
				out[k] = sub
			}
		case !found || !equal(d, r):
			out[k] = d
		}
	}

	return out
}

// deltaMetadata returns the metadata of a delta's fields.
func deltaMetadata(d, meta map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range d {
		m, ok := meta[k]
		if !ok {
			continue
		}

		if sub, ok := v.(map[string]any); ok {
			if mm, ok := m.(map[string]any); ok {
				out[k] = deltaMetadata(sub, mm)
				continue
			}
		}

		out[k] = m
	}

	return out
}

// merge applies an update to a section: null deletes a field, objects merge,
// and any other value replaces the field.
func merge(cur, update map[string]any) map[string]any {
	for k, v := range update {
		switch u := v.(type) {
		case nil:
			delete(cur, k)
		case map[string]any:
			c, ok := cur[k].(map[string]any)
			if !ok {
				c = map[string]any{}
			}

			cur[k] = merge(c, u)
		default:
			cur[k] = v
		}
	}

	return cur
}

// metadataOf returns the metadata of a value set at ts: a timestamp for
// each leaf.
func metadataOf(v any, ts int64) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = metadataOf(e, ts)
		}

		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = metadataOf(e, ts)
		}

		return out
	}

	return map[string]any{keyTimestamp: ts}
}

// mergeMetadata applies an update's timestamps to a section's metadata.
func mergeMetadata(cur, update map[string]any, ts int64) map[string]any {
	for k, v := range update {
		switch u := v.(type) {
		case nil:
			delete(cur, k)
		case map[string]any:
			c, ok := cur[k].(map[string]any)
			if _, leaf := c[keyTimestamp]; !ok || leaf && len(c) == 1 {
				c = map[string]any{}
			}

			cur[k] = mergeMetadata(c, u, ts)
		default:
			cur[k] = metadataOf(v, ts)
		}
	}

	return cur
}

// depth returns how deeply a value nests objects and arrays.
func depth(v any) int {
	n := 0
	switch x := v.(type) {
	case map[string]any:
		for _, e := range x {
			n = max(n, depth(e))
		}

		return n + 1
	case []any:
		for _, e := range x {
			n = max(n, depth(e))
		}

		return n + 1
	}

	return 0
}

// shadowRequestBody is a shadow request's common members.
type shadowRequestBody struct {
	doc         map[string]any
	version     *int64
	clientToken string
}

func parseShadowRequest(payload []byte, needState bool) (*shadowRequestBody, *shadowError) {
	body := &shadowRequestBody{doc: map[string]any{}}
	if strings.TrimSpace(string(payload)) == "" && !needState {
		return body, nil
	}

	doc, ok := decodePayload(payload).(map[string]any)
	if !ok {
		return nil, shadowFailure(http.StatusBadRequest, "Payload contains invalid json")
	}

	body.doc = doc
	if v, ok := doc[keyVersion]; ok {
		n, ok := v.(int64)
		if !ok {
			return nil, shadowFailure(http.StatusBadRequest, "Invalid version")
		}

		body.version = &n
	}

	if t, ok := doc["clientToken"]; ok {
		s, ok := t.(string)
		if !ok || len(s) > maxClientToken {
			return nil, shadowFailure(http.StatusBadRequest, "Invalid clientToken")
		}

		body.clientToken = s
	}

	return body, nil
}

// withToken adds a request's client token to a response.
func withToken(m map[string]any, token string) map[string]any {
	if token != "" {
		m["clientToken"] = token
	}

	return m
}

// getShadow returns a shadow's document.
func (s *Service) getShadow(ctx context.Context, scope request.Scope, ref shadowRef, token string) (
	map[string]any, *shadowError,
) {
	var r *shadowRecord
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		var err error
		r, _, err = storage.Read[shadowRecord](tx, ref.key(scope))
		return err
	})
	if err != nil {
		return nil, shadowFailure(http.StatusInternalServerError, err.Error())
	}

	if r == nil || r.Deleted {
		return nil, shadowFailure(http.StatusNotFound, "No shadow exists with name: '"+ref.display()+"'")
	}

	return withToken(r.state().document(true), token), nil
}

// updateShadow applies an update request and returns the accepted response
// and the notifications to publish.
func (s *Service) updateShadow(ctx context.Context, scope request.Scope, ref shadowRef, payload []byte) (
	map[string]any, []notification, *shadowError,
) {
	body, failure := parseShadowRequest(payload, true)
	if failure != nil {
		return nil, nil, failure
	}

	state, ok := body.doc[keyState].(map[string]any)
	if !ok {
		if _, present := body.doc[keyState]; !present {
			return nil, nil, shadowFailure(http.StatusBadRequest, "Missing required node: state")
		}

		return nil, nil, shadowFailure(http.StatusBadRequest, "State node must be an object")
	}

	for k, v := range state {
		if k != sectionDesired && k != sectionReported {
			return nil, nil, shadowFailure(http.StatusBadRequest, "State contains an invalid node: '"+k+"'")
		}

		if _, isObject := v.(map[string]any); v != nil && !isObject {
			return nil, nil, shadowFailure(http.StatusBadRequest, k+" node must be an object")
		}

		if depth(v) > maxShadowDepth {
			return nil, nil, shadowFailure(http.StatusBadRequest,
				"JSON contains too many levels of nesting; maximum is 6")
		}
	}

	ts := s.now().Unix()
	var previous, current *shadowState
	var tooLarge, conflict bool
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, found, err := storage.Read[shadowRecord](tx, ref.key(scope))
		if err != nil {
			return err
		}

		if !found {
			r = &shadowRecord{}
		}

		if r.Deleted {
			r = &shadowRecord{Version: r.Version}
		} else if found {
			previous = r.state()
		}

		if body.version != nil && found && !r.Deleted && *body.version != r.Version {
			conflict = true
			return nil
		}

		st := r.state()
		meta := st.metadata
		for _, name := range []string{sectionDesired, sectionReported} {
			v, present := state[name]
			if !present {
				continue
			}

			section := st.desired
			if name == sectionReported {
				section = st.reported
			}

			sectionMeta, _ := meta[name].(map[string]any)
			if sectionMeta == nil {
				sectionMeta = map[string]any{}
			}

			if v == nil {
				section, sectionMeta = map[string]any{}, map[string]any{}
			} else {
				section = merge(section, v.(map[string]any))
				sectionMeta = mergeMetadata(sectionMeta, v.(map[string]any), ts)
			}

			if name == sectionDesired {
				st.desired = section
			} else {
				st.reported = section
			}

			if len(section) == 0 {
				delete(meta, name)
			} else {
				meta[name] = sectionMeta
			}
		}

		if len(encodeResult(st.desired))+len(encodeResult(st.reported)) > maxShadowState {
			tooLarge = true
			return nil
		}

		st.version, st.timestamp = r.Version+1, ts
		current = st
		return storage.Write(tx, ref.key(scope), &shadowRecord{
			Desired: encodeResult(st.desired), Reported: encodeResult(st.reported), Metadata: encodeResult(meta),
			Version: st.version, Timestamp: ts,
		})
	})

	switch {
	case err != nil:
		return nil, nil, shadowFailure(http.StatusInternalServerError, err.Error())
	case conflict:
		return nil, nil, shadowFailure(http.StatusConflict, "Version conflict")
	case tooLarge:
		return nil, nil, shadowFailure(http.StatusRequestEntityTooLarge,
			"The payload exceeds the maximum size allowed")
	}

	requestMeta := map[string]any{}
	for k, v := range state {
		if v != nil {
			requestMeta[k] = metadataOf(v, ts)
		}
	}

	accepted := withToken(map[string]any{
		keyState: state, keyMetadata: requestMeta, keyVersion: current.version, keyTimestamp: ts,
	}, body.clientToken)

	notes := []notification{{topic: ref.topic() + "/update/accepted", payload: accepted}}
	if _, changed := state[sectionDesired]; changed {
		if d := delta(current.desired, current.reported); len(d) > 0 {
			desiredMeta, _ := current.metadata[sectionDesired].(map[string]any)
			notes = append(notes, notification{topic: ref.topic() + "/update/delta", payload: withToken(map[string]any{
				keyVersion: current.version, keyTimestamp: ts, keyState: d, keyMetadata: deltaMetadata(d, desiredMeta),
			}, body.clientToken)})
		}
	}

	documents := map[string]any{"current": snapshot(current), keyTimestamp: ts}
	if previous != nil {
		documents["previous"] = snapshot(previous)
	}

	notes = append(notes, notification{
		topic:   ref.topic() + "/update/documents",
		payload: withToken(documents, body.clientToken),
	})
	return accepted, notes, nil
}

// snapshot renders a shadow as update/documents does.
func snapshot(st *shadowState) map[string]any {
	doc := st.document(false)
	delete(doc, "timestamp")
	return doc
}

// deleteShadow deletes a shadow, keeping its version for the next one.
func (s *Service) deleteShadow(ctx context.Context, scope request.Scope, ref shadowRef, payload []byte) (
	map[string]any, []notification, *shadowError,
) {
	body, failure := parseShadowRequest(payload, false)
	if failure != nil {
		return nil, nil, failure
	}

	ts := s.now().Unix()
	var version int64
	var missing, conflict bool
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, found, err := storage.Read[shadowRecord](tx, ref.key(scope))
		if err != nil {
			return err
		}

		switch {
		case !found || r.Deleted:
			missing = true
			return nil
		case body.version != nil && *body.version != r.Version:
			conflict = true
			return nil
		}

		version = r.Version
		return storage.Write(tx, ref.key(scope), &shadowRecord{Version: r.Version, Timestamp: ts, Deleted: true})
	})

	switch {
	case err != nil:
		return nil, nil, shadowFailure(http.StatusInternalServerError, err.Error())
	case missing:
		return nil, nil, shadowFailure(http.StatusNotFound, "No shadow exists with name: '"+ref.display()+"'")
	case conflict:
		return nil, nil, shadowFailure(http.StatusConflict, "Version conflict")
	}

	accepted := withToken(map[string]any{keyVersion: version, "timestamp": ts}, body.clientToken)
	return accepted, []notification{{topic: ref.topic() + "/delete/accepted", payload: accepted}}, nil
}

// notify publishes a shadow change's notifications in scope.
func (s *Service) notify(scope request.Scope, notes []notification) {
	if len(notes) == 0 {
		return
	}

	b, err := s.broker(scope)
	if err != nil {
		s.logger.Warn("iot shadow notifications not sent", "error", err)
		return
	}

	for _, n := range notes {
		if err := b.server.Publish(n.topic, encodeResult(n.payload), false, 0); err != nil {
			s.logger.Warn("iot shadow notification not sent", keyTopic, n.topic, "error", err)
		}
	}
}

// serveShadow answers a shadow request a client published over MQTT on the
// request's accepted or rejected topic.
func (s *Service) serveShadow(ctx context.Context, scope request.Scope, topic string, payload []byte) {
	ref, op, _ := shadowRequest(topic)
	var accepted map[string]any
	var notes []notification
	var failure *shadowError
	switch op {
	case opGet:
		var body *shadowRequestBody
		if body, failure = parseShadowRequest(payload, false); failure == nil {
			accepted, failure = s.getShadow(ctx, scope, ref, body.clientToken)
		}
	case opUpdate:
		accepted, notes, failure = s.updateShadow(ctx, scope, ref, payload)
	case opDelete:
		accepted, notes, failure = s.deleteShadow(ctx, scope, ref, payload)
	}

	if failure == nil {
		if op == opGet {
			notes = []notification{{topic: topic + "/accepted", payload: accepted}}
		}

		s.notify(scope, notes)
		return
	}

	rejected := map[string]any{"code": failure.code, keyMessage: failure.message, "timestamp": s.now().Unix()}
	if body, ok := decodePayload(payload).(map[string]any); ok {
		if token, ok := body["clientToken"].(string); ok {
			rejected["clientToken"] = token
		}
	}

	s.notify(scope, []notification{{topic: topic + "/rejected", payload: rejected}})
}
