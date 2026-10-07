package iot

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/iot"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	kindRoleAlias = "rolealias"
	// credentialsPrincipal is the service principal role aliases' roles
	// must trust.
	credentialsPrincipal = "credentials.iot.amazonaws.com"
	// Role aliases' credentials last 15 minutes to 12 hours, an hour by
	// default.
	minCredentialSeconds     = 900
	maxCredentialSeconds     = 43200
	defaultCredentialSeconds = 3600
	// thingNameHeader names the thing a credentials request is for.
	thingNameHeader = "x-amzn-iot-thingname"
)

var (
	roleAliasPattern       = regexp.MustCompile(`^[\w=,@-]{1,128}$`)
	credentialsPathPattern = regexp.MustCompile(`^/role-aliases/([\w=,@-]{1,128})/credentials$`)
)

// RoleCredentials issues credentials for a role to an AWS service principal,
// as sts.Service.AssumeRoleForService does.
type RoleCredentials func(ctx context.Context, principal, roleARN, sessionName string, seconds int64) (
	accessKey, secret, token string, expires time.Time, err error)

// UseRoleCredentials lets the credentials provider exchange device
// certificates for role credentials. Call it before serving requests.
func (s *Service) UseRoleCredentials(fn RoleCredentials) { s.roleCredentials = fn }

// roleAlias is a role alias's record.
type roleAlias struct {
	Tags     map[string]string
	Alias    string
	RoleARN  string
	Seconds  int64
	Created  float64
	Modified float64
}

func roleAliasKey(scope request.Scope, alias string) string {
	return scope.Key(service, kindRoleAlias, alias)
}

func loadRoleAlias(tx storage.ReadTx, scope request.Scope, alias string) (*roleAlias, error) {
	r, found, err := storage.Read[roleAlias](tx, roleAliasKey(scope, alias))
	if err == nil && !found {
		err = notFound("Role alias %s cannot be found.", alias)
	}

	return r, err
}

func checkRoleAlias(r *roleAlias) error {
	switch {
	case !rolePattern.MatchString(r.RoleARN):
		return invalid("The role %s is not an IAM role ARN.", r.RoleARN)
	case r.Seconds < minCredentialSeconds || r.Seconds > maxCredentialSeconds:
		return invalid("credentialDurationSeconds must be between %d and %d.", minCredentialSeconds,
			maxCredentialSeconds)
	}

	return nil
}

func (s *Service) CreateRoleAlias(
	ctx context.Context,
	in *api.CreateRoleAliasRequest,
) (*api.CreateRoleAliasResponse, error) {
	alias := common.Value(in.RoleAlias)
	if !roleAliasPattern.MatchString(alias) {
		return nil, invalid("The role alias must be 1 to 128 word characters or =,@-.")
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	now := common.Unix(s.now())
	r := &roleAlias{
		Alias: alias, RoleARN: common.Value(in.RoleArn), Tags: tags, Created: now, Modified: now,
		Seconds: common.Value(common.OrElse(in.CredentialDurationSeconds, new(int64(defaultCredentialSeconds)))),
	}
	if checkErr := checkRoleAlias(r); checkErr != nil {
		return nil, checkErr
	}

	var out *api.CreateRoleAliasResponse
	err = s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if exists(tx, roleAliasKey(scope, alias)) {
			return alreadyExists("Role alias %s already exists.", alias)
		}

		out = &api.CreateRoleAliasResponse{RoleAlias: new(alias), RoleAliasArn: new(arn(scope, kindRoleAlias, alias))}
		return storage.Write(tx, roleAliasKey(scope, alias), r)
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DescribeRoleAlias(
	ctx context.Context,
	in *api.DescribeRoleAliasRequest,
) (*api.DescribeRoleAliasResponse, error) {
	var out *api.DescribeRoleAliasResponse
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		r, err := loadRoleAlias(tx, scope, common.Value(in.RoleAlias))
		if err != nil {
			return err
		}

		out = &api.DescribeRoleAliasResponse{RoleAliasDescription: &api.RoleAliasDescription{
			RoleAlias: new(r.Alias), RoleAliasArn: new(arn(scope, kindRoleAlias, r.Alias)), RoleArn: new(r.RoleARN),
			Owner: new(scope.AccountID), CredentialDurationSeconds: new(r.Seconds), CreationDate: new(r.Created),
			LastModifiedDate: new(r.Modified),
		}}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) UpdateRoleAlias(
	ctx context.Context,
	in *api.UpdateRoleAliasRequest,
) (*api.UpdateRoleAliasResponse, error) {
	var out *api.UpdateRoleAliasResponse
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := loadRoleAlias(tx, scope, common.Value(in.RoleAlias))
		if err != nil {
			return err
		}

		r.RoleARN = common.Value(common.OrElse(in.RoleArn, &r.RoleARN))
		r.Seconds = common.Value(common.OrElse(in.CredentialDurationSeconds, &r.Seconds))
		if err := checkRoleAlias(r); err != nil {
			return err
		}

		r.Modified = common.Unix(s.now())
		out = &api.UpdateRoleAliasResponse{
			RoleAlias: new(r.Alias), RoleAliasArn: new(arn(scope, kindRoleAlias, r.Alias)),
		}
		return storage.Write(tx, roleAliasKey(scope, r.Alias), r)
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DeleteRoleAlias(
	ctx context.Context,
	in *api.DeleteRoleAliasRequest,
) (*api.DeleteRoleAliasResponse, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if _, err := loadRoleAlias(tx, scope, common.Value(in.RoleAlias)); err != nil {
			return err
		}

		tx.Delete(roleAliasKey(scope, common.Value(in.RoleAlias)))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteRoleAliasResponse{}, nil
}

func (s *Service) ListRoleAliases(
	ctx context.Context,
	in *api.ListRoleAliasesRequest,
) (*api.ListRoleAliasesResponse, error) {
	var aliases []string
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		prefix := scope.Prefix(service, kindRoleAlias)
		for _, key := range tx.Keys(prefix) {
			aliases = append(aliases, strings.TrimPrefix(key, prefix))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	if in.AscendingOrder != nil && !*in.AscendingOrder {
		slices.Reverse(aliases)
	}

	selected, next, err := page(aliases, func(a string) string { return a }, in.Marker, in.PageSize)
	if err != nil {
		return nil, err
	}

	if selected == nil {
		selected = []string{}
	}

	return &api.ListRoleAliasesResponse{RoleAliases: selected, NextMarker: next}, nil
}

// bufferedConn reads a connection through the reader that peeked at it.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// serveCredentials answers one HTTP request on a TLS connection: the
// credentials provider's GET /role-aliases/<alias>/credentials, which
// exchanges the client's certificate for its role alias's credentials.
func (s *Service) serveCredentials(ctx context.Context, c *connection, r *bufio.Reader) {
	defer func() { _ = c.Close() }()

	req, err := http.ReadRequest(r)
	if err != nil {
		return
	}

	status, body := s.credentials(ctx, c, req)
	resp := &http.Response{
		StatusCode: status, ProtoMajor: 1, ProtoMinor: 1, Header: http.Header{}, Close: true,
		ContentLength: int64(len(body)), Body: io.NopCloser(bytes.NewReader(body)),
	}
	resp.Header.Set("Content-Type", "application/json")
	_ = resp.Write(c)
}

// credentials decides a credentials request: its status and JSON body.
func (s *Service) credentials(ctx context.Context, c *connection, req *http.Request) (status int, body []byte) {
	failure := func(status int, message string) (int, []byte) {
		return status, encodeResult(map[string]any{keyMessage: message})
	}

	m := credentialsPathPattern.FindStringSubmatch(req.URL.Path)
	switch {
	case req.Method != http.MethodGet || m == nil:
		return failure(http.StatusNotFound, "Not found")
	case c.cert == nil:
		return failure(http.StatusForbidden, "A client certificate is required")
	case s.roleCredentials == nil:
		return failure(http.StatusInternalServerError, "The credentials provider is not available")
	}

	thingName := req.Header.Get(thingNameHeader)
	var alias *roleAlias
	attached := true
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		var err error
		if alias, err = loadRoleAlias(tx, c.scope, m[1]); err != nil {
			return err
		}

		if thingName != "" {
			p, err := loadPrincipal(tx, c.scope, c.principal)
			if err != nil {
				return err
			}

			attached = slices.Contains(p.Things, thingName)
		}

		return nil
	})
	switch {
	case err != nil:
		return failure(http.StatusNotFound, "Role alias does not exist")
	case !attached:
		return failure(http.StatusForbidden, "The thing is not attached to the certificate")
	}

	// In identity.mode verify the certificate's policies must allow
	// iot:AssumeRoleWithCertificate on the role alias, with the thing's
	// policy variables when the request names one.
	h := &hook{service: s}
	if s.verify && !h.authorize(ctx, c, thingName, "iot:AssumeRoleWithCertificate", kindRoleAlias+"/"+alias.Alias) {
		return failure(http.StatusForbidden, "Access to the role alias is denied")
	}

	session := thingName
	if session == "" {
		session = c.caller
	}

	key, secret, token, expires, err := s.roleCredentials(ctx, credentialsPrincipal, alias.RoleARN, session,
		alias.Seconds)
	if err != nil {
		return failure(http.StatusForbidden, err.Error())
	}

	return http.StatusOK, encodeResult(map[string]any{"credentials": map[string]any{
		"accessKeyId": key, "secretAccessKey": secret, "sessionToken": token,
		"expiration": expires.UTC().Format(time.RFC3339),
	}})
}

// isHTTP reports whether a connection's first bytes are an HTTP request
// rather than an MQTT CONNECT.
func isHTTP(first []byte) bool {
	return len(first) > 0 && (first[0] >= 'A' && first[0] <= 'Z')
}
