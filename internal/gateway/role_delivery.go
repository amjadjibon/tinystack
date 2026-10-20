package gateway

import (
	"context"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/config"
)

// authorizeRole checks a call an AWS service makes under a role: the role's trust policy must let the service
// principal assume it, and the call is then decided as the role's session, through the same evaluation a signed
// request goes through. A call that does not decode is left to fail on its own.
func (s *Server) authorizeRole(ctx context.Context, mode string, d *catalog.ServiceDelivery) error {
	role := d.Role
	service := s.services[role.Service]
	if service == nil {
		return nil
	}

	op, input, err := s.consoleCaller.Decode(role.Service, role.Operation, role.Params)
	if err != nil {
		return nil //nolint:nilerr // A call that does not decode has nothing to authorize.
	}

	// A delivery from a request, such as API Gateway's, carries no account or Region of its own: the resource that
	// delivers is where the call is made.
	meta, _ := request.FromContext(ctx)
	if meta.AccountID == "" {
		meta.AccountID = d.SourceAccount
	}

	if meta.Region == "" {
		meta.Region = arnField(d.SourceARN, regionField)
	}

	fields := map[string]string{
		"service": d.Principal, "role": role.ARN, "source_arn": d.SourceARN, "source_account": d.SourceAccount,
	}
	entry := &deliveryEntry{
		Path: role.ARN, Service: "sts", Operation: "AssumeRole", Account: arnField(role.ARN, accountField),
		Region: meta.Region, Fields: fields,
	}
	record := &AuthorizationRecord{Mode: mode, Principal: d.Principal}
	if s.serviceSession == nil {
		err = awserror.New(accessDenied, "Role sessions are not available.")
	}

	var principal request.Principal
	if err == nil {
		principal, err = s.serviceSession(ctx, d.Principal, role.ARN, role.Session, d.SourceARN, d.SourceAccount)
	}

	if err != nil {
		record.Message = awserror.From(err).Message
		if mode == config.AuthorizationSoft {
			record.Message = "would deny: " + record.Message
		}

		return s.finishDelivery(ctx, record, entry, accessDenied)
	}

	snapshot, lookupFailed := s.identitySnapshot(ctx, &principal)
	principal = snapshot.Principal
	meta.Principal = &principal
	meta.Service, meta.Operation = role.Service, op.Name
	meta.ResourceAuthorization = resourceAuthorization(role.Service)
	meta.AuthorizationValues = authorizationValues(nil, &principal, snapshot.Tags, meta.Region)
	record = s.evaluate(request.WithMetadata(ctx, meta), mode, &evaluation{
		service: service, op: op, input: input, snapshot: snapshot, values: meta.AuthorizationValues,
		serviceName: role.Service, lookupFailed: lookupFailed,
	})

	fields["principal"] = principal.ARN
	entry.Service, entry.Operation, entry.Account = role.Service, op.Name, meta.AccountID
	if len(record.Checks) > 0 {
		entry.Path = record.Checks[0].Resource
	}

	return s.finishDelivery(ctx, record, entry, denialCode(service))
}

// The fields of an ARN, by their position.
const (
	regionField  = 3
	accountField = 4
)

// arnField is one field of an ARN, or empty when it has none.
func arnField(arn string, field int) string {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) < 6 {
		return ""
	}

	return parts[field]
}

// denialCode is the code a service reports a denial with: AccessDeniedException for the JSON services and
// AccessDenied for the XML and form ones.
func denialCode(service *catalog.Service) string {
	if slices.ContainsFunc(service.Protocols, func(p string) bool {
		return p == catalog.AWSJSON10 || p == catalog.AWSJSON11 || p == catalog.RESTJSON || p == catalog.RPCV2CBOR
	}) {
		return "AccessDeniedException"
	}

	return accessDenied
}
