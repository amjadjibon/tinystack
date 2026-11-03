package integration_test

import (
	"net/http"
	"testing"
)

func TestConsoleELBv2Lifecycle(t *testing.T) {
	srv := server(t)
	const account, region = "000000000000", "eu-west-1"
	callService := func(service, operation string, input map[string]any) map[string]any {
		t.Helper()
		return consoleResponse(t, srv,
			consoleLambdaRequest(t, srv, service, operation, account, region, input),
			http.StatusOK)["output"].(map[string]any)
	}
	call := func(operation string, input map[string]any) map[string]any {
		t.Helper()
		return callService("elbv2", operation, input)
	}
	fail := func(operation string, input map[string]any) {
		t.Helper()
		consoleResponse(t, srv,
			consoleLambdaRequest(t, srv, "elbv2", operation, account, region, input),
			http.StatusBadRequest)
	}
	first := func(out map[string]any, key string) map[string]any {
		t.Helper()
		items, _ := out[key].([]any)
		if len(items) == 0 {
			t.Fatal(key, out)
		}

		return items[0].(map[string]any)
	}

	var capabilities map[string]any
	if internal(t, srv, http.MethodGet, consoleActionsPath, &capabilities) != http.StatusOK ||
		!consolePage(capabilities, "elbv2") {
		t.Fatal(capabilities)
	}

	vpc := callService("ec2", "CreateVpc", map[string]any{"CidrBlock": "10.0.0.0/16"})["Vpc"].(map[string]any)
	vpcID := vpc["VpcId"].(string)
	subnets := []string{}
	for i, zone := range []string{"eu-west-1a", "eu-west-1b"} {
		subnet := callService("ec2", "CreateSubnet", map[string]any{
			"VpcId": vpcID, "CidrBlock": []string{"10.0.1.0/24", "10.0.2.0/24"}[i], "AvailabilityZone": zone,
		})["Subnet"].(map[string]any)
		subnets = append(subnets, subnet["SubnetId"].(string))
	}

	balancer := first(call("CreateLoadBalancer", map[string]any{
		"Name": "console-alb", "Type": "application", "Scheme": "internet-facing", "Subnets": subnets,
		"Tags": []map[string]any{{"Key": "team", "Value": "web"}},
	}), "LoadBalancers")
	lbARN := balancer["LoadBalancerArn"].(string)
	if balancer["DNSName"] == "" || balancer["State"].(map[string]any)["Code"] == "" {
		t.Fatal(balancer)
	}

	call("ModifyLoadBalancerAttributes", map[string]any{
		"LoadBalancerArn": lbARN,
		"Attributes": []map[string]any{
			{"Key": "idle_timeout.timeout_seconds", "Value": "120"},
			{"Key": "deletion_protection.enabled", "Value": "true"},
		},
	})
	attributes := map[string]string{}
	described := call("DescribeLoadBalancerAttributes", map[string]any{"LoadBalancerArn": lbARN})
	for _, a := range described["Attributes"].([]any) {
		attributes[a.(map[string]any)["Key"].(string)] = a.(map[string]any)["Value"].(string)
	}

	if attributes["idle_timeout.timeout_seconds"] != "120" || attributes["deletion_protection.enabled"] != "true" {
		t.Fatal(attributes)
	}

	fail("DeleteLoadBalancer", map[string]any{"LoadBalancerArn": lbARN})
	call("SetSubnets", map[string]any{"LoadBalancerArn": lbARN, "Subnets": subnets})

	groupOut := callService("ec2", "CreateSecurityGroup", map[string]any{
		"GroupName": "console-web", "Description": "web", "VpcId": vpcID,
	})
	call("SetSecurityGroups", map[string]any{
		"LoadBalancerArn": lbARN, "SecurityGroups": []string{groupOut["GroupId"].(string)},
	})

	group := first(call("CreateTargetGroup", map[string]any{
		"Name": "console-tg", "Protocol": "HTTP", "Port": 80, "VpcId": vpcID, "TargetType": "ip",
		"HealthCheckPath": "/health",
	}), "TargetGroups")
	tgARN := group["TargetGroupArn"].(string)
	call("ModifyTargetGroup", map[string]any{
		"TargetGroupArn": tgARN, "HealthCheckPath": "/ready", "HealthCheckIntervalSeconds": 20,
		"Matcher": map[string]any{"HttpCode": "200-299"},
	})
	group = first(call("DescribeTargetGroups", map[string]any{"TargetGroupArns": []string{tgARN}}), "TargetGroups")
	if group["HealthCheckPath"] != "/ready" || group["HealthCheckIntervalSeconds"] != float64(20) {
		t.Fatal(group)
	}

	call("ModifyTargetGroupAttributes", map[string]any{
		"TargetGroupArn": tgARN,
		"Attributes":     []map[string]any{{"Key": "deregistration_delay.timeout_seconds", "Value": "30"}},
	})
	call("RegisterTargets", map[string]any{
		"TargetGroupArn": tgARN, "Targets": []map[string]any{{"Id": "10.0.1.10", "Port": 8080}},
	})
	health := call("DescribeTargetHealth", map[string]any{"TargetGroupArn": tgARN})["TargetHealthDescriptions"].([]any)
	if len(health) != 1 {
		t.Fatal(health)
	}

	call("DeregisterTargets", map[string]any{
		"TargetGroupArn": tgARN, "Targets": []map[string]any{{"Id": "10.0.1.10", "Port": 8080}},
	})

	listener := first(call("CreateListener", map[string]any{
		"LoadBalancerArn": lbARN, "Protocol": "HTTP", "Port": 80,
		"DefaultActions": []map[string]any{{"Type": "forward", "TargetGroupArn": tgARN}},
	}), "Listeners")
	listenerARN := listener["ListenerArn"].(string)
	call("ModifyListener", map[string]any{
		"ListenerArn": listenerARN,
		"DefaultActions": []map[string]any{{
			"Type": "fixed-response",
			"FixedResponseConfig": map[string]any{
				"StatusCode": "503", "ContentType": "text/plain", "MessageBody": "down",
			},
		}},
	})
	call("DescribeListenerAttributes", map[string]any{"ListenerArn": listenerARN})
	call("DescribeListenerCertificates", map[string]any{"ListenerArn": listenerARN})

	if policies := call("DescribeSSLPolicies", map[string]any{})["SslPolicies"].([]any); len(policies) == 0 {
		t.Fatal("no SSL policies")
	}

	forward := []map[string]any{{"Type": "forward", "TargetGroupArn": tgARN}}
	apiRule := first(call("CreateRule", map[string]any{
		"ListenerArn": listenerARN, "Priority": 10, "Actions": forward,
		"Conditions": []map[string]any{{
			"Field": "path-pattern", "PathPatternConfig": map[string]any{"Values": []string{"/api/*"}},
		}},
	}), "Rules")
	staticRule := first(call("CreateRule", map[string]any{
		"ListenerArn": listenerARN, "Priority": 20,
		"Actions": []map[string]any{{
			"Type":           "redirect",
			"RedirectConfig": map[string]any{"StatusCode": "HTTP_301", "Host": "static.example.com"},
		}},
		"Conditions": []map[string]any{
			{"Field": "host-header", "HostHeaderConfig": map[string]any{"Values": []string{"old.example.com"}}},
			{"Field": "http-header", "HttpHeaderConfig": map[string]any{
				"HttpHeaderName": "X-Env", "Values": []string{"prod"},
			}},
			{"Field": "source-ip", "SourceIpConfig": map[string]any{"Values": []string{"10.0.0.0/8"}}},
		},
	}), "Rules")
	call("ModifyRule", map[string]any{
		"RuleArn": apiRule["RuleArn"],
		"Conditions": []map[string]any{{
			"Field": "query-string",
			"QueryStringConfig": map[string]any{
				"Values": []map[string]any{{"Key": "v", "Value": "2"}},
			},
		}},
		"Actions": forward,
	})
	call("SetRulePriorities", map[string]any{"RulePriorities": []map[string]any{
		{"RuleArn": apiRule["RuleArn"], "Priority": 30},
		{"RuleArn": staticRule["RuleArn"], "Priority": 5},
	}})

	rules := call("DescribeRules", map[string]any{"ListenerArn": listenerARN})["Rules"].([]any)
	priorities := map[string]bool{}
	defaultRule := ""
	for _, r := range rules {
		rule := r.(map[string]any)
		priorities[rule["Priority"].(string)] = true
		if rule["IsDefault"] == true {
			defaultRule = rule["RuleArn"].(string)
		}
	}

	if len(rules) != 3 || !priorities["30"] || !priorities["5"] || !priorities["default"] {
		t.Fatal(rules)
	}

	fail("DeleteRule", map[string]any{"RuleArn": defaultRule})
	call("DeleteRule", map[string]any{"RuleArn": staticRule["RuleArn"]})

	call("AddTags", map[string]any{
		"ResourceArns": []string{lbARN, tgARN}, "Tags": []map[string]any{{"Key": "env", "Value": "dev"}},
	})
	call("RemoveTags", map[string]any{"ResourceArns": []string{lbARN}, "TagKeys": []string{"team"}})
	descriptions := call("DescribeTags", map[string]any{"ResourceArns": []string{lbARN}})["TagDescriptions"].([]any)
	if tags := descriptions[0].(map[string]any)["Tags"].([]any); len(tags) != 1 {
		t.Fatal(tags)
	}

	fail("DeleteTargetGroup", map[string]any{"TargetGroupArn": tgARN})
	call("DeleteListener", map[string]any{"ListenerArn": listenerARN})
	call("DeleteTargetGroup", map[string]any{"TargetGroupArn": tgARN})
	call("ModifyLoadBalancerAttributes", map[string]any{
		"LoadBalancerArn": lbARN,
		"Attributes":      []map[string]any{{"Key": "deletion_protection.enabled", "Value": "false"}},
	})
	call("DeleteLoadBalancer", map[string]any{"LoadBalancerArn": lbARN})

	if left, _ := call("DescribeLoadBalancers", map[string]any{})["LoadBalancers"].([]any); len(left) != 0 {
		t.Fatal(left)
	}
}
