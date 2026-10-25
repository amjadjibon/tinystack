package cloudformation

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"uuid"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
)

// Template keywords, and the member and property names several resource
// types share.
const (
	propertyVersioning         = "VersioningConfiguration"
	propertyFifoQueue          = "FifoQueue"
	propertyDelaySeconds       = "DelaySeconds"
	propertyKmsReusePeriod     = "KmsDataKeyReusePeriodSeconds"
	propertyMaximumMessageSize = "MaximumMessageSize"
	propertyMessageRetention   = "MessageRetentionPeriod"
	propertyReceiveWait        = "ReceiveMessageWaitTimeSeconds"
	propertyRedriveAllowPolicy = "RedriveAllowPolicy"
	propertyRedrivePolicy      = "RedrivePolicy"
	propertyManagedSse         = "SqsManagedSseEnabled"
	propertyVisibilityTimeout  = "VisibilityTimeout"

	keyCondition         = "Condition"
	fnGetAtt             = "Fn::GetAtt"
	fnSub                = "Fn::Sub"
	fnIf                 = "Fn::If"
	fnBase64             = "Fn::Base64"
	fnAnd                = "Fn::And"
	fnRef                = "Ref"
	fnGetAZs             = "Fn::GetAZs"
	fnNot                = "Fn::Not"
	textTrue             = "true"
	textFalse            = "false"
	nameDefault          = "default"
	opTagResource        = "TagResource"
	opUntagResource      = "UntagResource"
	policyRetain         = "Retain"
	capabilityIAM        = "CAPABILITY_IAM"
	capabilityAutoExpand = "CAPABILITY_AUTO_EXPAND"
	onFailureNothing     = "DO_NOTHING"
	onFailureRollback    = "ROLLBACK"

	memberArn                         = "Arn"
	memberName                        = "Name"
	memberDescription                 = "Description"
	memberKey                         = "Key"
	memberEnabled                     = "Enabled"
	memberResourceArn                 = "ResourceArn"
	memberAttributes                  = "Attributes"
	memberBucketName                  = "BucketName"
	memberBucket                      = "Bucket"
	memberStreamArn                   = "StreamArn"
	memberLogGroupName                = "LogGroupName"
	memberStreamEnabled               = "StreamEnabled"
	memberFunctionName                = "FunctionName"
	memberContentBasedDeduplication   = "ContentBasedDeduplication"
	memberKmsMasterKeyID              = "KmsMasterKeyId"
	memberQueueName                   = "QueueName"
	memberRoleName                    = "RoleName"
	memberKeyID                       = "KeyId"
	memberAliasName                   = "AliasName"
	memberSecretID                    = "SecretId"
	memberSecretArn                   = "SecretArn"
	memberPolicyName                  = "PolicyName"
	memberTags                        = "Tags"
	memberTagSpecifications           = "TagSpecifications"
	memberTableName                   = "TableName"
	memberType                        = "Type"
	memberQueueURL                    = "QueueUrl"
	memberTopicName                   = "TopicName"
	memberTopicArn                    = "TopicArn"
	memberValue                       = "Value"
	memberCidrIP                      = "CidrIp"
	memberCidrIPv6                    = "CidrIpv6"
	memberIPProtocol                  = "IpProtocol"
	memberCidrBlock                   = "CidrBlock"
	memberVpcID                       = "VpcId"
	memberAvailabilityZone            = "AvailabilityZone"
	memberGroupName                   = "GroupName"
	memberGroupID                     = "GroupId"
	memberCluster                     = "Cluster"
	memberClusterName                 = "ClusterName"
	memberEngine                      = "Engine"
	memberPort                        = "Port"
	memberCacheSubnetGroupName        = "CacheSubnetGroupName"
	memberCacheParameterGroupName     = "CacheParameterGroupName"
	memberCacheClusterID              = "CacheClusterId"
	memberReplicationGroupID          = "ReplicationGroupId"
	memberReplicationGroupDescription = "ReplicationGroupDescription"
	memberServerlessCacheName         = "ServerlessCacheName"
	memberMajorEngineVersion          = "MajorEngineVersion"
	memberCacheParameterGroupFamily   = "CacheParameterGroupFamily"
	memberParameterName               = "ParameterName"
	memberRepositoryName              = "RepositoryName"
	memberStreamName                  = "StreamName"
	memberStateMachineArn             = "StateMachineArn"
	memberPolicyDocument              = "PolicyDocument"
	memberPolicyArn                   = "PolicyArn"
	memberFunctionVersion             = "FunctionVersion"
	memberRoutingConfig               = "RoutingConfig"
	memberResourceARN                 = "ResourceARN"
	memberDefinition                  = "Definition"
	memberLoggingConfiguration        = "LoggingConfiguration"
	memberTracingConfiguration        = "TracingConfiguration"
	memberRoleArn                     = "RoleArn"
	memberServiceName                 = "ServiceName"
	memberPath                        = "Path"
	memberDefaultVersionID            = "DefaultVersionId"
	memberAdditionalVersionWeights    = "AdditionalVersionWeights"
	memberSubnetID                    = "SubnetId"
	memberAlarmName                   = "AlarmName"
	keyResources                      = "Resources"
	propertyRoles                     = "Roles"
	propertyUsers                     = "Users"
	propertyGroups                    = "Groups"
	propertyManagedPolicyName         = "ManagedPolicyName"
	propertyStateMachineName          = "StateMachineName"
	memberPolicyID                    = "PolicyId"
	memberRestAPIID                   = "RestApiId"
	memberDeploymentID                = "DeploymentId"
	memberDisableExecuteAPIEndpoint   = "DisableExecuteApiEndpoint"
	memberStageName                   = "StageName"
	memberAuthorizerID                = "AuthorizerId"
	memberHTTPMethod                  = "HttpMethod"
	memberStatusCode                  = "StatusCode"
	memberAPIID                       = "ApiId"
	memberAuthorizerResultTTL         = "AuthorizerResultTtlInSeconds"
	memberAuthorizerURI               = "AuthorizerUri"
	memberIdentitySource              = "IdentitySource"
	memberPolicy                      = "Policy"
	memberProtocolType                = "ProtocolType"
	memberResourceID                  = "ResourceId"
	memberVariables                   = "Variables"
	memberVersion                     = "Version"
	memberKmsKeyID                    = "KmsKeyId"
	memberStatus                      = "Status"
	memberSubnetIDs                   = "SubnetIds"
	eventSQS                          = "SQS"
	eventSchedule                     = "Schedule"
	keyProperties                     = "Properties"
	keyService                        = "Service"
	propertyPathPart                  = "PathPart"
	propertyPayloadFormatVersion      = "PayloadFormatVersion"
	propertyPrincipal                 = "Principal"
	propertyProtocol                  = "Protocol"
	propertyProvisionedThroughput     = "ProvisionedThroughput"
	propertyRole                      = "Role"
	propertyRouteKey                  = "RouteKey"
	propertyState                     = "State"
	propertyStateMachineType          = "StateMachineType"
	propertyStream                    = "Stream"
	propertyTarget                    = "Target"
	propertyTargets                   = "Targets"
	propertyTimeoutInMillis           = "TimeoutInMillis"
	stageProcessed                    = "Processed"
	typeString                        = "String"
	eventAPI                          = "Api"
	eventDynamoDB                     = "DynamoDB"
	eventHTTPAPI                      = "HttpApi"
	eventKinesis                      = "Kinesis"
	fnJoin                            = "Fn::Join"
	memberAttributeName               = "AttributeName"
	memberEventBusName                = "EventBusName"
	methodAny                         = "ANY"
	propertyAction                    = "Action"
	propertyAssumeRolePolicyDocument  = "AssumeRolePolicyDocument"
	propertyAttributeDefinitions      = "AttributeDefinitions"
	propertyAuthorizationType         = "AuthorizationType"
	propertyBatchSize                 = "BatchSize"
	propertyBillingMode               = "BillingMode"
	propertyKinesisSpecification      = "KinesisStreamSpecification"
	propertyPrecision                 = "ApproximateCreationDateTimePrecision"
	propertyBinaryMediaTypes          = "BinaryMediaTypes"
	propertyDefinitionSubstitutions   = "DefinitionSubstitutions"
	propertyEndpoint                  = "Endpoint"
	propertyEndpointConfiguration     = "EndpointConfiguration"
	propertyEnvironment               = "Environment"
	propertyFilterCriteria            = "FilterCriteria"
	propertyFunctionResponseTypes     = "FunctionResponseTypes"
	propertyInput                     = "Input"
	propertyKeySchema                 = "KeySchema"
	propertyMaximumBatchingWindow     = "MaximumBatchingWindowInSeconds"
	propertyMinimumCompressionSize    = "MinimumCompressionSize"
	propertyPackageType               = "PackageType"
	propertyParentID                  = "ParentId"
	typeNumber                        = "Number"
)

// resourceType implements a CloudFormation resource type with the operations
// of the service that owns it. Ref returns a resource's physical ID.
type resourceType struct {
	// create makes the resource and returns its physical ID and attributes.
	create func(ctx context.Context, c *client, p props) (string, map[string]any, error)
	// update changes a resource in place and returns its attributes; nil means
	// any change replaces the resource.
	update func(ctx context.Context, c *client, current *resource, p props) (map[string]any, error)
	// remove deletes it; a resource that is already gone counts as deleted.
	remove func(ctx context.Context, c *client, current *resource) error
	// name is the property that names the resource, generated when absent.
	name string
	// properties are the properties TinyStack supports.
	properties []string
	// attributes are the Fn::GetAtt attributes.
	attributes []string
	// replace are the properties whose change replaces the resource.
	replace []string
	// pending marks resources completed by a later runner visit.
	pending bool
	// iam marks types that need CAPABILITY_IAM.
	iam bool
}

// types are the resource types TinyStack supports.
var types = map[string]*resourceType{}

func register(name string, t *resourceType) { types[name] = t }

func resourceTypeOf(name string) *resourceType {
	if isCustomResource(name) {
		return customResourceType
	}
	return types[name]
}

func supportsAttribute(typ, name string) bool {
	return isCustomResource(typ) || typ == stackType && strings.HasPrefix(name, "Outputs.") ||
		slices.Contains(resourceTypeOf(typ).attributes, name)
}

// replaces reports whether going from old to p replaces a resource.
func (t *resourceType) replaces(old, p props) bool {
	if t == types["AWS::Bedrock::DataSource"] {
		return bedrockDataSourceReplaces(old, p)
	}

	if t == types[glueClassifierType] {
		return glueClassifierReplaces(old, p)
	}

	if t == types["AWS::MediaConnect::Flow"] {
		return mediaConnectFlowReplaces(old, p)
	}

	if t == types[mwaaEnvironmentType] {
		return mwaaReplaces(old, p)
	}
	if t == types[mskClusterType] {
		return mskReplaces(old, p)
	}
	if t == types[xraySamplingRuleType] {
		return xraySamplingRuleReplaces(old, p)
	}
	if t == types[inspector2FilterType] {
		return inspector2FilterReplaces(old, p)
	}
	if t == customResourceType || t == types[stackType] {
		return false
	}

	if t.update == nil {
		return true
	}

	for _, k := range t.replace {
		if p.changed(old, k) {
			return true
		}
	}

	return false
}

// createsFirst reports whether going from old to p creates the replacement
// before deleting the old resource, as CloudFormation does. A replacement
// with the old resource's name cannot exist beside it, so it is deleted first.
func (t *resourceType) createsFirst(old, p props) bool {
	if t == types["AWS::EC2::VPCDHCPOptionsAssociation"] || t == types["AWS::EC2::EgressOnlyInternetGateway"] {
		return p.str(memberVpcID) != old.str(memberVpcID)
	}
	if t == types["AWS::EC2::NetworkInterfaceAttachment"] || t == types["AWS::EC2::SubnetCidrBlock"] {
		return false
	}
	// Associations for the same VPC and domain cannot coexist during replacement.
	if t == types["AWS::Route53Resolver::ResolverRuleAssociation"] {
		return p.str(resolverVPCID) != old.str(resolverVPCID)
	}

	return t.name == "" || p.str(t.name) == "" || p.str(t.name) != old.str(t.name)
}

// client calls other services for one stack resource.
type client struct {
	mediaConvert mediaConvertRecords
	mediaConnect mediaConnectRecords

	mq       mqRecords
	msk      mskRecords
	mwaa     mwaaRecords
	dataSync dataSyncRecords
	transfer transferRecords
	location locationRecords
	s3tables s3tablesRecords
	caller   *catalog.Caller
	scope    request.Scope
	endpoint string
	stack    string
	logical  string
}

// call runs an operation of service in the stack's account and region.
func (c *client) call(ctx context.Context, svc, operation string, params map[string]any) (map[string]any, error) {
	ctx = request.WithMetadata(ctx, request.Metadata{
		Scope: c.scope, Service: svc, Operation: operation, RequestID: uuid.NewV4().String(), Endpoint: c.endpoint,
	})
	return c.caller.Call(ctx, svc, operation, params)
}

// generate returns a name CloudFormation would make for the resource:
// the stack and logical ID with a random suffix, cut to limit characters.
func (c *client) generate(limit int) string {
	suffix := strings.ToUpper(strings.ReplaceAll(uuid.NewV4().String(), "-", "")[:12])
	prefix := c.stack + "-" + c.logical
	if room := limit - len(suffix) - 1; len(prefix) > room {
		prefix = prefix[:max(room, 0)]
	}

	return prefix + "-" + suffix
}

// nameOf returns the name property of p, or a generated name.
func (c *client) nameOf(p props, property string, limit int) string {
	if n := p.str(property); n != "" {
		return n
	}

	return c.generate(limit)
}

// gone reports whether err is one of codes, an error for a missing resource.
func gone(err error, codes ...string) bool {
	var e *awserror.Error
	return errors.As(err, &e) && slices.Contains(codes, e.Code)
}

// props are a resource's evaluated properties.
type props map[string]any

// str returns a property as text.
func (p props) str(k string) string {
	if v, ok := p[k]; ok {
		return scalarText(v)
	}

	return ""
}

func (p props) has(k string) bool { _, ok := p[k]; return ok }

// truth reports whether a property is true, whether as a boolean or text.
func (p props) truth(k string) bool { return p.str(k) == textTrue }

func (p props) list(k string) []any {
	l, _ := p[k].([]any)
	return l
}

// strings returns a list property as text.
func (p props) strings(k string) []string {
	var out []string
	for _, v := range p.list(k) {
		out = append(out, scalarText(v))
	}

	return out
}

// document returns a property that is JSON, written as an object or as
// text, as text.
func (p props) document(k string) string {
	switch v := p[k].(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		data, _ := json.Marshal(v)
		return string(data)
	}
}

// attribute renders a property as an attribute value: objects and lists as
// JSON, and anything else as text.
func (p props) attribute(k string) string {
	switch p[k].(type) {
	case map[string]any, []any:
		return p.document(k)
	default:
		return p.str(k)
	}
}

// changed reports whether property k differs from old's.
func (p props) changed(old props, k string) bool { return !reflect.DeepEqual(old[k], p[k]) }

// tags reads the Tags property, as a list of Key and Value objects or as a map.
func (p props) tags() map[string]string {
	out := map[string]string{}
	switch v := p[memberTags].(type) {
	case []any:
		for _, el := range v {
			m, _ := el.(map[string]any)
			out[scalarText(m[memberKey])] = scalarText(m[memberValue])
		}
	case map[string]any:
		for key, val := range v {
			out[key] = scalarText(val)
		}
	}

	return out
}

// tagList renders tags as a list of objects with the given key and value
// member names.
func tagList(tags map[string]string, keyName, valueName string) []any {
	out := []any{}
	for _, k := range slices.Sorted(maps.Keys(tags)) {
		out = append(out, map[string]any{keyName: k, valueName: tags[k]})
	}

	return out
}

func tagMap(tags map[string]string) map[string]any {
	out := map[string]any{}
	for k, v := range tags {
		out[k] = v
	}

	return out
}

// tagChanges returns the tags to set and the keys to remove to go from old
// to tags.
func tagChanges(old, tags map[string]string) (set map[string]string, removed []any) {
	set = map[string]string{}
	for k, v := range tags {
		if prev, ok := old[k]; !ok || prev != v {
			set[k] = v
		}
	}

	for _, k := range slices.Sorted(maps.Keys(old)) {
		if _, ok := tags[k]; !ok {
			removed = append(removed, k)
		}
	}

	return set, removed
}

// tagger names a service's operations that tag and untag a resource.
type tagger struct {
	// target identifies the resource in both operations.
	target map[string]any
	// service, tag, and untag name the operations.
	service, tag, untag string
	// key and value name a tag list's members; empty means tags are a map.
	key, value string
	// member names the tag payload member on the tag operation; empty means memberTags ("Tags").
	member string
}

// retag moves a resource's tags from old's Tags property to p's.
func retag(ctx context.Context, c *client, old, p props, t *tagger) error {
	set, removed := tagChanges(old.tags(), p.tags())
	if len(removed) > 0 {
		params := maps.Clone(t.target)
		params["TagKeys"] = removed
		if _, err := c.call(ctx, t.service, t.untag, params); err != nil {
			return err
		}
	}

	if len(set) == 0 {
		return nil
	}

	params := maps.Clone(t.target)
	tagMember := memberTags
	if t.member != "" {
		tagMember = t.member
	}
	params[tagMember] = tagMap(set)
	if t.key != "" {
		params[tagMember] = tagList(set, t.key, t.value)
	}

	_, err := c.call(ctx, t.service, t.tag, params)
	return err
}

// pick copies the properties named by keys that p has into params.
func pick(params map[string]any, p props, keys ...string) map[string]any {
	for _, k := range keys {
		if v, ok := p[k]; ok {
			params[k] = v
		}
	}

	return params
}

// outText returns a value from an operation's output as text.
func outText(out map[string]any, path ...string) string {
	var v any = out
	for _, k := range path {
		m, _ := v.(map[string]any)
		v = m[k]
	}

	s, _ := v.(string)
	return s
}
