package elbv2

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/amjadjibon/tinystack/internal/aws/execute"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/elbv2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/apigateway/proxy"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	headerServer = "Server"
	serverName   = "awselb/2.0"
)

// errUnavailable means a load balancer has no listener or target group to
// take a request.
var errUnavailable = errors.New("unavailable")

// dnsEntry finds a load balancer from its DNS label.
type dnsEntry struct {
	Account string
	Region  string
	Name    string
}

// route is what a request to a load balancer resolved to: the action of the
// first rule that matched, and for a forward action, the target group it
// picked.
type route struct {
	group    *targetGroup
	action   api.Action
	scope    request.Scope
	protocol string
	port     int64
	timeout  time.Duration
}

// Execute serves a request to the load balancer whose DNS label is id
// ("<label>" for its port 80 listener, or its lowest port, or
// "<label>:<port>"). It reports false when no load balancer has the label.
func (s *Service) Execute(ctx context.Context, id string, r *execute.Request) (*execute.Response, bool, error) {
	label, port, hasPort := strings.Cut(strings.ToLower(id), ":")
	var rt *route
	found := false
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		entry, ok, err := read[dnsEntry](tx, dnsPrefix+label)
		if err != nil || !ok {
			return err
		}

		scope := request.Scope{AccountID: entry.Account, Region: entry.Region}
		b, ok, err := read[balancer](tx, key(scope, kindLB, entry.Name))
		if err != nil || !ok {
			return err
		}

		found = true
		l, err := pickListener(tx, scope, b, port, hasPort)
		if err != nil {
			return err
		}

		rt, err = resolve(tx, scope, b, l, r)
		return err
	})

	switch {
	case errors.Is(err, errUnavailable):
		return failure(http.StatusServiceUnavailable), true, nil
	case err != nil:
		return nil, false, err
	case !found:
		return nil, false, nil
	}

	switch a := rt.action; value(a.Type) {
	case actionRedirect:
		return redirect(a.RedirectConfig, r, rt.protocol, rt.port), true, nil
	case actionFixed:
		f := a.FixedResponseConfig
		status, _ := strconv.Atoi(value(f.StatusCode))
		contentType := value(f.ContentType)
		if contentType == "" {
			contentType = "text/plain; charset=utf-8"
		}

		return &execute.Response{
			Status: status, Header: http.Header{"Content-Type": {contentType}, headerServer: {serverName}},
			Body: []byte(value(f.MessageBody)),
		}, true, nil
	}

	return s.forward(ctx, rt, r), true, nil
}

// pickListener returns the listener on the port a request names, or else the
// one on port 80, or else the one on the lowest port.
func pickListener(tx storage.ReadTx, scope request.Scope, b *balancer, port string, hasPort bool) (
	*listener, error,
) {
	listeners, err := list[listener](tx, key(scope, kindListener, b.name()+"/"))
	switch {
	case err != nil:
		return nil, err
	case len(listeners) == 0:
		return nil, errUnavailable
	}

	slices.SortFunc(listeners, func(a, b *listener) int { return int(value(a.L.Port) - value(b.L.Port)) })
	want := "80"
	if hasPort {
		want = port
	}

	for _, l := range listeners {
		if strconv.FormatInt(value(l.L.Port), 10) == want {
			return l, nil
		}
	}

	if hasPort {
		return nil, errUnavailable
	}

	return listeners[0], nil
}

// resolve evaluates a listener's rules against a request.
func resolve(tx storage.ReadTx, scope request.Scope, b *balancer, l *listener, r *execute.Request) (
	*route, error,
) {
	rules, err := listenerRules(tx, scope, l)
	if err != nil {
		return nil, err
	}

	timeout := 60 * time.Second
	if seconds, parseErr := strconv.Atoi(b.Attributes["idle_timeout.timeout_seconds"]); parseErr == nil {
		timeout = time.Duration(seconds) * time.Second
	}

	rt := &route{scope: scope, protocol: value(l.L.Protocol), port: value(l.L.Port), timeout: timeout}
	for _, rule := range rules {
		if !slices.ContainsFunc(rule.Conditions, func(c api.RuleCondition) bool { return !matches(&c, r) }) {
			rt.action = rule.Actions[len(rule.Actions)-1]
			break
		}
	}

	if value(rt.action.Type) != actionForward {
		return rt, nil
	}

	groups := rt.action.ForwardConfig.TargetGroups
	total := int64(0)
	for _, g := range groups {
		total += value(g.Weight)
	}

	if total == 0 {
		return nil, errUnavailable
	}

	n, _ := rand.Int(rand.Reader, big.NewInt(total))
	pick := n.Int64()
	for _, g := range groups {
		if pick -= value(g.Weight); pick < 0 {
			rt.group, err = findTG(tx, scope, value(g.TargetGroupArn))
			return rt, err
		}
	}

	return nil, errUnavailable
}

// matches reports whether a request meets a rule condition: one of the
// condition's values matches.
func matches(c *api.RuleCondition, r *execute.Request) bool {
	switch value(c.Field) {
	case fieldHost:
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}

		return slices.ContainsFunc(c.HostHeaderConfig.Values, func(p string) bool { return wildcard(p, host, true) })
	case fieldPath:
		return slices.ContainsFunc(c.PathPatternConfig.Values, func(p string) bool {
			return wildcard(p, r.Path, false)
		})
	case fieldHeader:
		values := r.Header.Values(value(c.HttpHeaderConfig.HttpHeaderName))
		return slices.ContainsFunc(c.HttpHeaderConfig.Values, func(p string) bool {
			return slices.ContainsFunc(values, func(v string) bool { return wildcard(p, v, true) })
		})
	case fieldMethod:
		return slices.Contains(c.HttpRequestMethodConfig.Values, r.Method)
	case fieldQuery:
		return slices.ContainsFunc(c.QueryStringConfig.Values, func(p api.QueryStringKeyValuePair) bool {
			for k, values := range r.Query {
				if p.Key != nil && !wildcard(*p.Key, k, true) {
					continue
				}

				if slices.ContainsFunc(values, func(v string) bool { return wildcard(value(p.Value), v, true) }) {
					return true
				}
			}

			return false
		})
	case fieldSourceIP:
		addr, err := netip.ParseAddr(r.SourceIP)
		return err == nil && slices.ContainsFunc(c.SourceIpConfig.Values, func(cidr string) bool {
			prefix, parseErr := netip.ParsePrefix(cidr)
			return parseErr == nil && prefix.Contains(addr.Unmap())
		})
	}

	return false
}

// wildcard matches s against a pattern in which "*" matches any characters
// and "?" one.
func wildcard(pattern, s string, fold bool) bool {
	quoted := strings.NewReplacer(`\*`, ".*", `\?`, ".").Replace(regexp.QuoteMeta(pattern))
	if fold {
		quoted = "(?i)" + quoted
	}

	return regexp.MustCompile("^" + quoted + "$").MatchString(s)
}

// redirect answers with the location a redirect action builds.
func redirect(c *api.RedirectActionConfig, r *execute.Request, protocol string, port int64) *execute.Response {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	replace := strings.NewReplacer(
		"#{protocol}", strings.ToLower(protocol), "#{host}", host, "#{port}", strconv.FormatInt(port, 10),
		"#{path}", strings.TrimPrefix(r.Path, "/"), "#{query}", r.RawQuery,
	).Replace
	scheme := strings.ToLower(replace(value(c.Protocol)))
	location := scheme + "://" + replace(value(c.Host))
	if p := replace(value(c.Port)); (scheme != "http" || p != "80") && (scheme != "https" || p != "443") {
		location += ":" + p
	}

	location += replace(value(c.Path))
	if q := replace(value(c.Query)); q != "" {
		location += "?" + q
	}

	status := http.StatusMovedPermanently
	if value(c.StatusCode) == "HTTP_302" {
		status = http.StatusFound
	}

	return &execute.Response{Status: status, Header: http.Header{"Location": {location}, headerServer: {serverName}}}
}

// failure is the page a load balancer answers an error with.
func failure(status int) *execute.Response {
	text := strconv.Itoa(status) + " " + http.StatusText(status)
	return &execute.Response{
		Status: status, Header: http.Header{"Content-Type": {"text/html"}, headerServer: {serverName}},
		Body: []byte("<html>\r\n<head><title>" + text + "</title></head>\r\n<body>\r\n<center><h1>" + text +
			"</h1></center>\r\n</body>\r\n</html>\r\n"),
	}
}

// forward sends a request to the picked target group: a Lambda function, or
// an IP target picked in turn. Instances run no software, so a target group
// of instances cannot answer.
func (s *Service) forward(ctx context.Context, rt *route, r *execute.Request) *execute.Response {
	t := rt.group
	if len(t.Targets) == 0 || t.kind() == targetInstance {
		return failure(http.StatusServiceUnavailable)
	}

	header := r.Header.Clone()
	if header == nil {
		header = http.Header{}
	}

	if prior := header.Get("X-Forwarded-For"); prior != "" {
		header.Set("X-Forwarded-For", prior+", "+r.SourceIP)
	} else {
		header.Set("X-Forwarded-For", r.SourceIP)
	}

	header.Set("X-Forwarded-Proto", strings.ToLower(rt.protocol))
	header.Set("X-Forwarded-Port", strconv.FormatInt(rt.port, 10))
	header.Set("X-Amzn-Trace-Id", "Root=1-"+strconv.FormatInt(r.Time.Unix(), 16)+"-"+resourceID()+resourceID()[:8])
	if t.kind() == targetLambda {
		return s.invoke(ctx, rt, r, header)
	}

	target := t.Targets[s.turn.Add(1)%uint64(len(t.Targets))]
	scheme := "http"
	if value(t.TG.Protocol) == protocolHTTPS {
		scheme = "https"
	}

	url := scheme + "://" + net.JoinHostPort(value(target.Id), strconv.FormatInt(targetPort(t, target), 10)) + r.Path
	if r.RawQuery != "" {
		url += "?" + r.RawQuery
	}

	header.Set("Host", r.Host)
	return proxy.Forward(ctx, r.Method, url, header, r.Body, rt.timeout)
}

// invoke sends a request to a Lambda target as an Application Load Balancer
// event.
func (s *Service) invoke(ctx context.Context, rt *route, r *execute.Request, header http.Header) *execute.Response {
	header.Set("Host", r.Host)
	event := map[string]any{
		"requestContext": map[string]any{"elb": map[string]any{"targetGroupArn": rt.group.arnValue()}},
		"httpMethod":     r.Method, "path": r.Path, "body": "", "isBase64Encoded": false,
	}
	if len(r.Body) > 0 {
		if utf8.Valid(r.Body) {
			event["body"] = string(r.Body)
		} else {
			event["body"], event["isBase64Encoded"] = base64.StdEncoding.EncodeToString(r.Body), true
		}
	}

	query := map[string][]string{}
	for _, pair := range strings.Split(r.RawQuery, "&") {
		if pair != "" {
			k, v, _ := strings.Cut(pair, "=")
			query[k] = append(query[k], v)
		}
	}

	headers := map[string][]string{}
	for k, values := range header {
		headers[strings.ToLower(k)] = values
	}

	if rt.group.Attributes["lambda.multi_value_headers.enabled"] == valueTrue {
		event["multiValueHeaders"], event["multiValueQueryStringParameters"] = headers, query
	} else {
		event["headers"], event["queryStringParameters"] = lastValues(headers), lastValues(query)
	}

	payload, err := json.Marshal(event)
	if err != nil {
		return failure(http.StatusInternalServerError)
	}

	out, err := s.call(ctx, rt.scope, "lambda", "Invoke",
		map[string]any{"FunctionName": value(rt.group.Targets[0].Id), "Payload": string(payload)})
	if err != nil || out["FunctionError"] != nil {
		return failure(http.StatusBadGateway)
	}

	result, _ := out["Payload"].(string)
	resp, ok := proxy.Structured([]byte(result))
	if !ok {
		return failure(http.StatusBadGateway)
	}

	return resp
}

func lastValues(values map[string][]string) map[string]string {
	out := map[string]string{}
	for k, v := range values {
		if len(v) > 0 {
			out[k] = v[len(v)-1]
		}
	}

	return out
}
