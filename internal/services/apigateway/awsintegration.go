package apigateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/execute"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigateway"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/apigateway/proxy"
	"github.com/amjadjibon/tinystack/internal/services/sqs"
)

const (
	sqsIntegration = "sqs"
	snsIntegration = "sns"
	sendMessage    = "SendMessage"
	publishAction  = "Publish"
	queueURLKey    = "QueueUrl"
	topicARNKey    = "TopicArn"
	formContent    = "application/x-www-form-urlencoded"
)

type awsTarget struct {
	region    string
	service   string
	operation string
	lambda    string
	queuePath string
}

var (
	integrationRole = regexp.MustCompile(`^arn:aws:iam::(\d{12}):role/\S+$`)
	awsRegion       = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)+$`)
)

func parseAWSTarget(uri string) (awsTarget, error) {
	parts := strings.SplitN(uri, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[1] != "aws" || parts[2] != "apigateway" ||
		!awsRegion.MatchString(parts[3]) {
		return awsTarget{}, fmt.Errorf("invalid AWS integration URI %q", uri)
	}

	target := awsTarget{region: parts[3], service: parts[4]}
	if target.service == "lambda" {
		if arn, ok := proxy.LambdaARN(uri); ok {
			target.lambda = arn
			return target, nil
		}
	}

	if target.service == sqsIntegration {
		if path, ok := strings.CutPrefix(parts[5], "path/"); ok {
			account, queue, ok := strings.Cut(path, "/")
			if ok && account != "" && queue != "" && !strings.Contains(queue, "/") {
				target.operation, target.queuePath = sendMessage, path
				return target, nil
			}
		}
	}

	operation, ok := strings.CutPrefix(parts[5], "action/")
	if !ok {
		return awsTarget{}, fmt.Errorf("unsupported AWS integration target %q", uri)
	}

	target.operation = operation
	switch target.service + ":" + target.operation {
	case "sqs:SendMessage", "sns:Publish", "dynamodb:PutItem", "dynamodb:GetItem":
		return target, nil
	case "states:StartExecution":
		target.service = "sfn"
		return target, nil
	}

	return awsTarget{}, fmt.Errorf("unsupported AWS integration target %q", uri)
}

func checkAWSIntegration(i *api.Integration) error {
	if common.Value(i.HttpMethod) != http.MethodPost {
		return badRequest("AWS integrations require POST as the integration HTTP method")
	}

	uri := common.Value(i.Uri)
	target, err := parseAWSTarget(uri)
	if err != nil {
		return badRequest("%s", err.Error())
	}

	role := common.Value(i.Credentials)
	if role != "" && !integrationRole.MatchString(role) {
		return badRequest("Unsupported AWS integration credentials %q", role)
	}

	if role == "" && (target.service == "dynamodb" || target.service == "sfn") {
		return badRequest("AWS integration with %s requires a credentials role", target.service)
	}

	for key := range i.RequestParameters {
		location, _, _ := mappingParameter(key, integrationRequest)
		switch {
		case target.lambda != "":
			return badRequest("AWS Lambda integrations do not support integration request parameters")
		case location == locationPath && target.queuePath == "":
			return badRequest("AWS integration path parameters require an SQS path URI")
		}
	}

	return nil
}

func (s *Service) mappedAWS(
	ctx context.Context,
	c *call,
	i *api.Integration,
	uri string,
	header http.Header,
	query url.Values,
	path map[string]string,
	body []byte,
	variables map[string]any,
	timeout time.Duration,
) *execute.Response {
	target, err := parseAWSTarget(uri)
	if err != nil {
		return c.mappingFailure(err)
	}

	backend := &execute.Response{
		Status: http.StatusOK, Header: http.Header{contentTypeHeader: {mediaJSON}},
	}
	var match *string
	if target.lambda != "" {
		if len(body) == 0 {
			body = []byte("null")
		}

		if !json.Valid(body) {
			return c.mappingFailure(errors.New("the Lambda integration payload must be valid JSON"))
		}

		var functionError bool
		backend.Body, functionError, err = s.invokeIntegrationLambda(ctx, c, i, target.lambda, body, timeout)
		if err == nil && functionError {
			var raised struct {
				Message string `json:"errorMessage"`
			}
			if err = json.Unmarshal(backend.Body, &raised); err == nil {
				match = &raised.Message
			}
		}
	} else {
		actionCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		var status int
		backend.Body, status, err = s.callAWSAction(
			actionCtx,
			c,
			&target,
			common.Value(i.Credentials),
			header,
			query,
			path,
			body,
		)
		backend.Status = status
		match = new(strconv.Itoa(status))
	}

	switch {
	case errors.Is(err, context.DeadlineExceeded):
		c.integrationError = "timed out"
		return execute.Message(http.StatusGatewayTimeout, "Endpoint request timed out")
	case err != nil:
		return c.mappingFailure(err)
	}

	return c.mapIntegrationResponse(i, backend, match, variables)
}

func actionParams(target *awsTarget, kind string, query url.Values, body []byte) (map[string]any, error) {
	params := map[string]any{}
	form := mediaType(kind) == formContent
	if form && target.service != sqsIntegration && target.service != snsIntegration {
		return nil, errors.New("this AWS action requires a JSON payload")
	}

	if len(body) > 0 {
		if form {
			fields, err := url.ParseQuery(string(body))
			if err != nil {
				return nil, err
			}

			for key, vs := range fields {
				if len(vs) != 1 {
					return nil, fmt.Errorf("expected one form value for %s", key)
				}

				params[key] = vs[0]
			}
		} else if err := json.Unmarshal(body, &params); err != nil || params == nil {
			return nil, errors.New("AWS action payload must be a JSON object or a supported form request")
		}
	}

	for key := range query {
		if _, exists := params[key]; exists {
			return nil, fmt.Errorf("duplicate AWS action parameter %s", key)
		}

		params[key] = query.Get(key)
	}

	seen := map[string]bool{}
	for key := range params {
		folded := strings.ToLower(key)
		if seen[folded] {
			return nil, fmt.Errorf("duplicate AWS action parameter %s", key)
		}

		seen[folded] = true
	}

	for key, value := range params {
		for _, canonical := range []string{queueURLKey, topicARNKey, "TargetArn", "PhoneNumber", "Action", "DelaySeconds"} {
			if key != canonical && strings.EqualFold(key, canonical) {
				delete(params, key)
				params[canonical] = value
				break
			}
		}
	}

	if action, ok := params["Action"]; ok {
		if action != target.operation {
			return nil, fmt.Errorf("AWS integration requires Action=%s", target.operation)
		}

		delete(params, "Action")
	}

	if raw, ok := params["DelaySeconds"].(string); ok && target.service == sqsIntegration {
		delay, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid DelaySeconds %q", raw)
		}

		params["DelaySeconds"] = float64(delay)
	}

	return params, nil
}

func actionResource(target *awsTarget, params map[string]any, path map[string]string) (arn string, err error) {
	switch target.service {
	case sqsIntegration:
		raw, _ := params[queueURLKey].(string)
		if target.queuePath != "" {
			if _, exists := params[queueURLKey]; exists {
				return "", errors.New("QueueUrl is supplied by the SQS integration URI")
			}

			bound, err := bindIntegrationPath(target.queuePath, path)
			if err != nil {
				return "", err
			}

			raw = "/" + bound
			params[queueURLKey] = raw
		}

		u, err := url.Parse(raw)
		if err != nil {
			return "", err
		}

		account, name, _ := strings.Cut(strings.Trim(u.Path, "/"), "/")
		arn = "arn:aws:sqs:" + target.region + ":" + account + ":" + name
		if _, ok := sqs.QueueURL(arn); !ok {
			return "", fmt.Errorf("invalid SQS queue URL %q", raw)
		}
	case snsIntegration:
		arn, _ = params[topicARNKey].(string)
		if !strings.HasPrefix(arn, "arn:aws:sns:"+target.region+":") || params["TargetArn"] != nil ||
			params["PhoneNumber"] != nil {
			return "", errors.New("SNS integration requires a TopicArn in the URI region")
		}
	}

	return arn, nil
}

func (s *Service) callAWSAction(
	ctx context.Context,
	c *call,
	target *awsTarget,
	credentials string,
	header http.Header,
	query url.Values,
	path map[string]string,
	body []byte,
) (bodyOut []byte, status int, err error) {
	if s.caller == nil {
		return nil, 0, errors.New("AWS integration service registry is unavailable")
	}

	params, err := actionParams(target, header.Get(contentTypeHeader), query, body)
	if err != nil {
		return nil, 0, err
	}

	resource, err := actionResource(target, params, path)
	if err != nil {
		return nil, 0, err
	}

	// Use the same decoded values for authorization and execution.
	caller := s.caller.Base64Blobs()
	op, input, err := caller.Decode(target.service, target.operation, params)
	if err != nil {
		return nil, 0, err
	}

	scope := request.Scope{Region: target.region, AccountID: c.scope.AccountID}
	delivery := catalog.ServiceDelivery{
		Principal: "apigateway.amazonaws.com", SourceARN: c.integrationARN(),
		SourceAccount: c.scope.AccountID, Target: resource,
	}
	if role := integrationRole.FindStringSubmatch(credentials); role != nil {
		scope.AccountID = role[1]
		delivery.Target = ""
		delivery.Role = &catalog.DeliveryRole{
			ARN: credentials, Session: "APIGateway-" + *c.api.API.Id,
			Service: target.service, Operation: target.operation, Params: params,
		}
	}

	meta := request.Metadata{
		Scope: scope, RequestID: c.requestID, Service: target.service, Operation: target.operation,
		ResourceAuthorization: resource != "",
	}
	ctx = request.WithMetadata(ctx, meta)
	if authErr := s.authorizer.Authorize(ctx, delivery); authErr != nil {
		return nil, 0, authErr
	}

	output, err := caller.Run(ctx, op, input)
	status = http.StatusOK
	if err != nil {
		awsErr := awserror.From(err)
		status = awsErr.StatusCode
		output = map[string]any{"__type": awsErr.Code, "message": awsErr.Message}
	}

	if ctx.Err() != nil {
		return nil, 0, ctx.Err()
	}

	data, err := json.Marshal(output)
	return data, status, err
}
