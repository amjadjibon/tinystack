package cloudformation

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/cloudformation"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

type nestedAncestry struct{ ParentID, RootID string }

type nestedAncestryKey struct{}

const (
	customResource      = "AWS::CloudFormation::CustomResource"
	customStatusFailed  = "FAILED"
	customRequestUpdate = "Update"
	customLambdaService = "lambda"
)

var customResourceType = &resourceType{pending: true}

func isCustomResource(typ string) bool {
	return typ == customResource || strings.HasPrefix(typ, "Custom::") && len(typ) > len("Custom::")
}

func init() {
	register(stackType, &resourceType{pending: true, properties: []string{
		"TemplateURL", propertyBatchParameters, "NotificationARNs", "Tags",
		propertyBuildTimeout,
	}})
}

func (s *Service) dispatchNested(
	ctx context.Context, parent *stack, p *pendingStep,
) (string, error) {
	if p.Kind == stepRemove || p.Kind == stepDiscard || p.Kind == stepReplace {
		_, err := s.DeleteStack(ctx, &api.DeleteStackInput{StackName: new(p.Physical)})
		return p.Physical, err
	}

	if p.Properties.has("NotificationARNs") {
		return "", validation("Nested stack NotificationARNs is not supported")
	}

	location := p.Properties.str("TemplateURL")
	if location == "" {
		return "", validation("Nested stack TemplateURL is required")
	}

	var params []api.Parameter
	if raw, ok := p.Properties[propertyBatchParameters]; ok {
		values, valid := raw.(map[string]any)
		if !valid {
			return "", validation("Nested stack Parameters must be an object")
		}

		for k, v := range values {
			switch v.(type) {
			case string, float64, json.Number:
			default:
				return "", validation("Nested stack parameter %s must be a scalar", k)
			}

			params = append(params, api.Parameter{ParameterKey: new(k), ParameterValue: new(scalarText(v))})
		}
	}

	tags := []api.Tag{}
	if raw, ok := p.Properties["Tags"]; ok {
		data, err := json.Marshal(raw)
		if err != nil {
			return "", err
		}

		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&tags); err != nil || tags == nil {
			return "", validation("Nested stack Tags must be a tag list")
		}

		for _, tag := range tags {
			if tag.Key == nil || tag.Value == nil {
				return "", validation("Nested stack tags require Key and Value")
			}
		}
	}

	var timeout *int64
	if v, ok := p.Properties[propertyBuildTimeout]; ok {
		text := scalarText(v)
		var n int64
		if err := json.Unmarshal([]byte(text), &n); err != nil || n <= 0 {
			return "", validation("Nested stack TimeoutInMinutes must be a positive integer")
		}

		timeout = new(n)
	}

	if p.Kind == stepUpdate {
		_, err := s.UpdateStack(ctx, &api.UpdateStackInput{
			StackName: new(p.Physical), TemplateURL: new(location),
			Parameters: params, Tags: tags, Capabilities: parent.Capabilities, ClientRequestToken: new(p.RequestID),
		})
		if err != nil {
			if failureReason(err) != "No updates are to be performed." {
				return "", err
			}

			p.Noop = true
		}

		// UpdateStack has no timeout member. Keep the nested resource's
		// configured creation timeout in the child record for DescribeStacks.
		err = s.store.Update(ctx, func(tx storage.WriteTx) error {
			metadata, metadataErr := request.Regional(ctx)
			if metadataErr != nil {
				return metadataErr
			}

			child, findErr := find(tx, metadata.Scope, p.Physical)
			if findErr != nil {
				return findErr
			}

			child.Timeout = common.Value(timeout)
			return storage.Write(tx, metadata.Key(service, kindStack, child.Name), child)
		})
		return p.Physical, err
	}

	root := parent.RootID
	if root == "" {
		root = parent.ID
	}

	ctx = context.WithValue(ctx, nestedAncestryKey{}, nestedAncestry{ParentID: parent.ID, RootID: root})
	out, err := s.CreateStack(ctx, &api.CreateStackInput{
		StackName: new(p.ChildName), TemplateURL: new(location),
		Parameters: params, Tags: tags, TimeoutInMinutes: timeout, Capabilities: parent.Capabilities,
		ClientRequestToken: new(p.RequestID),
	})
	if err != nil {
		return "", err
	}

	return common.Value(out.StackId), nil
}

func (s *Service) dispatchCustom(
	ctx context.Context, st *stack, p *pendingStep,
) (string, error) {
	properties := p.Properties
	var old *resource
	if p.Kind == stepDiscard {
		old = st.Replaced[0].Resource
	} else {
		old = st.Resources[p.Logical]
	}

	requestType := "Create"
	switch p.Kind {
	case stepUpdate:
		requestType = customRequestUpdate
	case stepRemove, stepDiscard, stepReplace:
		requestType, properties = "Delete", props(old.Properties)
	}

	token := properties.str("ServiceToken")
	if requestType == customRequestUpdate && token != props(old.Properties).str("ServiceToken") {
		return "", validation("Changing a custom resource ServiceToken is not supported")
	}

	parts := strings.SplitN(token, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[1] != "aws" || parts[3] == "" || parts[4] == "" ||
		parts[5] == "" || (parts[2] != customLambdaService && parts[2] != "sns") ||
		parts[2] == customLambdaService && (!strings.HasPrefix(parts[5], "function:") || parts[5] == "function:") {
		return "", validation("ServiceToken must be a Lambda function ARN or SNS topic ARN")
	}

	if st.Endpoint == "" {
		return "", validation("Custom resources require a stack Endpoint")
	}

	message := map[string]any{
		"RequestType": requestType,
		"ResponseURL": strings.TrimRight(st.Endpoint, "/") + "/_tinystack/cloudformation/responses/" + p.ResponseID,
		"StackId":     st.ID, "RequestId": p.RequestID, "ResourceType": p.Type, "LogicalResourceId": p.Logical,
		"ResourceProperties": properties, "ServiceToken": token,
	}

	if requestType != "Create" {
		message["PhysicalResourceId"] = p.Physical
	}

	if requestType == customRequestUpdate {
		message["OldResourceProperties"] = old.Properties
	}

	body, err := json.Marshal(message)
	if err != nil {
		return "", err
	}

	c := &client{caller: s.caller, scope: request.Scope{Region: parts[3], AccountID: parts[4]}, endpoint: st.Endpoint}
	if parts[2] == customLambdaService {
		_, err = c.call(
			ctx,
			customLambdaService,
			"Invoke",
			map[string]any{"FunctionName": token, "InvocationType": "Event", "Payload": string(body)},
		)
	} else {
		_, err = c.call(ctx, "sns", "Publish", map[string]any{"TopicArn": token, "Message": string(body)})
	}

	return p.Physical, err
}
