package iot

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/iot"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/policydoc"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	kindIndexing = "indexing"
	thingsIndex  = "AWS_Things"
	groupsIndex  = "AWS_ThingGroups"
	modeOff      = "OFF"
	modeOn       = "ON"
	modeRegistry = "REGISTRY"
	modeShadow   = "REGISTRY_AND_SHADOW"
	modeStatus   = "STATUS"
	queryVersion = "2017-09-30"
	// maxSearchResults is the most documents a SearchIndex page holds.
	maxSearchResults = 500
)

// indexingConfig is the account and Region's fleet indexing configuration.
type indexingConfig struct {
	Things       string
	Connectivity string
	NamedShadows string
	Groups       string
	ThingCustom  []api.Field
	GroupCustom  []api.Field
	ShadowNames  []string
}

func indexingKey(scope request.Scope) string { return scope.Key(service, kindIndexing, "config") }

func loadIndexing(tx storage.ReadTx, scope request.Scope) (*indexingConfig, error) {
	c, found, err := storage.Read[indexingConfig](tx, indexingKey(scope))
	if err != nil || !found {
		return &indexingConfig{Things: modeOff, Connectivity: modeOff, NamedShadows: modeOff, Groups: modeOff}, err
	}

	return c, nil
}

func indexField(name, typ string) api.Field { return api.Field{Name: new(name), Type: new(typ)} }

func (s *Service) UpdateIndexingConfiguration(
	ctx context.Context,
	in *api.UpdateIndexingConfigurationRequest,
) (*api.UpdateIndexingConfigurationResponse, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		c, err := loadIndexing(tx, scope)
		if err != nil {
			return err
		}

		if t := in.ThingIndexingConfiguration; t != nil {
			c.Things = common.Value(t.ThingIndexingMode)
			c.Connectivity = common.Value(common.OrElse(t.ThingConnectivityIndexingMode, new(modeOff)))
			c.NamedShadows = common.Value(common.OrElse(t.NamedShadowIndexingMode, new(modeOff)))
			c.ThingCustom, c.ShadowNames = t.CustomFields, nil
			if t.Filter != nil {
				c.ShadowNames = t.Filter.NamedShadowNames
			}

			switch {
			case !slices.Contains([]string{modeOff, modeRegistry, modeShadow}, c.Things):
				return invalid("thingIndexingMode must be OFF, REGISTRY, or REGISTRY_AND_SHADOW.")
			case c.Connectivity != modeOff && c.Connectivity != modeStatus:
				return invalid("thingConnectivityIndexingMode must be OFF or STATUS.")
			case c.NamedShadows != modeOff && c.NamedShadows != modeOn:
				return invalid("namedShadowIndexingMode must be OFF or ON.")
			case common.Value(t.DeviceDefenderIndexingMode) == "VIOLATIONS":
				return invalid("TinyStack does not implement Device Defender indexing.")
			case c.Things == modeOff && (c.Connectivity != modeOff || c.NamedShadows != modeOff):
				return invalid("Connectivity and named shadow indexing need thing indexing.")
			case c.NamedShadows == modeOn && c.Things != modeShadow:
				return invalid("Named shadow indexing needs thingIndexingMode REGISTRY_AND_SHADOW.")
			}
		}

		if g := in.ThingGroupIndexingConfiguration; g != nil {
			c.Groups, c.GroupCustom = common.Value(g.ThingGroupIndexingMode), g.CustomFields
			if c.Groups != modeOff && c.Groups != modeOn {
				return invalid("thingGroupIndexingMode must be OFF or ON.")
			}
		}

		return storage.Write(tx, indexingKey(scope), c)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateIndexingConfigurationResponse{}, nil
}

func (s *Service) GetIndexingConfiguration(
	ctx context.Context,
	_ *api.GetIndexingConfigurationRequest,
) (*api.GetIndexingConfigurationResponse, error) {
	var c *indexingConfig
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		var err error
		c, err = loadIndexing(tx, scope)
		return err
	})
	if err != nil {
		return nil, err
	}

	managed := []api.Field{}
	if c.Things != modeOff {
		managed = append(
			managed,
			indexField("thingName", "String"),
			indexField("thingId", "String"),
			indexField(
				"thingTypeName",
				"String",
			),
			indexField("thingGroupNames", "String"),
			indexField("registry.version", "Number"),
		)
	}

	if c.Things == modeShadow {
		managed = append(managed, indexField("shadow.version", "Number"), indexField("shadow.hasDelta", "Boolean"))
	}

	if c.Connectivity == modeStatus {
		managed = append(managed, indexField("connectivity.connected", "Boolean"),
			indexField("connectivity.timestamp", "Number"), indexField("connectivity.disconnectReason", "String"))
	}

	things := &api.ThingIndexingConfiguration{
		ThingIndexingMode: new(c.Things), ThingConnectivityIndexingMode: new(c.Connectivity),
		NamedShadowIndexingMode: new(c.NamedShadows), DeviceDefenderIndexingMode: new(modeOff),
		ManagedFields: managed, CustomFields: c.ThingCustom,
	}
	if c.ShadowNames != nil {
		things.Filter = &api.IndexingFilter{NamedShadowNames: c.ShadowNames}
	}

	groups := &api.ThingGroupIndexingConfiguration{ThingGroupIndexingMode: new(c.Groups), CustomFields: c.GroupCustom}
	if c.Groups == modeOn {
		groups.ManagedFields = []api.Field{
			indexField("thingGroupName", "String"), indexField("thingGroupId", "String"),
			indexField("thingGroupDescription", "String"), indexField("parentGroupNames", "String"),
		}
	}

	return &api.GetIndexingConfigurationResponse{
		ThingIndexingConfiguration: things, ThingGroupIndexingConfiguration: groups,
	}, nil
}

// indices returns the indices a configuration turns on.
func (c *indexingConfig) indices() []string {
	var out []string
	if c.Things != modeOff {
		out = append(out, thingsIndex)
	}

	if c.Groups == modeOn {
		out = append(out, groupsIndex)
	}

	return out
}

func (s *Service) ListIndices(ctx context.Context, in *api.ListIndicesRequest) (*api.ListIndicesResponse, error) {
	var names []string
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		c, err := loadIndexing(tx, scope)
		if err == nil {
			names = c.indices()
		}

		return err
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := page(names, func(n string) string { return n }, in.NextToken, in.MaxResults)
	if err != nil {
		return nil, err
	}

	if selected == nil {
		selected = []string{}
	}

	return &api.ListIndicesResponse{IndexNames: selected, NextToken: next}, nil
}

func (s *Service) DescribeIndex(ctx context.Context, in *api.DescribeIndexRequest) (*api.DescribeIndexResponse, error) {
	var out *api.DescribeIndexResponse
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		c, err := loadIndexing(tx, scope)
		if err != nil {
			return err
		}

		name := common.Value(in.IndexName)
		if !slices.Contains(c.indices(), name) {
			return notFound("Index %s does not exist.", name)
		}

		schema := "THING_GROUPS"
		if name == thingsIndex {
			schema = c.Things
			if c.Connectivity == modeStatus {
				schema += "_AND_CONNECTIVITY_STATUS"
			}
		}

		out = &api.DescribeIndexResponse{IndexName: new(name), IndexStatus: new("ACTIVE"), Schema: new(schema)}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// indexDoc is a document's fields, each with one or more values (strings,
// numbers, or booleans).
type indexDoc map[string][]any

func (d indexDoc) add(name string, v any) {
	switch x := v.(type) {
	case int64:
		v = float64(x)
	case map[string]any:
		for k, e := range x {
			d.add(name+"."+k, e)
		}

		return
	case []any:
		for _, e := range x {
			d.add(name, e)
		}

		return
	case nil:
		return
	}

	d[name] = append(d[name], v)
}

func (s *Service) SearchIndex(ctx context.Context, in *api.SearchIndexRequest) (*api.SearchIndexResponse, error) {
	if v := common.Value(in.QueryVersion); v != "" && v != queryVersion {
		return nil, invalid("The query version must be %s.", queryVersion)
	}

	n := int(common.Value(in.MaxResults))
	switch {
	case in.MaxResults == nil:
		n = maxSearchResults
	case n < 1 || n > maxSearchResults:
		return nil, invalid("maxResults must be between 1 and %d.", maxSearchResults)
	}

	q, err := parseSearch(common.Value(in.QueryString))
	if err != nil {
		return nil, awserror.New("InvalidQueryException", err.Error())
	}

	index := common.Value(common.OrElse(in.IndexName, new(thingsIndex)))
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.SearchIndexResponse{Things: []api.ThingDocument{}, ThingGroups: []api.ThingGroupDocument{}}
	var things []api.ThingDocument
	var groups []api.ThingGroupDocument
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		c, loadErr := loadIndexing(tx, scope)
		if loadErr != nil {
			return loadErr
		}

		if !slices.Contains(c.indices(), index) {
			return notFound("Index %s does not exist.", index)
		}

		var searchErr error
		if index == groupsIndex {
			groups, searchErr = s.searchGroups(tx, scope, q)
		} else {
			things, searchErr = s.searchThings(tx, scope, c, q)
		}

		return searchErr
	})
	if err != nil {
		return nil, err
	}

	if index == groupsIndex {
		selected, next, pageErr := common.Page(groups,
			func(g api.ThingGroupDocument) string { return *g.ThingGroupName }, in.NextToken, n,
			invalid("The pagination token is invalid."))
		if pageErr != nil {
			return nil, pageErr
		}

		out.ThingGroups, out.NextToken = append(out.ThingGroups, selected...), next
		return out, nil
	}

	selected, next, err := common.Page(things, func(t api.ThingDocument) string { return *t.ThingName },
		in.NextToken, n, invalid("The pagination token is invalid."))
	if err != nil {
		return nil, err
	}

	out.Things, out.NextToken = append(out.Things, selected...), next
	return out, nil
}

// searchThings returns the things a query matches, with the fields the
// configuration indexes.
func (s *Service) searchThings(tx storage.ReadTx, scope request.Scope, c *indexingConfig, q searchExpr) (
	[]api.ThingDocument, error,
) {
	things, err := storage.Records[thing](tx, scope.Prefix(service, kindThing))
	if err != nil {
		return nil, err
	}

	var out []api.ThingDocument
	for _, t := range things {
		doc := indexDoc{}
		doc.add("thingName", t.Name)
		doc.add("thingId", t.ID)
		doc.add("registry.version", t.Version)
		if t.Type != "" {
			doc.add("thingTypeName", t.Type)
		}

		for _, g := range t.Groups {
			doc.add("thingGroupNames", g)
		}

		for k, v := range t.Attributes {
			doc.add("attributes."+k, v)
		}

		result := api.ThingDocument{
			ThingName: new(t.Name), ThingId: new(t.ID), ThingGroupNames: t.Groups, Attributes: t.Attributes,
		}
		if t.Type != "" {
			result.ThingTypeName = new(t.Type)
		}

		if c.Things == modeShadow {
			shadow, err := indexShadows(tx, scope, c, t.Name, doc)
			if err != nil {
				return nil, err
			}

			if shadow != nil {
				result.Shadow = new(string(encodeResult(shadow)))
			}
		}

		if c.Connectivity == modeStatus {
			result.Connectivity = s.connectivity(scope, t.Name, doc)
		}

		if q.matches(doc) {
			out = append(out, result)
		}
	}

	return out, nil
}

// indexShadows adds a thing's classic shadow, and the named ones the
// configuration indexes, to its document, and returns the shadow document
// search results carry.
func indexShadows(tx storage.ReadTx, scope request.Scope, c *indexingConfig, thing string, doc indexDoc) (
	map[string]any, error,
) {
	var out map[string]any
	add := func(prefix string, r *shadowRecord) {
		st := r.state()
		section := map[string]any{"version": st.version, "hasDelta": len(delta(st.desired, st.reported)) > 0}
		if len(st.desired) > 0 {
			section[sectionDesired] = st.desired
		}

		if len(st.reported) > 0 {
			section[sectionReported] = st.reported
		}

		doc.add(prefix, section)
		if out == nil {
			out = map[string]any{}
		}

		if prefix == "shadow" {
			for k, v := range section {
				out[k] = v
			}

			return
		}

		names, _ := out["name"].(map[string]any)
		if names == nil {
			names = map[string]any{}
			out["name"] = names
		}

		names[strings.TrimPrefix(prefix, "shadow.name.")] = section
	}

	r, found, err := storage.Read[shadowRecord](tx, shadowRef{thing: thing}.key(scope))
	if err != nil {
		return nil, err
	}

	if found && !r.Deleted {
		add("shadow", r)
	}

	if c.NamedShadows == modeOn {
		for _, name := range c.ShadowNames {
			r, found, err := storage.Read[shadowRecord](tx, shadowRef{thing: thing, name: name}.key(scope))
			if err != nil {
				return nil, err
			}

			if found && !r.Deleted {
				add("shadow.name."+name, r)
			}
		}
	}

	return out, nil
}

// connectivity adds a thing's MQTT connection status, for a client whose ID
// is the thing's name, to its document.
func (s *Service) connectivity(scope request.Scope, thing string, doc indexDoc) *api.ThingConnectivity {
	out := &api.ThingConnectivity{Connected: new(false)}
	if b := s.existing(scope); b != nil {
		b.mu.Lock()
		info := b.connections[thing]
		if info != nil {
			out.Timestamp = new(info.connected.UnixMilli())
			if !info.disconnected.IsZero() {
				out.Timestamp, out.DisconnectReason = new(info.disconnected.UnixMilli()), new(info.reason)
			}
		}
		b.mu.Unlock()
		if cl, ok := b.server.Clients.Get(thing); ok && !cl.Net.Inline && !cl.Closed() {
			out.Connected, out.ClientId = new(true), new(thing)
		}
	}

	doc.add("connectivity.connected", *out.Connected)
	if out.Timestamp != nil {
		doc.add("connectivity.timestamp", *out.Timestamp)
	}

	if out.DisconnectReason != nil {
		doc.add("connectivity.disconnectReason", *out.DisconnectReason)
	}

	return out
}

func (s *Service) searchGroups(tx storage.ReadTx, scope request.Scope, q searchExpr) ([]api.ThingGroupDocument, error) {
	groups, err := storage.Records[thingGroup](tx, scope.Prefix(service, kindThingGroup))
	if err != nil {
		return nil, err
	}

	var out []api.ThingGroupDocument
	for _, g := range groups {
		parents, err := ancestors(tx, scope, g)
		if err != nil {
			return nil, err
		}

		doc := indexDoc{}
		doc.add("thingGroupName", g.Name)
		doc.add("thingGroupId", g.ID)
		if g.Description != "" {
			doc.add("thingGroupDescription", g.Description)
		}

		names := make([]string, len(parents))
		for i, p := range parents {
			names[i] = p.Name
			doc.add("parentGroupNames", p.Name)
		}

		for k, v := range g.Attributes {
			doc.add("attributes."+k, v)
		}

		if q.matches(doc) {
			d := api.ThingGroupDocument{
				ThingGroupName: new(g.Name), ThingGroupId: new(g.ID), Attributes: g.Attributes,
				ParentGroupNames: names,
			}
			if g.Description != "" {
				d.ThingGroupDescription = new(g.Description)
			}

			out = append(out, d)
		}
	}

	return out, nil
}

// searchExpr is a parsed fleet indexing query.
type searchExpr interface {
	matches(d indexDoc) bool
}

type searchAnd struct{ l, r searchExpr }

func (e *searchAnd) matches(d indexDoc) bool { return e.l.matches(d) && e.r.matches(d) }

type searchOr struct{ l, r searchExpr }

func (e *searchOr) matches(d indexDoc) bool { return e.l.matches(d) || e.r.matches(d) }

type searchNot struct{ x searchExpr }

func (e *searchNot) matches(d indexDoc) bool { return !e.x.matches(d) }

// searchTerm is field:value, field:[low TO high], or field>value.
type searchTerm struct {
	field, op, value, low, high string
	lowIn, highIn               bool
}

func (t *searchTerm) matches(d indexDoc) bool {
	values, ok := d[t.field]
	if !ok {
		return false
	}

	return slices.ContainsFunc(values, func(v any) bool {
		switch t.op {
		case ":":
			if t.value == "*" {
				return true
			}

			if strings.ContainsAny(t.value, "*?") {
				s, isText := v.(string)
				return isText && policydoc.Like(t.value, s, false)
			}

			return compareValue(v, t.value) == 0
		case "range":
			lowOK := t.low == "*" || compareValue(v, t.low) > 0 || t.lowIn && compareValue(v, t.low) == 0
			highOK := t.high == "*" || compareValue(v, t.high) < 0 || t.highIn && compareValue(v, t.high) == 0
			return lowOK && highOK
		}

		c := compareValue(v, t.value)
		switch t.op {
		case ">":
			return c > 0
		case ">=":
			return c >= 0
		case "<":
			return c < 0
		}

		return c <= 0
	})
}

// incomparable is what compareValue returns for values that cannot be
// ordered against each other.
const incomparable = 2

// compareValue orders a document value against query text: numbers
// numerically, booleans by their text, and strings lexically.
func compareValue(v any, text string) int {
	switch x := v.(type) {
	case float64:
		f, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return incomparable
		}

		switch {
		case x < f:
			return -1
		case x > f:
			return 1
		}

		return 0
	case bool:
		if strconv.FormatBool(x) == strings.ToLower(text) {
			return 0
		}

		return incomparable
	case string:
		return strings.Compare(x, text)
	}

	return incomparable
}

// searchParser parses the fleet indexing query language: terms joined by
// AND (or juxtaposition), OR, and NOT, with parentheses.
type searchParser struct {
	tokens []string
	pos    int
}

func parseSearch(query string) (searchExpr, error) {
	tokens, err := searchTokens(query)
	if err != nil {
		return nil, err
	}

	if len(tokens) == 0 {
		return nil, fmt.Errorf("the query is empty")
	}

	p := &searchParser{tokens: tokens}
	e, err := p.or()
	if err == nil && p.pos < len(p.tokens) {
		err = fmt.Errorf("unexpected %q", p.tokens[p.pos])
	}

	return e, err
}

// searchTokens splits a query into parentheses, operators, and terms, where
// a term's quoted value or range is one token.
func searchTokens(query string) ([]string, error) {
	var out []string
	for i := 0; i < len(query); {
		c := query[i]
		switch {
		case unicode.IsSpace(rune(c)):
			i++
		case c == '(' || c == ')':
			out = append(out, string(c))
			i++
		default:
			j, inQuote, depth := i, false, 0
			for ; j < len(query); j++ {
				ch := query[j]
				switch {
				case ch == '"':
					inQuote = !inQuote
				case inQuote:
				case ch == '[' || ch == '{':
					depth++
				case ch == ']' || ch == '}':
					depth--
				case depth == 0 && (unicode.IsSpace(rune(ch)) || ch == '(' || ch == ')'):
					goto done
				}
			}
		done:
			if inQuote || depth != 0 {
				return nil, fmt.Errorf("an unterminated quote or range in %q", query[i:])
			}

			out = append(out, query[i:j])
			i = j
		}
	}

	return out, nil
}

func (p *searchParser) peek() string {
	if p.pos < len(p.tokens) {
		return p.tokens[p.pos]
	}

	return ""
}

func (p *searchParser) or() (searchExpr, error) {
	l, err := p.and()
	for err == nil && p.peek() == "OR" {
		p.pos++
		var r searchExpr
		r, err = p.and()
		l = &searchOr{l: l, r: r}
	}

	return l, err
}

func (p *searchParser) and() (searchExpr, error) {
	l, err := p.not()
	for err == nil && p.peek() != "" && p.peek() != "OR" && p.peek() != ")" {
		if p.peek() == "AND" {
			p.pos++
		}

		var r searchExpr
		r, err = p.not()
		l = &searchAnd{l: l, r: r}
	}

	return l, err
}

func (p *searchParser) not() (searchExpr, error) {
	if p.peek() == "NOT" {
		p.pos++
		x, err := p.not()
		return &searchNot{x: x}, err
	}

	return p.primary()
}

func (p *searchParser) primary() (searchExpr, error) {
	t := p.peek()
	switch t {
	case "":
		return nil, fmt.Errorf("the query ends early")
	case "(":
		p.pos++
		e, err := p.or()
		if err != nil {
			return nil, err
		}

		if p.peek() != ")" {
			return nil, fmt.Errorf("a parenthesis is not closed")
		}

		p.pos++
		return e, nil
	case ")", "AND", "OR":
		return nil, fmt.Errorf("unexpected %s", t)
	}

	p.pos++
	return parseTerm(t)
}

// parseTerm parses field:value, field:"value", field:[a TO b], or a
// comparison such as field>=5.
func parseTerm(t string) (*searchTerm, error) {
	for _, op := range []string{">=", "<=", ">", "<", ":"} {
		i := strings.Index(t, op)
		if i <= 0 {
			continue
		}

		term := &searchTerm{field: t[:i], op: op, value: strings.Trim(t[i+len(op):], `"`)}
		if term.value == "" {
			return nil, fmt.Errorf("the term %q has no value", t)
		}

		rest := t[i+len(op):]
		if op == ":" && (rest[0] == '[' || rest[0] == '{') {
			end := rest[len(rest)-1]
			low, high, ok := strings.Cut(rest[1:len(rest)-1], " TO ")
			if !ok || (end != ']' && end != '}') {
				return nil, fmt.Errorf("the range %q must be [low TO high]", rest)
			}

			term.op, term.low, term.high = "range", strings.TrimSpace(low), strings.TrimSpace(high)
			term.lowIn, term.highIn = rest[0] == '[', end == ']'
		}

		return term, nil
	}

	return nil, fmt.Errorf("the term %q must be field:value", t)
}
