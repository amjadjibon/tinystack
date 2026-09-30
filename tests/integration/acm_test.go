package integration_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/acm"
	acmtypes "github.com/aws/aws-sdk-go-v2/service/acm/types"
)

func acmClient(s *httptest.Server) *acm.Client {
	return acm.New(acm.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
		}),
		Retryer: aws.NopRetryer{},
	})
}

// selfSigned returns a PEM certificate for name and its PEM private key.
func selfSigned(t *testing.T, name string) (certPEM, keyPEM []byte) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}

func TestACMRequestedCertificates(t *testing.T) {
	c := acmClient(server(t))
	ctx := context.Background()
	requested, err := c.RequestCertificate(ctx, &acm.RequestCertificateInput{
		DomainName: aws.String("example.com"), SubjectAlternativeNames: []string{"*.example.com", "example.com"},
		ValidationMethod: acmtypes.ValidationMethodDns, IdempotencyToken: aws.String("once"),
		Options: &acmtypes.CertificateOptions{Export: acmtypes.CertificateExportEnabled},
		Tags:    []acmtypes.Tag{{Key: aws.String("team"), Value: aws.String("web")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	again, err := c.RequestCertificate(ctx, &acm.RequestCertificateInput{
		DomainName: aws.String("example.com"), SubjectAlternativeNames: []string{"*.example.com", "example.com"},
		ValidationMethod: acmtypes.ValidationMethodDns, IdempotencyToken: aws.String("once"),
		Options: &acmtypes.CertificateOptions{Export: acmtypes.CertificateExportEnabled},
		Tags:    []acmtypes.Tag{{Key: aws.String("team"), Value: aws.String("web")}},
	})
	if err != nil || aws.ToString(again.CertificateArn) != aws.ToString(requested.CertificateArn) {
		t.Fatalf("idempotent request: %+v %v", again, err)
	}

	arn := requested.CertificateArn
	described, err := c.DescribeCertificate(ctx, &acm.DescribeCertificateInput{CertificateArn: arn})
	if err != nil {
		t.Fatal(err)
	}

	d := described.Certificate
	if d.Status != acmtypes.CertificateStatusIssued || d.Type != acmtypes.CertificateTypeAmazonIssued ||
		!slices.Equal(d.SubjectAlternativeNames, []string{"example.com", "*.example.com"}) ||
		len(d.DomainValidationOptions) != 2 || d.KeyAlgorithm != acmtypes.KeyAlgorithmRsa2048 ||
		d.Options.Export != acmtypes.CertificateExportEnabled {
		t.Fatalf("certificate: %+v", d)
	}

	record := d.DomainValidationOptions[0].ResourceRecord
	wildcard := d.DomainValidationOptions[1].ResourceRecord
	if record == nil || !strings.HasSuffix(aws.ToString(record.Name), ".example.com.") ||
		record.Type != acmtypes.RecordTypeCname || aws.ToString(wildcard.Name) != aws.ToString(record.Name) {
		t.Fatalf("validation records: %+v %+v", record, wildcard)
	}

	got, err := c.GetCertificate(ctx, &acm.GetCertificateInput{CertificateArn: arn})
	if err != nil {
		t.Fatal(err)
	}

	block, _ := pem.Decode([]byte(aws.ToString(got.Certificate)))
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}

	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM([]byte(aws.ToString(got.CertificateChain)))
	if _, err = leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "www.example.com"}); err != nil {
		t.Fatalf("verifying the issued certificate: %v", err)
	}

	exported, err := c.ExportCertificate(ctx, &acm.ExportCertificateInput{
		CertificateArn: arn, Passphrase: []byte("secret"),
	})
	if err != nil || !strings.HasPrefix(aws.ToString(exported.PrivateKey), "-----BEGIN ENCRYPTED PRIVATE KEY-----") {
		t.Fatalf("export: %+v %v", exported, err)
	}

	ec, err := c.RequestCertificate(ctx, &acm.RequestCertificateInput{
		DomainName: aws.String("api.example.com"), KeyAlgorithm: acmtypes.KeyAlgorithmEcPrime256v1,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Without a key type filter only RSA certificates are listed, as AWS does.
	listed, err := c.ListCertificates(ctx, &acm.ListCertificatesInput{})
	if err != nil || len(listed.CertificateSummaryList) != 1 {
		t.Fatalf("default listing: %+v %v", listed, err)
	}

	listed, err = c.ListCertificates(ctx, &acm.ListCertificatesInput{
		Includes: &acmtypes.Filters{KeyTypes: []acmtypes.KeyAlgorithm{acmtypes.KeyAlgorithmEcPrime256v1}},
	})
	if err != nil || len(listed.CertificateSummaryList) != 1 ||
		aws.ToString(listed.CertificateSummaryList[0].CertificateArn) != aws.ToString(ec.CertificateArn) {
		t.Fatalf("EC listing: %+v %v", listed, err)
	}

	if _, err = c.RemoveTagsFromCertificate(ctx, &acm.RemoveTagsFromCertificateInput{
		CertificateArn: arn, Tags: []acmtypes.Tag{{Key: aws.String("team"), Value: aws.String("other")}},
	}); err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListTagsForCertificate(ctx, &acm.ListTagsForCertificateInput{CertificateArn: arn})
	if err != nil || len(tags.Tags) != 1 {
		t.Fatalf("a tag with another value was removed: %+v %v", tags, err)
	}

	var invalid *acmtypes.InvalidParameterException
	if _, err = c.RequestCertificate(ctx, &acm.RequestCertificateInput{
		DomainName: aws.String("localhost"),
	}); !errors.As(err, &invalid) {
		t.Fatalf("unqualified name: %v", err)
	}

	if _, err = c.DeleteCertificate(ctx, &acm.DeleteCertificateInput{CertificateArn: arn}); err != nil {
		t.Fatal(err)
	}

	var notFound *acmtypes.ResourceNotFoundException
	if _, err = c.DescribeCertificate(ctx, &acm.DescribeCertificateInput{
		CertificateArn: arn,
	}); !errors.As(err, &notFound) {
		t.Fatalf("deleted certificate: %v", err)
	}
}

func TestACMImportedCertificates(t *testing.T) {
	c := acmClient(server(t))
	ctx := context.Background()
	certPEM, keyPEM := selfSigned(t, "internal.test")
	imported, err := c.ImportCertificate(ctx, &acm.ImportCertificateInput{
		Certificate: certPEM, PrivateKey: keyPEM,
		Tags: []acmtypes.Tag{{Key: aws.String("source"), Value: aws.String("test")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	described, err := c.DescribeCertificate(ctx, &acm.DescribeCertificateInput{CertificateArn: imported.CertificateArn})
	if err != nil {
		t.Fatal(err)
	}

	if d := described.Certificate; d.Type != acmtypes.CertificateTypeImported ||
		aws.ToString(d.DomainName) != "internal.test" || d.KeyAlgorithm != acmtypes.KeyAlgorithmEcPrime256v1 ||
		d.ImportedAt == nil || aws.ToString(d.Serial) != "07" {
		t.Fatalf("imported: %+v", d)
	}

	got, err := c.GetCertificate(ctx, &acm.GetCertificateInput{CertificateArn: imported.CertificateArn})
	if err != nil || aws.ToString(got.Certificate) != string(certPEM) || got.CertificateChain != nil {
		t.Fatalf("%+v %v", got, err)
	}

	otherCert, otherKey := selfSigned(t, "other.test")
	if _, err = c.ImportCertificate(ctx, &acm.ImportCertificateInput{
		Certificate: certPEM, PrivateKey: otherKey,
	}); err == nil {
		t.Fatal("a certificate was imported with another certificate's key")
	}

	reimported, err := c.ImportCertificate(ctx, &acm.ImportCertificateInput{
		CertificateArn: imported.CertificateArn, Certificate: otherCert, PrivateKey: otherKey,
	})
	if err != nil || aws.ToString(reimported.CertificateArn) != aws.ToString(imported.CertificateArn) {
		t.Fatalf("reimport: %+v %v", reimported, err)
	}

	tags, err := c.ListTagsForCertificate(ctx, &acm.ListTagsForCertificateInput{
		CertificateArn: imported.CertificateArn,
	})
	if err != nil || len(tags.Tags) != 1 {
		t.Fatalf("reimporting lost the tags: %+v %v", tags, err)
	}

	if _, err = c.ExportCertificate(ctx, &acm.ExportCertificateInput{
		CertificateArn: imported.CertificateArn, Passphrase: []byte("secret"),
	}); err == nil {
		t.Fatal("an imported certificate was exported")
	}
}
