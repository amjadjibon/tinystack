package sesv2

import (
	"context"
	"net/mail"
	"regexp"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/sesv2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// identity is an email address or domain's record.
type identity struct {
	Tags             map[string]string
	Name             string
	Type             string
	ConfigurationSet string
	DkimTokens       []string
	Created          float64
}

// domainPattern is what a domain identity may be.
var domainPattern = regexp.MustCompile(`^([A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)+[A-Za-z]{2,63}$`)

func (i *identity) dkim() *api.DkimAttributes {
	if i.Type == identityEmail {
		return &api.DkimAttributes{SigningEnabled: new(false), Status: new(statusNotStarted)}
	}

	return &api.DkimAttributes{
		SigningEnabled: new(true), Status: new(statusSuccess), Tokens: i.DkimTokens,
		SigningAttributesOrigin: new("AWS_SES"), CurrentSigningKeyLength: new("RSA_2048_BIT"),
		NextSigningKeyLength: new("RSA_2048_BIT"), LastKeyGenerationTimestamp: new(i.Created),
	}
}

func loadIdentity(tx storage.ReadTx, scope request.Scope, name *string) (*identity, error) {
	i, ok, err := storage.Read[identity](tx, identityKey(scope, common.Value(name)))
	if err != nil || !ok {
		return nil, common.ErrorsOr(
			err,
			notFound(kindIdentity, "Email identity %s does not exist.", common.Value(name)),
		)
	}

	return i, nil
}

func (s *Service) CreateEmailIdentity(
	ctx context.Context,
	in *api.CreateEmailIdentityRequest,
) (*api.CreateEmailIdentityResponse, error) {
	name := common.Value(in.EmailIdentity)
	i := &identity{Name: name, Type: identityDomain, Created: common.Unix(s.now())}
	if strings.Contains(name, "@") {
		if a, err := mail.ParseAddress(name); err != nil || a.Address != name {
			return nil, badRequest("Invalid email address %s.", name)
		}

		i.Type = identityEmail
	} else {
		if !domainPattern.MatchString(name) {
			return nil, badRequest("Invalid domain name %s.", name)
		}

		for range 3 {
			i.DkimTokens = append(i.DkimTokens, common.RandomID(32, "abcdefghijklmnopqrstuvwxyz0123456789"))
		}
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	i.Tags, i.ConfigurationSet = tags, common.Value(in.ConfigurationSetName)
	err = s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		key := identityKey(scope, name)
		if _, found, readErr := storage.Read[identity](tx, key); readErr != nil || found {
			return common.ErrorsOr(readErr, alreadyExists("Email identity %s already exists.", name))
		}

		if i.ConfigurationSet != "" {
			if _, setErr := loadConfigSet(tx, scope, &i.ConfigurationSet); setErr != nil {
				return setErr
			}
		}

		return storage.Write(tx, key, i)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateEmailIdentityResponse{
		IdentityType: new(i.Type), VerifiedForSendingStatus: new(true), DkimAttributes: i.dkim(),
	}, nil
}

func (s *Service) GetEmailIdentity(
	ctx context.Context,
	in *api.GetEmailIdentityRequest,
) (*api.GetEmailIdentityResponse, error) {
	var i *identity
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		var err error
		i, err = loadIdentity(tx, scope, in.EmailIdentity)
		return err
	})
	if err != nil {
		return nil, err
	}

	out := &api.GetEmailIdentityResponse{
		IdentityType: new(i.Type), FeedbackForwardingStatus: new(true), VerifiedForSendingStatus: new(true),
		DkimAttributes: i.dkim(), Tags: tagList(i.Tags), VerificationStatus: new(statusSuccess),
	}
	if i.ConfigurationSet != "" {
		out.ConfigurationSetName = new(i.ConfigurationSet)
	}

	return out, nil
}

func (s *Service) ListEmailIdentities(
	ctx context.Context,
	in *api.ListEmailIdentitiesRequest,
) (*api.ListEmailIdentitiesResponse, error) {
	var all []*identity
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		var err error
		all, err = storage.Records[identity](tx, scope.Prefix(service, kindIdentity))
		return err
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := page(all, func(i *identity) string { return strings.ToLower(i.Name) }, in.NextToken,
		in.PageSize, 100, 1000)
	if err != nil {
		return nil, err
	}

	out := &api.ListEmailIdentitiesResponse{EmailIdentities: []api.IdentityInfo{}, NextToken: next}
	for _, i := range selected {
		out.EmailIdentities = append(out.EmailIdentities, api.IdentityInfo{
			IdentityType: new(i.Type), IdentityName: new(i.Name), SendingEnabled: new(true),
			VerificationStatus: new(statusSuccess),
		})
	}

	return out, nil
}

func (s *Service) DeleteEmailIdentity(
	ctx context.Context,
	in *api.DeleteEmailIdentityRequest,
) (*api.DeleteEmailIdentityResponse, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if _, err := loadIdentity(tx, scope, in.EmailIdentity); err != nil {
			return err
		}

		tx.Delete(identityKey(scope, *in.EmailIdentity))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteEmailIdentityResponse{}, nil
}

// verifiedIdentity returns the identity that lets address send: the address
// itself or its domain.
func verifiedIdentity(tx storage.ReadTx, scope request.Scope, address string) (*identity, error) {
	_, domain, _ := strings.Cut(address, "@")
	for _, name := range []string{address, domain} {
		i, ok, err := storage.Read[identity](tx, identityKey(scope, name))
		if err != nil || ok {
			return i, err
		}
	}

	return nil, messageRejected("Email address is not verified. The following identities failed the check in "+
		"region %s: %s", strings.ToUpper(scope.Region), address)
}
