package iam

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/iam"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

type serverCertificate struct {
	Tags             map[string]string `json:",omitempty"`
	Name             string
	ID               string
	Path             string
	CertificateBody  string
	CertificateChain string `json:",omitempty"`
	UploadDate       int64
	Expiration       int64
}

func serverCertARN(account string, c *serverCertificate) string {
	return arn(account, kindServerCert+c.Path+c.Name)
}

func serverCertMetadata(account string, c *serverCertificate) *api.ServerCertificateMetadata {
	return &api.ServerCertificateMetadata{
		Path:                  new(c.Path),
		ServerCertificateName: new(c.Name),
		ServerCertificateId:   new(c.ID),
		Arn:                   new(serverCertARN(account, c)),
		UploadDate:            millis(c.UploadDate),
		Expiration:            millis(c.Expiration),
	}
}

func parseServerCertificatePair(certBody, privateKey, certChain string) (time.Time, error) {
	block, _ := pem.Decode([]byte(certBody))
	if block == nil {
		return time.Time{}, awserror.New("MalformedCertificateException", "Unable to parse certificate PEM.")
	}

	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return time.Time{}, awserror.New("MalformedCertificateException", "Unable to parse certificate: "+err.Error())
	}

	if certChain != "" {
		rest := []byte(certChain)
		found := false
		for strings.TrimSpace(string(rest)) != "" {
			var chainBlock *pem.Block
			chainBlock, rest = pem.Decode(rest)
			if chainBlock == nil {
				return time.Time{}, awserror.New(
					"MalformedCertificateException",
					"Unable to parse certificate chain PEM.",
				)
			}

			if _, chainErr := x509.ParseCertificate(chainBlock.Bytes); chainErr != nil {
				return time.Time{}, awserror.New(
					"MalformedCertificateException",
					"Unable to parse certificate chain: "+chainErr.Error(),
				)
			}

			found = true
		}

		if !found {
			return time.Time{}, awserror.New("MalformedCertificateException", "Certificate chain is empty.")
		}
	}

	keyBlock, _ := pem.Decode([]byte(privateKey))
	if keyBlock == nil {
		return time.Time{}, awserror.New("MalformedCertificateException", "Unable to parse private key PEM.")
	}

	if _, err = tls.X509KeyPair([]byte(certBody), []byte(privateKey)); err != nil {
		return time.Time{}, awserror.New("KeyPairMismatchException", "Certificate and private key do not match.")
	}

	return leaf.NotAfter.UTC(), nil
}

func loadServerCertificate(tx storage.ReadTx, account string, name *string) (*serverCertificate, string, error) {
	if err := checkName("serverCertificateName", name, policyName); err != nil {
		return nil, "", err
	}

	ck := key(account, kindServerCert, *name)
	c, ok, err := storage.Read[serverCertificate](tx, ck)
	if err == nil && !ok {
		err = noSuchEntity("The Server Certificate with name %s cannot be found.", *name)
	}

	return c, ck, err
}

func (s *Service) UploadServerCertificate(
	ctx context.Context,
	in *api.UploadServerCertificateRequest,
) (*api.UploadServerCertificateResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	if guardErr := checkName("serverCertificateName", in.ServerCertificateName, policyName); guardErr != nil {
		return nil, guardErr
	}

	path, err := checkPath(in.Path, rolePath)
	if err != nil {
		return nil, err
	}

	if guardErr := required("certificateBody", in.CertificateBody); guardErr != nil {
		return nil, guardErr
	}

	if guardErr := required("privateKey", in.PrivateKey); guardErr != nil {
		return nil, guardErr
	}

	expiration, err := parseServerCertificatePair(
		*in.CertificateBody, *in.PrivateKey, common.Value(in.CertificateChain),
	)
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

	c := &serverCertificate{
		Tags:             tags,
		Name:             *in.ServerCertificateName,
		ID:               newID("ASCA"),
		Path:             path,
		CertificateBody:  *in.CertificateBody,
		CertificateChain: common.Value(in.CertificateChain),
		UploadDate:       s.now().UnixMilli(),
		Expiration:       expiration.UnixMilli(),
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		ck := key(m.AccountID, kindServerCert, c.Name)
		if _, exists := tx.Get(ck); exists {
			return alreadyExists("The Server Certificate with name %s already exists.", c.Name)
		}

		return storage.Write(tx, ck, c)
	})
	if err != nil {
		return nil, err
	}

	return &api.UploadServerCertificateResponse{
		ServerCertificateMetadata: serverCertMetadata(m.AccountID, c),
		Tags:                      tagList(c.Tags),
	}, nil
}

func (s *Service) GetServerCertificate(
	ctx context.Context,
	in *api.GetServerCertificateRequest,
) (*api.GetServerCertificateResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	var c *serverCertificate
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var loadErr error
		c, _, loadErr = loadServerCertificate(tx, m.AccountID, in.ServerCertificateName)
		return loadErr
	})
	if err != nil {
		return nil, err
	}

	out := &api.ServerCertificate{
		ServerCertificateMetadata: serverCertMetadata(m.AccountID, c),
		CertificateBody:           new(c.CertificateBody),
		Tags:                      tagList(c.Tags),
	}
	if c.CertificateChain != "" {
		out.CertificateChain = new(c.CertificateChain)
	}

	return &api.GetServerCertificateResponse{ServerCertificate: out}, nil
}

func (s *Service) UpdateServerCertificate(
	ctx context.Context,
	in *api.UpdateServerCertificateRequest,
) (*api.UpdateServerCertificateOutput, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	if in.NewServerCertificateName != nil {
		if guardErr := checkName("newServerCertificateName", in.NewServerCertificateName, policyName); guardErr != nil {
			return nil, guardErr
		}
	}

	var path string
	if in.NewPath != nil {
		if path, err = checkPath(in.NewPath, rolePath); err != nil {
			return nil, err
		}
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		c, ck, loadErr := loadServerCertificate(tx, m.AccountID, in.ServerCertificateName)
		if loadErr != nil {
			return loadErr
		}

		if in.NewPath != nil {
			c.Path = path
		}

		if in.NewServerCertificateName != nil {
			target := key(m.AccountID, kindServerCert, *in.NewServerCertificateName)
			if _, exists := tx.Get(target); exists && target != ck {
				return alreadyExists(
					"The Server Certificate with name %s already exists.",
					*in.NewServerCertificateName,
				)
			}

			tx.Delete(ck)
			c.Name, ck = *in.NewServerCertificateName, target
		}

		return storage.Write(tx, ck, c)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateServerCertificateOutput{}, nil
}

func (s *Service) DeleteServerCertificate(
	ctx context.Context,
	in *api.DeleteServerCertificateRequest,
) (*api.DeleteServerCertificateOutput, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		_, ck, loadErr := loadServerCertificate(tx, m.AccountID, in.ServerCertificateName)
		if loadErr != nil {
			return loadErr
		}

		tx.Delete(ck)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteServerCertificateOutput{}, nil
}

func (s *Service) ListServerCertificates(
	ctx context.Context,
	in *api.ListServerCertificatesRequest,
) (*api.ListServerCertificatesResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	if in.PathPrefix != nil && !pathPrefix.MatchString(*in.PathPrefix) {
		return nil, validation("The specified value for pathPrefix is invalid.")
	}

	var matched []*serverCertificate
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		all, listErr := list[serverCertificate](tx, m.AccountID, kindServerCert)
		for _, c := range all {
			if strings.HasPrefix(c.Path, common.Value(in.PathPrefix)) {
				matched = append(matched, c)
			}
		}

		return listErr
	})
	if err != nil {
		return nil, err
	}

	selected, truncated, next, err := page(
		matched,
		func(c *serverCertificate) string { return strings.ToLower(c.Name) },
		in.Marker,
		in.MaxItems,
	)
	if err != nil {
		return nil, err
	}

	out := &api.ListServerCertificatesResponse{
		ServerCertificateMetadataList: []api.ServerCertificateMetadata{},
		IsTruncated:                   truncated,
		Marker:                        next,
	}
	for _, c := range selected {
		out.ServerCertificateMetadataList = append(
			out.ServerCertificateMetadataList,
			*serverCertMetadata(m.AccountID, c),
		)
	}

	return out, nil
}

func (s *Service) TagServerCertificate(
	ctx context.Context,
	in *api.TagServerCertificateRequest,
) (*api.TagServerCertificateOutput, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	add, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		c, ck, loadErr := loadServerCertificate(tx, m.AccountID, in.ServerCertificateName)
		if loadErr != nil {
			return loadErr
		}

		merged, mergeErr := tagging.Merge(c.Tags, add)
		if mergeErr != nil {
			return tagError(mergeErr)
		}

		c.Tags = merged
		return storage.Write(tx, ck, c)
	})
	if err != nil {
		return nil, err
	}

	return &api.TagServerCertificateOutput{}, nil
}

func (s *Service) UntagServerCertificate(
	ctx context.Context,
	in *api.UntagServerCertificateRequest,
) (*api.UntagServerCertificateOutput, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		c, ck, loadErr := loadServerCertificate(tx, m.AccountID, in.ServerCertificateName)
		if loadErr != nil {
			return loadErr
		}

		c.Tags = tagging.Remove(c.Tags, in.TagKeys)
		return storage.Write(tx, ck, c)
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagServerCertificateOutput{}, nil
}

func (s *Service) ListServerCertificateTags(
	ctx context.Context,
	in *api.ListServerCertificateTagsRequest,
) (*api.ListServerCertificateTagsResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.ListServerCertificateTagsResponse{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		c, _, loadErr := loadServerCertificate(tx, m.AccountID, in.ServerCertificateName)
		if loadErr != nil {
			return loadErr
		}

		out.Tags, out.IsTruncated, out.Marker, loadErr = page(
			tagList(c.Tags), func(t api.Tag) string { return *t.Key }, in.Marker, in.MaxItems)
		return loadErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
