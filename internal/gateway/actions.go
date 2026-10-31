package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/services/resourcegroupstaggingapi"
)

const consoleCognito = "cognitoidp"

const consoleIAM = "iam"

const consoleSTS = "sts"

const opGetCallerIdentity = "GetCallerIdentity"

const consoleS3 = "s3"

const consoleSQS = "sqs"

const consoleSNS = "sns"

const consoleLambda = "lambda"

const consoleLogs = "logs"

const consoleDynamoDB = "dynamodb"

const consoleEventBridge = "eventbridge"

const consoleCloudWatch = "cloudwatch"

const consoleAPIGateway = "apigateway"

const consoleHTTPAPI = "apigatewayv2"

const consoleSFN = "sfn"

const consoleEC2 = "ec2"

const consoleSecretsManager = "secretsmanager"

const consoleRDS = "rds"

const consoleBudgets = "budgets"

const consoleCostExplorer = "costexplorer"

const consoleCUR = "costandusagereportservice"

var consoleBudgetsOperations = strings.Fields(`
	CreateBudget
	DescribeBudget
	DescribeBudgets
	UpdateBudget
	DeleteBudget
	CreateNotification
	UpdateNotification
	DeleteNotification
	DescribeNotificationsForBudget
	CreateSubscriber
	UpdateSubscriber
	DeleteSubscriber
	DescribeSubscribersForNotification
	TagResource
	UntagResource
	ListTagsForResource
`)

var consoleCostExplorerOperations = strings.Fields(`
	GetCostAndUsage
	GetDimensionValues
	GetTags
	CreateAnomalyMonitor
	CreateAnomalySubscription
	CreateCostCategoryDefinition
	DeleteAnomalyMonitor
	DeleteAnomalySubscription
	DeleteCostCategoryDefinition
	DescribeCostCategoryDefinition
	GetAnomalyMonitors
	GetAnomalySubscriptions
	ListCostCategoryDefinitions
	ListTagsForResource
	TagResource
	UntagResource
	UpdateAnomalyMonitor
	UpdateAnomalySubscription
	UpdateCostCategoryDefinition
`)

var consoleCUROperations = strings.Fields(`
	DeleteReportDefinition
	DescribeReportDefinitions
	ListTagsForResource
	ModifyReportDefinition
	PutReportDefinition
	TagResource
	UntagResource
`)

// Console actions are an explicit subset of the registered SDK operations.
// IAM and cost definitions are account-wide; the region is only used when verifying signatures.
var consoleIAMOperations = strings.Fields(`
	CreateUser GetUser UpdateUser DeleteUser TagUser UntagUser
	CreateGroup GetGroup UpdateGroup DeleteGroup AddUserToGroup RemoveUserFromGroup ListGroupsForUser
	CreateRole GetRole UpdateRole DeleteRole UpdateAssumeRolePolicy TagRole UntagRole
	CreatePolicy GetPolicy DeletePolicy CreatePolicyVersion GetPolicyVersion ListPolicyVersions
	DeletePolicyVersion SetDefaultPolicyVersion TagPolicy UntagPolicy
	AttachUserPolicy DetachUserPolicy PutUserPolicy GetUserPolicy DeleteUserPolicy
	AttachGroupPolicy DetachGroupPolicy PutGroupPolicy GetGroupPolicy DeleteGroupPolicy
	AttachRolePolicy DetachRolePolicy PutRolePolicy GetRolePolicy DeleteRolePolicy
	CreateAccessKey ListAccessKeys UpdateAccessKey DeleteAccessKey
	CreateInstanceProfile GetInstanceProfile DeleteInstanceProfile AddRoleToInstanceProfile
	RemoveRoleFromInstanceProfile ListInstanceProfilesForRole
	SimulatePrincipalPolicy SimulateCustomPolicy
`)

var consoleS3Operations = strings.Fields(`
	CreateBucket DeleteBucket HeadBucket ListBuckets
	ListObjectsV2 ListObjectVersions HeadObject GetObject PutObject CopyObject DeleteObject DeleteObjects
	ListMultipartUploads AbortMultipartUpload
	GetBucketVersioning PutBucketVersioning GetBucketTagging PutBucketTagging DeleteBucketTagging
	GetObjectTagging PutObjectTagging DeleteObjectTagging
	GetBucketPolicy PutBucketPolicy DeleteBucketPolicy
	GetBucketCors PutBucketCors DeleteBucketCors
	GetBucketEncryption PutBucketEncryption DeleteBucketEncryption
	GetPublicAccessBlock PutPublicAccessBlock DeletePublicAccessBlock
	GetBucketOwnershipControls PutBucketOwnershipControls DeleteBucketOwnershipControls
	GetBucketLifecycleConfiguration PutBucketLifecycleConfiguration DeleteBucketLifecycle
	GetBucketWebsite PutBucketWebsite DeleteBucketWebsite
	GetBucketNotificationConfiguration PutBucketNotificationConfiguration
`)

var consoleSQSOperations = strings.Fields(`
	CreateQueue DeleteQueue GetQueueUrl ListQueues GetQueueAttributes SetQueueAttributes PurgeQueue
	SendMessage ReceiveMessage DeleteMessage ChangeMessageVisibility
	TagQueue UntagQueue ListQueueTags
	ListDeadLetterSourceQueues StartMessageMoveTask ListMessageMoveTasks CancelMessageMoveTask
`)

var consoleSNSOperations = strings.Fields(`
	CreateTopic DeleteTopic ListTopics GetTopicAttributes SetTopicAttributes
	Subscribe ConfirmSubscription Unsubscribe ListSubscriptions ListSubscriptionsByTopic
	GetSubscriptionAttributes SetSubscriptionAttributes Publish
	TagResource UntagResource ListTagsForResource
`)

var consoleLambdaOperations = strings.Fields(`
	CreateFunction DeleteFunction GetFunction ListFunctions
	UpdateFunctionCode UpdateFunctionConfiguration Invoke
	CreateEventSourceMapping GetEventSourceMapping ListEventSourceMappings
	UpdateEventSourceMapping DeleteEventSourceMapping
	ListTags TagResource UntagResource
	CreateFunctionUrlConfig GetFunctionUrlConfig UpdateFunctionUrlConfig
	DeleteFunctionUrlConfig ListFunctionUrlConfigs
	AddPermission GetPolicy RemovePermission
`)

var consoleLogsOperations = strings.Fields(`
	CreateLogGroup DeleteLogGroup DescribeLogGroups
	CreateLogStream DeleteLogStream DescribeLogStreams GetLogEvents FilterLogEvents PutLogEvents
	PutRetentionPolicy DeleteRetentionPolicy TagResource UntagResource ListTagsForResource
	StartQuery GetQueryResults DescribeQueries
`)

var consoleDynamoDBOperations = strings.Fields(`
	CreateTable DeleteTable DescribeTable ListTables UpdateTable
	GetItem PutItem UpdateItem DeleteItem Query Scan
	DescribeTimeToLive UpdateTimeToLive ListTagsOfResource TagResource UntagResource
`)

var consoleEventBridgeOperations = strings.Fields(`
	CreateEventBus UpdateEventBus DeleteEventBus DescribeEventBus ListEventBuses
	PutRule DescribeRule ListRules EnableRule DisableRule DeleteRule
	PutTargets RemoveTargets ListTargetsByRule TestEventPattern PutEvents
	TagResource UntagResource ListTagsForResource
`)

var consoleCloudWatchOperations = strings.Fields(`
	ListMetrics GetMetricData GetMetricStatistics PutMetricData
	PutMetricAlarm PutCompositeAlarm DescribeAlarms DescribeAlarmsForMetric DescribeAlarmHistory
	DeleteAlarms SetAlarmState EnableAlarmActions DisableAlarmActions
	PutDashboard GetDashboard ListDashboards DeleteDashboards
	TagResource UntagResource ListTagsForResource
`)

var consoleHTTPAPIOperations = strings.Fields(`
	CreateApi GetApi GetApis UpdateApi DeleteApi DeleteCorsConfiguration
	CreateRoute GetRoute GetRoutes UpdateRoute DeleteRoute
	CreateIntegration GetIntegration GetIntegrations UpdateIntegration DeleteIntegration
	CreateStage GetStage GetStages UpdateStage DeleteStage
	CreateDeployment GetDeployment GetDeployments UpdateDeployment DeleteDeployment
	CreateAuthorizer GetAuthorizer GetAuthorizers UpdateAuthorizer DeleteAuthorizer
	GetTags TagResource UntagResource InvokeApi
`)

var consoleRESTAPIOperations = strings.Fields(`
	CreateRestApi GetRestApi GetRestApis UpdateRestApi DeleteRestApi
	CreateResource GetResource GetResources UpdateResource DeleteResource
	PutMethod GetMethod UpdateMethod DeleteMethod
	PutIntegration GetIntegration UpdateIntegration DeleteIntegration
	CreateStage GetStage GetStages UpdateStage DeleteStage
	CreateDeployment GetDeployment GetDeployments UpdateDeployment DeleteDeployment
	CreateAuthorizer GetAuthorizer GetAuthorizers UpdateAuthorizer DeleteAuthorizer
	GetTags TagResource UntagResource InvokeApi
`)

var consoleSFNOperations = strings.Fields(`
	CreateStateMachine DescribeStateMachine ListStateMachines UpdateStateMachine DeleteStateMachine
	ValidateStateMachineDefinition StartExecution StartSyncExecution DescribeExecution ListExecutions
	StopExecution GetExecutionHistory ListTagsForResource TagResource UntagResource
`)

var consoleEC2Operations = strings.Fields(`
 DescribeAvailabilityZones DescribeImages DescribeInstances DescribeInstanceStatus
 RunInstances StartInstances StopInstances RebootInstances TerminateInstances ModifyInstanceMetadataOptions
 DescribeIamInstanceProfileAssociations AssociateIamInstanceProfile ReplaceIamInstanceProfileAssociation
 DisassociateIamInstanceProfile DescribeKeyPairs CreateKeyPair ImportKeyPair DeleteKeyPair
 DescribeVpcs CreateVpc DeleteVpc DescribeVpcAttribute ModifyVpcAttribute
 DescribeSubnets CreateSubnet DeleteSubnet ModifySubnetAttribute
 DescribeSecurityGroups CreateSecurityGroup DeleteSecurityGroup DescribeSecurityGroupRules
 AuthorizeSecurityGroupIngress AuthorizeSecurityGroupEgress RevokeSecurityGroupIngress RevokeSecurityGroupEgress
 DescribeInternetGateways CreateInternetGateway AttachInternetGateway DetachInternetGateway DeleteInternetGateway
 DescribeRouteTables CreateRouteTable DeleteRouteTable CreateRoute ReplaceRoute DeleteRoute
 AssociateRouteTable DisassociateRouteTable ReplaceRouteTableAssociation
 DescribeAddresses AllocateAddress AssociateAddress DisassociateAddress ReleaseAddress
 DescribeNatGateways CreateNatGateway DeleteNatGateway
 DescribeNetworkAcls CreateNetworkAcl DeleteNetworkAcl CreateNetworkAclEntry ReplaceNetworkAclEntry
 DeleteNetworkAclEntry ReplaceNetworkAclAssociation
 DescribeVpcEndpoints CreateVpcEndpoint ModifyVpcEndpoint DeleteVpcEndpoints DescribeNetworkInterfaces
 CreateTags DeleteTags DescribeTags
`)

var consoleSecretsManagerOperations = strings.Fields(`
 CreateSecret DescribeSecret ListSecrets UpdateSecret DeleteSecret RestoreSecret
 PutSecretValue GetSecretValue ListSecretVersionIds UpdateSecretVersionStage
 TagResource UntagResource GetResourcePolicy ValidateResourcePolicy PutResourcePolicy DeleteResourcePolicy
 RotateSecret CancelRotateSecret GetRandomPassword
`)

var consoleRDSOperations = strings.Fields(`
 CreateDBInstance DescribeDBInstances ModifyDBInstance DeleteDBInstance StopDBInstance StartDBInstance RebootDBInstance
 CreateDBSubnetGroup DescribeDBSubnetGroups DeleteDBSubnetGroup
 CreateDBParameterGroup DescribeDBParameterGroups DeleteDBParameterGroup
 AddTagsToResource RemoveTagsFromResource ListTagsForResource
`)

// Cognito exposes only operations used by the user pools console.
var consoleCognitoOperations = strings.Fields(`
 ListUserPools DescribeUserPool ListUsers AdminGetUser AdminCreateUser AdminConfirmSignUp
 AdminEnableUser AdminDisableUser AdminResetUserPassword AdminSetUserPassword AdminDeleteUser
 AdminUpdateUserAttributes AdminListGroupsForUser AdminAddUserToGroup AdminRemoveUserFromGroup
 ListGroups CreateGroup DeleteGroup ListUsersInGroup ListUserPoolClients DescribeUserPoolClient
 DescribeUserPoolDomain
`)

// Only the tagging writes are exposed; discovery remains on the inventory endpoint.
var consoleResourceGroupsTaggingOperations = strings.Fields(`TagResources UntagResources`)

// consoleResourceDelete describes a single-identifier delete from an inventory record.
// Service and inventory type form the key; types are not globally unique.
type consoleResourceDelete struct {
	Service    string `json:"service"`
	Operation  string `json:"operation"`
	Field      string `json:"field"`
	Identifier string `json:"identifier"`
}

// Identifier sources for consoleResourceDelete: the inventory name, the full ARN, or the ARN's last path segment.
const (
	consoleIdentifierName      = "name"
	consoleIdentifierARN       = "arn"
	consoleIdentifierARNSuffix = "arn-suffix"
)

var consoleResourceDeletes = map[string]consoleResourceDelete{
	"kinesis/stream":            {"kinesis", "DeleteStream", "StreamARN", consoleIdentifierARN},
	"ssm/parameter":             {"ssm", "DeleteParameter", "Name", consoleIdentifierName},
	"ecr/repository":            {"ecr", "DeleteRepository", "RepositoryName", consoleIdentifierName},
	"route53/hosted-zone":       {"route53", "DeleteHostedZone", "Id", consoleIdentifierARNSuffix},
	"cognitoidp/user-pool":      {consoleCognito, "DeleteUserPool", "UserPoolId", consoleIdentifierARNSuffix},
	"cloudformation/stack":      {"cloudformation", "DeleteStack", "StackName", consoleIdentifierARN},
	"elasticache/cache-cluster": {"elasticache", "DeleteCacheCluster", "CacheClusterId", consoleIdentifierName},
	"memorydb/cluster":          {"memorydb", "DeleteCluster", "ClusterName", consoleIdentifierName},
}

func (s *Server) consoleDeleteCapabilities() map[string]consoleResourceDelete {
	out := make(map[string]consoleResourceDelete)
	for key, action := range consoleResourceDeletes {
		if _, ok := s.services[action.Service]; ok {
			out[key] = action
		}
	}

	return out
}

var consoleAccount = regexp.MustCompile(`^\d{12}$`)

var consoleRegion = regexp.MustCompile(`^[a-z]{2}(?:-[a-z]+)+-\d+$`)

type consoleAction struct {
	Input     map[string]any `json:"input"`
	Service   string         `json:"service"`
	Operation string         `json:"operation"`
	Account   string         `json:"account"`
	Region    string         `json:"region,omitempty"`
}

func (s *Server) serveConsoleActions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method == http.MethodGet && r.URL.RawQuery == "" {
		_, iam := s.services[consoleIAM]
		_, s3 := s.services[consoleS3]
		_, sqs := s.services[consoleSQS]
		_, sns := s.services[consoleSNS]
		_, lambda := s.services[consoleLambda]
		_, logs := s.services[consoleLogs]
		_, dynamodb := s.services[consoleDynamoDB]
		_, eventbridge := s.services[consoleEventBridge]
		_, cloudwatch := s.services[consoleCloudWatch]
		_, restAPI := s.services[consoleAPIGateway]
		_, httpAPI := s.services[consoleHTTPAPI]
		_, sfn := s.services[consoleSFN]
		_, ec2 := s.services[consoleEC2]
		_, secretsmanager := s.services[consoleSecretsManager]
		_, budgets := s.services[consoleBudgets]
		_, costexplorer := s.services[consoleCostExplorer]
		_, cur := s.services[consoleCUR]
		_, rds := s.services[consoleRDS]
		accounts := []string{s.cfg.Defaults.Account}
		for _, account := range s.cfg.Identity.Accounts {
			accounts = append(accounts, account)
		}

		for account := range s.cfg.Accounts {
			accounts = append(accounts, account)
		}

		slices.Sort(accounts)
		signingNames := make(map[string]string, len(s.services))
		for name, service := range s.services {
			signingNames[name] = service.SigningName
		}

		capabilities := map[string]any{
			"account": s.cfg.Defaults.Account, "region": s.cfg.Defaults.Region,
			"accounts":      slices.Compact(accounts),
			"signing_names": signingNames,
			consoleIAM:      iam, consoleS3: s3, consoleSQS: sqs, consoleSNS: sns,
			consoleLambda: lambda, consoleLogs: logs,
			consoleDynamoDB:       dynamodb,
			consoleEventBridge:    eventbridge,
			consoleCloudWatch:     cloudwatch,
			consoleAPIGateway:     restAPI,
			consoleHTTPAPI:        httpAPI,
			consoleSFN:            sfn,
			consoleEC2:            ec2,
			consoleSecretsManager: secretsmanager,
			consoleBudgets:        budgets,
			consoleCostExplorer:   costexplorer,
			consoleCUR:            cur,
			consoleRDS:            rds,
			"requires_signature":  s.cfg.Identity.Mode == config.IdentityVerify,
			"taggable_services":   resourcegroupstaggingapi.TaggableServices(),
			"resource_deletes":    s.consoleDeleteCapabilities(),
		}

		if s.cfg.Console.Auth.OIDC != nil {
			capabilities["sso"] = map[string]string{"login_url": "/_tinystack/auth/login"}
		}

		_ = json.NewEncoder(w).Encode(capabilities)
		return
	}

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	c := &call{meta: request.Metadata{
		Scope:     request.Scope{AccountID: s.cfg.Defaults.Account, Region: s.cfg.Defaults.Region},
		RequestID: common.RandomID(32), Service: consoleIAM, Protocol: catalog.AWSJSON10,
	}}
	c.service = s.services[consoleIAM]
	w.Header().Set("x-amzn-RequestId", c.meta.RequestID)
	start := time.Now()
	body := &capturingBody{ReadCloser: r.Body}
	r.Body = body
	cw := &capturingWriter{ResponseWriter: w}
	w = cw
	defer func() { s.record(r, c, start, body, cw) }()

	if r.URL.RawQuery != "" || !s.sameOrigin(r) {
		s.consoleFail(w, c, &awserror.Error{
			Code: accessDenied, Message: "Console actions require a same-origin request without query parameters.",
			StatusCode: http.StatusForbidden,
		})
		return
	}

	contentType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if contentType != "application/json" {
		s.consoleFail(w, c, awserror.Validation("Console actions require application/json."))
		return
	}

	raw, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if readErr != nil {
		s.consoleFail(w, c, awserror.Validation("Cannot read console action: %s", readErr))
		return
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var in consoleAction
	if err := decoder.Decode(&in); err != nil {
		s.consoleFail(w, c, awserror.Validation("Invalid console action: %s", err))
		return
	}

	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		s.consoleFail(w, c, awserror.Validation("Console actions require one JSON object."))
		return
	}

	var operations []string
	switch in.Service {
	case consoleCognito:
		operations = consoleCognitoOperations
	case "resourcegroupstaggingapi":
		operations = consoleResourceGroupsTaggingOperations
	case consoleSTS:
		operations = []string{opGetCallerIdentity}
	case consoleIAM:
		operations = consoleIAMOperations
	case consoleS3:
		operations = consoleS3Operations
	case consoleSQS:
		operations = consoleSQSOperations
	case consoleSNS:
		operations = consoleSNSOperations
	case consoleLambda:
		operations = consoleLambdaOperations
	case consoleLogs:
		operations = consoleLogsOperations
	case consoleDynamoDB:
		operations = consoleDynamoDBOperations
	case consoleEventBridge:
		operations = consoleEventBridgeOperations
	case consoleCloudWatch:
		operations = consoleCloudWatchOperations
	case consoleAPIGateway:
		operations = consoleRESTAPIOperations
	case consoleHTTPAPI:
		operations = consoleHTTPAPIOperations
	case consoleSFN:
		operations = consoleSFNOperations
	case consoleEC2:
		operations = consoleEC2Operations
	case consoleSecretsManager:
		operations = consoleSecretsManagerOperations
	case consoleBudgets:
		operations = consoleBudgetsOperations
	case consoleCostExplorer:
		operations = consoleCostExplorerOperations
	case consoleCUR:
		operations = consoleCUROperations
	case consoleRDS:
		operations = consoleRDSOperations
	}

	for _, action := range consoleResourceDeletes {
		if in.Service == action.Service && in.Operation == action.Operation {
			operations = append(operations, action.Operation)
		}
	}

	c.service = s.services[in.Service]
	if c.service == nil || !slices.Contains(operations, in.Operation) {
		s.consoleFail(w, c, awserror.New("UnknownOperationException",
			"This operation is not available in the console."))
		return
	}

	c.meta.Operation = in.Operation
	c.meta.Service = in.Service
	if !consoleAccount.MatchString(in.Account) || in.Input == nil {
		s.consoleFail(w, c, awserror.Validation("A 12-digit account and an input object are required."))
		return
	}

	if in.Service == consoleSTS && s.cfg.Identity.Mode != config.IdentityVerify {
		s.consoleFail(w, c, awserror.Validation("Signed console sessions require identity.mode: verify."))
		return
	}

	regional := in.Service != consoleIAM && in.Service != consoleBudgets &&
		in.Service != consoleCostExplorer && in.Service != consoleCUR
	if regional && !consoleRegion.MatchString(in.Region) {
		s.consoleFail(
			w,
			c,
			awserror.Validation("%s console actions require a valid region.", strings.ToUpper(in.Service)),
		)
		return
	}

	if s.cfg.Identity.Mode == config.IdentityVerify {
		// The signed scope, never the JSON body, determines the verified account.
		// Reconstruct the captured body so authentication hashes exactly what arrived.
		r.Body = io.NopCloser(bytes.NewReader(raw))
		signedService, err := s.identify(r, &c.meta)
		if err == nil {
			err = s.resolveCredentials(r.Context(), c)
		}

		if err == nil {
			err = s.authenticate(w, r, c)
		}

		if err != nil {
			s.consoleFail(w, c, err)
			return
		}

		if signedService != c.service.SigningName || c.meta.AccountID != in.Account ||
			(regional && c.meta.Region != in.Region) {
			s.consoleFail(w, c, &awserror.Error{
				Code: accessDenied, Message: "The action scope must match the signed service, account and region.",
				StatusCode: http.StatusForbidden,
			})
			return
		}

		// People who signed in through the identity provider hold role sessions
		// that IAM, not this rule, limits; consoleCall applies it.
		if in.Service == consoleSTS && c.credentials != nil &&
			c.credentials.Principal.Kind != request.PrincipalRoot && !c.credentials.Federated {
			s.consoleFail(w, c, &awserror.Error{
				Code: accessDenied, Message: "The administrative console requires account root credentials.",
				StatusCode: http.StatusForbidden,
			})
			return
		}
	} else {
		c.meta.AccountID = in.Account
		if regional {
			c.meta.Region = in.Region
		}
	}

	c.meta.Service = in.Service
	c.meta.Protocol = "console"
	c.meta.Endpoint = s.consoleEndpoint(r)
	caller := s.consoleCaller
	if regional {
		caller = caller.Base64Blobs()
	}

	var output any
	var err error
	if in.Operation == "InvokeApi" && (in.Service == consoleAPIGateway || in.Service == consoleHTTPAPI) {
		output, err = s.consoleInvokeAPI(r, c, caller, in)
	} else {
		output, err = s.consoleCall(r, c, caller, &in)
	}

	if err != nil {
		s.consoleFail(w, c, err)
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]any{"output": output, "request_id": c.meta.RequestID})
}

// consoleCall runs a console action after IAM decides it, as the AWS listener
// does: root credentials and unsigned parse-mode actions bypass it; every other
// principal, including a person signed in through the identity provider, is
// evaluated.
func (s *Server) consoleCall(r *http.Request, c *call, caller *catalog.Caller, in *consoleAction) (any, error) {
	op, input, err := caller.Decode(in.Service, in.Operation, in.Input)
	if err != nil {
		return nil, err
	}

	if err := s.authorize(request.WithMetadata(r.Context(), c.meta), r, c, op, input); err != nil {
		return nil, err
	}

	// authorize records the evaluated principal in c.meta, which the operation sees.
	return caller.Run(request.WithMetadata(r.Context(), c.meta), op, input)
}

// SameOrigin reports whether a request did not come from another site's page.
const httpScheme = "http"

const httpsScheme = "https"

// JSON requests plus an Origin/Fetch-Metadata check prevent browser cross-site writes.
// Non-browser clients need not send Origin; verify mode still requires SigV4.
func SameOrigin(r *http.Request) bool {
	scheme := httpScheme
	if r.TLS != nil {
		scheme = httpsScheme
	}

	return SameOriginURL(r, scheme+"://"+r.Host)
}

// SameOriginURL checks against an operator-configured public origin, including
// behind a TLS-terminating proxy. It never trusts client-supplied forwarded headers.
func SameOriginURL(r *http.Request, origin string) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site == "cross-site" || site == "same-site" {
		return false
	}

	expected, err := url.Parse(origin)
	if err != nil || expected.Host == "" || expected.User != nil || expected.Opaque != "" ||
		(expected.Scheme != httpScheme && expected.Scheme != httpsScheme) ||
		originAuthority(expected) != originAuthority(&url.URL{Scheme: expected.Scheme, Host: r.Host}) {
		return false
	}

	if header := r.Header.Get("Origin"); header != "" {
		u, err := url.Parse(header)
		return err == nil && u.Scheme == expected.Scheme && u.Host != "" &&
			originAuthority(u) == originAuthority(expected) && u.User == nil && u.Opaque == "" &&
			u.Path == "" && u.RawQuery == "" && u.Fragment == ""
	}

	return true
}

// originAuthority matches browser origin serialization, which drops default ports
// and treats DNS hostnames case-insensitively. Non-default ports remain distinct.
func originAuthority(u *url.URL) string {
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case httpsScheme:
			port = "443"
		case httpScheme:
			port = "80"
		}
	}

	return net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

func (s *Server) sameOrigin(r *http.Request) bool {
	// Browser actions use the pinned public console origin, including behind a
	// TLS proxy. Programs without Origin or Fetch Metadata may use either listener;
	// verify mode still authenticates their signature and checks the action scope.
	if oidc := s.cfg.Console.Auth.OIDC; oidc != nil {
		if r.Header.Get("Origin") != "" {
			return SameOriginURL(r, oidc.RedirectURL)
		}

		for header := range r.Header {
			if strings.HasPrefix(strings.ToLower(header), "sec-fetch-") {
				return SameOriginURL(r, oidc.RedirectURL)
			}
		}
	}

	return SameOrigin(r)
}

// consoleEndpoint is the AWS listener's address for confirmation URLs. They
// must use it when the action arrived on the separately served console; direct
// API callers keep their request host.
func (s *Server) consoleEndpoint(r *http.Request) string {
	host := r.Host
	if hostname, port, splitErr := net.SplitHostPort(host); splitErr == nil &&
		port == strconv.Itoa(s.cfg.Server.ConsolePort) {
		host = net.JoinHostPort(hostname, strconv.Itoa(s.cfg.Server.Port))
	}

	if r.TLS != nil {
		return "https://" + host
	}

	return "http://" + host
}

func (s *Server) consoleFail(w http.ResponseWriter, c *call, err error) {
	e, status := s.consoleStatus(c, err)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"code": e.Code, "message": e.Message, "request_id": c.meta.RequestID,
	})
}

// consoleStatus records err as the failure of a console request and chooses the
// status code that reports it.
func (s *Server) consoleStatus(c *call, err error) (e *awserror.Error, status int) {
	e = awserror.From(err)
	c.failure = e
	status = e.StatusCode
	if c.service != nil {
		if binding, ok := c.service.Errors[e.Code]; ok {
			// An IAM denial stays 403 whatever status the service binds the code to.
			if c.authorization == nil || c.authorization.Allowed ||
				c.authorization.Mode != config.AuthorizationEnforce {
				status = binding.StatusCode
			}

			if binding.QueryCode != "" {
				e.Code = binding.QueryCode
			}
		}
	}

	if status == 0 {
		status = http.StatusBadRequest
	}

	return e, status
}
