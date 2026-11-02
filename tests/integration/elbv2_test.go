package integration_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	elb "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbtypes "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
)

func elbClient(s *httptest.Server) *elb.Client {
	return elb.New(elb.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
		}),
		Retryer: aws.NopRetryer{},
	})
}

// elbSubnets returns the default VPC's ID and two of its subnets.
func elbSubnets(t *testing.T, s *httptest.Server) (vpc string, subnets []string) {
	t.Helper()
	out, err := ec2Client(s, "test", "us-east-1").DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{
		Filters: []ec2types.Filter{ec2Filter("default-for-az", "true")},
	})
	if err != nil {
		t.Fatal(err)
	}

	return aws.ToString(out.Subnets[0].VpcId),
		[]string{aws.ToString(out.Subnets[0].SubnetId), aws.ToString(out.Subnets[1].SubnetId)}
}

// elbCall sends a request to a load balancer through /_aws/elb/<label>/.
func elbCall(t *testing.T, s *httptest.Server, dnsName, method, path string, header map[string]string) (
	status int, respHeader http.Header, respBody string,
) {
	t.Helper()
	label, _, _ := strings.Cut(dnsName, ".")
	req, err := http.NewRequestWithContext(context.Background(), method, s.URL+"/_aws/elb/"+label+path, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	for k, v := range header {
		if k == "Host" {
			req.Host = v
			continue
		}

		req.Header.Set(k, v)
	}

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	return resp.StatusCode, resp.Header, string(data)
}

func TestLoadBalancerManagement(t *testing.T) {
	s := server(t)
	c := elbClient(s)
	ctx := context.Background()
	vpc, subnets := elbSubnets(t, s)

	_, err := c.CreateLoadBalancer(ctx, &elb.CreateLoadBalancerInput{Name: aws.String("one"), Subnets: subnets[:1]})
	if err == nil {
		t.Fatal("an application load balancer needs two zones")
	}

	created, err := c.CreateLoadBalancer(ctx, &elb.CreateLoadBalancerInput{
		Name: aws.String("web"), Subnets: subnets,
		Tags: []elbtypes.Tag{{Key: aws.String("team"), Value: aws.String("core")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	lb := created.LoadBalancers[0]
	if lb.Type != elbtypes.LoadBalancerTypeEnumApplication || lb.State.Code != elbtypes.LoadBalancerStateEnumActive ||
		!strings.HasSuffix(aws.ToString(lb.DNSName), ".us-east-1.elb.amazonaws.com") || len(lb.SecurityGroups) != 1 ||
		aws.ToString(lb.VpcId) != vpc || len(lb.AvailabilityZones) != 2 {
		t.Fatalf("%+v", lb)
	}

	again, err := c.CreateLoadBalancer(ctx, &elb.CreateLoadBalancerInput{Name: aws.String("web"), Subnets: subnets})
	if err != nil || aws.ToString(again.LoadBalancers[0].LoadBalancerArn) != aws.ToString(lb.LoadBalancerArn) {
		t.Fatalf("idempotent create: %v", err)
	}

	_, err = c.DescribeLoadBalancers(ctx, &elb.DescribeLoadBalancersInput{Names: []string{"missing"}})
	if errorCode(err) != "LoadBalancerNotFound" {
		t.Fatalf("missing: %v", err)
	}

	group, err := c.CreateTargetGroup(ctx, &elb.CreateTargetGroupInput{
		Name: aws.String("ips"), TargetType: elbtypes.TargetTypeEnumIp, Protocol: elbtypes.ProtocolEnumHttp,
		Port: aws.Int32(8080), VpcId: &vpc,
	})
	if err != nil {
		t.Fatal(err)
	}

	tg := group.TargetGroups[0]
	if aws.ToString(tg.HealthCheckPath) != "/" || aws.ToInt32(tg.HealthCheckIntervalSeconds) != 30 {
		t.Fatalf("%+v", tg)
	}

	_, err = c.RegisterTargets(ctx, &elb.RegisterTargetsInput{
		TargetGroupArn: tg.TargetGroupArn, Targets: []elbtypes.TargetDescription{{Id: aws.String("10.0.0.300")}},
	})
	if errorCode(err) != "InvalidTarget" {
		t.Fatalf("bad IP: %v", err)
	}

	_, err = c.RegisterTargets(ctx, &elb.RegisterTargetsInput{
		TargetGroupArn: tg.TargetGroupArn, Targets: []elbtypes.TargetDescription{{Id: aws.String("172.31.0.10")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	health, err := c.DescribeTargetHealth(ctx, &elb.DescribeTargetHealthInput{TargetGroupArn: tg.TargetGroupArn})
	if err != nil || len(health.TargetHealthDescriptions) != 1 ||
		health.TargetHealthDescriptions[0].TargetHealth.State != elbtypes.TargetHealthStateEnumUnused {
		t.Fatalf("health before a listener: %+v %v", health, err)
	}

	listener, err := c.CreateListener(ctx, &elb.CreateListenerInput{
		LoadBalancerArn: lb.LoadBalancerArn, Protocol: elbtypes.ProtocolEnumHttp, Port: aws.Int32(80),
		DefaultActions: []elbtypes.Action{{Type: elbtypes.ActionTypeEnumForward, TargetGroupArn: tg.TargetGroupArn}},
	})
	if err != nil {
		t.Fatal(err)
	}

	listenerArn := listener.Listeners[0].ListenerArn
	_, err = c.CreateListener(ctx, &elb.CreateListenerInput{
		LoadBalancerArn: lb.LoadBalancerArn, Protocol: elbtypes.ProtocolEnumHttp, Port: aws.Int32(80),
		DefaultActions: []elbtypes.Action{{
			Type:                elbtypes.ActionTypeEnumFixedResponse,
			FixedResponseConfig: &elbtypes.FixedResponseActionConfig{StatusCode: aws.String("404")},
		}},
	})
	if errorCode(err) != "DuplicateListener" {
		t.Fatalf("duplicate listener: %v", err)
	}

	health, err = c.DescribeTargetHealth(ctx, &elb.DescribeTargetHealthInput{TargetGroupArn: tg.TargetGroupArn})
	if err != nil || health.TargetHealthDescriptions[0].TargetHealth.State != elbtypes.TargetHealthStateEnumHealthy {
		t.Fatalf("health: %+v %v", health, err)
	}

	pathRule := func(priority int32, path string) *elb.CreateRuleOutput {
		out, ruleErr := c.CreateRule(ctx, &elb.CreateRuleInput{
			ListenerArn: listenerArn, Priority: aws.Int32(priority),
			Conditions: []elbtypes.RuleCondition{{
				Field: aws.String("path-pattern"), PathPatternConfig: &elbtypes.PathPatternConditionConfig{
					Values: []string{path},
				},
			}},
			Actions: []elbtypes.Action{{
				Type: elbtypes.ActionTypeEnumFixedResponse, FixedResponseConfig: &elbtypes.FixedResponseActionConfig{
					StatusCode: aws.String("200"), MessageBody: aws.String(path),
				},
			}},
		})
		if ruleErr != nil {
			t.Fatal(ruleErr)
		}

		return out
	}

	first := pathRule(10, "/a/*")
	second := pathRule(20, "/b/*")
	_, err = c.CreateRule(ctx, &elb.CreateRuleInput{
		ListenerArn: listenerArn, Priority: aws.Int32(10), Conditions: first.Rules[0].Conditions,
		Actions: first.Rules[0].Actions,
	})
	if errorCode(err) != "PriorityInUse" {
		t.Fatalf("priority in use: %v", err)
	}

	_, err = c.SetRulePriorities(ctx, &elb.SetRulePrioritiesInput{RulePriorities: []elbtypes.RulePriorityPair{
		{RuleArn: first.Rules[0].RuleArn, Priority: aws.Int32(30)},
	}})
	if err != nil {
		t.Fatal(err)
	}

	rules, err := c.DescribeRules(ctx, &elb.DescribeRulesInput{ListenerArn: listenerArn})
	if err != nil {
		t.Fatal(err)
	}

	var order []string
	for _, r := range rules.Rules {
		order = append(order, aws.ToString(r.Priority))
	}

	if strings.Join(order, ",") != "20,30,default" ||
		aws.ToString(rules.Rules[0].RuleArn) != aws.ToString(second.Rules[0].RuleArn) ||
		!aws.ToBool(rules.Rules[2].IsDefault) {
		t.Fatalf("rules: %v", order)
	}

	// A reorder is atomic, so priorities can be swapped without an intermediate collision.
	_, err = c.SetRulePriorities(ctx, &elb.SetRulePrioritiesInput{RulePriorities: []elbtypes.RulePriorityPair{
		{RuleArn: first.Rules[0].RuleArn, Priority: new(int32(20))},
		{RuleArn: second.Rules[0].RuleArn, Priority: new(int32(30))},
	}})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.ModifyRule(ctx, &elb.ModifyRuleInput{
		RuleArn: first.Rules[0].RuleArn,
		Conditions: []elbtypes.RuleCondition{
			{
				Field:             new("path-pattern"),
				PathPatternConfig: &elbtypes.PathPatternConditionConfig{Values: []string{"/edited/*"}},
			},
		},
		Actions: []elbtypes.Action{
			{
				Type: elbtypes.ActionTypeEnumFixedResponse,
				FixedResponseConfig: &elbtypes.FixedResponseActionConfig{
					StatusCode:  new("403"),
					ContentType: new("text/plain"),
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	refreshed, err := c.DescribeRules(ctx, &elb.DescribeRulesInput{ListenerArn: listenerArn})
	if err != nil {
		t.Fatal(err)
	}

	edited := refreshed.Rules[0]
	if aws.ToString(edited.RuleArn) != aws.ToString(first.Rules[0].RuleArn) || aws.ToString(edited.Priority) != "20" ||
		!slices.Equal(edited.Conditions[0].PathPatternConfig.Values, []string{"/edited/*"}) ||
		edited.Actions[0].Type != elbtypes.ActionTypeEnumFixedResponse ||
		aws.ToString(edited.Actions[0].FixedResponseConfig.StatusCode) != "403" {
		t.Fatalf("rules did not refresh after edit: %#v", edited)
	}

	_, err = c.DeleteRule(ctx, &elb.DeleteRuleInput{RuleArn: rules.Rules[2].RuleArn})
	if errorCode(err) != "OperationNotPermitted" {
		t.Fatalf("delete default rule: %v", err)
	}

	_, err = c.DeleteTargetGroup(ctx, &elb.DeleteTargetGroupInput{TargetGroupArn: tg.TargetGroupArn})
	if errorCode(err) != "ResourceInUse" {
		t.Fatalf("delete used target group: %v", err)
	}

	// Tags on each kind of resource.
	arns := []string{
		aws.ToString(lb.LoadBalancerArn), aws.ToString(tg.TargetGroupArn), aws.ToString(listenerArn),
		aws.ToString(second.Rules[0].RuleArn), aws.ToString(rules.Rules[2].RuleArn),
	}
	_, err = c.AddTags(ctx, &elb.AddTagsInput{
		ResourceArns: arns, Tags: []elbtypes.Tag{{Key: aws.String("env"), Value: aws.String("dev")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.RemoveTags(ctx, &elb.RemoveTagsInput{ResourceArns: arns[:1], TagKeys: []string{"team"}})
	if err != nil {
		t.Fatal(err)
	}

	tags, err := c.DescribeTags(ctx, &elb.DescribeTagsInput{ResourceArns: arns})
	if err != nil || len(tags.TagDescriptions) != len(arns) {
		t.Fatalf("%+v %v", tags, err)
	}

	for _, d := range tags.TagDescriptions {
		if len(d.Tags) != 1 || aws.ToString(d.Tags[0].Key) != "env" {
			t.Fatalf("%s: %+v", aws.ToString(d.ResourceArn), d.Tags)
		}
	}

	policies, err := c.DescribeSSLPolicies(ctx, &elb.DescribeSSLPoliciesInput{
		Names: []string{"ELBSecurityPolicy-TLS13-1-2-2021-06"},
	})
	if err != nil || len(policies.SslPolicies) != 1 || len(policies.SslPolicies[0].Ciphers) == 0 {
		t.Fatalf("%+v %v", policies, err)
	}

	_, err = c.ModifyLoadBalancerAttributes(ctx, &elb.ModifyLoadBalancerAttributesInput{
		LoadBalancerArn: lb.LoadBalancerArn, Attributes: []elbtypes.LoadBalancerAttribute{
			{Key: aws.String("deletion_protection.enabled"), Value: aws.String("true")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteLoadBalancer(ctx, &elb.DeleteLoadBalancerInput{LoadBalancerArn: lb.LoadBalancerArn})
	if errorCode(err) != "OperationNotPermitted" {
		t.Fatalf("deletion protection: %v", err)
	}

	_, err = c.ModifyLoadBalancerAttributes(ctx, &elb.ModifyLoadBalancerAttributesInput{
		LoadBalancerArn: lb.LoadBalancerArn, Attributes: []elbtypes.LoadBalancerAttribute{
			{Key: aws.String("deletion_protection.enabled"), Value: aws.String("false")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteLoadBalancer(ctx, &elb.DeleteLoadBalancerInput{LoadBalancerArn: lb.LoadBalancerArn})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeleteTargetGroup(ctx, &elb.DeleteTargetGroupInput{TargetGroupArn: tg.TargetGroupArn}); err != nil {
		t.Fatal(err)
	}

	_, err = c.DescribeListeners(ctx, &elb.DescribeListenersInput{ListenerArns: []string{aws.ToString(listenerArn)}})
	if errorCode(err) != "ListenerNotFound" {
		t.Fatalf("listener after delete: %v", err)
	}
}

func TestLoadBalancerServing(t *testing.T) {
	s := server(t)
	c := elbClient(s)
	ctx := context.Background()
	vpc, subnets := elbSubnets(t, s)
	functions := lambdaClient(s, "test")
	respond := createFunction(t, functions, "respond", "app.respond")

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Backend", "ip")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"path": r.URL.Path, "query": r.URL.RawQuery, "proto": r.Header.Get("X-Forwarded-Proto"),
			"for": r.Header.Get("X-Forwarded-For"),
		})
	}))
	defer backend.Close()

	backendURL, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}

	host, portText, err := net.SplitHostPort(backendURL.Host)
	if err != nil {
		t.Fatal(err)
	}

	port, _ := strconv.Atoi(portText)
	created, err := c.CreateLoadBalancer(ctx, &elb.CreateLoadBalancerInput{Name: aws.String("front"), Subnets: subnets})
	if err != nil {
		t.Fatal(err)
	}

	lb := created.LoadBalancers[0]
	dnsName := aws.ToString(lb.DNSName)
	ips, err := c.CreateTargetGroup(ctx, &elb.CreateTargetGroupInput{
		Name: aws.String("ips"), TargetType: elbtypes.TargetTypeEnumIp, Protocol: elbtypes.ProtocolEnumHttp,
		Port: aws.Int32(int32(port)), VpcId: &vpc,
	})
	if err != nil {
		t.Fatal(err)
	}

	fns, err := c.CreateTargetGroup(ctx, &elb.CreateTargetGroupInput{
		Name: aws.String("fns"), TargetType: elbtypes.TargetTypeEnumLambda,
	})
	if err != nil {
		t.Fatal(err)
	}

	listener, err := c.CreateListener(ctx, &elb.CreateListenerInput{
		LoadBalancerArn: lb.LoadBalancerArn, Protocol: elbtypes.ProtocolEnumHttp, Port: aws.Int32(80),
		DefaultActions: []elbtypes.Action{{
			Type: elbtypes.ActionTypeEnumFixedResponse, FixedResponseConfig: &elbtypes.FixedResponseActionConfig{
				StatusCode: aws.String("404"), ContentType: aws.String("text/plain"), MessageBody: aws.String("nope"),
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	listenerArn := listener.Listeners[0].ListenerArn
	rule := func(priority int32, condition elbtypes.RuleCondition, action elbtypes.Action) {
		_, ruleErr := c.CreateRule(ctx, &elb.CreateRuleInput{
			ListenerArn: listenerArn, Priority: aws.Int32(priority),
			Conditions: []elbtypes.RuleCondition{condition}, Actions: []elbtypes.Action{action},
		})
		if ruleErr != nil {
			t.Fatal(ruleErr)
		}
	}

	rule(10, elbtypes.RuleCondition{
		Field:             aws.String("path-pattern"),
		PathPatternConfig: &elbtypes.PathPatternConditionConfig{Values: []string{"/api/*"}},
	}, elbtypes.Action{Type: elbtypes.ActionTypeEnumForward, TargetGroupArn: ips.TargetGroups[0].TargetGroupArn})
	rule(20, elbtypes.RuleCondition{
		Field: aws.String("http-header"), HttpHeaderConfig: &elbtypes.HttpHeaderConditionConfig{
			HttpHeaderName: aws.String("X-Route"), Values: []string{"fn*"},
		},
	}, elbtypes.Action{Type: elbtypes.ActionTypeEnumForward, TargetGroupArn: fns.TargetGroups[0].TargetGroupArn})
	rule(30, elbtypes.RuleCondition{
		Field: aws.String("host-header"), HostHeaderConfig: &elbtypes.HostHeaderConditionConfig{
			Values: []string{"old.example.com"},
		},
	}, elbtypes.Action{Type: elbtypes.ActionTypeEnumRedirect, RedirectConfig: &elbtypes.RedirectActionConfig{
		StatusCode: elbtypes.RedirectActionStatusCodeEnumHttp301, Host: aws.String("new.example.com"),
		Protocol: aws.String("HTTPS"), Port: aws.String("443"),
	}})

	// The default action answers what no rule matches.
	status, _, body := elbCall(t, s, dnsName, "GET", "/other", nil)
	if status != http.StatusNotFound || body != "nope" {
		t.Fatalf("default: %d %s", status, body)
	}

	// A target group with no targets is unavailable.
	status, _, _ = elbCall(t, s, dnsName, "GET", "/api/items", nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("no targets: %d", status)
	}

	_, err = c.RegisterTargets(ctx, &elb.RegisterTargetsInput{
		TargetGroupArn: ips.TargetGroups[0].TargetGroupArn, Targets: []elbtypes.TargetDescription{{Id: &host}},
	})
	if err != nil {
		t.Fatal(err)
	}

	status, header, body := elbCall(t, s, dnsName, "GET", "/api/items?x=1", nil)
	var forwarded map[string]string
	if status != http.StatusOK || header.Get("X-Backend") != "ip" || json.Unmarshal([]byte(body), &forwarded) != nil ||
		forwarded["path"] != "/api/items" || forwarded["query"] != "x=1" || forwarded["proto"] != "http" ||
		forwarded["for"] == "" {
		t.Fatalf("ip target: %d %s", status, body)
	}

	_, err = c.RegisterTargets(ctx, &elb.RegisterTargetsInput{
		TargetGroupArn: fns.TargetGroups[0].TargetGroupArn,
		Targets:        []elbtypes.TargetDescription{{Id: respond.FunctionArn}},
	})
	if err != nil {
		t.Fatal(err)
	}

	status, header, body = elbCall(t, s, dnsName, "POST", "/lambda?a=1&a=2",
		map[string]string{"X-Route": "fn-1"})
	var event struct {
		RequestContext struct {
			ELB struct {
				TargetGroupArn string `json:"targetGroupArn"`
			} `json:"elb"`
		} `json:"requestContext"`
		Headers               map[string]string `json:"headers"`
		QueryStringParameters map[string]string `json:"queryStringParameters"`
		HTTPMethod            string            `json:"httpMethod"`
		Path                  string            `json:"path"`
	}
	if status != http.StatusCreated || header.Get("X-Handler") != "respond" ||
		json.Unmarshal([]byte(body), &event) != nil ||
		event.RequestContext.ELB.TargetGroupArn != aws.ToString(fns.TargetGroups[0].TargetGroupArn) ||
		event.HTTPMethod != "POST" || event.Path != "/lambda" || event.QueryStringParameters["a"] != "2" ||
		event.Headers["x-route"] != "fn-1" || event.Headers["x-forwarded-port"] != "80" {
		t.Fatalf("lambda target: %d %s", status, body)
	}

	// Multi-value headers and query strings when the target group asks.
	_, err = c.ModifyTargetGroupAttributes(ctx, &elb.ModifyTargetGroupAttributesInput{
		TargetGroupArn: fns.TargetGroups[0].TargetGroupArn, Attributes: []elbtypes.TargetGroupAttribute{
			{Key: aws.String("lambda.multi_value_headers.enabled"), Value: aws.String("true")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, _, body = elbCall(t, s, dnsName, "GET", "/lambda?a=1&a=2", map[string]string{"X-Route": "fn"})
	var multi struct {
		MultiValueQueryStringParameters map[string][]string `json:"multiValueQueryStringParameters"`
	}
	if json.Unmarshal([]byte(body), &multi) != nil || len(multi.MultiValueQueryStringParameters["a"]) != 2 {
		t.Fatalf("multi-value: %s", body)
	}

	status, header, _ = elbCall(t, s, dnsName, "GET", "/path?q=1", map[string]string{"Host": "old.example.com"})
	if status != http.StatusMovedPermanently || header.Get("Location") != "https://new.example.com/path?q=1" {
		t.Fatalf("redirect: %d %s", status, header.Get("Location"))
	}

	// The DNS name routes too.
	req, err := http.NewRequestWithContext(ctx, "GET", s.URL+"/other", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	req.Host = dnsName
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("DNS name: %d", resp.StatusCode)
	}

	// A listener on another port, by "<label>:<port>".
	status, _, _ = elbCall(t, s, strings.Replace(dnsName, ".", ":8080.", 1), "GET", "/", nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("missing listener: %d", status)
	}
}
