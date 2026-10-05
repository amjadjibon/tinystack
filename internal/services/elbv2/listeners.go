package elbv2

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"regexp"
	"slices"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/elbv2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	actionForward  = "forward"
	actionRedirect = "redirect"
	actionFixed    = "fixed-response"
	defaultPolicy  = "ELBSecurityPolicy-2016-08"
	maxOrder       = 50000
	maxFixedBody   = 1024
	maxForwardTGs  = 5
	maxWeight      = 999
)

var (
	statusPattern = regexp.MustCompile(`^[2-5]\d\d$`)
	contentTypes  = []string{"text/plain", "text/css", "text/html", "application/javascript", "application/json"}
)

// listener is a listener's record, with its default rule.
type listener struct {
	Attributes map[string]string
	Tags       map[string]string `json:",omitempty"`
	// RuleTags are the default rule's tags.
	RuleTags map[string]string `json:",omitempty"`
	ID       string
	RuleID   string
	Balancer string
	L        api.Listener
	// Extra are the certificates added with AddListenerCertificates.
	Extra []api.Certificate
}

func (l *listener) arnValue() string { return common.Value(l.L.ListenerArn) }

func (l *listener) ruleARN(scope request.Scope, id string) string {
	parts, _ := arnParts(l.arnValue(), "listener", scope)
	return arn(scope, "listener-rule/"+parts[0]+"/"+parts[1]+"/"+parts[2]+"/"+l.ID+"/"+id)
}

func listenerNotFound() error {
	return awserror.New("ListenerNotFoundException", "One or more listeners not found")
}

func findListener(tx storage.ReadTx, scope request.Scope, arnValue string) (*listener, error) {
	parts, ok := arnParts(arnValue, "listener", scope)
	if !ok || len(parts) != 4 {
		return nil, validation("'%s' is not a valid listener ARN", arnValue)
	}

	l, found, err := storage.Read[listener](tx, scope.Key(service, kindListener, parts[1]+"/"+parts[3]))
	switch {
	case err != nil:
		return nil, err
	case !found || l.arnValue() != arnValue:
		return nil, listenerNotFound()
	}

	return l, nil
}

func invalidAction(message string) error {
	return awserror.New("InvalidLoadBalancerActionException", message)
}

// checkActions checks the actions of a listener or rule on a load balancer
// and returns them as ELB reports them, ordered, with forward actions given
// both as a target group ARN (when there is one) and a forward config.
//
//nolint:gocyclo,cyclop // One rule per action type.
func checkActions(tx storage.ReadTx, scope request.Scope, b *balancer, protocol string, actions []api.Action) (
	[]api.Action, error,
) {
	if len(actions) == 0 {
		return nil, validation("At least one action must be specified")
	}

	used, err := usage(tx, scope)
	if err != nil {
		return nil, err
	}

	out := make([]api.Action, len(actions))
	orders := map[int64]bool{}
	for i, a := range actions {
		kind := common.Value(a.Type)
		if a.Order == nil {
			a.Order = new(int64(i + 1))
		}

		switch order := *a.Order; {
		case order < 1 || order > maxOrder || orders[order]:
			return nil, invalidAction("Action orders must be unique and 1 to 50000")
		case b.kind() == typeNetwork && kind != actionForward:
			return nil, invalidAction("Network load balancer listeners support only forward actions")
		}

		orders[*a.Order] = true
		switch kind {
		case actionForward:
			if checkErr := checkForward(tx, scope, b, protocol, &a, used); checkErr != nil {
				return nil, checkErr
			}
		case actionRedirect:
			r := a.RedirectConfig
			if r == nil || (common.Value(r.StatusCode) != "HTTP_301" && common.Value(r.StatusCode) != "HTTP_302") {
				return nil, invalidAction("A redirect action needs a RedirectConfig with status HTTP_301 or HTTP_302")
			}

			for _, d := range []struct {
				field **string
				value string
			}{
				{&r.Protocol, "#{protocol}"},
				{&r.Port, "#{port}"},
				{&r.Host, "#{host}"},
				{&r.Path, "/#{path}"},
				{&r.Query, "#{query}"},
			} {
				if *d.field == nil {
					*d.field = new(d.value)
				}
			}
		case actionFixed:
			f := a.FixedResponseConfig
			switch {
			case f == nil || !statusPattern.MatchString(common.Value(f.StatusCode)):
				return nil, invalidAction("A fixed-response action needs a FixedResponseConfig with a 2XX to 5XX " +
					"status")
			case f.ContentType != nil && !slices.Contains(contentTypes, *f.ContentType):
				return nil, invalidAction("The fixed response content type is not supported")
			case len(common.Value(f.MessageBody)) > maxFixedBody:
				return nil, invalidAction("A fixed response body is at most 1024 characters")
			}
		default:
			return nil, invalidAction("TinyStack supports forward, redirect, and fixed-response actions, not " + kind)
		}

		out[i] = a
	}

	slices.SortFunc(out, func(a, b api.Action) int { return int(common.Value(a.Order) - common.Value(b.Order)) })
	if last := common.Value(
		out[len(out)-1].Type,
	); last != actionForward && last != actionRedirect &&
		last != actionFixed {
		return nil, invalidAction("The last action must be forward, redirect, or fixed-response")
	}

	if len(out) > 1 {
		return nil, awserror.New("TooManyActionsException", "Only one forward, redirect, or fixed-response "+
			"action is allowed")
	}

	return out, nil
}

// checkForward checks a forward action's target groups and fills in the
// forms ELB reports.
func checkForward(tx storage.ReadTx, scope request.Scope, b *balancer, protocol string, a *api.Action,
	used map[string][]string,
) error {
	if a.ForwardConfig == nil {
		if a.TargetGroupArn == nil {
			return invalidAction("A forward action needs a TargetGroupArn or a ForwardConfig")
		}

		a.ForwardConfig = &api.ForwardActionConfig{
			TargetGroups: []api.TargetGroupTuple{{TargetGroupArn: a.TargetGroupArn, Weight: new(int64(1))}},
		}
	}

	groups := a.ForwardConfig.TargetGroups
	switch {
	case len(groups) == 0 || len(groups) > maxForwardTGs:
		return invalidAction("A forward action has 1 to 5 target groups")
	case a.TargetGroupArn != nil && (len(groups) != 1 || common.Value(groups[0].TargetGroupArn) != *a.TargetGroupArn):
		return invalidAction("TargetGroupArn and ForwardConfig must name the same target group")
	case len(groups) == 1:
		a.TargetGroupArn = groups[0].TargetGroupArn
	}

	if a.ForwardConfig.TargetGroupStickinessConfig == nil {
		a.ForwardConfig.TargetGroupStickinessConfig = &api.TargetGroupStickinessConfig{Enabled: new(false)}
	}

	for i := range groups {
		if groups[i].Weight == nil {
			groups[i].Weight = new(int64(1))
		}

		if w := *groups[i].Weight; w < 0 || w > maxWeight {
			return invalidAction("Target group weights are 0 to 999")
		}

		t, err := findTG(tx, scope, common.Value(groups[i].TargetGroupArn))
		if err != nil {
			return err
		}

		for _, lb := range used[t.arnValue()] {
			if lb != b.arnValue() {
				return awserror.New("TargetGroupAssociationLimitException", "The following target groups cannot be "+
					"associated with more than one load balancer: "+t.arnValue())
			}
		}

		web := slices.Contains(applicationProtocols, protocol)
		switch tgProtocol := common.Value(t.TG.Protocol); {
		case web && t.kind() != targetLambda && !slices.Contains(applicationProtocols, tgProtocol),
			!web && (t.kind() == targetLambda || !slices.Contains(networkProtocols, tgProtocol)):
			return awserror.New("IncompatibleProtocolsException", "The target group protocol is not compatible "+
				"with the listener protocol "+protocol)
		}
	}

	return nil
}

// checkListener checks a listener's protocol, port, certificates, and SSL
// policy, filling in defaults.
func checkListener(b *balancer, l *api.Listener) error {
	protocol, port := common.Value(l.Protocol), common.Value(l.Port)
	secure := protocol == protocolHTTPS || protocol == protocolTLS
	switch {
	case b.kind() == typeApplication && !slices.Contains(applicationProtocols, protocol),
		b.kind() == typeNetwork && !slices.Contains(networkProtocols, protocol):
		return awserror.New("UnsupportedProtocolException", "Protocol '"+protocol+"' is not supported by "+
			b.kind()+" load balancers")
	case port < 1 || port > 65535:
		return validation("Port must be 1 to 65535")
	case secure && len(l.Certificates) != 1:
		return awserror.New("CertificateNotFoundException", "A certificate must be specified for "+protocol+
			" listeners")
	case !secure && (len(l.Certificates) > 0 || l.SslPolicy != nil):
		return validation("Certificates and SSL policies are only valid for HTTPS and TLS listeners")
	case len(l.AlpnPolicy) > 0 && protocol != protocolTLS:
		return validation("An ALPN policy is only valid for TLS listeners")
	}

	if secure {
		if l.SslPolicy == nil {
			l.SslPolicy = new(defaultPolicy)
		}

		if !slices.ContainsFunc(
			sslPolicies,
			func(p api.SslPolicy) bool { return common.Value(p.Name) == *l.SslPolicy },
		) {
			return awserror.New("SSLPolicyNotFoundException", "SSL policy '"+*l.SslPolicy+"' not found")
		}
	}

	l.Certificates = slices.Clone(l.Certificates)
	for i := range l.Certificates {
		l.Certificates[i].IsDefault = nil
	}

	return nil
}

// checkCertificates checks certificates in ACM, outside any transaction.
func (s *Service) checkCertificates(ctx context.Context, scope request.Scope, certs []api.Certificate) error {
	for _, c := range certs {
		if _, err := s.call(ctx, scope, "acm", "DescribeCertificate",
			map[string]any{"CertificateArn": common.Value(c.CertificateArn)}); err != nil {
			return awserror.New("CertificateNotFoundException", "Certificate '"+common.Value(c.CertificateArn)+
				"' not found")
		}
	}

	return nil
}

func (s *Service) CreateListener(ctx context.Context, in *api.CreateListenerInput) (*api.CreateListenerOutput,
	error,
) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if checkErr := s.checkCertificates(ctx, m.Scope, in.Certificates); checkErr != nil {
		return nil, checkErr
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	var l *listener
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		b, findErr := findLB(tx, m.Scope, common.Value(in.LoadBalancerArn))
		if findErr != nil {
			return findErr
		}

		spec := api.Listener{
			LoadBalancerArn: in.LoadBalancerArn, Port: in.Port, Protocol: in.Protocol, Certificates: in.Certificates,
			SslPolicy: in.SslPolicy, AlpnPolicy: in.AlpnPolicy,
		}
		if checkErr := checkListener(b, &spec); checkErr != nil {
			return checkErr
		}

		actions, actionsErr := checkActions(tx, m.Scope, b, common.Value(in.Protocol), in.DefaultActions)
		if actionsErr != nil {
			return actionsErr
		}

		spec.DefaultActions = actions
		existing, listErr := storage.Records[listener](tx, m.Key(service, kindListener, b.name()+"/"))
		if listErr != nil {
			return listErr
		}

		for _, o := range existing {
			if common.Value(o.L.Port) != common.Value(in.Port) {
				continue
			}

			// Creating a listener again with the same settings returns it.
			if same(&o.L, &spec) {
				l = o
				return nil
			}

			return awserror.New("DuplicateListenerException", "A listener already exists on this port for this "+
				"load balancer")
		}

		id := resourceID()
		spec.ListenerArn = new(arn(m.Scope, "listener/"+short(b.kind())+"/"+b.name()+"/"+b.ID+"/"+id))
		attributes := listenerAttributes
		if b.kind() == typeNetwork {
			attributes = networkListenerAttributes
		}

		l = &listener{
			ID: id, RuleID: resourceID(), Balancer: b.name(), L: spec, Tags: tags, Attributes: attributes,
			Extra: []api.Certificate{},
		}
		return storage.Write(tx, m.Key(service, kindListener, b.name()+"/"+id), l)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateListenerOutput{Listeners: []api.Listener{l.L}}, nil
}

// same reports whether two listeners have the same settings.
func same(a, b *api.Listener) bool {
	left, right := *a, *b
	left.ListenerArn, right.ListenerArn = nil, nil
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	return bytes.Equal(leftJSON, rightJSON)
}

func (s *Service) DescribeListeners(
	ctx context.Context, in *api.DescribeListenersInput,
) (*api.DescribeListenersOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if (in.LoadBalancerArn == nil) == (len(in.ListenerArns) == 0) {
		return nil, validation("You must specify either listener ARNs or a load balancer ARN")
	}

	var selected []*listener
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if in.LoadBalancerArn != nil {
			b, findErr := findLB(tx, m.Scope, *in.LoadBalancerArn)
			if findErr != nil {
				return findErr
			}

			var listErr error
			selected, listErr = storage.Records[listener](tx, m.Key(service, kindListener, b.name()+"/"))
			return listErr
		}

		for _, a := range in.ListenerArns {
			l, findErr := findListener(tx, m.Scope, a)
			if findErr != nil {
				return findErr
			}

			selected = append(selected, l)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	slices.SortFunc(selected, func(a, b *listener) int { return int(common.Value(a.L.Port) - common.Value(b.L.Port)) })
	selected, next, err := page(selected, (*listener).arnValue, in.Marker, in.PageSize)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeListenersOutput{Listeners: []api.Listener{}, NextMarker: next}
	for _, l := range selected {
		out.Listeners = append(out.Listeners, l.L)
	}

	return out, nil
}

// changeListener runs fn on a listener and its load balancer in a
// transaction and saves the listener.
func (s *Service) changeListener(ctx context.Context, arnValue string,
	fn func(tx storage.WriteTx, scope request.Scope, b *balancer, l *listener) error,
) (*listener, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var l *listener
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var findErr error
		if l, findErr = findListener(tx, m.Scope, arnValue); findErr != nil {
			return findErr
		}

		b, found, readErr := storage.Read[balancer](tx, m.Key(service, kindLB, l.Balancer))
		switch {
		case readErr != nil:
			return readErr
		case !found:
			return listenerNotFound()
		}

		if fnErr := fn(tx, m.Scope, b, l); fnErr != nil {
			return fnErr
		}

		return storage.Write(tx, m.Key(service, kindListener, l.Balancer+"/"+l.ID), l)
	})

	return l, err
}

func (s *Service) ModifyListener(ctx context.Context, in *api.ModifyListenerInput) (*api.ModifyListenerOutput,
	error,
) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if checkErr := s.checkCertificates(ctx, m.Scope, in.Certificates); checkErr != nil {
		return nil, checkErr
	}

	l, err := s.changeListener(ctx, common.Value(in.ListenerArn), func(tx storage.WriteTx, scope request.Scope,
		b *balancer, l *listener,
	) error {
		spec := l.L
		if in.Protocol != nil {
			spec.Protocol = in.Protocol
			if *in.Protocol == protocolHTTP || *in.Protocol == protocolTCP {
				spec.Certificates, spec.SslPolicy = nil, nil
			}
		}

		spec.Port = cmp.Or(in.Port, spec.Port)
		spec.SslPolicy = cmp.Or(in.SslPolicy, spec.SslPolicy)
		if in.Certificates != nil {
			spec.Certificates = in.Certificates
		}

		if in.AlpnPolicy != nil {
			spec.AlpnPolicy = in.AlpnPolicy
		}

		if checkErr := checkListener(b, &spec); checkErr != nil {
			return checkErr
		}

		if in.DefaultActions != nil || in.Protocol != nil {
			actions := in.DefaultActions
			if actions == nil {
				actions = spec.DefaultActions
			}

			checked, actionsErr := checkActions(tx, scope, b, common.Value(spec.Protocol), actions)
			if actionsErr != nil {
				return actionsErr
			}

			spec.DefaultActions = checked
		}

		if common.Value(spec.Port) != common.Value(l.L.Port) {
			others, listErr := storage.Records[listener](tx, scope.Key(service, kindListener, b.name()+"/"))
			if listErr != nil {
				return listErr
			}

			if slices.ContainsFunc(
				others,
				func(o *listener) bool { return common.Value(o.L.Port) == common.Value(spec.Port) },
			) {
				return awserror.New("DuplicateListenerException", "A listener already exists on this port for "+
					"this load balancer")
			}
		}

		l.L = spec
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.ModifyListenerOutput{Listeners: []api.Listener{l.L}}, nil
}

func (s *Service) DeleteListener(ctx context.Context, in *api.DeleteListenerInput) (*api.DeleteListenerOutput,
	error,
) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	return &api.DeleteListenerOutput{}, s.store.Update(ctx, func(tx storage.WriteTx) error {
		l, findErr := findListener(tx, m.Scope, common.Value(in.ListenerArn))
		if findErr != nil {
			return findErr
		}

		for _, k := range tx.Keys(m.Key(service, kindRule, l.ID+"/")) {
			tx.Delete(k)
		}

		tx.Delete(m.Key(service, kindListener, l.Balancer+"/"+l.ID))
		return nil
	})
}

func listenerAttributeList(l *listener) []api.ListenerAttribute {
	out := []api.ListenerAttribute{}
	for _, k := range attributeKeys(l.Attributes) {
		out = append(out, api.ListenerAttribute{Key: new(k), Value: new(l.Attributes[k])})
	}

	return out
}

func (s *Service) DescribeListenerAttributes(
	ctx context.Context, in *api.DescribeListenerAttributesInput,
) (*api.DescribeListenerAttributesOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var l *listener
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		l, err = findListener(tx, m.Scope, common.Value(in.ListenerArn))
		return err
	})
	if err != nil {
		return nil, err
	}

	return &api.DescribeListenerAttributesOutput{Attributes: listenerAttributeList(l)}, nil
}

func (s *Service) ModifyListenerAttributes(
	ctx context.Context, in *api.ModifyListenerAttributesInput,
) (*api.ModifyListenerAttributesOutput, error) {
	keys, values := make([]string, len(in.Attributes)), make([]string, len(in.Attributes))
	for i, a := range in.Attributes {
		keys[i], values[i] = common.Value(a.Key), common.Value(a.Value)
	}

	l, err := s.changeListener(ctx, common.Value(in.ListenerArn), func(_ storage.WriteTx, _ request.Scope, b *balancer,
		l *listener,
	) error {
		defaults := listenerAttributes
		if b.kind() == typeNetwork {
			defaults = networkListenerAttributes
		}

		var setErr error
		l.Attributes, setErr = setAttributes(l.Attributes, defaults, keys, values)
		return setErr
	})
	if err != nil {
		return nil, err
	}

	return &api.ModifyListenerAttributesOutput{Attributes: listenerAttributeList(l)}, nil
}

// certificates returns a listener's certificates, the default one first.
func (l *listener) certificates() []api.Certificate {
	out := []api.Certificate{}
	for _, c := range l.L.Certificates {
		out = append(out, api.Certificate{CertificateArn: c.CertificateArn, IsDefault: new(true)})
	}

	for _, c := range l.Extra {
		out = append(out, api.Certificate{CertificateArn: c.CertificateArn, IsDefault: new(false)})
	}

	return out
}

func (s *Service) AddListenerCertificates(
	ctx context.Context, in *api.AddListenerCertificatesInput,
) (*api.AddListenerCertificatesOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if checkErr := s.checkCertificates(ctx, m.Scope, in.Certificates); checkErr != nil {
		return nil, checkErr
	}

	l, err := s.changeListener(ctx, common.Value(in.ListenerArn), func(_ storage.WriteTx, _ request.Scope, _ *balancer,
		l *listener,
	) error {
		if len(l.L.Certificates) == 0 {
			return validation("Certificates can only be added to HTTPS and TLS listeners")
		}

		for _, c := range in.Certificates {
			if !slices.ContainsFunc(l.certificates(), func(o api.Certificate) bool {
				return common.Value(o.CertificateArn) == common.Value(c.CertificateArn)
			}) {
				l.Extra = append(l.Extra, api.Certificate{CertificateArn: c.CertificateArn})
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.AddListenerCertificatesOutput{Certificates: l.certificates()[1:]}, nil
}

func (s *Service) DescribeListenerCertificates(
	ctx context.Context, in *api.DescribeListenerCertificatesInput,
) (*api.DescribeListenerCertificatesOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var l *listener
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		l, err = findListener(tx, m.Scope, common.Value(in.ListenerArn))
		return err
	})
	if err != nil {
		return nil, err
	}

	certs, next, err := page(l.certificates(), func(c api.Certificate) string { return common.Value(c.CertificateArn) },
		in.Marker, in.PageSize)
	if err != nil {
		return nil, err
	}

	return &api.DescribeListenerCertificatesOutput{Certificates: certs, NextMarker: next}, nil
}

func (s *Service) RemoveListenerCertificates(
	ctx context.Context, in *api.RemoveListenerCertificatesInput,
) (*api.RemoveListenerCertificatesOutput, error) {
	_, err := s.changeListener(ctx, common.Value(in.ListenerArn), func(_ storage.WriteTx, _ request.Scope, _ *balancer,
		l *listener,
	) error {
		for _, c := range in.Certificates {
			if slices.ContainsFunc(l.L.Certificates, func(o api.Certificate) bool {
				return common.Value(o.CertificateArn) == common.Value(c.CertificateArn)
			}) {
				return awserror.New("OperationNotPermittedException", "The default certificate cannot be removed")
			}

			l.Extra = slices.DeleteFunc(l.Extra, func(o api.Certificate) bool {
				return common.Value(o.CertificateArn) == common.Value(c.CertificateArn)
			})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.RemoveListenerCertificatesOutput{}, nil
}
