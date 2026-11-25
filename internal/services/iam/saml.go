package iam

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"io"
	"regexp"
	"strings"
	"time"
	"uuid"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/iam"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const maxSAMLMetadata = 10_000_000

var samlProviderName = regexp.MustCompile(`^[\w.-]{1,128}$`)

type samlProvider struct {
	Tags             map[string]string `json:",omitempty"`
	Name             string
	UUID             string
	MetadataDocument string
	CreatedAt        int64
	ValidUntil       int64
}

func samlProviderARN(account, name string) string {
	return arn(account, kindSAML+"/"+name)
}

func parseSAMLProviderARN(account string, raw *string) (string, error) {
	if err := required("SAMLProviderArn", raw); err != nil {
		return "", err
	}

	name, ok := strings.CutPrefix(*raw, "arn:aws:iam::"+account+":"+kindSAML+"/")
	if !ok || !samlProviderName.MatchString(name) {
		return "", invalidInput("Invalid SAMLProviderArn %s", *raw)
	}

	return name, nil
}

// parseSAMLMetadata validates that doc is non-empty well-formed XML and
// extracts a ValidUntil timestamp from either the root validUntil attribute or
// an embedded X509Certificate element.
func parseSAMLMetadata(doc *string, now time.Time) (int64, error) {
	if err := required("SAMLMetadataDocument", doc); err != nil {
		return 0, err
	}

	raw := strings.TrimSpace(*doc)
	if raw == "" || len(*doc) > maxSAMLMetadata {
		return 0, invalidInput("SAMLMetadataDocument must not be empty")
	}

	dec := xml.NewDecoder(strings.NewReader(raw))
	hasElement := false
	var validUntil time.Time
	inCert := false
	var certText strings.Builder

	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return 0, invalidInput("Invalid SAMLMetadataDocument XML: %v", err)
		}

		switch t := tok.(type) {
		case xml.StartElement:
			hasElement = true
			for _, attr := range t.Attr {
				if attr.Name.Local == "validUntil" && validUntil.IsZero() {
					if parsed, parseErr := time.Parse(time.RFC3339, attr.Value); parseErr == nil {
						validUntil = parsed.UTC()
					}
				}
			}

			if t.Name.Local == "X509Certificate" {
				inCert = true
				certText.Reset()
			}
		case xml.CharData:
			if inCert {
				certText.Write(t)
			}
		case xml.EndElement:
			if t.Name.Local == "X509Certificate" && inCert {
				inCert = false
				if validUntil.IsZero() {
					cleaned := strings.Join(strings.Fields(certText.String()), "")
					if der, decErr := base64.StdEncoding.DecodeString(cleaned); decErr == nil {
						if cert, certErr := x509.ParseCertificate(der); certErr == nil {
							validUntil = cert.NotAfter.UTC()
						}
					}
				}
			}
		}
	}

	if !hasElement {
		return 0, invalidInput("SAMLMetadataDocument must contain valid XML")
	}

	if validUntil.IsZero() {
		validUntil = now.UTC().AddDate(100, 0, 0)
	}

	return validUntil.UnixMilli(), nil
}

func loadSAMLProvider(tx storage.ReadTx, account string, rawARN *string) (*samlProvider, string, error) {
	name, err := parseSAMLProviderARN(account, rawARN)
	if err != nil {
		return nil, "", err
	}

	k := key(account, kindSAML, name)
	p, ok, err := storage.Read[samlProvider](tx, k)
	if err == nil && !ok {
		err = noSuchEntity("SAML Provider not found for arn %s", *rawARN)
	}

	return p, k, err
}

func (s *Service) updateSAMLProvider(
	ctx context.Context,
	rawARN *string,
	fn func(p *samlProvider) error,
) (string, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return "", err
	}

	var canonicalARN string
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		p, k, loadErr := loadSAMLProvider(tx, m.AccountID, rawARN)
		if loadErr != nil {
			return loadErr
		}

		if fnErr := fn(p); fnErr != nil {
			return fnErr
		}

		canonicalARN = samlProviderARN(m.AccountID, p.Name)
		return storage.Write(tx, k, p)
	})
	return canonicalARN, err
}

func (s *Service) CreateSAMLProvider(
	ctx context.Context,
	in *api.CreateSAMLProviderRequest,
) (*api.CreateSAMLProviderResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	if guardErr := checkName("Name", in.Name, samlProviderName); guardErr != nil {
		return nil, guardErr
	}

	now := s.now()
	validUntil, err := parseSAMLMetadata(in.SAMLMetadataDocument, now)
	if err != nil {
		return nil, err
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	if guardErr := tagError(tagging.Check(tags)); guardErr != nil {
		return nil, guardErr
	}

	p := &samlProvider{
		Tags:             tags,
		Name:             *in.Name,
		UUID:             uuid.NewV4().String(),
		MetadataDocument: *in.SAMLMetadataDocument,
		CreatedAt:        now.UnixMilli(),
		ValidUntil:       validUntil,
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		k := key(m.AccountID, kindSAML, p.Name)
		if _, exists := tx.Get(k); exists {
			return alreadyExists("SAML Provider with name %s already exists.", p.Name)
		}

		return storage.Write(tx, k, p)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateSAMLProviderResponse{
		SAMLProviderArn: new(samlProviderARN(m.AccountID, p.Name)),
		Tags:            tagList(tags),
	}, nil
}

func (s *Service) GetSAMLProvider(
	ctx context.Context,
	in *api.GetSAMLProviderRequest,
) (*api.GetSAMLProviderResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	var p *samlProvider
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var loadErr error
		p, _, loadErr = loadSAMLProvider(tx, m.AccountID, in.SAMLProviderArn)
		return loadErr
	})
	if err != nil {
		return nil, err
	}

	return &api.GetSAMLProviderResponse{
		SAMLProviderUUID:     new(p.UUID),
		SAMLMetadataDocument: new(p.MetadataDocument),
		CreateDate:           millis(p.CreatedAt),
		ValidUntil:           millis(p.ValidUntil),
		Tags:                 tagList(p.Tags),
	}, nil
}

func (s *Service) UpdateSAMLProvider(
	ctx context.Context,
	in *api.UpdateSAMLProviderRequest,
) (*api.UpdateSAMLProviderResponse, error) {
	validUntil, err := parseSAMLMetadata(in.SAMLMetadataDocument, s.now())
	if err != nil {
		return nil, err
	}

	canonicalARN, err := s.updateSAMLProvider(ctx, in.SAMLProviderArn, func(p *samlProvider) error {
		p.MetadataDocument = *in.SAMLMetadataDocument
		p.ValidUntil = validUntil
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateSAMLProviderResponse{SAMLProviderArn: new(canonicalARN)}, nil
}

func (s *Service) DeleteSAMLProvider(
	ctx context.Context,
	in *api.DeleteSAMLProviderRequest,
) (*api.DeleteSAMLProviderOutput, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		_, k, loadErr := loadSAMLProvider(tx, m.AccountID, in.SAMLProviderArn)
		if loadErr != nil {
			return loadErr
		}

		tx.Delete(k)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteSAMLProviderOutput{}, nil
}

func (s *Service) ListSAMLProviders(
	ctx context.Context,
	_ *api.ListSAMLProvidersRequest,
) (*api.ListSAMLProvidersResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.ListSAMLProvidersResponse{SAMLProviderList: []api.SAMLProviderListEntry{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		providers, listErr := list[samlProvider](tx, m.AccountID, kindSAML)
		for _, p := range providers {
			out.SAMLProviderList = append(out.SAMLProviderList, api.SAMLProviderListEntry{
				Arn:        new(samlProviderARN(m.AccountID, p.Name)),
				CreateDate: millis(p.CreatedAt),
				ValidUntil: millis(p.ValidUntil),
			})
		}

		return listErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) TagSAMLProvider(
	ctx context.Context,
	in *api.TagSAMLProviderRequest,
) (*api.TagSAMLProviderOutput, error) {
	add, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	_, err = s.updateSAMLProvider(ctx, in.SAMLProviderArn, func(p *samlProvider) error {
		merged, mergeErr := tagging.Merge(p.Tags, add)
		if mergeErr != nil {
			return tagError(mergeErr)
		}

		p.Tags = merged
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.TagSAMLProviderOutput{}, nil
}

func (s *Service) UntagSAMLProvider(
	ctx context.Context,
	in *api.UntagSAMLProviderRequest,
) (*api.UntagSAMLProviderOutput, error) {
	_, err := s.updateSAMLProvider(ctx, in.SAMLProviderArn, func(p *samlProvider) error {
		p.Tags = tagging.Remove(p.Tags, in.TagKeys)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagSAMLProviderOutput{}, nil
}

func (s *Service) ListSAMLProviderTags(
	ctx context.Context,
	in *api.ListSAMLProviderTagsRequest,
) (*api.ListSAMLProviderTagsResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.ListSAMLProviderTagsResponse{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		p, _, loadErr := loadSAMLProvider(tx, m.AccountID, in.SAMLProviderArn)
		if loadErr != nil {
			return loadErr
		}

		out.Tags, out.IsTruncated, out.Marker, loadErr = page(
			tagList(p.Tags), func(t api.Tag) string { return *t.Key }, in.Marker, in.MaxItems)
		return loadErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
