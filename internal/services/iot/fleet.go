package iot

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/iot"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	kindOwnershipToken = "ownership-token"
	// ownershipTokenTTL is how long a certificate ownership token can
	// provision its certificate.
	ownershipTokenTTL = time.Hour
	// The fleet provisioning request topics.
	topicCreateKeys    = "$aws/certificates/create/json"
	topicCreateFromCSR = "$aws/certificates/create-from-csr/json"
	templatesPrefix    = "$aws/provisioning-templates/"
	provisionSuffix    = "/provision/json"
	// Members of the fleet provisioning messages.
	keyCertificateID      = "certificateId"
	codeInvalidParameters = "InvalidParameters"
)

// ownershipToken records the certificate a fleet provisioning token proves
// ownership of.
type ownershipToken struct {
	Certificate string
	Expires     int64
}

func ownershipKey(scope request.Scope, token string) string {
	return scope.Key(service, kindOwnershipToken, token)
}

// provisioningRequest reports whether topic is a fleet provisioning request,
// and for a provisioning one the template it names.
func provisioningRequest(topic string) (template string, ok bool) {
	if topic == topicCreateKeys || topic == topicCreateFromCSR {
		return "", true
	}

	rest, found := strings.CutPrefix(topic, templatesPrefix)
	if !found {
		return "", false
	}

	name, found := strings.CutSuffix(rest, provisionSuffix)
	return name, found && templateNamePattern.MatchString(name)
}

// fleetError is a rejected fleet provisioning request.
type fleetError struct {
	code    string
	message string
	status  int
}

func (e *fleetError) Error() string { return e.message }

// fleetFailure reports err as a fleet provisioning rejection: its awserror
// code, or InternalFailure.
func fleetFailure(err error) *fleetError {
	var fe *fleetError
	if errors.As(err, &fe) {
		return fe
	}

	var ae *awserror.Error
	if errors.As(err, &ae) {
		return &fleetError{
			status: http.StatusBadRequest, code: strings.TrimSuffix(ae.Code, "Exception"),
			message: ae.Message,
		}
	}

	return &fleetError{status: http.StatusInternalServerError, code: "InternalFailure", message: err.Error()}
}

// serveFleet answers a fleet provisioning request a device published over
// MQTT on the request's accepted or rejected topic.
func (s *Service) serveFleet(ctx context.Context, scope request.Scope, clientID, claim, topic string,
	payload []byte,
) {
	ctx = request.WithMetadata(ctx, request.Metadata{Scope: scope, Service: service, RequestID: common.RandomID(32,
		common.AlphabetLowerHex)})
	doc, _ := decodePayload(payload).(map[string]any)
	var accepted map[string]any
	var err error
	switch template, _ := provisioningRequest(topic); {
	case doc == nil:
		err = &fleetError{status: http.StatusBadRequest, code: "InvalidPayload", message: "The payload is not JSON."}
	case topic == topicCreateKeys:
		accepted, err = s.createFleetKeys(ctx, scope)
	case topic == topicCreateFromCSR:
		csr, _ := doc["certificateSigningRequest"].(string)
		accepted, err = s.createFleetCertificate(ctx, scope, csr)
	default:
		accepted, err = s.provision(ctx, scope, template, clientID, claim, doc)
	}

	if err == nil {
		s.notify(scope, []notification{{topic: topic + "/accepted", payload: accepted}})
		return
	}

	f := fleetFailure(err)
	s.notify(scope, []notification{{topic: topic + "/rejected", payload: map[string]any{
		"statusCode": f.status, "errorCode": f.code, "errorMessage": f.message,
	}}})
}

// issueToken records an ownership token for a new certificate.
func (s *Service) issueToken(ctx context.Context, scope request.Scope, certificate string) (string, error) {
	token := common.RandomID(64)
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		return storage.Write(tx, ownershipKey(scope, token), &ownershipToken{
			Certificate: certificate, Expires: s.now().Add(ownershipTokenTTL).Unix(),
		})
	})

	return token, err
}

func (s *Service) createFleetKeys(ctx context.Context, scope request.Scope) (map[string]any, error) {
	out, err := s.CreateKeysAndCertificate(ctx, &api.CreateKeysAndCertificateRequest{SetAsActive: new(false)})
	if err != nil {
		return nil, err
	}

	token, err := s.issueToken(ctx, scope, *out.CertificateId)
	if err != nil {
		return nil, err
	}

	return map[string]any{
		keyCertificateID: *out.CertificateId, "certificatePem": *out.CertificatePem,
		"privateKey": *out.KeyPair.PrivateKey, "certificateOwnershipToken": token,
	}, nil
}

func (s *Service) createFleetCertificate(ctx context.Context, scope request.Scope, csr string) (map[string]any,
	error,
) {
	out, err := s.CreateCertificateFromCsr(ctx, &api.CreateCertificateFromCsrRequest{
		CertificateSigningRequest: new(csr), SetAsActive: new(false),
	})
	if err != nil {
		return nil, err
	}

	token, err := s.issueToken(ctx, scope, *out.CertificateId)
	if err != nil {
		return nil, err
	}

	return map[string]any{
		keyCertificateID: *out.CertificateId, "certificatePem": *out.CertificatePem,
		"certificateOwnershipToken": token,
	}, nil
}

// provision runs a fleet provisioning template for the certificate a
// request's ownership token names, creating its resources.
func (s *Service) provision(ctx context.Context, scope request.Scope, name, clientID, claim string,
	doc map[string]any,
) (map[string]any, error) {
	token, _ := doc["certificateOwnershipToken"].(string)
	var t *provisioningTemplate
	var owned *ownershipToken
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		var err error
		if t, err = loadTemplate(tx, scope, name); err != nil {
			return err
		}

		owned, _, err = storage.Read[ownershipToken](tx, ownershipKey(scope, token))
		if err != nil {
			return err
		}

		// A token provisions its certificate once.
		tx.Delete(ownershipKey(scope, token))
		return nil
	})
	switch {
	case err != nil:
		return nil, err
	case !t.Enabled:
		return nil, &fleetError{
			status: http.StatusForbidden, code: "TemplateDisabled",
			message: "The provisioning template " + name + " is disabled.",
		}
	case token == "" || owned == nil || owned.Expires < s.now().Unix():
		return nil, &fleetError{
			status: http.StatusBadRequest, code: "InvalidCertificateOwnershipToken",
			message: "The certificate ownership token is invalid or expired.",
		}
	}

	body, err := parseTemplateBody(t.version(t.Default).Body)
	if err != nil {
		return nil, err
	}

	params := map[string]string{}
	for k, p := range body.Parameters {
		if p.Default != nil {
			params[k] = *p.Default
		}
	}

	requested, _ := doc["parameters"].(map[string]any)
	for k, v := range requested {
		params[k] = stringOf(v)
	}

	params[certificateIDParameter] = owned.Certificate
	if hookErr := s.preProvision(ctx, scope, t, clientID, claim, owned.Certificate, params); hookErr != nil {
		return nil, hookErr
	}

	for k := range body.Parameters {
		if _, ok := params[k]; !ok {
			return nil, &fleetError{
				status: http.StatusBadRequest, code: codeInvalidParameters,
				message: "The parameter " + k + " has no value.",
			}
		}
	}

	thingName, err := s.provisionResources(ctx, scope, t.Name, body, params, owned.Certificate)
	if err != nil {
		return nil, err
	}

	config, err := body.resolve(body.DeviceConfiguration, params)
	if err != nil {
		return nil, &fleetError{status: http.StatusBadRequest, code: codeInvalidParameters, message: err.Error()}
	}

	out := map[string]any{"deviceConfiguration": config}
	if thingName != "" {
		out["thingName"] = thingName
	}

	return out, nil
}

// preProvision calls a template's pre-provisioning hook, which may refuse
// the request or override its parameters.
func (s *Service) preProvision(ctx context.Context, scope request.Scope, t *provisioningTemplate, clientID,
	claim, certificate string, params map[string]string,
) error {
	if t.Hook == nil {
		return nil
	}

	event := map[string]any{
		"claimCertificateId": claim, keyCertificateID: certificate, "templateArn": arn(scope, arnTemplate, t.Name),
		keyClientID: clientID, "parameters": params,
	}
	out, err := s.invokeSync(ctx, scope, *t.Hook.TargetArn, event)
	if err != nil {
		return &fleetError{
			status: http.StatusInternalServerError, code: "InternalFailure",
			message: "The pre-provisioning hook failed: " + err.Error(),
		}
	}

	resp, _ := out.(map[string]any)
	if allow, _ := resp["allowProvisioning"].(bool); !allow {
		return &fleetError{
			status: http.StatusForbidden, code: "AccessDenied",
			message: "The pre-provisioning hook refused the request.",
		}
	}

	overrides, _ := resp["parameterOverrides"].(map[string]any)
	for k, v := range overrides {
		params[k] = stringOf(v)
	}

	return nil
}

// provisionResources creates or updates a template's thing, activates and
// attaches its certificate, and attaches its policies, returning the thing's
// name.
func (s *Service) provisionResources(ctx context.Context, scope request.Scope, template string,
	body *templateBody, params map[string]string, certificate string,
) (thingName string, err error) {
	props := map[string]map[string]any{}
	for name, r := range body.Resources {
		resolved, resolveErr := body.resolve(r.Properties, params)
		if resolveErr != nil {
			return "", &fleetError{
				status: http.StatusBadRequest, code: codeInvalidParameters, message: resolveErr.Error(),
			}
		}

		m, _ := resolved.(map[string]any)
		props[r.Type+"\x00"+name] = m
	}

	certARN := arn(scope, "cert", certificate)
	for key, p := range props {
		typ, _, _ := strings.Cut(key, "\x00")
		switch typ {
		case typeThing:
			if thingName, err = s.provisionThing(ctx, p); err != nil {
				return "", err
			}
		case typeCertificate:
			if id := propText(p["CertificateId"]); id != certificate {
				return "", &fleetError{
					status: http.StatusBadRequest, code: codeInvalidParameters,
					message: "The template's certificate is not the one the ownership token names.",
				}
			}

			status := strings.ToUpper(common.Value(common.OrElse(anyString(p["Status"]), new(statusActive))))
			if _, err = s.UpdateCertificate(ctx, &api.UpdateCertificateRequest{
				CertificateId: new(certificate), NewStatus: new(status),
			}); err != nil {
				return "", err
			}
		}
	}

	if thingName != "" {
		if _, err = s.AttachThingPrincipal(ctx, &api.AttachThingPrincipalRequest{
			ThingName: new(thingName), Principal: new(certARN),
		}); err != nil {
			return "", err
		}
	}

	for key, p := range props {
		if typ, _, _ := strings.Cut(key, "\x00"); typ != typePolicy {
			continue
		}

		policy := propText(p["PolicyName"])
		if document, ok := p["PolicyDocument"]; ok {
			// An inline document becomes a policy named for the template and
			// the certificate.
			policy = template + "_" + certificate[:16]
			text, isText := document.(string)
			if !isText {
				text = string(encodeResult(document))
			}

			if _, err = s.CreatePolicy(ctx, &api.CreatePolicyRequest{
				PolicyName: new(policy), PolicyDocument: new(text),
			}); err != nil && !isCode(err, "ResourceAlreadyExistsException") {
				return "", err
			}
		}

		if _, err = s.AttachPolicy(ctx, &api.AttachPolicyRequest{
			PolicyName: new(policy), Target: new(certARN),
		}); err != nil {
			return "", err
		}
	}

	return thingName, nil
}

// provisionThing creates a template's thing, or updates an existing one's
// attributes, and adds it to the template's thing groups.
func (s *Service) provisionThing(ctx context.Context, p map[string]any) (string, error) {
	name := propText(p["ThingName"])
	attrs := map[string]string{}
	if payload, ok := p["AttributePayload"].(map[string]any); ok {
		for k, v := range payload {
			attrs[k] = stringOf(v)
		}
	}

	_, err := s.DescribeThing(ctx, &api.DescribeThingRequest{ThingName: new(name)})
	switch {
	case isCode(err, "ResourceNotFoundException"):
		in := &api.CreateThingRequest{ThingName: new(name), AttributePayload: &api.AttributePayload{Attributes: attrs}}
		if typ := propText(p["ThingTypeName"]); typ != "" {
			in.ThingTypeName = new(typ)
		}

		_, err = s.CreateThing(ctx, in)
	case err == nil && len(attrs) > 0:
		_, err = s.UpdateThing(ctx, &api.UpdateThingRequest{
			ThingName: new(name), AttributePayload: &api.AttributePayload{Attributes: attrs, Merge: new(true)},
		})
	}

	if err != nil {
		return "", err
	}

	groups, _ := p["ThingGroups"].([]any)
	for _, g := range groups {
		if _, err := s.AddThingToThingGroup(ctx, &api.AddThingToThingGroupRequest{
			ThingName: new(name), ThingGroupName: new(stringOf(g)),
		}); err != nil {
			return "", err
		}
	}

	return name, nil
}

// propText renders a template property as text; a missing one is empty.
func propText(v any) string {
	if v == nil {
		return ""
	}

	return stringOf(v)
}

// anyString returns a template value that is text, or nil.
func anyString(v any) *string {
	if s, ok := v.(string); ok {
		return &s
	}

	return nil
}

// isCode reports whether err is an awserror with code.
func isCode(err error, code string) bool {
	var ae *awserror.Error
	return errors.As(err, &ae) && ae.Code == code
}
