package cognitoidp

import (
	"context"
	"crypto/hmac"
	"regexp"
	"strconv"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/cognitoidp"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const maxClientSecrets = 2

var clientSecretPattern = regexp.MustCompile(`^[\w+]+$`)

type clientSecret struct {
	Created time.Time
	ID      string
}

func secretPrefix(scope request.Scope, pool, clientID string) string {
	return scope.Prefix(service, "clientsecret") + pool + "/" + clientID + "/"
}

func (c *client) secrets() []string {
	if c.Secret == "" {
		return nil
	}

	out := []string{c.Secret}
	if c.ExtraSecret != "" {
		out = append(out, c.ExtraSecret)
	}

	return out
}

func (c *client) acceptsSecret(secret string) bool {
	if c.Secret == "" {
		return true
	}

	for _, active := range c.secrets() {
		if hmac.Equal([]byte(active), []byte(secret)) {
			return true
		}
	}

	return false
}

func (r *clientSecret) output() api.ClientSecretDescriptorType {
	return api.ClientSecretDescriptorType{
		ClientSecretId:         new(r.ID),
		ClientSecretCreateDate: common.SecondsMilli(r.Created),
	}
}

// ensureSecretRecord upgrades records created before secret rotation was supported.
func ensureSecretRecord(tx storage.WriteTx, scope request.Scope, p *pool, c *client) error {
	if c.Secret == "" || c.SecretID != "" {
		return nil
	}

	c.SecretID = c.ID + "--" + strconv.FormatInt(c.Created.UnixMilli(), 10)
	if err := storage.Write(tx, secretPrefix(scope, p.ID, c.ID)+c.SecretID,
		clientSecret{ID: c.SecretID, Created: c.Created}); err != nil {
		return err
	}

	return storage.Write(tx, clientKey(scope, p.ID, c.ID), c)
}

func (s *Service) AddUserPoolClientSecret(
	ctx context.Context, in *api.AddUserPoolClientSecretRequest,
) (*api.AddUserPoolClientSecretResponse, error) {
	if in.ClientSecret != nil && (len(*in.ClientSecret) < 24 || len(*in.ClientSecret) > 64 ||
		!clientSecretPattern.MatchString(*in.ClientSecret)) {
		return nil, invalidParameter("ClientSecret must be 24 to 64 characters matching ^[\\w+]+$.")
	}

	var descriptor api.ClientSecretDescriptorType
	err := s.byAdminClient(ctx, in.UserPoolId, in.ClientId,
		func(tx storage.WriteTx, scope request.Scope, p *pool, c *client) error {
			if c.Secret == "" {
				return invalidParameter("Cannot add a secret to a public app client.")
			}

			if c.ExtraSecret != "" {
				return awserror.New("LimitExceededException", "An app client can have at most two secrets.")
			}

			if err := ensureSecretRecord(tx, scope, p, c); err != nil {
				return err
			}

			secret := common.Value(in.ClientSecret)
			if in.ClientSecret == nil {
				secret = common.RandomID(51, lower)
			}

			now := s.now().UTC()
			id := c.ID + "--" + strconv.FormatInt(now.UnixMilli(), 10)
			if id == c.SecretID {
				now = now.Add(time.Millisecond)
				id = c.ID + "--" + strconv.FormatInt(now.UnixMilli(), 10)
			}

			record := clientSecret{ID: id, Created: now}
			if err := storage.Write(tx, secretPrefix(scope, p.ID, c.ID)+id, record); err != nil {
				return err
			}

			c.ExtraSecret, c.ExtraSecretID, c.Modified = secret, id, s.now().UTC()
			descriptor = record.output()
			if in.ClientSecret == nil {
				descriptor.ClientSecretValue = new(secret)
			}

			return storage.Write(tx, clientKey(scope, p.ID, c.ID), c)
		})
	if err != nil {
		return nil, err
	}

	return &api.AddUserPoolClientSecretResponse{ClientSecretDescriptor: &descriptor}, nil
}

func (s *Service) ListUserPoolClientSecrets(
	ctx context.Context, in *api.ListUserPoolClientSecretsRequest,
) (*api.ListUserPoolClientSecretsResponse, error) {
	out := &api.ListUserPoolClientSecretsResponse{ClientSecrets: []api.ClientSecretDescriptorType{}}
	err := s.byAdminClient(ctx, in.UserPoolId, in.ClientId,
		func(tx storage.WriteTx, scope request.Scope, p *pool, c *client) error {
			if err := ensureSecretRecord(tx, scope, p, c); err != nil {
				return err
			}

			records, err := storage.Records[clientSecret](tx, secretPrefix(scope, p.ID, c.ID))
			if err != nil {
				return err
			}

			selected, next, err := common.Page(records, func(r *clientSecret) string { return r.ID },
				in.NextToken, maxClientSecrets, errPageToken)
			if err != nil {
				return err
			}

			out.NextToken = next
			for _, record := range selected {
				out.ClientSecrets = append(out.ClientSecrets, record.output())
			}

			return nil
		})
	return out, err
}

func (s *Service) DeleteUserPoolClientSecret(
	ctx context.Context, in *api.DeleteUserPoolClientSecretRequest,
) (*api.DeleteUserPoolClientSecretResponse, error) {
	if len(common.Value(in.ClientSecretId)) < 1 || len(common.Value(in.ClientSecretId)) > 128 {
		return nil, invalidParameter("ClientSecretId must be 1 to 128 characters.")
	}

	err := s.byAdminClient(ctx, in.UserPoolId, in.ClientId,
		func(tx storage.WriteTx, scope request.Scope, p *pool, c *client) error {
			if err := ensureSecretRecord(tx, scope, p, c); err != nil {
				return err
			}

			id := *in.ClientSecretId
			if id != c.SecretID && id != c.ExtraSecretID {
				return notFound("Client secret %s does not exist.", id)
			}

			if c.ExtraSecret == "" {
				return invalidParameter("Cannot delete the last app client secret.")
			}

			if id == c.SecretID {
				c.Secret, c.SecretID = c.ExtraSecret, c.ExtraSecretID
			}

			c.ExtraSecret, c.ExtraSecretID, c.Modified = "", "", s.now().UTC()
			tx.Delete(secretPrefix(scope, p.ID, c.ID) + id)
			return storage.Write(tx, clientKey(scope, p.ID, c.ID), c)
		})
	if err != nil {
		return nil, err
	}

	return &api.DeleteUserPoolClientSecretResponse{}, nil
}
