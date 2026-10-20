package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	restapi "github.com/aws/aws-sdk-go-v2/service/apigateway"
	resttypes "github.com/aws/aws-sdk-go-v2/service/apigateway/types"
	httpapi "github.com/aws/aws-sdk-go-v2/service/apigatewayv2"
	httptypes "github.com/aws/aws-sdk-go-v2/service/apigatewayv2/types"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	logstypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	elb "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbtypes "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
	sdklambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/sns"

	"github.com/amjadjibon/tinystack/internal/config"
)

// revokePermissions removes every permission from the function's resource policy.
func revokePermissions(t *testing.T, s *httptest.Server, function string) {
	t.Helper()
	ctx := context.Background()
	c := lambdaClient(s, "test")
	policy, err := c.GetPolicy(ctx, &sdklambda.GetPolicyInput{FunctionName: &function})
	mustAuthorize(t, err)
	var document struct {
		Statement []struct{ Sid string }
	}
	if err = json.Unmarshal([]byte(aws.ToString(policy.Policy)), &document); err != nil {
		t.Fatal(err)
	}

	for _, statement := range document.Statement {
		_, err = c.RemovePermission(ctx, &sdklambda.RemovePermissionInput{
			FunctionName: &function, StatementId: &statement.Sid,
		})
		mustAuthorize(t, err)
	}
}

// A CloudWatch alarm publishes to a topic and invokes a function as the CloudWatch principal, with the alarm for the
// source. A denied action is recorded in the alarm's history as failed.
func TestServicePrincipalAlarmActions(t *testing.T) {
	const alarm = "arn:aws:cloudwatch:us-east-1:000000000000:alarm:high"
	for _, target := range []string{"sns", "lambda"} {
		principal := "cloudwatch.amazonaws.com"
		if target == "lambda" {
			principal = "lambda.alarms.cloudwatch.amazonaws.com"
		}

		for _, mode := range deliveryModes {
			for _, choice := range deliveryChoices {
				t.Run(fmt.Sprintf("%s/%s/%s", target, mode, choice), func(t *testing.T) {
					s, runtime := deliveryServer(t, mode)
					ctx := context.Background()
					var action string
					if target == "sns" {
						topic, err := snsClient(s, "test").CreateTopic(ctx, &sns.CreateTopicInput{Name: new("alarms")})
						mustAuthorize(t, err)
						action = *topic.TopicArn
					} else {
						action = *createFunction(t, lambdaClient(s, "test"), "react", "app.echo").FunctionArn
					}

					shapePolicy(t, s, action, principal, alarm, choice)
					c := cloudwatchClient(s)
					_, err := c.PutMetricAlarm(ctx, &cloudwatch.PutMetricAlarmInput{
						AlarmName: new("high"), Namespace: new("App"), MetricName: new("Load"),
						Statistic: cwtypes.StatisticMaximum, Period: new(int32(60)), EvaluationPeriods: new(int32(1)),
						Threshold: new(5.0), ComparisonOperator: cwtypes.ComparisonOperatorGreaterThanThreshold,
						AlarmActions: []string{action},
					})
					mustAuthorize(t, err)
					_, err = c.PutMetricData(ctx, &cloudwatch.PutMetricDataInput{
						Namespace:  new("App"),
						MetricData: []cwtypes.MetricDatum{{MetricName: new("Load"), Value: new(9.0)}},
					})
					mustAuthorize(t, err)

					var summary string
					waitFor(t, "the action's history", func() bool {
						history, historyErr := c.DescribeAlarmHistory(ctx, &cloudwatch.DescribeAlarmHistoryInput{
							AlarmName: new("high"), HistoryItemType: cwtypes.HistoryItemTypeAction,
						})
						mustAuthorize(t, historyErr)
						if len(history.AlarmHistoryItems) == 0 {
							return false
						}

						summary = aws.ToString(history.AlarmHistoryItems[0].HistorySummary)
						return true
					})

					want := "Successfully executed action " + action
					if !delivered(mode, choice) {
						want = "Failed to execute action " + action
					}

					if !strings.HasPrefix(summary, want) {
						t.Fatalf("history = %q, want %q", summary, want)
					}

					if target == "lambda" && functionInvoked(runtime) != delivered(mode, choice) {
						t.Fatalf("function invoked = %t, want %t", !delivered(mode, choice), delivered(mode, choice))
					}

					wantDeliveryRecord(t, s, mode, choice, action)
				})
			}
		}
	}
}

// AWS sends a test message to a subscription filter's destination as the filter is saved, so a Lambda function that
// does not allow CloudWatch Logs fails PutSubscriptionFilter. The log group is the source, as it is for events.
func TestServicePrincipalLogsSubscriptionLambda(t *testing.T) {
	const source = "arn:aws:logs:us-east-1:000000000000:log-group:app:*"
	for _, mode := range deliveryModes {
		for _, choice := range deliveryChoices {
			t.Run(fmt.Sprintf("%s/%s", mode, choice), func(t *testing.T) {
				s, runtime := deliveryServer(t, mode)
				ctx := context.Background()
				fn := createFunction(t, lambdaClient(s, "test"), "shipper", "app.echo")
				shapePolicy(t, s, *fn.FunctionArn, "logs.amazonaws.com", source, choice)
				c := logsClient(s, "test")
				_, err := c.CreateLogGroup(ctx, &cloudwatchlogs.CreateLogGroupInput{LogGroupName: new("app")})
				mustAuthorize(t, err)
				_, err = c.CreateLogStream(ctx, &cloudwatchlogs.CreateLogStreamInput{
					LogGroupName: new("app"), LogStreamName: new("web"),
				})
				mustAuthorize(t, err)
				_, err = c.PutSubscriptionFilter(ctx, &cloudwatchlogs.PutSubscriptionFilterInput{
					LogGroupName: new("app"), FilterName: new("all"), FilterPattern: new(""),
					DestinationArn: fn.FunctionArn,
				})
				if !delivered(mode, choice) {
					if err == nil || !strings.Contains(err.Error(), "Could not execute the lambda function") {
						t.Fatalf("saving the filter: %v", err)
					}

					wantDeliveryRecord(t, s, mode, choice, *fn.FunctionArn)
					return
				}

				mustAuthorize(t, err)
				_, err = c.PutLogEvents(ctx, &cloudwatchlogs.PutLogEventsInput{
					LogGroupName: new("app"), LogStreamName: new("web"),
					LogEvents: []logstypes.InputLogEvent{{Timestamp: new(time.Now().UnixMilli()), Message: new("hi")}},
				})
				mustAuthorize(t, err)
				if !functionInvoked(runtime) {
					t.Fatal("the function was not invoked")
				}

				wantDeliveryRecord(t, s, mode, choice, *fn.FunctionArn)
			})
		}
	}
}

// A permission removed after the filter was saved denies the delivery of events, which is dropped.
func TestServicePrincipalLogsSubscriptionRevoked(t *testing.T) {
	s, runtime := deliveryServer(t, config.AuthorizationEnforce)
	ctx := context.Background()
	fn := createFunction(t, lambdaClient(s, "test"), "shipper", "app.echo")
	shapePolicy(t, s, *fn.FunctionArn, "logs.amazonaws.com", "arn:aws:logs:us-east-1:000000000000:log-group:app:*",
		rightPolicy)
	c := logsClient(s, "test")
	_, err := c.CreateLogGroup(ctx, &cloudwatchlogs.CreateLogGroupInput{LogGroupName: new("app")})
	mustAuthorize(t, err)
	_, err = c.CreateLogStream(ctx, &cloudwatchlogs.CreateLogStreamInput{
		LogGroupName: new("app"), LogStreamName: new("web"),
	})
	mustAuthorize(t, err)
	_, err = c.PutSubscriptionFilter(ctx, &cloudwatchlogs.PutSubscriptionFilterInput{
		LogGroupName: new("app"), FilterName: new("all"), FilterPattern: new(""), DestinationArn: fn.FunctionArn,
	})
	mustAuthorize(t, err)
	revokePermissions(t, s, *fn.FunctionArn)
	_, err = c.PutLogEvents(ctx, &cloudwatchlogs.PutLogEventsInput{
		LogGroupName: new("app"), LogStreamName: new("web"),
		LogEvents: []logstypes.InputLogEvent{{Timestamp: new(time.Now().UnixMilli()), Message: new("hi")}},
	})
	mustAuthorize(t, err)
	if functionInvoked(runtime) {
		t.Fatal("a function without a permission was invoked")
	}

	waitFor(t, "a denied delivery", func() bool {
		for _, record := range deliveryChecks(t, s, *fn.FunctionArn) {
			if !record.Authorization.Allowed && record.Status == http.StatusForbidden {
				return true
			}
		}

		return false
	})
}

// elbLambdaGroup creates a load balancer that forwards everything to a Lambda target group and returns the group's
// ARN and the balancer's DNS name.
func elbLambdaGroup(t *testing.T, s *httptest.Server) (group, dnsName string) {
	t.Helper()
	ctx := context.Background()
	c := elbClient(s)
	_, subnets := elbSubnets(t, s)
	created, err := c.CreateLoadBalancer(ctx, &elb.CreateLoadBalancerInput{Name: new("front"), Subnets: subnets})
	mustAuthorize(t, err)
	tg, err := c.CreateTargetGroup(ctx, &elb.CreateTargetGroupInput{
		Name: new("fns"), TargetType: elbtypes.TargetTypeEnumLambda,
	})
	mustAuthorize(t, err)
	group = aws.ToString(tg.TargetGroups[0].TargetGroupArn)
	_, err = c.CreateListener(ctx, &elb.CreateListenerInput{
		LoadBalancerArn: created.LoadBalancers[0].LoadBalancerArn,
		Protocol:        elbtypes.ProtocolEnumHttp,
		Port:            new(int32(80)),
		DefaultActions:  []elbtypes.Action{{Type: elbtypes.ActionTypeEnumForward, TargetGroupArn: &group}},
	})
	mustAuthorize(t, err)

	return group, aws.ToString(created.LoadBalancers[0].DNSName)
}

// A Lambda target must allow the load balancer's target group to invoke it: registering the function fails without
// the permission, as AWS does.
func TestServicePrincipalELBRegisterTargets(t *testing.T) {
	for _, mode := range deliveryModes {
		for _, choice := range deliveryChoices {
			t.Run(fmt.Sprintf("%s/%s", mode, choice), func(t *testing.T) {
				s, _ := deliveryServer(t, mode)
				fn := createFunction(t, lambdaClient(s, "test"), "respond", "app.respond")
				group, _ := elbLambdaGroup(t, s)
				shapePolicy(t, s, *fn.FunctionArn, "elasticloadbalancing.amazonaws.com", group, choice)
				_, err := elbClient(s).RegisterTargets(context.Background(), &elb.RegisterTargetsInput{
					TargetGroupArn: &group, Targets: []elbtypes.TargetDescription{{Id: fn.FunctionArn}},
				})
				if delivered(mode, choice) {
					mustAuthorize(t, err)
				} else if err == nil || !strings.Contains(err.Error(), "AccessDenied") ||
					!strings.Contains(
						err.Error(),
						"elasticloadbalancing principal does not have permission to invoke arn "+
							*fn.FunctionArn+" from target group "+group,
					) {
					t.Fatalf("RegisterTargets: %v", err)
				}

				wantDeliveryRecord(t, s, mode, choice, *fn.FunctionArn)
			})
		}
	}
}

// A request to a Lambda target whose permission is gone fails with a 502, as AWS answers.
func TestServicePrincipalELBServing(t *testing.T) {
	for _, mode := range []string{config.AuthorizationEnforce, config.AuthorizationSoft} {
		t.Run(mode, func(t *testing.T) {
			s, _ := deliveryServer(t, mode)
			fn := createFunction(t, lambdaClient(s, "test"), "respond", "app.respond")
			group, dnsName := elbLambdaGroup(t, s)
			shapePolicy(t, s, *fn.FunctionArn, "elasticloadbalancing.amazonaws.com", group, rightPolicy)
			_, err := elbClient(s).RegisterTargets(context.Background(), &elb.RegisterTargetsInput{
				TargetGroupArn: &group, Targets: []elbtypes.TargetDescription{{Id: fn.FunctionArn}},
			})
			mustAuthorize(t, err)
			status, _, _ := elbCall(t, s, dnsName, http.MethodGet, "/", nil)
			if status != http.StatusCreated {
				t.Fatalf("status = %d with the permission", status)
			}

			revokePermissions(t, s, *fn.FunctionArn)
			want := http.StatusBadGateway
			if mode == config.AuthorizationSoft {
				want = http.StatusCreated
			}

			if status, _, _ = elbCall(t, s, dnsName, http.MethodGet, "/", nil); status != want {
				t.Fatalf("status = %d without the permission, want %d", status, want)
			}
		})
	}
}

// authorizedAPI publishes GET /pets of a REST or HTTP API behind a Lambda authorizer, in stage v1 (REST) or
// $default (HTTP), and returns the API's and the authorizer's ID. The integration may always be invoked by API
// Gateway, so only the authorizer's invocation is under test.
func authorizedAPI(
	t *testing.T,
	s *httptest.Server,
	kind string,
	authorizer, role *string,
) (apiID, authorizerID string) {
	t.Helper()
	ctx := context.Background()
	pets := createFunction(t, lambdaClient(s, "test"), "pets", "app.respond")
	uri := "arn:aws:apigateway:us-east-1:lambda:path/2015-03-31/functions/" + *authorizer + "/invocations"
	if kind == "REST" {
		c := restClient(s)
		api, err := c.CreateRestApi(ctx, &restapi.CreateRestApiInput{Name: new("pets")})
		mustAuthorize(t, err)
		allowService(t, s, "pets", "apigateway.amazonaws.com", executeSource(*api.Id))
		resource, err := c.CreateResource(ctx, &restapi.CreateResourceInput{
			RestApiId: api.Id, ParentId: api.RootResourceId, PathPart: new("pets"),
		})
		mustAuthorize(t, err)
		auth, err := c.CreateAuthorizer(ctx, &restapi.CreateAuthorizerInput{
			RestApiId: api.Id, Name: new("token"), Type: resttypes.AuthorizerTypeToken, AuthorizerUri: &uri,
			IdentitySource: new("method.request.header.X-Token"), AuthorizerCredentials: role,
		})
		mustAuthorize(t, err)
		_, err = c.PutMethod(ctx, &restapi.PutMethodInput{
			RestApiId: api.Id, ResourceId: resource.Id, HttpMethod: new("GET"), AuthorizationType: new("CUSTOM"),
			AuthorizerId: auth.Id,
		})
		mustAuthorize(t, err)
		_, err = c.PutIntegration(ctx, &restapi.PutIntegrationInput{
			RestApiId: api.Id, ResourceId: resource.Id, HttpMethod: new("GET"), Type: resttypes.IntegrationTypeAwsProxy,
			IntegrationHttpMethod: new("POST"), Uri: lambdaURI(pets.FunctionArn),
		})
		mustAuthorize(t, err)
		_, err = c.CreateDeployment(ctx, &restapi.CreateDeploymentInput{RestApiId: api.Id, StageName: new("v1")})
		mustAuthorize(t, err)

		return *api.Id, *auth.Id
	}

	c := apigwClient(s)
	api, err := c.CreateApi(ctx, &httpapi.CreateApiInput{Name: new("pets"), ProtocolType: httptypes.ProtocolTypeHttp})
	mustAuthorize(t, err)
	allowService(t, s, "pets", "apigateway.amazonaws.com", executeSource(*api.ApiId))
	auth, err := c.CreateAuthorizer(ctx, &httpapi.CreateAuthorizerInput{
		ApiId: api.ApiId, Name: new("lambda"), AuthorizerType: httptypes.AuthorizerTypeRequest, AuthorizerUri: &uri,
		AuthorizerPayloadFormatVersion: new("2.0"), EnableSimpleResponses: new(true),
		IdentitySource: []string{"$request.header.X-Token"},
	})
	mustAuthorize(t, err)
	integration, err := c.CreateIntegration(ctx, &httpapi.CreateIntegrationInput{
		ApiId: api.ApiId, IntegrationType: httptypes.IntegrationTypeAwsProxy, IntegrationUri: pets.FunctionArn,
		PayloadFormatVersion: new("2.0"),
	})
	mustAuthorize(t, err)
	_, err = c.CreateRoute(ctx, &httpapi.CreateRouteInput{
		ApiId: api.ApiId, RouteKey: new("GET /pets"), AuthorizationType: httptypes.AuthorizationTypeCustom,
		AuthorizerId: auth.AuthorizerId, Target: new("integrations/" + aws.ToString(integration.IntegrationId)),
	})
	mustAuthorize(t, err)
	_, err = c.CreateStage(ctx, &httpapi.CreateStageInput{
		ApiId: api.ApiId, StageName: new("$default"), AutoDeploy: new(true),
	})
	mustAuthorize(t, err)

	return *api.ApiId, *auth.AuthorizerId
}

// authorizedCall sends the request the authorizer lets through, to GET /pets of the API.
func authorizedCall(t *testing.T, s *httptest.Server, kind, apiID string) int {
	t.Helper()
	if kind == "REST" {
		status, _, _ := call(t, s, http.MethodGet, apiID, "/v1/pets", map[string]string{"X-Token": "allow"}, "")
		return status
	}

	status, _, _ := call(t, s, http.MethodGet, apiID, "/pets", map[string]string{"X-Token": "let-me-in"}, "")

	return status
}

// A Lambda authorizer is invoked by API Gateway as the authorizer, whose ARN is the source: without a permission
// for it the authorizer fails and API Gateway answers 500.
func TestServicePrincipalLambdaAuthorizer(t *testing.T) {
	for _, kind := range []string{"REST", "HTTP"} {
		for _, mode := range deliveryModes {
			for _, choice := range deliveryChoices {
				t.Run(fmt.Sprintf("%s/%s/%s", kind, mode, choice), func(t *testing.T) {
					s, _ := deliveryServer(t, mode)
					handler := "app.policy"
					if kind == "HTTP" {
						handler = "app.authorize"
					}

					fn := createFunction(t, lambdaClient(s, "test"), "authorizer", handler)
					apiID, authorizerID := authorizedAPI(t, s, kind, fn.FunctionArn, nil)
					source := "arn:aws:execute-api:us-east-1:000000000000:" + apiID + "/authorizers/" + authorizerID
					shapePolicy(t, s, *fn.FunctionArn, "apigateway.amazonaws.com", source, choice)
					want := http.StatusCreated
					if !delivered(mode, choice) {
						want = http.StatusInternalServerError
					}

					if status := authorizedCall(t, s, kind, apiID); status != want {
						t.Fatalf("status = %d, want %d", status, want)
					}

					wantDeliveryRecord(t, s, mode, choice, *fn.FunctionArn)
				})
			}
		}
	}
}

// A REST authorizer with credentials is invoked as that role, which trust must allow API Gateway for the authorizer
// and which must be permitted to invoke the function. HTTP API authorizers cannot name credentials.
func TestServicePrincipalLambdaAuthorizerCredentials(t *testing.T) {
	for _, mode := range []string{config.AuthorizationEnforce, config.AuthorizationSoft} {
		for _, choice := range roleChoices {
			t.Run(fmt.Sprintf("%s/%s", mode, choice), func(t *testing.T) {
				s, _ := deliveryServer(t, mode)
				fn := createFunction(t, lambdaClient(s, "test"), "authorizer", "app.policy")
				trust, permissions := choice.documentsFor(
					"apigateway.amazonaws.com", "arn:aws:execute-api:us-east-1:000000000000:*", *fn.FunctionArn,
					"lambda:InvokeFunction",
				)
				role := createDeliveryRole(t, s, "gateway", trust, permissions)
				apiID, _ := authorizedAPI(t, s, "REST", fn.FunctionArn, &role)
				want := http.StatusCreated
				if choice != roleAllowed && mode == config.AuthorizationEnforce {
					want = http.StatusInternalServerError
				}

				if status := authorizedCall(t, s, "REST", apiID); status != want {
					t.Fatalf("status = %d, want %d", status, want)
				}

				records := roleChecks(t, s, role)
				if len(records) != 1 || records[0].Authorization.Allowed != (choice == roleAllowed) ||
					records[0].Authorization.Mode != mode {
					t.Fatalf("checks = %+v", records)
				}
			})
		}
	}
}
