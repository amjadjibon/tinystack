//go:build console

package gateway

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	stsapi "github.com/amjadjibon/tinystack/internal/aws/generated/sts"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/config"
)

// shellPath is the console listener's CloudShell endpoint.
const shellPath = "/_tinystack/shell"

// shellMaxOutput caps what one command prints.
const shellMaxOutput = 4 << 20

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
	Candidates []string `json:"candidates"`
}

// shellSupport exists only in console builds.
type shellSupport struct {
	issueShell func(context.Context, *request.Credentials) (*stsapi.Credentials, error)
}

// UseShellIssuer connects the in-process STS session issuer before serving.
func (s *Server) UseShellIssuer(issue func(context.Context, *request.Credentials) (*stsapi.Credentials, error)) {
	s.issueShell = issue
}

func (s *Server) registerShell() {
	s.console[shellPath] = http.HandlerFunc(s.serveShell)
}

func (s *Server) serveShell(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)

		return
	}

	c := &call{sensitive: true, meta: request.Metadata{
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

	c.meta.Service = "cloudshell"
	if c.credentials != nil {
		c.meta.Principal = &c.credentials.Principal
	}
	if !s.shellAdmin(c) {
		s.consoleFail(w, c, &awserror.Error{
			Code:       accessDenied,
			StatusCode: http.StatusForbidden,
			Message: "CloudShell is available only to administrators when SSO is configured " +
				"because the AWS CLI can access host files.",
		})
		return
	}

	s.runShell(w, r, c, &in)
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

// shellAdmin restricts host file access to administrators when OIDC is enabled.
func (s *Server) shellAdmin(c *call) bool {
	if s.cfg.Console.Auth.OIDC == nil {
		return true
	}

	if s.cfg.Identity.Mode != config.IdentityVerify {
		return false
	}

	if c.credentials == nil || c.credentials.Principal.Kind == request.PrincipalRoot {
		return true
	}

	return c.credentials.Federated && s.adminRoles[c.credentials.Principal.PolicyARN]
}
