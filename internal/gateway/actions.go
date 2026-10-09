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

const consoleS3 = "s3"

const consoleSQS = "sqs"

const consoleSNS = "sns"

const consoleLambda = "lambda"

const consoleLogs = "logs"

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

// Lambda's monitor tab reads CloudWatch logs without exposing log mutations.
var consoleLogsOperations = strings.Fields(`FilterLogEvents`)

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
		_ = json.NewEncoder(w).Encode(map[string]any{
			"account": s.cfg.Defaults.Account, "region": s.cfg.Defaults.Region,
			consoleIAM: iam, consoleS3: s3, consoleSQS: sqs, consoleSNS: sns,
			consoleLambda: lambda, consoleLogs: logs,
			"requires_signature": s.cfg.Identity.Mode == config.IdentityVerify,
		})
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

	if r.URL.RawQuery != "" || !consoleOrigin(r) {
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

		if signedService != in.Service || c.meta.AccountID != in.Account ||
			(regional && c.meta.Region != in.Region) {
			s.consoleFail(w, c, &awserror.Error{
				Code: accessDenied, Message: "The action scope must match the signed service, account and region.",
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

	output, err := caller.Call(request.WithMetadata(r.Context(), c.meta), in.Service, in.Operation, in.Input)
	if err != nil {
		s.consoleFail(w, c, err)
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]any{"output": output, "request_id": c.meta.RequestID})
}

// JSON requests plus an Origin/Fetch-Metadata check prevent browser cross-site writes.
// Non-browser clients need not send Origin; verify mode still requires SigV4.
func consoleOrigin(r *http.Request) bool {
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
			status = binding.StatusCode
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
