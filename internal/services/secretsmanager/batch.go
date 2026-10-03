package secretsmanager

import (
	"context"
	"crypto/rand"
	"math/big"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/secretsmanager"
)

const (
	defaultPasswordLength = 32
	maxPasswordLength     = 4096
	maxBatchSecrets       = 20
	lowercase             = "abcdefghijklmnopqrstuvwxyz"
	uppercase             = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	numbers               = "0123456789"
	punctuation           = "!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~"
)

// GetRandomPassword generates a password from the character types the request
// leaves in, by default with at least one character of each.
func (s *Service) GetRandomPassword(
	_ context.Context,
	in *api.GetRandomPasswordRequest,
) (*api.GetRandomPasswordResponse, error) {
	length := int64(defaultPasswordLength)
	if in.PasswordLength != nil {
		if length = *in.PasswordLength; length < 1 || length > maxPasswordLength {
			return nil, invalidParameter("PasswordLength must be between 1 and %d.", maxPasswordLength)
		}
	}

	excluded := value0(in.ExcludeCharacters)
	var groups []string
	for _, g := range []struct {
		chars string
		skip  bool
	}{
		{lowercase, isSet(in.ExcludeLowercase)},
		{uppercase, isSet(in.ExcludeUppercase)},
		{numbers, isSet(in.ExcludeNumbers)},
		{punctuation, isSet(in.ExcludePunctuation)},
		{" ", !isSet(in.IncludeSpace)},
	} {
		chars := strings.Map(func(r rune) rune {
			if strings.ContainsRune(excluded, r) {
				return -1
			}

			return r
		}, g.chars)
		if !g.skip && chars != "" {
			groups = append(groups, chars)
		}
	}

	if len(groups) == 0 {
		return nil, invalidParameter("The password must be able to contain at least one character.")
	}

	required := in.RequireEachIncludedType == nil || *in.RequireEachIncludedType
	if required && int64(len(groups)) > length {
		return nil, invalidParameter("PasswordLength is too short to include every character type.")
	}

	all := strings.Join(groups, "")
	password := make([]byte, 0, length)
	if required {
		for _, g := range groups {
			password = append(password, pick(g))
		}
	}

	for int64(len(password)) < length {
		password = append(password, pick(all))
	}

	// Shuffle so the required characters are not always first.
	for i := len(password) - 1; i > 0; i-- {
		j := randomInt(i + 1)
		password[i], password[j] = password[j], password[i]
	}

	return &api.GetRandomPasswordResponse{RandomPassword: new(string(password))}, nil
}

func isSet(p *bool) bool { return p != nil && *p }

func randomInt(n int) int {
	i, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		panic(err) // crypto/rand does not fail on supported platforms.
	}

	return int(i.Int64())
}

func pick(chars string) byte { return chars[randomInt(len(chars))] }

// BatchGetSecretValue returns the current values of up to 20 secrets, named
// by SecretIdList or matched by Filters (paged as ListSecrets pages). A secret
// that cannot be read is reported in Errors instead of failing the request.
func (s *Service) BatchGetSecretValue(
	ctx context.Context,
	in *api.BatchGetSecretValueRequest,
) (*api.BatchGetSecretValueResponse, error) {
	switch {
	case (len(in.SecretIdList) == 0) == (len(in.Filters) == 0):
		return nil, invalidParameter("Specify either SecretIdList or Filters.")
	case len(in.SecretIdList) > maxBatchSecrets:
		return nil, invalidParameter("SecretIdList can have at most %d secrets.", maxBatchSecrets)
	case len(in.SecretIdList) > 0 && (in.MaxResults != nil || in.NextToken != nil):
		return nil, invalidParameter("MaxResults and NextToken can only be used with Filters.")
	case in.MaxResults != nil && (*in.MaxResults < 1 || *in.MaxResults > maxBatchSecrets):
		return nil, invalidParameter("MaxResults must be between 1 and %d.", maxBatchSecrets)
	}

	out := &api.BatchGetSecretValueResponse{SecretValues: []api.SecretValueEntry{}}
	ids := in.SecretIdList
	if len(in.Filters) > 0 {
		limit := int64(maxBatchSecrets)
		if in.MaxResults != nil {
			limit = *in.MaxResults
		}

		listed, err := s.ListSecrets(ctx, &api.ListSecretsRequest{
			Filters: in.Filters, MaxResults: &limit, NextToken: in.NextToken,
		})
		if err != nil {
			return nil, err
		}

		for i := range listed.SecretList {
			ids = append(ids, value0(listed.SecretList[i].ARN))
		}

		out.NextToken = listed.NextToken
	}

	for _, id := range ids {
		v, err := s.GetSecretValue(ctx, &api.GetSecretValueRequest{SecretId: new(id)})
		if err != nil {
			e := awserror.From(err)
			out.Errors = append(out.Errors, api.APIErrorType{
				SecretId: new(id), ErrorCode: new(e.Code), Message: new(e.Message),
			})

			continue
		}

		out.SecretValues = append(out.SecretValues, api.SecretValueEntry{
			ARN: v.ARN, Name: v.Name, VersionId: v.VersionId, SecretBinary: v.SecretBinary,
			SecretString: v.SecretString, VersionStages: v.VersionStages, CreatedDate: v.CreatedDate,
		})
	}

	return out, nil
}
