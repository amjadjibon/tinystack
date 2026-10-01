package elbv2

import (
	"context"
	"slices"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/elbv2"
)

const (
	tls12       = "TLSv1.2"
	tls13       = "TLSv1.3"
	ecdsaAES128 = "ECDHE-ECDSA-AES128-GCM-SHA256"
	rsaAES128   = "ECDHE-RSA-AES128-GCM-SHA256"
	ecdsaAES256 = "ECDHE-ECDSA-AES256-GCM-SHA384"
	rsaAES256   = "ECDHE-RSA-AES256-GCM-SHA384"
)

var (
	// gcmCiphers are the forward secret AES-GCM ciphers restricted policies keep.
	gcmCiphers   = []string{ecdsaAES128, rsaAES128, ecdsaAES256, rsaAES256}
	tls12Ciphers = []string{
		ecdsaAES128, rsaAES128, "ECDHE-ECDSA-AES128-SHA256", "ECDHE-RSA-AES128-SHA256", ecdsaAES256, rsaAES256,
		"ECDHE-ECDSA-AES256-SHA384", "ECDHE-RSA-AES256-SHA384",
	}
	tls13Ciphers = []string{"TLS_AES_128_GCM_SHA256", "TLS_AES_256_GCM_SHA384", "TLS_CHACHA20_POLY1305_SHA256"}
	rsaCiphers   = []string{"AES128-GCM-SHA256", "AES128-SHA256", "AES256-GCM-SHA384", "AES256-SHA256"}

	// sslPolicies are the predefined security policies listeners can use.
	sslPolicies = []api.SslPolicy{
		sslPolicy(defaultPolicy, []string{"TLSv1", "TLSv1.1", tls12}, tls12Ciphers,
			[]string{
				"ECDHE-ECDSA-AES128-SHA", "ECDHE-RSA-AES128-SHA", "ECDHE-RSA-AES256-SHA", "ECDHE-ECDSA-AES256-SHA",
			},
			rsaCiphers, []string{"AES128-SHA", "AES256-SHA"}),
		sslPolicy("ELBSecurityPolicy-FS-1-2-Res-2020-10", []string{tls12}, gcmCiphers),
		sslPolicy("ELBSecurityPolicy-TLS-1-2-2017-01", []string{tls12}, tls12Ciphers, rsaCiphers),
		sslPolicy("ELBSecurityPolicy-TLS13-1-2-2021-06", []string{tls12, tls13}, tls13Ciphers, tls12Ciphers),
		sslPolicy("ELBSecurityPolicy-TLS13-1-2-Res-2021-06", []string{tls12, tls13}, tls13Ciphers, gcmCiphers),
		sslPolicy("ELBSecurityPolicy-TLS13-1-3-2021-06", []string{tls13}, tls13Ciphers),
	}
)

// sslPolicy builds a policy whose ciphers have the priorities of their order.
func sslPolicy(name string, protocols []string, ciphers ...[]string) api.SslPolicy {
	p := api.SslPolicy{
		Name: new(name), SslProtocols: protocols, SupportedLoadBalancerTypes: []string{typeApplication, typeNetwork},
	}
	for _, c := range slices.Concat(ciphers...) {
		p.Ciphers = append(p.Ciphers, api.Cipher{Name: new(c), Priority: new(int64(len(p.Ciphers) + 1))})
	}

	return p
}

func (s *Service) DescribeSSLPolicies(
	_ context.Context, in *api.DescribeSSLPoliciesInput,
) (*api.DescribeSSLPoliciesOutput, error) {
	kind := value(in.LoadBalancerType)
	if kind != "" && kind != typeApplication && kind != typeNetwork {
		return nil, validation("LoadBalancerType must be application or network")
	}

	for _, name := range in.Names {
		if !slices.ContainsFunc(sslPolicies, func(p api.SslPolicy) bool { return value(p.Name) == name }) {
			return nil, awserror.New("SSLPolicyNotFoundException", "SSL policy '"+name+"' not found")
		}
	}

	var selected []api.SslPolicy
	for _, p := range sslPolicies {
		if len(in.Names) == 0 || slices.Contains(in.Names, value(p.Name)) {
			selected = append(selected, p)
		}
	}

	selected, next, err := page(selected, func(p api.SslPolicy) string { return value(p.Name) }, in.Marker, in.PageSize)
	if err != nil {
		return nil, err
	}

	return &api.DescribeSSLPoliciesOutput{SslPolicies: append([]api.SslPolicy{}, selected...), NextMarker: next}, nil
}
