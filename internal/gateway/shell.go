//go:build console

package gateway

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"slices"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/shell"
)

// shellPath is the console listener's CloudShell endpoint.
const shellPath = "/_tinystack/shell"

// shellMaxOutput caps what one command prints.
const shellMaxOutput = 4 << 20

// The exit statuses of the AWS CLI.
const (
	shellExitUsage   = 252
	shellExitService = 254
)

// shellAdminOnly lists, by catalog service, the operations that start
// containers or issue credentials. When people sign in through the identity
// provider only administrators run them in the CloudShell; IAM still decides
// the rest for everyone.
var shellAdminOnly = map[string][]string{
	"lambda": {"Invoke"},
	"ecs":    {"RunTask", "CreateService", "UpdateService"},
	"eks":    {"CreateCluster", "CreateNodegroup"},
	"elasticache": {
		"CreateCacheCluster", "CreateReplicationGroup", "CreateServerlessCache", "ModifyCacheCluster",
		"ModifyReplicationGroup", "ModifyServerlessCache",
	},
	"rds":             {"CreateDBInstance", "StartDBInstance", "RebootDBInstance", "CreateDBCluster"},
	"sts":             {"AssumeRole", "AssumeRoleWithWebIdentity", "GetSessionToken"},
	"iam":             {"CreateAccessKey"},
	"cognitoidentity": {"GetCredentialsForIdentity", "GetOpenIdToken", "GetOpenIdTokenForDeveloperIdentity"},
}

// shellRequest is one line typed into the CloudShell.
type shellRequest struct {
	Command string `json:"command"`
	Account string `json:"account"`
	Region  string `json:"region,omitempty"`
	// Complete asks what could finish the line instead of running it.
	Complete bool `json:"complete,omitempty"`
}

// shellCandidates answers a request to complete a line.
type shellCandidates struct {
	RequestID  string   `json:"request_id"`
	Candidates []string `json:"candidates"`
}

// shellResult is what a command printed.
type shellResult struct {
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	RequestID string `json:"request_id"`
	Exit      int    `json:"exit"`
}

// registerShell serves the CloudShell on the console's listener. Every command
// runs in-process through the path console actions use, as the person who
// signed the request; nothing reaches a host shell.
func (s *Server) registerShell() {
	services := make([]catalog.Service, 0, len(s.services))
	for _, service := range s.services {
		services = append(services, *service)
	}

	sh := shell.New(services)
	s.console[shellPath] = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.serveShell(w, r, sh) })
}

func (s *Server) serveShell(w http.ResponseWriter, r *http.Request, sh *shell.Shell) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)

		return
	}

	c := &call{meta: request.Metadata{
		Scope:     request.Scope{AccountID: s.cfg.Defaults.Account, Region: s.cfg.Defaults.Region},
		RequestID: common.RandomID(32), Service: "cloudshell", Operation: "Command", Protocol: "console",
	}}
	w.Header().Set("x-amzn-RequestId", c.meta.RequestID)
	start := time.Now()
	body := &capturingBody{ReadCloser: r.Body}
	r.Body = body
	cw := &capturingWriter{ResponseWriter: w}
	w = cw
	defer func() { s.record(r, c, start, body, cw) }()

	if r.URL.RawQuery != "" || !s.sameOrigin(r) {
		s.consoleFail(w, c, &awserror.Error{
			Code: accessDenied, Message: "The CloudShell requires a same-origin request without query parameters.",
			StatusCode: http.StatusForbidden,
		})

		return
	}

	if contentType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); contentType != "application/json" {
		s.consoleFail(w, c, awserror.Validation("The CloudShell requires application/json."))
		return
	}

	raw, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if readErr != nil {
		s.consoleFail(w, c, awserror.Validation("Cannot read the command: %s", readErr))
		return
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var in shellRequest
	if err := decoder.Decode(&in); err != nil {
		s.consoleFail(w, c, awserror.Validation("Invalid CloudShell request: %s", err))
		return
	}

	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		s.consoleFail(w, c, awserror.Validation("The CloudShell requires one JSON object."))
		return
	}

	if !consoleAccount.MatchString(in.Account) {
		s.consoleFail(w, c, awserror.Validation("A 12-digit account is required."))
		return
	}

	if err := s.shellAuthenticate(w, r, c, raw, &in); err != nil {
		s.consoleFail(w, c, err)
		return
	}

	if in.Complete {
		_ = json.NewEncoder(w).Encode(shellCandidates{
			Candidates: append([]string{}, sh.Complete(in.Command)...), RequestID: c.meta.RequestID,
		})

		return
	}

	s.runShell(w, r, c, sh, &in)
}

// shellAuthenticate checks who signed a request. Requests are signed for the
// "tinystack" service, as the other endpoints of TinyStack's own are, because
// the service a command calls is not known until it is read.
func (s *Server) shellAuthenticate(w http.ResponseWriter, r *http.Request, c *call, raw []byte,
	in *shellRequest,
) error {
	region := cmp.Or(in.Region, s.cfg.Defaults.Region)
	if !consoleRegion.MatchString(region) {
		return awserror.Validation("A valid region is required.")
	}

	c.meta.Region = region
	if s.cfg.Identity.Mode != config.IdentityVerify {
		c.meta.AccountID = in.Account
		return nil
	}

	c.service = &catalog.Service{Name: AdminSigningName, SigningName: AdminSigningName}
	defer func() { c.service = nil }()

	// Reconstruct the captured body so authentication hashes exactly what arrived.
	r.Body = io.NopCloser(bytes.NewReader(raw))
	a, signed, err := signature(r)
	if err != nil {
		return err
	}

	if signed && a.Expires != 0 {
		return &awserror.Error{
			Code: accessDenied, Message: "The CloudShell does not accept presigned requests.",
			StatusCode: http.StatusForbidden,
		}
	}

	signedService, err := s.identify(r, &c.meta)
	if err == nil {
		err = s.resolveCredentials(r.Context(), c)
	}

	if err == nil {
		err = s.authenticate(w, r, c)
	}

	if err != nil {
		return err
	}

	if signedService != AdminSigningName || c.meta.AccountID != in.Account {
		return &awserror.Error{
			Code: accessDenied, Message: "The CloudShell request must be signed for the " + AdminSigningName +
				" service in the account it runs in.",
			StatusCode: http.StatusForbidden,
		}
	}

	// The signed Region only scopes the signature; a command may name another.
	c.meta.Region = region

	return nil
}

// shellAdmin reports whether the signer may run the operations of
// shellAdminOnly: always, unless people sign in through the identity provider
// and this one is not an administrator.
func (s *Server) shellAdmin(c *call) bool {
	if s.cfg.Console.Auth.OIDC == nil || c.credentials == nil {
		return true
	}

	switch {
	case c.credentials.Principal.Kind == request.PrincipalRoot:
		return true
	case c.credentials.Federated:
		return s.adminRoles[c.credentials.Principal.PolicyARN]
	default:
		return false
	}
}

func (s *Server) runShell(w http.ResponseWriter, r *http.Request, c *call, sh *shell.Shell, in *shellRequest) {
	cmd, err := sh.Parse(in.Command)
	if err != nil {
		s.shellFail(w, c, awserror.New("CloudShellUsage", err.Error()), shellExitUsage, err.Error())
		return
	}

	if cmd.Builtin != "" {
		s.shellPrint(w, c, "", cmd.Builtin+" is run by the terminal in the browser.", shellExitUsage)
		return
	}

	if cmd.Service == "" {
		s.shellPrint(w, c, cmd.Text, "", 0)
		return
	}

	if region := cmp.Or(cmd.Region, in.Region); region != "" {
		if !consoleRegion.MatchString(region) {
			s.shellFail(w, c, awserror.New("CloudShellUsage", "The region is not valid."), shellExitUsage,
				"The region is not valid.")

			return
		}

		c.meta.Region = region
	}

	c.service = s.services[cmd.Service]
	c.meta.Service, c.meta.Operation = cmd.Service, cmd.Operation
	if slices.Contains(shellAdminOnly[cmd.Service], cmd.Operation) && !s.shellAdmin(c) {
		denied := &awserror.Error{
			Code: accessDenied, StatusCode: http.StatusForbidden,
			Message: fmt.Sprintf("%s:%s starts containers or issues credentials, which only administrators run in "+
				"the CloudShell.", c.service.SigningName, cmd.Operation),
		}
		s.shellFail(w, c, denied, shellExitService, denied.Message)

		return
	}

	c.meta.Endpoint = s.consoleEndpoint(r)
	action := &consoleAction{Service: cmd.Service, Operation: cmd.Operation, Input: cmd.Input}
	output, err := s.consoleCall(r, c, s.consoleCaller.Base64Blobs(), action)
	if err != nil {
		e := awserror.From(err)
		s.shellFail(w, c, e, shellExitService,
			fmt.Sprintf("An error occurred (%s) when calling the %s operation: %s", e.Code, cmd.Operation, e.Message))

		return
	}

	out, _ := output.(map[string]any)
	text, err := shell.Format(cmd, out)
	if err != nil {
		s.shellFail(w, c, awserror.New("InternalFailure", err.Error()), shellExitService, err.Error())
		return
	}

	s.shellPrint(w, c, text, "", 0)
}

// shellFail reports a failed command: the request is recorded as failed, with
// the status its error carries, and the terminal gets the message.
func (s *Server) shellFail(w http.ResponseWriter, c *call, e *awserror.Error, exit int, message string) {
	_, status := s.consoleStatus(c, e)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(shellResult{Stderr: message + "\n", Exit: exit, RequestID: c.meta.RequestID})
}

func (s *Server) shellPrint(w http.ResponseWriter, c *call, stdout, stderr string, exit int) {
	if len(stdout) > shellMaxOutput {
		stdout = stdout[:shellMaxOutput]
		stderr += fmt.Sprintf("\nOutput truncated at %d MiB.", shellMaxOutput>>20)
	}

	if stderr != "" {
		stderr += "\n"
	}

	_ = json.NewEncoder(w).Encode(shellResult{Stdout: stdout, Stderr: stderr, Exit: exit, RequestID: c.meta.RequestID})
}
