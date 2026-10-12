package elb

import (
	"context"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/elb"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	protocolHTTP  = "HTTP"
	protocolHTTPS = "HTTPS"
	protocolTCP   = "TCP"
	protocolSSL   = "SSL"
	maxPort       = 65535
)

func secure(protocol string) bool { return protocol == protocolHTTPS || protocol == protocolSSL }

// application reports whether a protocol is one of the HTTP pair; the others are the TCP pair.
func application(protocol string) bool { return protocol == protocolHTTP || protocol == protocolHTTPS }

func supportedProtocol(protocol string) bool {
	return slices.Contains([]string{protocolHTTP, protocolHTTPS, protocolTCP, protocolSSL}, protocol)
}

func portInRange(port int64) bool { return port >= 1 && port <= maxPort }

// listenerOf returns the listener with the protocol names upper-cased and InstanceProtocol defaulted.
func listenerOf(l api.Listener) api.Listener {
	protocol := strings.ToUpper(common.Value(l.Protocol))
	l.Protocol = new(protocol)
	l.InstanceProtocol = new(strings.ToUpper(common.Value(common.OrElse(l.InstanceProtocol, new(protocol)))))
	return l
}

// checkListener checks one listener against the model and the documented protocol rules.
func checkListener(l api.Listener) error {
	protocol, backend := common.Value(l.Protocol), common.Value(l.InstanceProtocol)
	switch {
	case !supportedProtocol(protocol) || !supportedProtocol(backend):
		return awserror.New("UnsupportedProtocolException", "Protocol must be HTTP, HTTPS, TCP or SSL")
	case !portInRange(common.Value(l.LoadBalancerPort)):
		return invalid("LoadBalancerPort must be between 1 and %d", maxPort)
	case !portInRange(common.Value(l.InstancePort)):
		return invalid("InstancePort must be between 1 and %d", maxPort)
	case application(protocol) != application(backend):
		return configuration("InstanceProtocol must be HTTP or HTTPS for an HTTP or HTTPS listener, " +
			"and TCP or SSL for a TCP or SSL listener")
	case secure(protocol) && common.Value(l.SSLCertificateId) == "":
		return awserror.New("CertificateNotFoundException", "SSLCertificateId is required for HTTPS and SSL listeners")
	}

	return nil
}

// checkListeners checks a complete listener set and returns it as descriptions ordered by port. A port may appear once,
// and the listeners that share an InstancePort must all use secure back ends or all use plain ones.
func checkListeners(listeners []api.Listener) ([]api.ListenerDescription, error) {
	if len(listeners) == 0 {
		return nil, invalid("At least one listener is required")
	}

	out := make([]api.ListenerDescription, 0, len(listeners))
	for _, l := range listeners {
		l = listenerOf(l)
		if err := checkListener(l); err != nil {
			return nil, err
		}

		for _, other := range out {
			switch {
			case common.Value(other.Listener.LoadBalancerPort) == common.Value(l.LoadBalancerPort):
				return nil, awserror.New("DuplicateListenerException",
					"A listener already exists for the load balancer port")
			case common.Value(other.Listener.InstancePort) == common.Value(l.InstancePort) &&
				secure(common.Value(other.Listener.InstanceProtocol)) != secure(common.Value(l.InstanceProtocol)):
				return nil, configuration("Listeners with the same InstancePort must all use secure " +
					"InstanceProtocols (HTTPS, SSL) or all use HTTP or TCP")
			}
		}

		out = append(out, api.ListenerDescription{Listener: &l})
	}

	slices.SortFunc(out, func(a, b api.ListenerDescription) int {
		return int(common.Value(a.Listener.LoadBalancerPort) - common.Value(b.Listener.LoadBalancerPort))
	})

	return out, nil
}

// CreateLoadBalancerListeners adds listeners; one already on its port with the same settings is left as it is.
func (s *Service) CreateLoadBalancerListeners(
	ctx context.Context,
	in *api.CreateLoadBalancerListenerInput,
) (*api.CreateLoadBalancerListenerOutput, error) {
	if in == nil {
		return nil, invalid("Input is required")
	}

	in = common.Clone(in)
	if _, err := checkListeners(in.Listeners); err != nil {
		return nil, err
	}

	_, err := s.update(ctx, in.LoadBalancerName, func(_ storage.WriteTx, _ request.Scope, b *balancer) error {
		current := make([]api.Listener, 0, len(b.Description.ListenerDescriptions)+len(in.Listeners))
		for _, d := range b.Description.ListenerDescriptions {
			current = append(current, *d.Listener)
		}

		for _, l := range in.Listeners {
			l = listenerOf(l)
			i := slices.IndexFunc(current, func(c api.Listener) bool {
				return common.Value(c.LoadBalancerPort) == common.Value(l.LoadBalancerPort)
			})
			switch {
			case i < 0:
				current = append(current, l)
			case !sameListener(current[i], l):
				return awserror.New("DuplicateListenerException",
					"A listener already exists on the load balancer port with different settings")
			}
		}

		descriptions, err := checkListeners(current)
		b.Description.ListenerDescriptions = descriptions
		return err
	})

	return &api.CreateLoadBalancerListenerOutput{}, err
}

func sameListener(a, b api.Listener) bool {
	return common.Value(a.Protocol) == common.Value(b.Protocol) &&
		common.Value(a.InstanceProtocol) == common.Value(b.InstanceProtocol) &&
		common.Value(a.InstancePort) == common.Value(b.InstancePort) &&
		common.Value(a.SSLCertificateId) == common.Value(b.SSLCertificateId)
}

// DeleteLoadBalancerListeners removes the listeners on the ports; ports with no listener are ignored.
func (s *Service) DeleteLoadBalancerListeners(
	ctx context.Context,
	in *api.DeleteLoadBalancerListenerInput,
) (*api.DeleteLoadBalancerListenerOutput, error) {
	if in == nil || len(in.LoadBalancerPorts) == 0 {
		return nil, invalid("LoadBalancerPorts is required")
	}

	_, err := s.update(ctx, in.LoadBalancerName, func(_ storage.WriteTx, _ request.Scope, b *balancer) error {
		b.Description.ListenerDescriptions = slices.DeleteFunc(b.Description.ListenerDescriptions,
			func(d api.ListenerDescription) bool {
				return slices.Contains(in.LoadBalancerPorts, common.Value(d.Listener.LoadBalancerPort))
			})
		return nil
	})

	return &api.DeleteLoadBalancerListenerOutput{}, err
}
