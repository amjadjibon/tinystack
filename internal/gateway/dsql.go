package gateway

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/aws/sigv4"
	"github.com/amjadjibon/tinystack/internal/config"
)

const dsqlService = "dsql"

// AuthorizeDSQL authenticates a prevalidated database token using the gateway's identity snapshot.
func (s *Server) AuthorizeDSQL(ctx context.Context, scope request.Scope, arn, user, raw, endpoint string) error {
	u, err := url.Parse("https://" + raw)
	if err != nil {
		return err
	}

	a, err := sigv4.ParseQuery(u.Query(), 0)
	if err != nil {
		return err
	}

	token := u.Query().Get(sigv4.QuerySecurityToken)
	if s.cfg.Identity.Mode == config.IdentityVerify {
		secret, _, ok := s.gitKey(ctx, a.AccessKey, token)
		if !ok {
			return errors.New("unknown database signing identity")
		}

		signed := &http.Request{Method: http.MethodGet, URL: u, Host: endpoint, Header: http.Header{}}
		if err := sigv4.Verify(signed, a, secret, emptyHash, false, time.Now()); err != nil {
			return err
		}
	}

	mode := s.cfg.Identity.Authorization
	if mode == "" || mode == config.AuthorizationOff {
		return nil
	}

	action := "DbConnect"
	if user == "admin" {
		action = "DbConnectAdmin"
	}

	principal := s.gitPrincipal(ctx, a.AccessKey, token, s.gitAccount(ctx, a.AccessKey))
	snapshot, failed := s.identitySnapshot(ctx, &principal)
	principal = snapshot.Principal
	r := &http.Request{
		Method: http.MethodGet, URL: u, Host: endpoint, Header: http.Header{},
		RemoteAddr: "127.0.0.1:0", TLS: new(tls.ConnectionState),
	}

	values := authorizationValues(r, &principal, snapshot.Tags, scope.Region)
	ctx = request.WithMetadata(ctx, request.Metadata{
		Scope: scope, Service: dsqlService, Operation: action,
		Protocol: "postgres", Principal: &principal, AuthorizationValues: values,
	})
	decision := s.evaluate(ctx, mode, &evaluation{
		service:  &catalog.Service{IAMPrefix: dsqlService},
		op:       &catalog.Operation{Name: action, IAMAction: "dsql:" + action},
		snapshot: snapshot, values: values,
		resources: []catalog.AuthorizationResource{{ARN: arn}}, serviceName: dsqlService, lookupFailed: failed,
	})
	status := http.StatusOK
	if !decision.Allowed && mode != config.AuthorizationSoft {
		status = http.StatusForbidden
	}

	s.requests.Add(Record{
		Authorization: decision, Time: time.Now().UTC(), RequestID: requestID(),
		Method: "CONNECT", Path: "/" + strings.TrimPrefix(arn, "arn:aws:"), Service: dsqlService, Operation: action,
		Protocol: "postgres", Account: scope.AccountID, Region: scope.Region, Status: status,
	})
	if status != http.StatusOK {
		return errors.New("database authorization denied")
	}

	return nil
}
