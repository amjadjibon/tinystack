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
)

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

// Console actions are an explicit subset of the registered SDK operations.
// IAM is account-wide; the region is only used when verifying signatures.
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
 RunInstances StartInstances StopInstances RebootInstances TerminateInstances
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
			consoleRDS:            rds,
			"requires_signature":  s.cfg.Identity.Mode == config.IdentityVerify,
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

	if r.URL.RawQuery != "" || !SameOrigin(r) {
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
	case consoleRDS:
		operations = consoleRDSOperations
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

	regional := in.Service != consoleIAM
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
	// Confirmation URLs must use the AWS listener when the action arrived on
	// the separately served console. Direct API callers keep their request host.
	host := r.Host
	if hostname, port, splitErr := net.SplitHostPort(host); splitErr == nil &&
		port == strconv.Itoa(s.cfg.Server.ConsolePort) {
		host = net.JoinHostPort(hostname, strconv.Itoa(s.cfg.Server.Port))
	}
	c.meta.Endpoint = "http://" + host
	if r.TLS != nil {
		c.meta.Endpoint = "https://" + host
	}
	caller := s.consoleCaller
	if regional {
		caller = caller.Base64Blobs()
	}

	var output any
	var err error
	if in.Operation == "InvokeApi" && (in.Service == consoleAPIGateway || in.Service == consoleHTTPAPI) {
		ctx := request.WithMetadata(r.Context(), c.meta)
		err = s.authorize(ctx, r, c, &catalog.Operation{Name: in.Operation, IAMAction: "execute-api:Invoke"}, nil)
		if err == nil {
			output, err = s.consoleInvokeAPI(r, c, caller, in)
		}
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
// JSON requests plus an Origin/Fetch-Metadata check prevent browser cross-site writes.
// Non-browser clients need not send Origin; verify mode still requires SigV4.
func SameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site == "cross-site" || site == "same-site" {
		return false
	}

	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}

		return err == nil && u.Scheme == scheme && u.Host == r.Host && u.User == nil &&
			u.Path == "" && u.RawQuery == "" && u.Fragment == ""
	}

	return true
}

func (s *Server) consoleFail(w http.ResponseWriter, c *call, err error) {
	e := awserror.From(err)
	c.failure = e
	status := e.StatusCode
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

	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"code": e.Code, "message": e.Message, "request_id": c.meta.RequestID,
	})
}
