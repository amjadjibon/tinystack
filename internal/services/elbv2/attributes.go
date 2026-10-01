package elbv2

import (
	"maps"
	"slices"
	"strconv"
	"strings"
)

// Attribute defaults. A request may set only keys its resource has; values
// are checked where TinyStack reads them.
const (
	valueTrue    = "true"
	valueFalse   = "false"
	valueOff     = "off"
	keyCrossZone = "load_balancing.cross_zone.enabled"
)

var (
	applicationAttributes = map[string]string{
		"access_logs.s3.enabled":                                   valueFalse,
		"access_logs.s3.bucket":                                    "",
		"access_logs.s3.prefix":                                    "",
		"connection_logs.s3.enabled":                               valueFalse,
		"connection_logs.s3.bucket":                                "",
		"connection_logs.s3.prefix":                                "",
		"health_check_logs.s3.enabled":                             valueFalse,
		"health_check_logs.s3.bucket":                              "",
		"health_check_logs.s3.prefix":                              "",
		"client_keep_alive.seconds":                                "3600",
		"deletion_protection.enabled":                              valueFalse,
		"idle_timeout.timeout_seconds":                             "60",
		keyCrossZone:                                               valueTrue,
		"routing.http.desync_mitigation_mode":                      "defensive",
		"routing.http.drop_invalid_header_fields.enabled":          valueFalse,
		"routing.http.preserve_host_header.enabled":                valueFalse,
		"routing.http.x_amzn_tls_version_and_cipher_suite.enabled": valueFalse,
		"routing.http.xff_client_port.enabled":                     valueFalse,
		"routing.http.xff_header_processing.mode":                  "append",
		"routing.http2.enabled":                                    valueTrue,
		"waf.fail_open.enabled":                                    valueFalse,
		"zonal_shift.config.enabled":                               valueFalse,
	}
	networkAttributes = map[string]string{
		"access_logs.s3.enabled":           valueFalse,
		"access_logs.s3.bucket":            "",
		"access_logs.s3.prefix":            "",
		"deletion_protection.enabled":      valueFalse,
		keyCrossZone:                       valueFalse,
		"dns_record.client_routing_policy": "any_availability_zone",
		"zonal_shift.config.enabled":       valueFalse,
		"ipv6.deny_all_igw_traffic":        valueFalse,
	}
	targetGroupAttributes = map[string]string{
		"deregistration_delay.timeout_seconds":                                           "300",
		"stickiness.enabled":                                                             valueFalse,
		"stickiness.type":                                                                "lb_cookie",
		"stickiness.lb_cookie.duration_seconds":                                          "86400",
		"stickiness.app_cookie.cookie_name":                                              "",
		"stickiness.app_cookie.duration_seconds":                                         "86400",
		"load_balancing.algorithm.type":                                                  "round_robin",
		"load_balancing.algorithm.anomaly_mitigation":                                    valueOff,
		"slow_start.duration_seconds":                                                    "0",
		keyCrossZone:                                                                     "use_load_balancer_configuration",
		"target_group_health.dns_failover.minimum_healthy_targets.count":                 "1",
		"target_group_health.dns_failover.minimum_healthy_targets.percentage":            valueOff,
		"target_group_health.unhealthy_state_routing.minimum_healthy_targets.count":      "1",
		"target_group_health.unhealthy_state_routing.minimum_healthy_targets.percentage": valueOff,
	}
	networkTargetGroupAttributes = map[string]string{
		"deregistration_delay.timeout_seconds":                                           "300",
		"deregistration_delay.connection_termination.enabled":                            valueFalse,
		"stickiness.enabled":                                                             valueFalse,
		"stickiness.type":                                                                "source_ip",
		"proxy_protocol_v2.enabled":                                                      valueFalse,
		"preserve_client_ip.enabled":                                                     valueTrue,
		"connection_termination.enabled":                                                 valueFalse,
		keyCrossZone:                                                                     "use_load_balancer_configuration",
		"target_health_state.unhealthy.connection_termination.enabled":                   valueTrue,
		"target_health_state.unhealthy.draining_interval_seconds":                        "0",
		"target_failover.on_deregistration":                                              "no_rebalance",
		"target_failover.on_unhealthy":                                                   "no_rebalance",
		"target_group_health.dns_failover.minimum_healthy_targets.count":                 "1",
		"target_group_health.dns_failover.minimum_healthy_targets.percentage":            valueOff,
		"target_group_health.unhealthy_state_routing.minimum_healthy_targets.count":      "1",
		"target_group_health.unhealthy_state_routing.minimum_healthy_targets.percentage": valueOff,
	}
	lambdaTargetGroupAttributes = map[string]string{"lambda.multi_value_headers.enabled": valueFalse}
	listenerAttributes          = map[string]string{
		"routing.http.response.server.enabled":                                  valueTrue,
		"routing.http.request.x_amzn_mtls_clientcert_serial_number.header_name": "",
		"routing.http.request.x_amzn_mtls_clientcert_issuer.header_name":        "",
		"routing.http.request.x_amzn_mtls_clientcert_subject.header_name":       "",
		"routing.http.request.x_amzn_mtls_clientcert_validity.header_name":      "",
		"routing.http.request.x_amzn_mtls_clientcert_leaf.header_name":          "",
		"routing.http.request.x_amzn_mtls_clientcert.header_name":               "",
		"routing.http.request.x_amzn_tls_version.header_name":                   "",
		"routing.http.request.x_amzn_tls_cipher_suite.header_name":              "",
		"routing.http.response.strict_transport_security.header_value":          "",
		"routing.http.response.access_control_allow_origin.header_value":        "",
		"routing.http.response.access_control_allow_methods.header_value":       "",
		"routing.http.response.access_control_allow_headers.header_value":       "",
		"routing.http.response.access_control_allow_credentials.header_value":   "",
		"routing.http.response.access_control_expose_headers.header_value":      "",
		"routing.http.response.access_control_max_age.header_value":             "",
		"routing.http.response.content_security_policy.header_value":            "",
		"routing.http.response.x_content_type_options.header_value":             "",
		"routing.http.response.x_frame_options.header_value":                    "",
	}
	networkListenerAttributes = map[string]string{"tcp.idle_timeout.seconds": "350"}
)

// setAttributes applies changes to attributes, which must all be keys of
// defaults.
func setAttributes(current, defaults map[string]string, keys, values []string) (map[string]string, error) {
	out := maps.Clone(defaults)
	maps.Copy(out, current)
	for i, k := range keys {
		if _, ok := defaults[k]; !ok {
			return nil, validation("Attribute key '%s' is not recognized", k)
		}

		v := values[i]
		if strings.HasSuffix(k, ".enabled") && v != valueTrue && v != valueFalse {
			return nil, validation("Attribute '%s' must be 'true' or 'false'", k)
		}

		if strings.HasSuffix(k, "_seconds") || strings.HasSuffix(k, ".seconds") {
			if _, err := strconv.ParseUint(v, 10, 32); err != nil {
				return nil, validation("Attribute '%s' must be a whole number of seconds", k)
			}
		}

		out[k] = v
	}

	return out, nil
}

// attributeKeys returns attributes' keys in order.
func attributeKeys(attributes map[string]string) []string {
	return slices.Sorted(maps.Keys(attributes))
}
