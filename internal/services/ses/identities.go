package ses

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"maps"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ses"
	v2 "github.com/amjadjibon/tinystack/internal/aws/generated/sesv2"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/sesv2"
)

const (
	typeEmail  = "EmailAddress"
	typeDomain = "Domain"
)

// token is the verification token of a domain identity, which depends on the
// domain alone.
func token(domain string) string {
	sum := sha256.Sum256([]byte("tinystack-ses-verification:" + strings.ToLower(domain)))
	return base64.RawURLEncoding.EncodeToString(sum[:30])
}

// ensure creates an identity unless it exists, as verifying twice is allowed
// in API v1, and returns it.
func (s *Service) ensure(ctx context.Context, name *string) (*v2.GetEmailIdentityResponse, error) {
	_, err := s.v2.CreateEmailIdentity(ctx, &v2.CreateEmailIdentityRequest{EmailIdentity: name})
	if err != nil && awserror.From(err).Code != "AlreadyExistsException" {
		return nil, translate(err, "InvalidParameterValue")
	}

	out, err := s.v2.GetEmailIdentity(ctx, &v2.GetEmailIdentityRequest{EmailIdentity: name})
	return out, translate(err, "InvalidParameterValue")
}

// verifyAddress verifies an email address, which needs to be one.
func (s *Service) verifyAddress(ctx context.Context, address *string) error {
	if !strings.Contains(common.Value(address), "@") {
		return invalid("%s is not an email address.", common.Value(address))
	}

	_, err := s.ensure(ctx, address)
	return err
}

func (s *Service) VerifyEmailIdentity(
	ctx context.Context,
	in *api.VerifyEmailIdentityRequest,
) (*api.VerifyEmailIdentityResponse, error) {
	return &api.VerifyEmailIdentityResponse{}, s.verifyAddress(ctx, in.EmailAddress)
}

func (s *Service) VerifyEmailAddress(
	ctx context.Context,
	in *api.VerifyEmailAddressRequest,
) (*api.VerifyEmailAddressOutput, error) {
	return &api.VerifyEmailAddressOutput{}, s.verifyAddress(ctx, in.EmailAddress)
}

// domain returns the domain identity name, which cannot be an address.
func domain(name *string) (*string, error) {
	if strings.Contains(common.Value(name), "@") {
		return nil, invalid("%s is not a domain.", common.Value(name))
	}

	return name, nil
}

func (s *Service) VerifyDomainIdentity(
	ctx context.Context,
	in *api.VerifyDomainIdentityRequest,
) (*api.VerifyDomainIdentityResponse, error) {
	name, err := domain(in.Domain)
	if err != nil {
		return nil, err
	}

	if _, err = s.ensure(ctx, name); err != nil {
		return nil, err
	}

	return &api.VerifyDomainIdentityResponse{VerificationToken: new(token(*name))}, nil
}

func (s *Service) VerifyDomainDkim(
	ctx context.Context,
	in *api.VerifyDomainDkimRequest,
) (*api.VerifyDomainDkimResponse, error) {
	name, err := domain(in.Domain)
	if err != nil {
		return nil, err
	}

	i, err := s.ensure(ctx, name)
	if err != nil {
		return nil, err
	}

	if i.DkimAttributes == nil || len(i.DkimAttributes.Tokens) == 0 {
		return nil, invalid("%s is not a domain identity.", *name)
	}

	return &api.VerifyDomainDkimResponse{DkimTokens: i.DkimAttributes.Tokens}, nil
}

// identities lists the caller's identities, filtered by v1 identity type.
func (s *Service) identities(ctx context.Context, kind string) ([]string, error) {
	var names []string
	var next *string
	for {
		out, err := s.v2.ListEmailIdentities(
			ctx,
			&v2.ListEmailIdentitiesRequest{NextToken: next, PageSize: new(int64(1000))},
		)
		if err != nil {
			return nil, err
		}

		for _, i := range out.EmailIdentities {
			if kind == "" || (kind == typeEmail) == (common.Value(i.IdentityType) == "EMAIL_ADDRESS") {
				names = append(names, common.Value(i.IdentityName))
			}
		}

		if next = out.NextToken; next == nil {
			return names, nil
		}
	}
}

func (s *Service) ListIdentities(
	ctx context.Context,
	in *api.ListIdentitiesRequest,
) (*api.ListIdentitiesResponse, error) {
	kind := common.Value(in.IdentityType)
	if kind != "" && kind != typeEmail && kind != typeDomain {
		return nil, invalid("IdentityType must be %s or %s.", typeEmail, typeDomain)
	}

	size := int(*common.OrElse(in.MaxItems, new(int64(100))))
	if size < 1 || size > 1000 {
		return nil, invalid("MaxItems must be between 1 and 1000.")
	}

	names, err := s.identities(ctx, kind)
	if err != nil {
		return nil, translate(err, "InvalidParameterValue")
	}

	selected, next, err := common.Page(names, strings.ToLower, in.NextToken, size,
		invalid("Invalid NextToken."))
	if err != nil {
		return nil, err
	}

	return &api.ListIdentitiesResponse{Identities: append([]string{}, selected...), NextToken: next}, nil
}

func (s *Service) GetIdentityVerificationAttributes(
	ctx context.Context,
	in *api.GetIdentityVerificationAttributesRequest,
) (*api.GetIdentityVerificationAttributesResponse, error) {
	out := map[string]api.IdentityVerificationAttributes{}
	for _, name := range in.Identities {
		i, err := s.v2.GetEmailIdentity(ctx, &v2.GetEmailIdentityRequest{EmailIdentity: new(name)})
		if sesv2.Missing(err) != "" {
			continue
		}

		if err != nil {
			return nil, translate(err, "InvalidParameterValue")
		}

		attributes := api.IdentityVerificationAttributes{VerificationStatus: new("Success")}
		if common.Value(i.IdentityType) == "DOMAIN" {
			attributes.VerificationToken = new(token(name))
		}

		out[name] = attributes
	}

	return &api.GetIdentityVerificationAttributesResponse{VerificationAttributes: out}, nil
}

// DeleteIdentity deletes an identity, which API v1 does whether it exists or
// not.
func (s *Service) DeleteIdentity(
	ctx context.Context,
	in *api.DeleteIdentityRequest,
) (*api.DeleteIdentityResponse, error) {
	_, err := s.v2.DeleteEmailIdentity(ctx, &v2.DeleteEmailIdentityRequest{EmailIdentity: in.Identity})
	if sesv2.Missing(err) == "" && err != nil {
		return nil, translate(err, "InvalidParameterValue")
	}

	return &api.DeleteIdentityResponse{}, nil
}

func (s *Service) ListVerifiedEmailAddresses(
	ctx context.Context,
	_ *api.ListVerifiedEmailAddressesInput,
) (*api.ListVerifiedEmailAddressesResponse, error) {
	names, err := s.identities(ctx, typeEmail)
	if err != nil {
		return nil, translate(err, "InvalidParameterValue")
	}

	return &api.ListVerifiedEmailAddressesResponse{VerifiedEmailAddresses: append([]string{}, names...)}, nil
}

func (s *Service) GetSendQuota(ctx context.Context, _ *api.GetSendQuotaInput) (*api.GetSendQuotaResponse, error) {
	account, err := s.v2.GetAccount(ctx, &v2.GetAccountRequest{})
	if err != nil {
		return nil, translate(err, "InvalidParameterValue")
	}

	q := common.OrElse(account.SendQuota, &v2.SendQuota{})
	return &api.GetSendQuotaResponse{
		Max24HourSend: q.Max24HourSend, MaxSendRate: q.MaxSendRate, SentLast24Hours: q.SentLast24Hours,
	}, nil
}

// GetSendStatistics reports one data point for each hour that sent messages.
func (s *Service) GetSendStatistics(
	ctx context.Context,
	_ *api.GetSendStatisticsInput,
) (*api.GetSendStatisticsResponse, error) {
	hours, err := s.v2.SentByHour(ctx)
	if err != nil {
		return nil, translate(err, "InvalidParameterValue")
	}

	out := &api.GetSendStatisticsResponse{SendDataPoints: []api.SendDataPoint{}}
	for _, hour := range slices.Sorted(maps.Keys(hours)) {
		out.SendDataPoints = append(out.SendDataPoints, api.SendDataPoint{
			Timestamp: timestamp(float64(hour * 3600)), DeliveryAttempts: new(hours[hour]),
			Bounces: new(int64(0)), Complaints: new(int64(0)), Rejects: new(int64(0)),
		})
	}

	return out, nil
}
