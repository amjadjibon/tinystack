package cloudformation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strings"
)

// serverlessTransform is the AWS SAM transform, which expand rewrites into
// the resource types TinyStack registers, as AWS's transform does.
const serverlessTransform = "AWS::Serverless-2016-10-31"

const (
	samFunction     = "AWS::Serverless::Function"
	samAPI          = "AWS::Serverless::Api"
	samHTTPAPI      = "AWS::Serverless::HttpApi"
	samSimpleTable  = "AWS::Serverless::SimpleTable"
	samStateMachine = "AWS::Serverless::StateMachine"
	implicitAPI     = "ServerlessRestApi"
	implicitHTTPAPI = "ServerlessHttpApi"
	defaultStage    = "$default"
	propertyEvents  = "Events"
	propertyPolicy  = "Policies"
)

// The SAM properties expand translates; any other fails the transform.
var (
	samProperties = map[string][]string{
		samFunction: {
			memberFunctionName,
			"Handler",
			"Runtime",
			"CodeUri",
			"InlineCode",
			"ImageUri",
			propertyPackageType,
			memberDescription,
			"MemorySize",
			"Timeout",
			propertyEnvironment,
			"Architectures",
			memberTags,
			propertyRole,
			propertyPolicy,
			propertyAssumeRolePolicyDocument,
			"RolePath",
			"AutoPublishAlias",
			"AutoPublishCodeSha256",
			propertyEvents,
		},
		samAPI: {
			memberName, memberStageName, memberDescription, propertyBinaryMediaTypes, propertyMinimumCompressionSize,
			propertyEndpointConfiguration, memberVariables, memberTags, "OpenApiVersion",
		},
		samHTTPAPI: {
			memberName, memberStageName, memberDescription, "StageVariables", "CorsConfiguration",
			memberDisableExecuteAPIEndpoint, memberTags,
		},
		samSimpleTable: {"PrimaryKey", memberTableName, propertyProvisionedThroughput, memberTags},
		samStateMachine: {
			memberName,
			memberType,
			memberDefinition,
			propertyDefinitionSubstitutions,
			propertyRole,
			propertyPolicy,
			"Logging",
			"Tracing",
			memberTags,
		},
	}
	// eventProperties are the properties each event source type takes.
	eventProperties = map[string][]string{
		eventAPI:     {memberPath, "Method", "RestApiId"},
		eventHTTPAPI: {memberPath, "Method", memberAPIID, propertyPayloadFormatVersion, propertyTimeoutInMillis},
		eventSQS: {
			"Queue", propertyBatchSize, memberEnabled, propertyMaximumBatchingWindow, propertyFilterCriteria,
			propertyFunctionResponseTypes,
		},
		eventKinesis:  streamEventProperties,
		eventDynamoDB: streamEventProperties,
		"SNS":         {"Topic", "FilterPolicy", "FilterPolicyScope"},
		eventSchedule: {eventSchedule, propertyInput, memberEnabled, propertyState, memberName, memberDescription},
		"EventBridgeRule": {
			"Pattern", memberEventBusName, propertyInput, "InputPath", propertyState, "RuleName",
		},
		"CloudWatchEvent": {"Pattern", memberEventBusName, propertyInput, "InputPath", propertyState, "RuleName"},
	}
	streamEventProperties = []string{
		propertyStream, "StartingPosition", propertyBatchSize, memberEnabled, propertyMaximumBatchingWindow,
		"MaximumRetryAttempts", propertyFilterCriteria, propertyFunctionResponseTypes,
	}
	// eventPolicies are the AWS managed policies a generated role gets for
	// the functions' polled event sources.
	eventPolicies = map[string]string{
		eventSQS:      "service-role/AWSLambdaSQSQueueExecutionRole",
		eventKinesis:  "service-role/AWSLambdaKinesisExecutionRole",
		eventDynamoDB: "service-role/AWSLambdaDynamoDBExecutionRole",
	}
	// globalSections are the Globals sections and the types they apply to.
	globalSections = map[string]string{
		"Function": samFunction, eventAPI: samAPI, eventHTTPAPI: samHTTPAPI, "SimpleTable": samSimpleTable,
		"StateMachine": samStateMachine,
	}
	nonAlphanumeric = regexp.MustCompile(`[^A-Za-z0-9]+`)
)

// policyTemplate is a SAM policy template: its one parameter, actions, and
// resources, which are Fn::Sub strings that may name the parameter.
type policyTemplate struct {
	param     string
	actions   []string
	resources []string
}

// policyTemplates are the SAM policy templates TinyStack expands.
var policyTemplates = map[string]policyTemplate{
	"SQSPollerPolicy": {"QueueName", []string{
		"sqs:ChangeMessageVisibility", "sqs:ChangeMessageVisibilityBatch", "sqs:DeleteMessage",
		"sqs:DeleteMessageBatch", "sqs:GetQueueAttributes", "sqs:ReceiveMessage",
	}, []string{arnOf("sqs", "${QueueName}")}},
	"SQSSendMessagePolicy": {"QueueName", []string{"sqs:SendMessage*"}, []string{arnOf("sqs", "${QueueName}")}},
	"SNSPublishMessagePolicy": {
		"TopicName", []string{"sns:Publish"}, []string{arnOf("sns", "${TopicName}")},
	},
	"DynamoDBCrudPolicy": {memberTableName, []string{
		"dynamodb:GetItem", "dynamodb:DeleteItem", "dynamodb:PutItem", "dynamodb:Scan", "dynamodb:Query",
		"dynamodb:UpdateItem", "dynamodb:BatchWriteItem", "dynamodb:BatchGetItem", "dynamodb:DescribeTable",
		"dynamodb:ConditionCheckItem",
	}, tableResources},
	"DynamoDBReadPolicy": {memberTableName, []string{
		"dynamodb:GetItem", "dynamodb:Scan", "dynamodb:Query", "dynamodb:BatchGetItem", "dynamodb:DescribeTable",
	}, tableResources},
	"DynamoDBWritePolicy": {memberTableName, []string{
		"dynamodb:PutItem", "dynamodb:UpdateItem", "dynamodb:BatchWriteItem",
	}, tableResources},
	"S3ReadPolicy": {memberBucketName, []string{
		"s3:GetObject", "s3:ListBucket", "s3:GetBucketLocation", "s3:GetObjectVersion",
		"s3:GetLifecycleConfiguration",
	}, bucketResources},
	"S3WritePolicy": {memberBucketName, []string{
		"s3:PutObject", "s3:PutObjectAcl", "s3:PutLifecycleConfiguration",
	}, bucketResources},
	"S3CrudPolicy": {memberBucketName, []string{
		"s3:GetObject", "s3:ListBucket", "s3:GetBucketLocation", "s3:GetObjectVersion", "s3:PutObject",
		"s3:PutObjectAcl", "s3:GetLifecycleConfiguration", "s3:PutLifecycleConfiguration", "s3:DeleteObject",
	}, bucketResources},
	"LambdaInvokePolicy": {
		"FunctionName", []string{"lambda:InvokeFunction"}, []string{arnOf("lambda", "function:${FunctionName}*")},
	},
	"StepFunctionsExecutionPolicy": {
		"StateMachineName",
		[]string{"states:StartExecution"},
		[]string{arnOf("states", "stateMachine:${StateMachineName}")},
	},
	"SSMParameterReadPolicy": {memberParameterName, []string{
		"ssm:DescribeParameters", "ssm:GetParameters", "ssm:GetParameter", "ssm:GetParametersByPath",
	}, []string{arnOf("ssm", "parameter/${ParameterName}")}},
	"AWSSecretsManagerGetSecretValuePolicy": {
		"SecretArn", []string{"secretsmanager:GetSecretValue"}, []string{"${SecretArn}"},
	},
	"KMSDecryptPolicy": {"KeyId", []string{"kms:Decrypt"}, []string{arnOf("kms", "key/${KeyId}")}},
	"EventBridgePutEventsPolicy": {
		memberEventBusName, []string{"events:PutEvents"}, []string{arnOf("events", "event-bus/${EventBusName}")},
	},
	"CloudWatchPutMetricPolicy": {"", []string{"cloudwatch:PutMetricData"}, []string{"*"}},
}

var (
	tableResources  = []string{arnOf("dynamodb", "table/${TableName}"), arnOf("dynamodb", "table/${TableName}/index/*")}
	bucketResources = []string{
		"arn:${AWS::Partition}:s3:::${BucketName}", "arn:${AWS::Partition}:s3:::${BucketName}/*",
	}
)

// arnOf returns an Fn::Sub ARN of a regional service resource in the stack's
// account.
func arnOf(svc, resource string) string {
	return "arn:${AWS::Partition}:" + svc + ":${AWS::Region}:${AWS::AccountId}:" + resource
}

// expander rewrites a SAM template's serverless resources.
type expander struct {
	// out are the expanded template's resources.
	out map[string]any
	// globals are the Globals properties by resource type.
	globals map[string]map[string]any
	// rest and http are the APIs functions' events attach to.
	rest map[string]*restAPI
	http map[string]*httpAPI
	// published maps a function to its alias and version, which Ref
	// <function>.Alias and <function>.Version name.
	published map[string][2]string
}

// restAPI is a REST API to generate, with its functions' routes.
type restAPI struct {
	decl   map[string]any
	props  props
	routes []restRoute
}

type restRoute struct {
	uri          any
	path, method string
}

// httpAPI is an HTTP API to generate, with the routes events gave it.
type httpAPI struct {
	decl   map[string]any
	props  props
	routes []string
}

// transform applies a template's transforms to the decoded document. SAM
// is the only one TinyStack supports.
func transform(doc map[string]any, names []string) (map[string]any, error) {
	if len(names) == 0 {
		return doc, nil
	}

	if !slices.Equal(names, []string{serverlessTransform}) {
		return nil, validation("TinyStack does not support template transforms: [%s]", strings.Join(names, ", "))
	}

	return expand(doc)
}

// processed renders a template with its transforms applied, as GetTemplate's
// Processed stage does; a template without transforms is returned as is.
func processed(body string) string {
	doc, err := decode(body)
	if err != nil {
		return body
	}

	names, err := stringList(doc["Transform"], "Transform")
	if err != nil || len(names) == 0 {
		return body
	}

	doc, err = transform(doc, names)
	if err != nil {
		return body
	}

	data, _ := json.Marshal(doc)
	return string(data)
}

func samError(id, format string, args ...any) error {
	return validation("Transform "+serverlessTransform+" failed with: Invalid Serverless Application Specification "+
		"document. Number of errors found: 1. Resource with id [%s] is invalid. %s", id, fmt.Sprintf(format, args...))
}

// expand applies the SAM transform to doc and returns the expanded document.
func expand(doc map[string]any) (map[string]any, error) {
	e := &expander{
		out: map[string]any{}, globals: map[string]map[string]any{}, rest: map[string]*restAPI{},
		http: map[string]*httpAPI{}, published: map[string][2]string{},
	}

	globals, err := object(doc["Globals"], "Globals")
	if err != nil {
		return nil, err
	}

	for section, v := range globals {
		typ, ok := globalSections[section]
		if !ok {
			return nil, samError("Globals", "'%s' is not supported in Globals", section)
		}

		if e.globals[typ], err = object(v, "Globals "+section); err != nil {
			return nil, err
		}
	}

	resources, ok := doc[keyResources].(map[string]any)
	if !ok {
		return nil, formatError("At least one Resources member must be defined.")
	}

	// Copy the other resources, and declare the APIs before the functions
	// whose events attach to them.
	var functions, rest []string
	declared := map[string]props{}
	for _, id := range slices.Sorted(maps.Keys(resources)) {
		decl, _ := resources[id].(map[string]any)
		typ, _ := decl[memberType].(string)
		if !strings.HasPrefix(typ, "AWS::Serverless::") {
			e.out[id] = resources[id]
			continue
		}

		if declared[id], err = e.properties(id, decl, typ); err != nil {
			return nil, err
		}

		p := declared[id]
		switch typ {
		case samAPI:
			e.rest[id] = &restAPI{decl: decl, props: p}
		case samHTTPAPI:
			e.http[id] = &httpAPI{decl: decl, props: p}
		case samFunction:
			functions = append(functions, id)
		default:
			rest = append(rest, id)
		}
	}

	for _, id := range functions {
		err = e.function(id, resources[id].(map[string]any), declared[id])
		if err != nil {
			return nil, err
		}
	}

	for _, id := range rest {
		decl := resources[id].(map[string]any)
		p := declared[id]
		switch decl[memberType] {
		case samSimpleTable:
			err = e.simpleTable(id, decl, p)
		case samStateMachine:
			err = e.stateMachine(id, decl, p)
		default:
			e.out[id] = decl
		}

		if err != nil {
			return nil, err
		}
	}

	for _, id := range slices.Sorted(maps.Keys(e.rest)) {
		if err := e.restAPI(id, e.rest[id]); err != nil {
			return nil, err
		}
	}

	for _, id := range slices.Sorted(maps.Keys(e.http)) {
		if err := e.httpAPI(id, e.http[id]); err != nil {
			return nil, err
		}
	}

	expanded := map[string]any{}
	for k, v := range doc {
		if k != "Transform" && k != "Globals" {
			expanded[k] = e.rewrite(v)
		}
	}

	expanded[keyResources] = e.rewrite(e.out)
	return expanded, nil
}

// properties returns a serverless resource's properties merged with its
// Globals, after checking that expand translates each of them.
func (e *expander) properties(id string, decl map[string]any, typ string) (props, error) {
	own, err := object(decl[keyProperties], keyProperties)
	if err != nil {
		return nil, formatError("[/Resources/%s/Properties] Properties must be an object", id)
	}

	p := props(merge(e.globals[typ], own))
	supported, ok := samProperties[typ]
	if !ok {
		return p, nil
	}

	for _, k := range slices.Sorted(maps.Keys(p)) {
		if !slices.Contains(supported, k) {
			return nil, samError(id, "TinyStack does not support the %s property of %s", k, typ)
		}
	}

	return p, nil
}

// merge applies SAM's Globals rules: maps merge, lists concatenate, and any
// other value of the resource's own replaces the global one.
func merge(global, own map[string]any) map[string]any {
	out := maps.Clone(own)
	if out == nil {
		out = map[string]any{}
	}

	for k, g := range global {
		v, ok := out[k]
		if !ok {
			out[k] = g
			continue
		}

		gm, gIsMap := g.(map[string]any)
		vm, vIsMap := v.(map[string]any)
		if gIsMap && vIsMap && !intrinsic(gm) && !intrinsic(vm) {
			out[k] = merge(gm, vm)
			continue
		}

		gl, gIsList := g.([]any)
		vl, vIsList := v.([]any)
		if gIsList && vIsList {
			out[k] = append(slices.Clone(gl), vl...)
		}
	}

	return out
}

// intrinsic reports whether m is a function call rather than an object.
func intrinsic(m map[string]any) bool {
	if len(m) != 1 {
		return false
	}

	for k := range m {
		return k == fnRef || k == keyCondition || strings.HasPrefix(k, "Fn::")
	}

	return false
}

// add puts a generated resource into the template, failing if the template
// already has one with its logical ID.
func (e *expander) add(owner, id string, decl map[string]any) error {
	if _, taken := e.out[id]; taken {
		return samError(owner, "the generated logical ID %s is already used by another resource", id)
	}

	e.out[id] = decl
	return nil
}

// derived returns a generated resource of type typ for the serverless
// resource decl, which shares its Condition.
func derived(decl map[string]any, typ string, properties map[string]any) map[string]any {
	out := map[string]any{memberType: typ, keyProperties: properties}
	if c, ok := decl[keyCondition]; ok {
		out[keyCondition] = c
	}

	return out
}

// primary returns the resource that replaces the serverless resource decl,
// keeping its resource attributes.
func primary(decl map[string]any, typ string, properties map[string]any) map[string]any {
	out := maps.Clone(decl)
	out[memberType], out[keyProperties] = typ, properties
	return out
}

func ref(id string) map[string]any { return map[string]any{fnRef: id} }

func getAtt(id, attribute string) map[string]any {
	return map[string]any{fnGetAtt: []any{id, attribute}}
}

func sub(s string) map[string]any { return map[string]any{fnSub: s} }

// tagEntries renders SAM's map of tags as a list of Key and Value objects.
func tagEntries(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}

	out := []any{}
	for _, k := range slices.Sorted(maps.Keys(m)) {
		out = append(out, map[string]any{memberKey: k, memberValue: m[k]})
	}

	return out
}

// hash returns the first n hex digits of v's JSON digest, which SAM puts
// in the logical IDs of resources that must be replaced when v changes.
func hash(v any, n int) string {
	data, _ := json.Marshal(v)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:n]
}

// alphanumeric drops the characters a logical ID cannot hold, capitalizing
// each word that follows them.
func alphanumeric(s string) string {
	var b strings.Builder
	for _, word := range nonAlphanumeric.Split(s, -1) {
		if word != "" {
			b.WriteString(strings.ToUpper(word[:1]) + word[1:])
		}
	}

	return b.String()
}

func (e *expander) function(id string, decl map[string]any, p props) error {
	fn := pick(map[string]any{}, p, memberFunctionName, "Handler", "Runtime", memberDescription, "MemorySize",
		"Timeout", propertyEnvironment, "Architectures", propertyPackageType)
	code, err := functionCode(id, p)
	if err != nil {
		return err
	}

	fn["Code"] = code
	if p.has("ImageUri") {
		fn[propertyPackageType] = "Image"
	}

	if p.has(memberTags) {
		fn[memberTags] = tagEntries(p[memberTags])
	}

	role := id + propertyRole
	if p.has(propertyRole) {
		fn[propertyRole] = p[propertyRole]
	} else {
		fn[propertyRole] = getAtt(role, memberArn)
	}

	err = e.add(id, id, primary(decl, "AWS::Lambda::Function", fn))
	if err != nil {
		return err
	}

	// target is what invokes the function: its ARN, or its alias's.
	var target any = getAtt(id, memberArn)
	invokeArn := "${" + id + ".Arn}"
	if p.has("AutoPublishAlias") {
		alias, ok := p["AutoPublishAlias"].(string)
		if !ok || alphanumeric(alias) == "" {
			return samError(id, "TinyStack requires AutoPublishAlias to be an alias name")
		}

		version := id + memberVersion + hash(code, 10)
		versioned := pick(map[string]any{memberFunctionName: ref(id)}, p, "AutoPublishCodeSha256")
		if sha, ok := versioned["AutoPublishCodeSha256"]; ok {
			delete(versioned, "AutoPublishCodeSha256")
			versioned["CodeSha256"] = sha
			version = id + memberVersion + hash([]any{code, sha}, 10)
		}

		v := derived(decl, "AWS::Lambda::Version", versioned)
		v["DeletionPolicy"] = policyRetain
		err = e.add(id, version, v)
		if err != nil {
			return err
		}

		aliasID := id + "Alias" + alphanumeric(alias)
		err = e.add(id, aliasID, derived(decl, "AWS::Lambda::Alias", map[string]any{
			memberName: alias, memberFunctionName: ref(id), memberFunctionVersion: getAtt(version, memberVersion),
		}))
		if err != nil {
			return err
		}

		e.published[id] = [2]string{aliasID, version}
		target, invokeArn = ref(aliasID), "${"+aliasID+"}"
	}

	managed := []any{sub("arn:${AWS::Partition}:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole")}
	events, err := object(p[propertyEvents], propertyEvents)
	if err != nil {
		return samError(id, "Events must be an object")
	}

	for _, name := range slices.Sorted(maps.Keys(events)) {
		typ, err := e.event(id, decl, name, events[name], target, invokeArn)
		if err != nil {
			return err
		}

		policy := sub("arn:${AWS::Partition}:iam::aws:policy/" + eventPolicies[typ])
		if eventPolicies[typ] != "" &&
			!slices.ContainsFunc(managed, func(m any) bool { return reflect.DeepEqual(m, policy) }) {
			managed = append(managed, policy)
		}
	}

	if p.has(propertyRole) {
		return nil
	}

	return e.role(id, decl, p, "lambda.amazonaws.com", managed)
}

// functionCode renders a function's InlineCode, ImageUri, or packaged
// CodeUri as AWS::Lambda::Function's Code.
func functionCode(id string, p props) (map[string]any, error) {
	switch {
	case p.has("InlineCode"):
		return map[string]any{"ZipFile": p["InlineCode"]}, nil
	case p.has("ImageUri"):
		return map[string]any{"ImageUri": p["ImageUri"]}, nil
	}

	switch uri := p["CodeUri"].(type) {
	case string:
		bucket, key, ok := strings.Cut(strings.TrimPrefix(uri, "s3://"), "/")
		if !strings.HasPrefix(uri, "s3://") || !ok || bucket == "" || key == "" {
			return nil, samError(id, "'CodeUri' is not a valid S3 Uri of the form 's3://bucket/key'; "+
				"package local code with sam deploy or sam package")
		}

		return map[string]any{"S3Bucket": bucket, "S3Key": key}, nil
	case map[string]any:
		code := map[string]any{"S3Bucket": uri["Bucket"], "S3Key": uri["Key"]}
		if v, ok := uri[memberVersion]; ok {
			code["S3ObjectVersion"] = v
		}

		return code, nil
	default:
		return nil, samError(id, "Only one of 'InlineCode', 'CodeUri', or 'ImageUri' must be set")
	}
}

// role generates the IAM role of a function or state machine that names no
// Role, with the trust policy for service and its Policies.
func (e *expander) role(id string, decl map[string]any, p props, service string, managed []any) error {
	trust := p[propertyAssumeRolePolicyDocument]
	if trust == nil {
		trust = map[string]any{
			memberVersion: "2012-10-17",
			"Statement": []any{map[string]any{
				"Effect": "Allow", propertyAction: []any{"sts:AssumeRole"},
				propertyPrincipal: map[string]any{keyService: []any{service}},
			}},
		}
	}

	var inline []any
	policies := p[propertyPolicy]
	if _, isList := policies.([]any); !isList && policies != nil {
		policies = []any{policies}
	}

	list, _ := policies.([]any)
	for i, el := range list {
		switch v := el.(type) {
		case string:
			if !strings.HasPrefix(v, "arn:") {
				managed = append(managed, sub("arn:${AWS::Partition}:iam::aws:policy/"+v))
			} else {
				managed = append(managed, v)
			}
		case map[string]any:
			// A function, such as a Ref to a parameter, names a managed policy.
			if intrinsic(v) {
				managed = append(managed, v)
				continue
			}

			document, err := policyDocument(id, v)
			if err != nil {
				return err
			}

			inline = append(inline, map[string]any{
				memberPolicyName: fmt.Sprintf("%sRolePolicy%d", id, i), memberPolicyDocument: document,
			})
		default:
			return samError(id, "Policies must be policy names, ARNs, documents, or policy templates")
		}
	}

	properties := map[string]any{propertyAssumeRolePolicyDocument: trust, "ManagedPolicyArns": managed}
	if len(inline) > 0 {
		properties[propertyPolicy] = inline
	}

	if p.has("RolePath") {
		properties[memberPath] = p["RolePath"]
	}

	if p.has(memberTags) {
		properties[memberTags] = tagEntries(p[memberTags])
	}

	return e.add(id, id+propertyRole, derived(decl, "AWS::IAM::Role", properties))
}

// policyDocument returns the document of an inline policy or a policy
// template.
func policyDocument(id string, v map[string]any) (any, error) {
	if _, ok := v["Statement"]; ok {
		return v, nil
	}

	if len(v) != 1 {
		return nil, samError(id, "a policy must be a document or one policy template")
	}

	name := slices.Collect(maps.Keys(v))[0]
	args, _ := v[name].(map[string]any)
	tpl, ok := policyTemplates[name]
	if !ok {
		return nil, samError(id, "TinyStack does not support the %s policy template", name)
	}

	var resources []any
	for _, r := range tpl.resources {
		if tpl.param == "" {
			resources = append(resources, r)
			continue
		}

		arg, ok := args[tpl.param]
		if !ok {
			return nil, samError(id, "the %s policy template requires the %s parameter", name, tpl.param)
		}

		resources = append(resources, map[string]any{fnSub: []any{r, map[string]any{tpl.param: arg}}})
	}

	actions := make([]any, 0, len(tpl.actions))
	for _, a := range tpl.actions {
		actions = append(actions, a)
	}

	return map[string]any{
		memberVersion: "2012-10-17",
		"Statement":   []any{map[string]any{"Effect": "Allow", propertyAction: actions, "Resource": resources}},
	}, nil
}

// event expands one of a function's event sources and returns its type.
// target invokes the function, and invokeArn is it as an Fn::Sub variable.
func (e *expander) event(fn string, decl map[string]any, name string, raw, target any, invokeArn string) (
	string, error,
) {
	if alphanumeric(name) != name {
		return "", samError(fn, "Event name %s must be alphanumeric", name)
	}

	ev, _ := raw.(map[string]any)
	typ, _ := ev[memberType].(string)
	supported, ok := eventProperties[typ]
	if !ok {
		return "", samError(fn, "TinyStack does not support %s events (event %s)", typ, name)
	}

	p, err := object(ev[keyProperties], keyProperties)
	if err != nil {
		return "", samError(fn, "Event %s: Properties must be an object", name)
	}

	for _, k := range slices.Sorted(maps.Keys(p)) {
		if !slices.Contains(supported, k) {
			return "", samError(fn, "TinyStack does not support the %s property of %s events (event %s)", k, typ, name)
		}
	}

	id := fn + name
	switch typ {
	case eventAPI, eventHTTPAPI:
		if _, conditional := decl[keyCondition]; conditional {
			return "", samError(fn, "TinyStack does not support %s events on a function with a Condition", typ)
		}

		if typ == eventAPI {
			err = e.apiEvent(fn, id, props(p), target, invokeArn)
		} else {
			err = e.httpAPIEvent(fn, id, props(p), target)
		}
	case eventSQS, eventKinesis, eventDynamoDB:
		source := map[string]string{eventSQS: "Queue", eventKinesis: propertyStream, eventDynamoDB: propertyStream}[typ]
		mapping := pick(map[string]any{"EventSourceArn": p[source], memberFunctionName: target}, props(p),
			"StartingPosition", propertyBatchSize, memberEnabled, propertyMaximumBatchingWindow, "MaximumRetryAttempts",
			propertyFilterCriteria, propertyFunctionResponseTypes)
		err = e.add(fn, id, derived(decl, "AWS::Lambda::EventSourceMapping", mapping))
	case "SNS":
		subscription := pick(
			map[string]any{memberTopicArn: p["Topic"], propertyProtocol: "lambda", propertyEndpoint: target},
			props(p),
			"FilterPolicy",
			"FilterPolicyScope",
		)
		err = e.add(fn, id, derived(decl, "AWS::SNS::Subscription", subscription))
		if err == nil {
			err = e.permission(fn, decl, id, target, "sns.amazonaws.com", p["Topic"])
		}
	default:
		err = e.rule(fn, decl, id, typ, props(p), target)
	}

	return typ, err
}

// permission lets principal invoke the function from source.
func (e *expander) permission(
	fn string,
	decl map[string]any,
	id string,
	target any,
	principal string,
	source any,
) error {
	return e.add(fn, id+"Permission", derived(decl, "AWS::Lambda::Permission", map[string]any{
		propertyAction:     "lambda:InvokeFunction",
		memberFunctionName: target,
		propertyPrincipal:  principal,
		"SourceArn":        source,
	}))
}

// rule expands a Schedule, EventBridgeRule, or CloudWatchEvent event.
func (e *expander) rule(fn string, decl map[string]any, id, typ string, p props, target any) error {
	t := pick(map[string]any{memberArn: target, "Id": id + "LambdaTarget"}, p, propertyInput, "InputPath")
	r := props{propertyTargets: []any{t}}
	if typ == eventSchedule {
		r["ScheduleExpression"] = p[eventSchedule]
		pick(r, p, memberName, memberDescription, propertyState)
		if p.has(memberEnabled) && !r.has(propertyState) {
			r[propertyState] = "ENABLED"
			if !p.truth(memberEnabled) {
				r[propertyState] = "DISABLED"
			}
		}
	} else {
		r["EventPattern"] = p["Pattern"]
		pick(r, p, memberEventBusName, propertyState)
		if p.has("RuleName") {
			r[memberName] = p["RuleName"]
		}
	}

	if err := e.add(fn, id, derived(decl, "AWS::Events::Rule", r)); err != nil {
		return err
	}

	return e.permission(fn, decl, id, target, "events.amazonaws.com", getAtt(id, memberArn))
}

// apiOf returns the API an event's RestApiId or ApiId names, or the
// implicit one when it names none.
func apiOf(fn string, p props, member, implicit string, known func(string) bool) (string, error) {
	if !p.has(member) {
		return implicit, nil
	}

	m, _ := p[member].(map[string]any)
	id, _ := m[fnRef].(string)
	if len(m) != 1 || !known(id) {
		return "", samError(fn, "%s must be a Ref to an API in this template", member)
	}

	return id, nil
}

func (e *expander) apiEvent(fn, id string, p props, target any, invokeArn string) error {
	api, err := apiOf(fn, p, "RestApiId", implicitAPI, func(name string) bool { return e.rest[name] != nil })
	if err != nil {
		return err
	}

	if e.rest[api] == nil {
		e.rest[api] = &restAPI{props: props(merge(e.globals[samAPI], nil)), decl: map[string]any{}}
	}

	path, method := p.str(memberPath), strings.ToUpper(p.str("Method"))
	if !strings.HasPrefix(path, "/") || method == "" {
		return samError(fn, "Api events need a Path that starts with / and a Method")
	}

	if method == methodAny || method == "X-AMAZON-APIGATEWAY-ANY-METHOD" {
		method = methodAny
	}

	a := e.rest[api]
	for _, r := range a.routes {
		if r.path == path && r.method == method {
			return samError(fn, "the API %s already has a %s %s route", api, method, path)
		}
	}

	a.routes = append(a.routes, restRoute{path: path, method: method, uri: sub(
		"arn:${AWS::Partition}:apigateway:${AWS::Region}:lambda:path/2015-03-31/functions/" + invokeArn +
			"/invocations",
	)})

	sourceMethod, sourcePath := method, regexp.MustCompile(`\{[^}]*\}`).ReplaceAllString(path, "*")
	if method == methodAny {
		sourceMethod = "*"
	}

	return e.permission(fn, map[string]any{}, id, target, "apigateway.amazonaws.com", sub(
		"arn:${AWS::Partition}:execute-api:${AWS::Region}:${AWS::AccountId}:${"+api+"}/*/"+sourceMethod+sourcePath,
	))
}

func (e *expander) httpAPIEvent(fn, id string, p props, target any) error {
	api, err := apiOf(fn, p, memberAPIID, implicitHTTPAPI, func(name string) bool { return e.http[name] != nil })
	if err != nil {
		return err
	}

	if e.http[api] == nil {
		e.http[api] = &httpAPI{props: props(merge(e.globals[samHTTPAPI], nil)), decl: map[string]any{}}
	}

	key := defaultStage
	if p.has(memberPath) || p.has("Method") {
		method := strings.ToUpper(p.str("Method"))
		if method == "" || !strings.HasPrefix(p.str(memberPath), "/") {
			return samError(fn, "HttpApi events need both a Path that starts with / and a Method, or neither")
		}

		key = method + " " + p.str(memberPath)
	}

	if slices.Contains(e.http[api].routes, key) {
		return samError(fn, "the API %s already has a %s route", api, key)
	}

	e.http[api].routes = append(e.http[api].routes, id+"Route")
	integration := pick(map[string]any{
		memberAPIID: ref(
			api,
		),
		"IntegrationType":            "AWS_PROXY",
		"IntegrationUri":             target,
		propertyPayloadFormatVersion: "2.0",
	}, p, propertyPayloadFormatVersion, propertyTimeoutInMillis)
	err = e.add(
		fn,
		id+"Integration",
		derived(map[string]any{}, "AWS::ApiGatewayV2::Integration", integration),
	)
	if err != nil {
		return err
	}

	err = e.add(fn, id+"Route", derived(map[string]any{}, "AWS::ApiGatewayV2::Route", map[string]any{
		memberAPIID: ref(api), propertyRouteKey: key,
		propertyTarget: map[string]any{fnJoin: []any{"/", []any{"integrations", ref(id + "Integration")}}},
	}))
	if err != nil {
		return err
	}

	return e.permission(fn, map[string]any{}, id, target, "apigateway.amazonaws.com",
		sub("arn:${AWS::Partition}:execute-api:${AWS::Region}:${AWS::AccountId}:${"+api+"}/*"))
}

// restAPI generates a REST API with a resource for each path its routes
// use, their methods, a deployment named for the routes, and a stage.
func (e *expander) restAPI(id string, a *restAPI) error {
	if len(a.routes) == 0 {
		return samError(id, "the API has no Api events, and TinyStack does not support DefinitionBody")
	}

	if id != implicitAPI && !a.props.has(memberStageName) {
		return samError(id, "Property 'StageName' is required")
	}

	api := pick(map[string]any{memberName: ref("AWS::StackName")}, a.props, memberName, memberDescription,
		propertyBinaryMediaTypes, propertyMinimumCompressionSize)
	switch endpoint := a.props[propertyEndpointConfiguration].(type) {
	case nil:
	case string:
		api[propertyEndpointConfiguration] = map[string]any{"Types": []any{endpoint}}
	case map[string]any:
		api[propertyEndpointConfiguration] = map[string]any{"Types": []any{endpoint[memberType]}}
	default:
		return samError(id, "EndpointConfiguration must be an endpoint type or an object with a Type")
	}

	if err := e.add(id, id, primary(a.decl, "AWS::ApiGateway::RestApi", api)); err != nil {
		return err
	}

	tree := map[string]any{"": getAtt(id, "RootResourceId")}
	names := map[string]string{}
	var methods, routes []any
	for _, r := range a.routes {
		path := strings.Trim(r.path, "/")
		parent, err := e.restResource(id, a.decl, path, tree, names)
		if err != nil {
			return err
		}

		suffix := alphanumeric(path)
		if suffix == "" {
			suffix = "Root"
		}

		method := e.unique(id+suffix+alphanumeric(strings.ToLower(r.method)), path+" "+r.method, names)
		err = e.add(id, method, derived(a.decl, "AWS::ApiGateway::Method", map[string]any{
			memberRestAPIID: ref(
				id,
			),
			memberResourceID:          parent,
			memberHTTPMethod:          r.method,
			propertyAuthorizationType: "NONE",
			"Integration": map[string]any{
				memberType:              "AWS_PROXY",
				"IntegrationHttpMethod": "POST",
				"Uri":                   r.uri,
			},
		}))
		if err != nil {
			return err
		}

		methods = append(methods, method)
		routes = append(routes, []any{r.path, r.method, r.uri})
	}

	digest := hash(routes, 10)
	deployment := derived(a.decl, "AWS::ApiGateway::Deployment", map[string]any{
		memberRestAPIID: ref(id), memberDescription: "RestApi deployment id: " + digest,
	})
	deployment["DependsOn"] = methods
	if err := e.add(id, id+"Deployment"+digest, deployment); err != nil {
		return err
	}

	stage := "Prod"
	if a.props.has(memberStageName) {
		stage = a.props.str(memberStageName)
	}

	properties := pick(map[string]any{
		memberRestAPIID: ref(id), memberDeploymentID: ref(id + "Deployment" + digest), memberStageName: stage,
	}, a.props, memberStageName, memberVariables)
	if a.props.has(memberTags) {
		properties[memberTags] = tagEntries(a.props[memberTags])
	}

	return e.add(id, stageID(id, a.props, "Prod"), derived(a.decl, "AWS::ApiGateway::Stage", properties))
}

// restResource returns the ID of the resource at path (without its outer
// slashes), generating it and its parents as needed.
func (e *expander) restResource(api string, decl map[string]any, path string, tree map[string]any,
	names map[string]string,
) (any, error) {
	if id, ok := tree[path]; ok {
		return id, nil
	}

	parentPath, part := "", path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		parentPath, part = path[:i], path[i+1:]
	}

	parent, err := e.restResource(api, decl, parentPath, tree, names)
	if err != nil {
		return nil, err
	}

	name := e.unique(api+"Resource"+alphanumeric(path), path, names)
	err = e.add(api, name, derived(decl, "AWS::ApiGateway::Resource", map[string]any{
		memberRestAPIID: ref(api), propertyParentID: parent, propertyPathPart: part,
	}))
	if err != nil {
		return nil, err
	}

	tree[path] = ref(name)
	return tree[path], nil
}

// unique returns name for key, adding key's digest when another key already
// has the name.
func (e *expander) unique(name, key string, names map[string]string) string {
	if other, taken := names[name]; taken && other != key {
		name += hash(key, 8)
	}

	names[name] = key
	return name
}

// stageID is the logical ID of an API's stage: the API's ID and the stage
// name, as SAM names it, or fallback's name when StageName is a function.
func stageID(api string, p props, fallback string) string {
	name := fallback
	if p.has(memberStageName) {
		name, _ = p[memberStageName].(string)
	}

	if name == defaultStage {
		return api + "ApiGatewayDefaultStage"
	}

	return api + alphanumeric(name) + "Stage"
}

// httpAPI generates an HTTP API and its stage, which deploys automatically.
func (e *expander) httpAPI(id string, a *httpAPI) error {
	api := pick(map[string]any{memberName: ref("AWS::StackName"), memberProtocolType: "HTTP"}, a.props, memberName,
		memberDescription, memberDisableExecuteAPIEndpoint, memberTags)
	switch cors := a.props["CorsConfiguration"].(type) {
	case nil:
	case map[string]any:
		api["CorsConfiguration"] = cors
	default:
		return samError(id, "TinyStack requires CorsConfiguration to be an object")
	}

	if err := e.add(id, id, primary(a.decl, "AWS::ApiGatewayV2::Api", api)); err != nil {
		return err
	}

	stage := pick(map[string]any{memberAPIID: ref(id), memberStageName: defaultStage, "AutoDeploy": true}, a.props,
		memberStageName, "StageVariables", memberTags)
	decl := derived(a.decl, "AWS::ApiGatewayV2::Stage", stage)
	if len(a.routes) > 0 {
		var routes []any
		for _, r := range slices.Sorted(slices.Values(a.routes)) {
			routes = append(routes, r)
		}

		decl["DependsOn"] = routes
	}

	return e.add(id, stageID(id, a.props, defaultStage), decl)
}

// simpleTable generates a DynamoDB table with a single partition key.
func (e *expander) simpleTable(id string, decl map[string]any, p props) error {
	key := props{memberName: "id", memberType: typeString}
	if p.has("PrimaryKey") {
		m, _ := p["PrimaryKey"].(map[string]any)
		key = props(m)
	}

	code, ok := map[string]string{typeString: "S", typeNumber: "N", "Binary": "B"}[key.str(memberType)]
	if !ok || key.str(memberName) == "" {
		return samError(id, "PrimaryKey needs a Name and a Type of String, Number, or Binary")
	}

	table := map[string]any{
		propertyAttributeDefinitions: []any{
			map[string]any{memberAttributeName: key[memberName], "AttributeType": code},
		},
		propertyKeySchema:   []any{map[string]any{memberAttributeName: key[memberName], "KeyType": "HASH"}},
		propertyBillingMode: "PAY_PER_REQUEST",
	}
	if p.has(propertyProvisionedThroughput) {
		table[propertyBillingMode], table[propertyProvisionedThroughput] = "PROVISIONED", p[propertyProvisionedThroughput]
	}

	pick(table, p, memberTableName)
	if p.has(memberTags) {
		table[memberTags] = tagEntries(p[memberTags])
	}

	return e.add(id, id, primary(decl, "AWS::DynamoDB::Table", table))
}

// stateMachine generates a Step Functions state machine and, when it names
// no Role, its role.
func (e *expander) stateMachine(id string, decl map[string]any, p props) error {
	if !p.has(memberDefinition) {
		return samError(id, "TinyStack requires a Definition; it does not support DefinitionUri")
	}

	machine := pick(map[string]any{}, p, memberDefinition, propertyDefinitionSubstitutions)
	for from, to := range map[string]string{
		memberName: propertyStateMachineName, memberType: propertyStateMachineType, "Logging": memberLoggingConfiguration,
		"Tracing": memberTracingConfiguration,
	} {
		if p.has(from) {
			machine[to] = p[from]
		}
	}

	if p.has(memberTags) {
		machine[memberTags] = tagEntries(p[memberTags])
	}

	machine[memberRoleArn] = p[propertyRole]
	if !p.has(propertyRole) {
		machine[memberRoleArn] = getAtt(id+propertyRole, memberArn)
	}

	if err := e.add(id, id, primary(decl, "AWS::StepFunctions::StateMachine", machine)); err != nil {
		return err
	}

	if p.has(propertyRole) {
		return nil
	}

	return e.role(id, decl, p, "states.amazonaws.com", []any{})
}

// rewrite resolves Ref <function>.Alias and Ref <function>.Version to the
// resources AutoPublishAlias generated.
func (e *expander) rewrite(v any) any {
	switch x := v.(type) {
	case map[string]any:
		if name, ok := x[fnRef].(string); ok && len(x) == 1 {
			fn, suffix, dotted := strings.Cut(name, ".")
			if published, ok := e.published[fn]; ok && dotted && (suffix == "Alias" || suffix == memberVersion) {
				if suffix == "Alias" {
					return ref(published[0])
				}

				return ref(published[1])
			}
		}

		out := make(map[string]any, len(x))
		for k, el := range x {
			out[k] = e.rewrite(el)
		}

		return out
	case []any:
		out := make([]any, len(x))
		for i, el := range x {
			out[i] = e.rewrite(el)
		}

		return out
	default:
		return v
	}
}
