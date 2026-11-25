package sns

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"strconv"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/sns"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/delivery"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	protocolApplication = "application"
	kindApp             = "app"
	kindEndpoint        = "endpoint"
	attrEnabled         = "Enabled"
	attrToken           = "Token"
	attrCustomUserData  = "CustomUserData"
)

var (
	platformAppName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,256}$`)
	validPlatforms  = map[string]bool{
		"APNS":         true,
		"APNS_SANDBOX": true,
		"GCM":          true,
		"ADM":          true,
		"BAIDU":        true,
		"WNS":          true,
		"MPNS":         true,
	}
	platformAppAttrs = map[string]bool{
		"PlatformCredential":         true,
		"PlatformPrincipal":          true,
		attrEnabled:                  true,
		"EventEndpointCreated":       true,
		"EventEndpointDeleted":       true,
		"EventEndpointUpdated":       true,
		"EventDeliveryFailure":       true,
		"SuccessFeedbackRoleArn":     true,
		"FailureFeedbackRoleArn":     true,
		"SuccessFeedbackSampleRate":  true,
		"AppleCertificateExpiryDate": true,
		"ApplePlatformTeamID":        true,
		"ApplePlatformBundleID":      true,
	}
)

type endpointRef struct {
	scope    request.Scope
	platform string
	appName  string
	id       string
}

type platformApplication struct {
	Attributes map[string]string `json:",omitempty"`
	Secrets    map[string]string `json:",omitempty"`
	Platform   string
	Name       string
	CreatedAt  int64
}

type platformEndpoint struct {
	ID             string
	Platform       string
	AppName        string
	Token          string
	CustomUserData string `json:",omitempty"`
	Enabled        bool
	CreatedAt      int64
}

func appKey(scope request.Scope, platform, name string) string {
	return scope.Key(serviceName, kindApp, platform+"/"+name)
}

func appARN(scope request.Scope, platform, name string) string {
	return "arn:aws:sns:" + scope.Region + ":" + scope.AccountID + ":app/" + platform + "/" + name
}

func endpointKey(scope request.Scope, platform, appName, id string) string {
	return scope.Key(serviceName, kindEndpoint, platform+"/"+appName+"/"+id)
}

func endpointARN(scope request.Scope, platform, appName, id string) string {
	return "arn:aws:sns:" + scope.Region + ":" + scope.AccountID + ":endpoint/" + platform + "/" + appName + "/" + id
}

func parsePlatformAppARN(raw *string) (scope request.Scope, platform, name string, err error) {
	parts := strings.Split(common.Value(raw), ":")
	if len(parts) != 6 || parts[0] != arnScheme || parts[2] != serviceName || parts[3] == "" ||
		!accountID.MatchString(parts[4]) {
		return request.Scope{}, "", "", invalid("PlatformApplicationArn")
	}

	segs := strings.Split(parts[5], "/")
	if len(segs) != 3 || segs[0] != kindApp || !validPlatforms[segs[1]] || !platformAppName.MatchString(segs[2]) {
		return request.Scope{}, "", "", invalid("PlatformApplicationArn")
	}

	return request.Scope{AccountID: parts[4], Region: parts[3]}, segs[1], segs[2], nil
}

func parsePlatformEndpointARN(raw string) (endpointRef, error) {
	parts := strings.Split(raw, ":")
	if len(parts) != 6 || parts[0] != arnScheme || parts[2] != serviceName || parts[3] == "" ||
		!accountID.MatchString(parts[4]) {
		return endpointRef{}, invalid("EndpointArn")
	}

	segs := strings.Split(parts[5], "/")
	if len(segs) != 4 || segs[0] != kindEndpoint || !validPlatforms[segs[1]] ||
		!platformAppName.MatchString(segs[2]) || segs[3] == "" {
		return endpointRef{}, invalid("EndpointArn")
	}

	return endpointRef{
		scope:    request.Scope{AccountID: parts[4], Region: parts[3]},
		platform: segs[1],
		appName:  segs[2],
		id:       segs[3],
	}, nil
}

func resolvePlatformApp(
	ctx context.Context,
	raw *string,
) (key string, scope request.Scope, platform, name string, err error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return "", request.Scope{}, "", "", err
	}

	scope, platform, name, err = parsePlatformAppARN(raw)
	if err != nil {
		return "", request.Scope{}, "", "", err
	}

	if scope.Region != m.Region || (scope.AccountID != m.AccountID && !m.ResourceAuthorization) {
		return "", request.Scope{}, "", "", notFound("PlatformApplication")
	}

	return appKey(scope, platform, name), scope, platform, name, nil
}

func resolvePlatformEndpoint(ctx context.Context, raw *string) (key string, ref endpointRef, err error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return "", endpointRef{}, err
	}

	ref, err = parsePlatformEndpointARN(common.Value(raw))
	if err != nil {
		return "", endpointRef{}, err
	}

	if ref.scope.Region != m.Region || (ref.scope.AccountID != m.AccountID && !m.ResourceAuthorization) {
		return "", endpointRef{}, notFound("Endpoint")
	}

	return endpointKey(ref.scope, ref.platform, ref.appName, ref.id), ref, nil
}

func checkPlatformAppAttributes(attrs map[string]string) (stored, secrets map[string]string, err error) {
	stored = map[string]string{}
	secrets = map[string]string{}
	for k, v := range attrs {
		if !platformAppAttrs[k] {
			return nil, nil, invalid("Attribute %s is not supported", k)
		}

		switch k {
		case attrEnabled:
			if v != valueTrue && v != valueFalse {
				return nil, nil, invalid("Enabled must be true or false")
			}

			stored[k] = v
		case "SuccessFeedbackSampleRate":
			if v != "" {
				n, parseErr := strconv.Atoi(v)
				if parseErr != nil || n < 0 || n > 100 {
					return nil, nil, invalid("SuccessFeedbackSampleRate must be an integer from 0 to 100")
				}
			}

			stored[k] = v
		case "PlatformCredential", "PlatformPrincipal":
			secrets[k] = v
		default:
			stored[k] = v
		}
	}

	return stored, secrets, nil
}

func (a *platformApplication) visibleAttributes() map[string]string {
	out := maps.Clone(a.Attributes)
	if out == nil {
		out = map[string]string{}
	}

	if out[attrEnabled] == "" {
		out[attrEnabled] = valueTrue
	}

	return out
}

func (a *platformApplication) enabled() bool {
	return a.Attributes[attrEnabled] != valueFalse
}

func (e *platformEndpoint) visibleAttributes() map[string]string {
	out := map[string]string{
		attrToken:   e.Token,
		attrEnabled: strconv.FormatBool(e.Enabled),
	}
	if e.CustomUserData != "" {
		out[attrCustomUserData] = e.CustomUserData
	}

	return out
}

func (s *Service) CreatePlatformApplication(
	ctx context.Context,
	in *api.CreatePlatformApplicationInput,
) (*api.CreatePlatformApplicationResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	name := common.Value(in.Name)
	if !platformAppName.MatchString(name) {
		return nil, invalid("Name must be 1 to 256 alphanumeric characters, hyphens, underscores, or periods")
	}

	platform := common.Value(in.Platform)
	if !validPlatforms[platform] {
		return nil, invalid("Platform %q is not supported", platform)
	}

	stored, secrets, err := checkPlatformAppAttributes(in.Attributes)
	if err != nil {
		return nil, err
	}

	if stored[attrEnabled] == "" {
		stored[attrEnabled] = valueTrue
	}

	key := appKey(m.Scope, platform, name)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		existing, found, readErr := storage.Read[platformApplication](tx, key)
		if readErr != nil {
			return fmt.Errorf("decode platform application: %w", readErr)
		}

		if found {
			if !maps.Equal(existing.visibleAttributes(), stored) || !maps.Equal(existing.Secrets, secrets) {
				return invalid("Platform application already exists with different attributes")
			}

			return nil
		}

		rec := &platformApplication{
			Platform:   platform,
			Name:       name,
			Attributes: stored,
			Secrets:    secrets,
			CreatedAt:  s.now().Unix(),
		}
		return storage.Write(tx, key, rec)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreatePlatformApplicationResponse{
		PlatformApplicationArn: new(appARN(m.Scope, platform, name)),
	}, nil
}

func (s *Service) DeletePlatformApplication(
	ctx context.Context,
	in *api.DeletePlatformApplicationInput,
) (*api.DeletePlatformApplicationOutput, error) {
	key, scope, platform, name, err := resolvePlatformApp(ctx, in.PlatformApplicationArn)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		tx.Delete(key)
		prefix := scope.Key(serviceName, kindEndpoint, platform+"/"+name+"/")
		for _, k := range tx.Keys(prefix) {
			tx.Delete(k)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeletePlatformApplicationOutput{}, nil
}

func (s *Service) GetPlatformApplicationAttributes(
	ctx context.Context,
	in *api.GetPlatformApplicationAttributesInput,
) (*api.GetPlatformApplicationAttributesResponse, error) {
	key, _, _, _, err := resolvePlatformApp(ctx, in.PlatformApplicationArn)
	if err != nil {
		return nil, err
	}

	var attrs map[string]string
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		rec, found, readErr := storage.Read[platformApplication](tx, key)
		if readErr != nil {
			return fmt.Errorf("decode platform application: %w", readErr)
		}

		if !found {
			return notFound("PlatformApplication")
		}

		attrs = rec.visibleAttributes()
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.GetPlatformApplicationAttributesResponse{Attributes: attrs}, nil
}

func (s *Service) SetPlatformApplicationAttributes(
	ctx context.Context,
	in *api.SetPlatformApplicationAttributesInput,
) (*api.SetPlatformApplicationAttributesOutput, error) {
	key, _, _, _, err := resolvePlatformApp(ctx, in.PlatformApplicationArn)
	if err != nil {
		return nil, err
	}

	stored, secrets, err := checkPlatformAppAttributes(in.Attributes)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rec, found, readErr := storage.Read[platformApplication](tx, key)
		if readErr != nil {
			return fmt.Errorf("decode platform application: %w", readErr)
		}

		if !found {
			return notFound("PlatformApplication")
		}

		if rec.Attributes == nil {
			rec.Attributes = map[string]string{}
		}

		if rec.Secrets == nil {
			rec.Secrets = map[string]string{}
		}

		for k, v := range stored {
			if v == "" && k != attrEnabled {
				delete(rec.Attributes, k)
				continue
			}

			rec.Attributes[k] = v
		}

		for k, v := range secrets {
			rec.Secrets[k] = v
		}

		return storage.Write(tx, key, rec)
	})
	if err != nil {
		return nil, err
	}

	return &api.SetPlatformApplicationAttributesOutput{}, nil
}

func (s *Service) ListPlatformApplications(
	ctx context.Context,
	in *api.ListPlatformApplicationsInput,
) (*api.ListPlatformApplicationsResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var items []api.PlatformApplication
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		recs, readErr := storage.Records[platformApplication](tx, m.Prefix(serviceName, kindApp))
		if readErr != nil {
			return readErr
		}

		for _, rec := range recs {
			items = append(items, api.PlatformApplication{
				PlatformApplicationArn: new(appARN(m.Scope, rec.Platform, rec.Name)),
				Attributes:             rec.visibleAttributes(),
			})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := common.Page(
		items,
		func(a api.PlatformApplication) string { return common.Value(a.PlatformApplicationArn) },
		in.NextToken,
		pageSize,
		invalid("NextToken"),
	)
	if err != nil {
		return nil, err
	}

	return &api.ListPlatformApplicationsResponse{PlatformApplications: selected, NextToken: next}, nil
}

func (s *Service) CreatePlatformEndpoint(
	ctx context.Context,
	in *api.CreatePlatformEndpointInput,
) (*api.CreateEndpointResponse, error) {
	aKey, scope, platform, appName, err := resolvePlatformApp(ctx, in.PlatformApplicationArn)
	if err != nil {
		return nil, err
	}

	token := common.Value(in.Token)
	if token == "" {
		return nil, invalid("Token is required")
	}

	customData := common.Value(in.CustomUserData)
	enabled := true
	for k, v := range in.Attributes {
		switch k {
		case attrEnabled:
			if v != valueTrue && v != valueFalse {
				return nil, invalid("Enabled must be true or false")
			}

			enabled = v == valueTrue
		case attrToken:
			if v == "" {
				return nil, invalid("Token is required")
			}

			token = v
		case attrCustomUserData:
			customData = v
		default:
			return nil, invalid("Attribute %s is not supported", k)
		}
	}

	var arn string
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, found := tx.Get(aKey); !found {
			return notFound("PlatformApplication")
		}

		prefix := scope.Key(serviceName, kindEndpoint, platform+"/"+appName+"/")
		existing, readErr := storage.Records[platformEndpoint](tx, prefix)
		if readErr != nil {
			return readErr
		}

		for _, ep := range existing {
			if ep.Token != token {
				continue
			}

			existingARN := endpointARN(scope, platform, appName, ep.ID)
			if ep.CustomUserData != customData || ep.Enabled != enabled {
				return invalid("Token Reason: Endpoint %s already exists with the same Token, "+
					"but different attributes.", existingARN)
			}

			arn = existingARN
			return nil
		}

		id := newID()
		ep := &platformEndpoint{
			ID:             id,
			Platform:       platform,
			AppName:        appName,
			Token:          token,
			CustomUserData: customData,
			Enabled:        enabled,
			CreatedAt:      s.now().Unix(),
		}
		arn = endpointARN(scope, platform, appName, id)
		return storage.Write(tx, endpointKey(scope, platform, appName, id), ep)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateEndpointResponse{EndpointArn: new(arn)}, nil
}

func (s *Service) DeleteEndpoint(
	ctx context.Context,
	in *api.DeleteEndpointInput,
) (*api.DeleteEndpointOutput, error) {
	key, _, err := resolvePlatformEndpoint(ctx, in.EndpointArn)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		tx.Delete(key)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteEndpointOutput{}, nil
}

func (s *Service) GetEndpointAttributes(
	ctx context.Context,
	in *api.GetEndpointAttributesInput,
) (*api.GetEndpointAttributesResponse, error) {
	key, _, err := resolvePlatformEndpoint(ctx, in.EndpointArn)
	if err != nil {
		return nil, err
	}

	var attrs map[string]string
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		ep, found, readErr := storage.Read[platformEndpoint](tx, key)
		if readErr != nil {
			return fmt.Errorf("decode platform endpoint: %w", readErr)
		}

		if !found {
			return notFound("Endpoint")
		}

		attrs = ep.visibleAttributes()
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.GetEndpointAttributesResponse{Attributes: attrs}, nil
}

func (s *Service) SetEndpointAttributes(
	ctx context.Context,
	in *api.SetEndpointAttributesInput,
) (*api.SetEndpointAttributesOutput, error) {
	key, _, err := resolvePlatformEndpoint(ctx, in.EndpointArn)
	if err != nil {
		return nil, err
	}

	for k, v := range in.Attributes {
		switch k {
		case attrEnabled:
			if v != valueTrue && v != valueFalse {
				return nil, invalid("Enabled must be true or false")
			}
		case attrToken:
			if v == "" {
				return nil, invalid("Token is required")
			}
		case attrCustomUserData:
		default:
			return nil, invalid("Attribute %s is not supported", k)
		}
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		ep, found, readErr := storage.Read[platformEndpoint](tx, key)
		if readErr != nil {
			return fmt.Errorf("decode platform endpoint: %w", readErr)
		}

		if !found {
			return notFound("Endpoint")
		}

		for k, v := range in.Attributes {
			switch k {
			case attrEnabled:
				ep.Enabled = v == valueTrue
			case attrToken:
				ep.Token = v
			case attrCustomUserData:
				ep.CustomUserData = v
			}
		}

		return storage.Write(tx, key, ep)
	})
	if err != nil {
		return nil, err
	}

	return &api.SetEndpointAttributesOutput{}, nil
}

func (s *Service) ListEndpointsByPlatformApplication(
	ctx context.Context,
	in *api.ListEndpointsByPlatformApplicationInput,
) (*api.ListEndpointsByPlatformApplicationResponse, error) {
	aKey, scope, platform, appName, err := resolvePlatformApp(ctx, in.PlatformApplicationArn)
	if err != nil {
		return nil, err
	}

	var items []api.Endpoint
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if _, found := tx.Get(aKey); !found {
			return notFound("PlatformApplication")
		}

		prefix := scope.Key(serviceName, kindEndpoint, platform+"/"+appName+"/")
		recs, readErr := storage.Records[platformEndpoint](tx, prefix)
		if readErr != nil {
			return readErr
		}

		for _, ep := range recs {
			items = append(items, api.Endpoint{
				EndpointArn: new(endpointARN(scope, platform, appName, ep.ID)),
				Attributes:  ep.visibleAttributes(),
			})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := common.Page(
		items,
		func(e api.Endpoint) string { return common.Value(e.EndpointArn) },
		in.NextToken,
		pageSize,
		invalid("NextToken"),
	)
	if err != nil {
		return nil, err
	}

	return &api.ListEndpointsByPlatformApplicationResponse{Endpoints: selected, NextToken: next}, nil
}

func (s *Service) checkPlatformEndpointDeliverable(ctx context.Context, ref *endpointRef) error {
	return s.store.View(ctx, func(tx storage.ReadTx) error {
		ep, found, readErr := storage.Read[platformEndpoint](
			tx, endpointKey(ref.scope, ref.platform, ref.appName, ref.id),
		)
		if readErr != nil {
			return fmt.Errorf("decode platform endpoint: %w", readErr)
		}

		if !found {
			return notFound("Endpoint")
		}

		app, appFound, appErr := storage.Read[platformApplication](tx, appKey(ref.scope, ref.platform, ref.appName))
		if appErr != nil {
			return fmt.Errorf("decode platform application: %w", appErr)
		}

		if appFound && !app.enabled() {
			return awserror.New("PlatformApplicationDisabledException", "Platform application is disabled")
		}

		if !ep.Enabled {
			return awserror.New("EndpointDisabledException", "Endpoint is disabled")
		}

		return nil
	})
}

// publishEndpoint publishes directly to a platform endpoint ARN via TargetArn.
func (s *Service) publishEndpoint(ctx context.Context, in *api.PublishInput) (*api.PublishResponse, error) {
	if in.MessageGroupId != nil || in.MessageDeduplicationId != nil {
		return nil, invalid("MessageGroupId and MessageDeduplicationId are only supported for FIFO topics")
	}

	_, ref, err := resolvePlatformEndpoint(ctx, in.TargetArn)
	if err != nil {
		return nil, err
	}

	textFor, err := checkMessage(&api.PublishBatchRequestEntry{
		Message:           in.Message,
		Subject:           in.Subject,
		MessageStructure:  in.MessageStructure,
		MessageAttributes: in.MessageAttributes,
	})
	if err != nil {
		return nil, err
	}

	if checkErr := s.checkPlatformEndpointDeliverable(ctx, &ref); checkErr != nil {
		return nil, checkErr
	}

	payload := textFor(ref.platform)
	if s.deliveries != nil {
		s.deliveries.Add(delivery.ViaSNS, "", common.Value(in.TargetArn), []byte(payload), nil)
	}

	return &api.PublishResponse{MessageId: new(newID())}, nil
}

// deliverApplication delivers a topic notification to a subscribed mobile
// platform endpoint, recording the push delivery locally and redriving to DLQ
// if the endpoint or platform application is disabled or missing.
func (s *Service) deliverApplication(
	ctx context.Context,
	sub *subscription,
	envelope *notification,
	e *api.PublishBatchRequestEntry,
	placed *sequenced,
) error {
	ref, err := parsePlatformEndpointARN(sub.Endpoint)
	if err == nil {
		err = s.checkPlatformEndpointDeliverable(ctx, &ref)
	}

	if s.deliveries != nil {
		s.deliveries.Add(delivery.ViaSNS, envelope.TopicArn, sub.Endpoint, []byte(envelope.Message), err)
	}

	s.notified(envelope.TopicArn, err)
	if err != nil {
		encoded, marshalErr := json.Marshal(envelope)
		if marshalErr != nil {
			return marshalErr
		}

		s.redrive(ctx, envelope.TopicArn, sub, string(encoded), e.MessageAttributes, placed, err)
	}

	return nil
}
