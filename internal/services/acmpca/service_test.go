package acmpca

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/acmpca"
	s3api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

type buckets map[string]bool

func (b buckets) HeadBucket(_ context.Context, in *s3api.HeadBucketRequest) (*s3api.HeadBucketOutput, error) {
	if !b[*in.Bucket] {
		return nil, errors.New("no such bucket")
	}

	return &s3api.HeadBucketOutput{}, nil
}

type fixture struct {
	svc   *Service
	store storage.Store
}

func newFixture() fixture {
	store := memory.New(nil, nil)
	return fixture{svc: New(store, buckets{"crl-bucket": true}), store: store}
}

func contextIn(account, region string) context.Context {
	return request.WithMetadata(
		context.Background(),
		request.Metadata{Scope: request.Scope{AccountID: account, Region: region}},
	)
}

func testContext() context.Context { return contextIn("123456789012", "us-east-1") }

func wantCode(t *testing.T, err error, code string) {
	t.Helper()

	var awsErr *awserror.Error
	if !errors.As(err, &awsErr) || awsErr.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func rootConfiguration() *api.CertificateAuthorityConfiguration {
	return &api.CertificateAuthorityConfiguration{
		KeyAlgorithm:     new("RSA_2048"),
		SigningAlgorithm: new("SHA256WITHRSA"),
		Subject:          &api.ASN1Subject{CommonName: new("root.example.com"), Country: new("US")},
	}
}

func (f fixture) create(t *testing.T, ctx context.Context, tags ...api.Tag) string {
	t.Helper()

	out, err := f.svc.CreateCertificateAuthority(ctx, &api.CreateCertificateAuthorityRequest{
		CertificateAuthorityConfiguration: rootConfiguration(),
		CertificateAuthorityType:          new("ROOT"),
		Tags:                              tags,
	})
	if err != nil {
		t.Fatal(err)
	}

	return *out.CertificateAuthorityArn
}

// force sets an authority's status, which an install of its certificate would, as this service never does.
func (f fixture) force(t *testing.T, arn, status string) {
	t.Helper()

	scope := request.Scope{AccountID: "123456789012", Region: "us-east-1"}
	err := f.store.Update(testContext(), func(tx storage.WriteTx) error {
		rec, key, err := load(tx, scope, &arn, time.Now())
		if err != nil {
			return err
		}

		rec.Authority.Status = &status
		return storage.Write(tx, key, rec)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (f fixture) status(t *testing.T, arn string) string {
	t.Helper()

	out, err := f.svc.DescribeCertificateAuthority(testContext(), &api.DescribeCertificateAuthorityRequest{
		CertificateAuthorityArn: &arn,
	})
	if err != nil {
		t.Fatal(err)
	}

	return *out.CertificateAuthority.Status
}

func TestCreateAndDescribe(t *testing.T) {
	f := newFixture()
	ctx := testContext()
	arn := f.create(t, ctx, api.Tag{Key: new("env"), Value: new("dev")})
	if !strings.HasPrefix(arn, "arn:aws:acm-pca:us-east-1:123456789012:certificate-authority/") {
		t.Fatalf("unexpected ARN %s", arn)
	}

	out, err := f.svc.DescribeCertificateAuthority(ctx, &api.DescribeCertificateAuthorityRequest{
		CertificateAuthorityArn: &arn,
	})
	if err != nil {
		t.Fatal(err)
	}

	ca := out.CertificateAuthority
	if *ca.Status != statusPending || *ca.Type != "ROOT" || *ca.OwnerAccount != "123456789012" ||
		*ca.UsageMode != "GENERAL_PURPOSE" || *ca.KeyStorageSecurityStandard != "FIPS_140_2_LEVEL_3_OR_HIGHER" ||
		*ca.CertificateAuthorityConfiguration.Subject.CommonName != "root.example.com" ||
		*ca.CreatedAt == 0 || ca.RestorableUntil != nil {
		t.Fatalf("unexpected authority %+v", ca)
	}

	tags, err := f.svc.ListTags(ctx, &api.ListTagsRequest{CertificateAuthorityArn: &arn})
	if err != nil || len(tags.Tags) != 1 || *tags.Tags[0].Key != "env" {
		t.Fatalf("unexpected tags %+v, %v", tags, err)
	}
}

func TestCreateIdempotencyToken(t *testing.T) {
	f := newFixture()
	ctx := testContext()
	in := &api.CreateCertificateAuthorityRequest{
		CertificateAuthorityConfiguration: rootConfiguration(),
		CertificateAuthorityType:          new("ROOT"),
		IdempotencyToken:                  new("token-1"),
	}

	first, err := f.svc.CreateCertificateAuthority(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	second, err := f.svc.CreateCertificateAuthority(ctx, in)
	if err != nil || *second.CertificateAuthorityArn != *first.CertificateAuthorityArn {
		t.Fatalf("the same token created another authority: %v, %v", second, err)
	}

	in.IdempotencyToken = new("token-2")
	third, err := f.svc.CreateCertificateAuthority(ctx, in)
	if err != nil || *third.CertificateAuthorityArn == *first.CertificateAuthorityArn {
		t.Fatalf("a new token reused an authority: %v, %v", third, err)
	}

	listed, err := f.svc.ListCertificateAuthorities(ctx, &api.ListCertificateAuthoritiesRequest{})
	if err != nil || len(listed.CertificateAuthorities) != 2 {
		t.Fatalf("want 2 authorities, got %+v, %v", listed, err)
	}
}

func TestCreateValidation(t *testing.T) {
	f := newFixture()
	ctx := testContext()
	crl := func(c *api.CrlConfiguration) *api.RevocationConfiguration {
		return &api.RevocationConfiguration{CrlConfiguration: c}
	}

	cases := map[string]func(*api.CreateCertificateAuthorityRequest){
		"type": func(in *api.CreateCertificateAuthorityRequest) { in.CertificateAuthorityType = new("LEAF") },
		"key algorithm": func(in *api.CreateCertificateAuthorityRequest) {
			in.CertificateAuthorityConfiguration.KeyAlgorithm = new("RSA_1")
		},
		"signing algorithm": func(in *api.CreateCertificateAuthorityRequest) {
			in.CertificateAuthorityConfiguration.SigningAlgorithm = new("MD5")
		},
		"rsa key signs with ecdsa": func(in *api.CreateCertificateAuthorityRequest) {
			in.CertificateAuthorityConfiguration.SigningAlgorithm = new("SHA256WITHECDSA")
		},
		"ec key signs with rsa": func(in *api.CreateCertificateAuthorityRequest) {
			in.CertificateAuthorityConfiguration.KeyAlgorithm = new("EC_prime256v1")
		},
		"ml-dsa key signs with another ml-dsa": func(in *api.CreateCertificateAuthorityRequest) {
			in.CertificateAuthorityConfiguration.KeyAlgorithm = new("ML_DSA_44")
			in.CertificateAuthorityConfiguration.SigningAlgorithm = new("ML_DSA_65")
		},
		"rsa pss is not a CA signing algorithm": func(in *api.CreateCertificateAuthorityRequest) {
			in.CertificateAuthorityConfiguration.SigningAlgorithm = new("SHA256WITHRSA_PSS")
		},
		"sm2 outside china": func(in *api.CreateCertificateAuthorityRequest) {
			in.CertificateAuthorityConfiguration.KeyAlgorithm = new("SM2")
			in.CertificateAuthorityConfiguration.SigningAlgorithm = new("SM3WITHSM2")
		},
		"configuration": func(in *api.CreateCertificateAuthorityRequest) { in.CertificateAuthorityConfiguration = nil },
		"subject": func(in *api.CreateCertificateAuthorityRequest) {
			in.CertificateAuthorityConfiguration.Subject = nil
		},
		"country": func(in *api.CreateCertificateAuthorityRequest) {
			in.CertificateAuthorityConfiguration.Subject.Country = new("USA")
		},
		"common name": func(in *api.CreateCertificateAuthorityRequest) {
			in.CertificateAuthorityConfiguration.Subject.CommonName = new(strings.Repeat("a", 65))
		},
		"serial number": func(in *api.CreateCertificateAuthorityRequest) {
			in.CertificateAuthorityConfiguration.Subject.SerialNumber = new("a_b")
		},
		"usage mode": func(in *api.CreateCertificateAuthorityRequest) { in.UsageMode = new("SOMETIMES") },
		"security":   func(in *api.CreateCertificateAuthorityRequest) { in.KeyStorageSecurityStandard = new("NONE") },
		"token":      func(in *api.CreateCertificateAuthorityRequest) { in.IdempotencyToken = new(strings.Repeat("t", 37)) },
		"reserved tag": func(in *api.CreateCertificateAuthorityRequest) {
			in.Tags = []api.Tag{{Key: new("aws:x"), Value: new("v")}}
		},
		"tag characters": func(in *api.CreateCertificateAuthorityRequest) {
			in.Tags = []api.Tag{{Key: new("k#"), Value: new("v")}}
		},
		"crl enabled without a bucket": func(in *api.CreateCertificateAuthorityRequest) {
			in.RevocationConfiguration = crl(&api.CrlConfiguration{Enabled: new(true)})
		},
		"crl bucket missing": func(in *api.CreateCertificateAuthorityRequest) {
			in.RevocationConfiguration = crl(
				&api.CrlConfiguration{Enabled: new(true), S3BucketName: new("other-bucket")},
			)
		},
		"crl disabled with a bucket": func(in *api.CreateCertificateAuthorityRequest) {
			in.RevocationConfiguration = crl(
				&api.CrlConfiguration{Enabled: new(false), S3BucketName: new("crl-bucket")},
			)
		},
		"crl without enabled": func(in *api.CreateCertificateAuthorityRequest) {
			in.RevocationConfiguration = crl(&api.CrlConfiguration{S3BucketName: new("crl-bucket")})
		},
		"crl expiration": func(in *api.CreateCertificateAuthorityRequest) {
			in.RevocationConfiguration = crl(&api.CrlConfiguration{
				Enabled: new(true), S3BucketName: new("crl-bucket"), ExpirationInDays: new(int64(5001)),
			})
		},
		"crl cname protocol": func(in *api.CreateCertificateAuthorityRequest) {
			in.RevocationConfiguration = crl(&api.CrlConfiguration{
				Enabled: new(true), S3BucketName: new("crl-bucket"), CustomCname: new("https://crl.example.com"),
			})
		},
		"crl acl": func(in *api.CreateCertificateAuthorityRequest) {
			in.RevocationConfiguration = crl(&api.CrlConfiguration{
				Enabled: new(true), S3BucketName: new("crl-bucket"), S3ObjectAcl: new("PRIVATE"),
			})
		},
		"ocsp disabled with a cname": func(in *api.CreateCertificateAuthorityRequest) {
			in.RevocationConfiguration = &api.RevocationConfiguration{
				OcspConfiguration: &api.OcspConfiguration{
					Enabled:         new(false),
					OcspCustomCname: new("ocsp.example.com"),
				},
			}
		},
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := &api.CreateCertificateAuthorityRequest{
				CertificateAuthorityConfiguration: rootConfiguration(),
				CertificateAuthorityType:          new("ROOT"),
			}
			mutate(in)

			_, err := f.svc.CreateCertificateAuthority(ctx, in)
			if strings.Contains(name, "tag") {
				wantCode(t, err, "InvalidTagException")
				return
			}

			wantCode(t, err, "InvalidArgsException")
		})
	}

	listed, err := f.svc.ListCertificateAuthorities(ctx, &api.ListCertificateAuthoritiesRequest{})
	if err != nil || len(listed.CertificateAuthorities) != 0 {
		t.Fatalf("a rejected create left %+v, %v", listed, err)
	}
}

func TestCreateAcceptsDocumentedAlgorithms(t *testing.T) {
	f := newFixture()
	pairs := [][2]string{
		{"RSA_3072", "SHA384WITHRSA"},
		{"RSA_4096", "SHA512WITHRSA"},
		{"EC_secp384r1", "SHA256WITHECDSA"},
		{"EC_secp521r1", "SHA512WITHECDSA"},
		{"ML_DSA_87", "ML_DSA_87"},
	}
	for _, p := range pairs {
		_, err := f.svc.CreateCertificateAuthority(testContext(), &api.CreateCertificateAuthorityRequest{
			CertificateAuthorityConfiguration: &api.CertificateAuthorityConfiguration{
				KeyAlgorithm: new(p[0]), SigningAlgorithm: new(p[1]), Subject: &api.ASN1Subject{CommonName: new("ca")},
			},
			CertificateAuthorityType: new("SUBORDINATE"),
			UsageMode:                new("SHORT_LIVED_CERTIFICATE"),
		})
		if err != nil {
			t.Fatalf("%v: %v", p, err)
		}
	}

	_, err := f.svc.CreateCertificateAuthority(contextIn("123456789012", "cn-north-1"),
		&api.CreateCertificateAuthorityRequest{
			CertificateAuthorityConfiguration: &api.CertificateAuthorityConfiguration{
				KeyAlgorithm: new("SM2"), SigningAlgorithm: new("SM3WITHSM2"), Subject: &api.ASN1Subject{},
			},
			CertificateAuthorityType: new("ROOT"),
		})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDescribeErrorsAndScopeIsolation(t *testing.T) {
	f := newFixture()
	arn := f.create(t, testContext())

	for _, bad := range []string{"", "not-an-arn", "arn:aws:acm-pca:us-east-1:123456789012:other/x"} {
		_, err := f.svc.DescribeCertificateAuthority(testContext(), &api.DescribeCertificateAuthorityRequest{
			CertificateAuthorityArn: &bad,
		})
		wantCode(t, err, "InvalidArnException")
	}

	unknown := arn[:len(arn)-1] + "0"
	if unknown == arn {
		unknown = arn[:len(arn)-1] + "1"
	}

	_, err := f.svc.DescribeCertificateAuthority(testContext(), &api.DescribeCertificateAuthorityRequest{
		CertificateAuthorityArn: &unknown,
	})
	wantCode(t, err, "ResourceNotFoundException")

	for _, ctx := range []context.Context{contextIn("999999999999", "us-east-1"), contextIn("123456789012", "us-west-2")} {
		_, err = f.svc.DescribeCertificateAuthority(
			ctx,
			&api.DescribeCertificateAuthorityRequest{CertificateAuthorityArn: &arn},
		)
		wantCode(t, err, "ResourceNotFoundException")

		listed, listErr := f.svc.ListCertificateAuthorities(ctx, &api.ListCertificateAuthoritiesRequest{})
		if listErr != nil || len(listed.CertificateAuthorities) != 0 {
			t.Fatalf("another scope lists %+v, %v", listed, listErr)
		}
	}
}

func TestListPagination(t *testing.T) {
	f := newFixture()
	ctx := testContext()
	want := map[string]bool{}
	for range 3 {
		want[f.create(t, ctx)] = true
	}

	seen := map[string]bool{}
	var token *string
	for pages := 0; ; pages++ {
		out, err := f.svc.ListCertificateAuthorities(ctx, &api.ListCertificateAuthoritiesRequest{
			MaxResults: new(int64(2)), NextToken: token,
		})
		if err != nil {
			t.Fatal(err)
		}

		for _, ca := range out.CertificateAuthorities {
			seen[*ca.Arn] = true
		}

		if token = out.NextToken; token == nil {
			if pages != 1 {
				t.Fatalf("want 2 pages, got %d", pages+1)
			}

			break
		}
	}

	if len(seen) != 3 {
		t.Fatalf("paging saw %v, want %v", seen, want)
	}

	_, err := f.svc.ListCertificateAuthorities(ctx, &api.ListCertificateAuthoritiesRequest{NextToken: new("!")})
	wantCode(t, err, "InvalidNextTokenException")

	_, err = f.svc.ListCertificateAuthorities(ctx, &api.ListCertificateAuthoritiesRequest{MaxResults: new(int64(0))})
	wantCode(t, err, "InvalidArgsException")

	other, err := f.svc.ListCertificateAuthorities(
		ctx,
		&api.ListCertificateAuthoritiesRequest{ResourceOwner: new("OTHER_ACCOUNTS")},
	)
	if err != nil || len(other.CertificateAuthorities) != 0 {
		t.Fatalf("shared authorities: %+v, %v", other, err)
	}
}

func TestUpdateStatusMachine(t *testing.T) {
	f := newFixture()
	ctx := testContext()
	arn := f.create(t, ctx)
	update := func(status string) error {
		_, err := f.svc.UpdateCertificateAuthority(ctx, &api.UpdateCertificateAuthorityRequest{
			CertificateAuthorityArn: &arn, Status: &status,
		})
		return err
	}

	// A pending authority has no certificate, so it can be neither activated nor disabled.
	wantCode(t, update("ACTIVE"), "InvalidStateException")
	wantCode(t, update("DISABLED"), "InvalidStateException")
	wantCode(t, update("DELETED"), "InvalidArgsException")
	wantCode(t, update("EXPIRED"), "InvalidArgsException")

	if got := f.status(t, arn); got != statusPending {
		t.Fatalf("a rejected update changed the status to %s", got)
	}

	f.force(t, arn, statusActive)
	if err := update("DISABLED"); err != nil || f.status(t, arn) != statusDisabled {
		t.Fatalf("disable: %v, %s", err, f.status(t, arn))
	}

	if err := update("ACTIVE"); err != nil || f.status(t, arn) != statusActive {
		t.Fatalf("enable: %v, %s", err, f.status(t, arn))
	}

	wantCode(t, update("PENDING_CERTIFICATE"), "InvalidArgsException")
}

func TestUpdateRevocationConfiguration(t *testing.T) {
	f := newFixture()
	ctx := testContext()
	arn := f.create(t, ctx)
	revoke := func(rev *api.RevocationConfiguration) error {
		_, err := f.svc.UpdateCertificateAuthority(ctx, &api.UpdateCertificateAuthorityRequest{
			CertificateAuthorityArn: &arn, RevocationConfiguration: rev,
		})
		return err
	}

	describe := func() *api.RevocationConfiguration {
		out, err := f.svc.DescribeCertificateAuthority(
			ctx,
			&api.DescribeCertificateAuthorityRequest{CertificateAuthorityArn: &arn},
		)
		if err != nil {
			t.Fatal(err)
		}

		return out.CertificateAuthority.RevocationConfiguration
	}

	err := revoke(&api.RevocationConfiguration{CrlConfiguration: &api.CrlConfiguration{
		Enabled: new(true), S3BucketName: new("crl-bucket"), ExpirationInDays: new(int64(30)),
	}})
	if err != nil {
		t.Fatal(err)
	}

	if err = revoke(
		&api.RevocationConfiguration{OcspConfiguration: &api.OcspConfiguration{Enabled: new(true)}},
	); err != nil {
		t.Fatal(err)
	}

	rev := describe()
	if !*rev.CrlConfiguration.Enabled || *rev.CrlConfiguration.ExpirationInDays != 30 ||
		!*rev.OcspConfiguration.Enabled {
		t.Fatalf("an OCSP update dropped the CRL block: %+v", rev)
	}

	// Rejected updates leave the stored configuration unchanged.
	wantCode(t, revoke(&api.RevocationConfiguration{CrlConfiguration: &api.CrlConfiguration{
		Enabled: new(true), S3BucketName: new("crl-bucket"), ExpirationInDays: new(int64(0)),
	}}), "InvalidArgsException")
	wantCode(t, revoke(&api.RevocationConfiguration{CrlConfiguration: &api.CrlConfiguration{
		Enabled: new(true), S3BucketName: new("gone-bucket"),
	}}), "ResourceNotFoundException")

	if got := describe(); *got.CrlConfiguration.S3BucketName != "crl-bucket" ||
		*got.CrlConfiguration.ExpirationInDays != 30 {
		t.Fatalf("a rejected update changed the configuration: %+v", got)
	}

	// Turning a block off replaces it with just Enabled=false, so its settings are gone.
	if err = revoke(
		&api.RevocationConfiguration{CrlConfiguration: &api.CrlConfiguration{Enabled: new(false)}},
	); err != nil {
		t.Fatal(err)
	}

	if got := describe(); *got.CrlConfiguration.Enabled || got.CrlConfiguration.S3BucketName != nil {
		t.Fatalf("a disabled CRL kept its settings: %+v", got.CrlConfiguration)
	}
}

func TestDeleteAndRestore(t *testing.T) {
	f := newFixture()
	ctx := testContext()
	remove := func(arn string, days *int64) error {
		_, err := f.svc.DeleteCertificateAuthority(ctx, &api.DeleteCertificateAuthorityRequest{
			CertificateAuthorityArn: &arn, PermanentDeletionTimeInDays: days,
		})
		return err
	}

	restore := func(arn string) error {
		_, err := f.svc.RestoreCertificateAuthority(
			ctx,
			&api.RestoreCertificateAuthorityRequest{CertificateAuthorityArn: &arn},
		)
		return err
	}

	pending := f.create(t, ctx)
	wantCode(t, remove(pending, new(int64(6))), "InvalidArgsException")
	wantCode(t, remove(pending, new(int64(31))), "InvalidArgsException")
	wantCode(t, restore(pending), "InvalidStateException")

	before := time.Now()
	if err := remove(pending, new(int64(7))); err != nil {
		t.Fatal(err)
	}

	out, err := f.svc.DescribeCertificateAuthority(
		ctx,
		&api.DescribeCertificateAuthorityRequest{CertificateAuthorityArn: &pending},
	)
	if err != nil || *out.CertificateAuthority.Status != statusDeleted {
		t.Fatalf("deleted authority: %+v, %v", out, err)
	}

	until := time.UnixMilli(int64(*out.CertificateAuthority.RestorableUntil * 1000))
	if until.Before(before.Add(7*24*time.Hour-time.Second)) || until.After(time.Now().Add(7*24*time.Hour+time.Second)) {
		t.Fatalf("RestorableUntil %v is not 7 days out", until)
	}

	listed, err := f.svc.ListCertificateAuthorities(ctx, &api.ListCertificateAuthoritiesRequest{})
	if err != nil || len(listed.CertificateAuthorities) != 1 ||
		*listed.CertificateAuthorities[0].Status != statusDeleted {
		t.Fatalf("a deleted authority is listed with its status: %+v, %v", listed, err)
	}

	wantCode(t, remove(pending, nil), "InvalidStateException")
	if err = restore(pending); err != nil || f.status(t, pending) != statusPending {
		t.Fatalf("restore: %v, %s", err, f.status(t, pending))
	}

	// An ACTIVE authority must be disabled first, and a restored one returns to the status it was deleted in.
	active := f.create(t, ctx)
	f.force(t, active, statusActive)
	wantCode(t, remove(active, nil), "InvalidStateException")
	f.force(t, active, statusDisabled)
	if err = remove(active, nil); err != nil {
		t.Fatal(err)
	}

	if err = restore(active); err != nil || f.status(t, active) != statusDisabled {
		t.Fatalf("restore: %v, %s", err, f.status(t, active))
	}

	restored, _ := f.svc.DescribeCertificateAuthority(
		ctx,
		&api.DescribeCertificateAuthorityRequest{CertificateAuthorityArn: &active},
	)
	if restored.CertificateAuthority.RestorableUntil != nil {
		t.Fatal("a restored authority keeps its restoration period")
	}
}

func TestDeletedAuthorityIsGoneAfterRestorationPeriod(t *testing.T) {
	f := newFixture()
	ctx := testContext()
	arn := f.create(t, ctx)
	if _, err := f.svc.DeleteCertificateAuthority(
		ctx,
		&api.DeleteCertificateAuthorityRequest{CertificateAuthorityArn: &arn},
	); err != nil {
		t.Fatal(err)
	}

	scope := request.Scope{AccountID: "123456789012", Region: "us-east-1"}
	err := f.store.Update(ctx, func(tx storage.WriteTx) error {
		rec, key, err := load(tx, scope, &arn, time.Now())
		if err != nil {
			return err
		}

		rec.Authority.RestorableUntil = new(float64(time.Now().Add(-time.Minute).Unix()))
		return storage.Write(tx, key, rec)
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = f.svc.DescribeCertificateAuthority(
		ctx,
		&api.DescribeCertificateAuthorityRequest{CertificateAuthorityArn: &arn},
	)
	wantCode(t, err, "ResourceNotFoundException")

	_, err = f.svc.RestoreCertificateAuthority(
		ctx,
		&api.RestoreCertificateAuthorityRequest{CertificateAuthorityArn: &arn},
	)
	wantCode(t, err, "ResourceNotFoundException")

	resources, err := f.svc.Resources(ctx)
	if err != nil || len(resources) != 0 {
		t.Fatalf("expired authority in inventory: %+v, %v", resources, err)
	}

	// The record is removed for good by the next create.
	f.create(t, ctx)
	err = f.store.View(ctx, func(tx storage.ReadTx) error {
		if _, found, _ := storage.Read[authorityRecord](
			tx,
			scope.Key(serviceName, authorityKind, arn[strings.LastIndex(arn, "/")+1:]),
		); found {
			t.Fatal("the expired record was not purged")
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestTags(t *testing.T) {
	f := newFixture()
	ctx := testContext()
	arn := f.create(t, ctx, api.Tag{Key: new("a"), Value: new("1")}, api.Tag{Key: new("b"), Value: new("2")})
	list := func() map[string]string {
		out, err := f.svc.ListTags(ctx, &api.ListTagsRequest{CertificateAuthorityArn: &arn})
		if err != nil {
			t.Fatal(err)
		}

		tags := map[string]string{}
		for _, tag := range out.Tags {
			tags[*tag.Key] = *tag.Value
		}

		return tags
	}

	if _, err := f.svc.TagCertificateAuthority(ctx, &api.TagCertificateAuthorityRequest{
		CertificateAuthorityArn: &arn,
		Tags:                    []api.Tag{{Key: new("b"), Value: new("3")}, {Key: new("c")}},
	}); err != nil {
		t.Fatal(err)
	}

	if got := list(); len(got) != 3 || got["b"] != "3" || got["c"] != "" {
		t.Fatalf("unexpected tags %v", got)
	}

	// A tag with a value is removed only when the value matches; one without removes it whatever its value is.
	if _, err := f.svc.UntagCertificateAuthority(ctx, &api.UntagCertificateAuthorityRequest{
		CertificateAuthorityArn: &arn,
		Tags: []api.Tag{
			{Key: new("a"), Value: new("other")},
			{Key: new("b"), Value: new("3")},
			{Key: new("c")},
		},
	}); err != nil {
		t.Fatal(err)
	}

	if got := list(); len(got) != 1 || got["a"] != "1" {
		t.Fatalf("unexpected tags %v", got)
	}

	_, err := f.svc.TagCertificateAuthority(ctx, &api.TagCertificateAuthorityRequest{CertificateAuthorityArn: &arn})
	wantCode(t, err, "InvalidTagException")

	var many []api.Tag
	for i := range 51 {
		many = append(many, api.Tag{Key: new(strings.Repeat("k", i+1)), Value: new("v")})
	}

	_, err = f.svc.TagCertificateAuthority(
		ctx,
		&api.TagCertificateAuthorityRequest{CertificateAuthorityArn: &arn, Tags: many},
	)
	wantCode(t, err, "TooManyTagsException")

	if got := list(); len(got) != 1 {
		t.Fatalf("a rejected tag call changed the tags: %v", got)
	}

	page, err := f.svc.ListTags(ctx, &api.ListTagsRequest{CertificateAuthorityArn: &arn, MaxResults: new(int64(1))})
	if err != nil || len(page.Tags) != 1 || page.NextToken != nil {
		t.Fatalf("unexpected page %+v, %v", page, err)
	}

	if _, err = f.svc.DeleteCertificateAuthority(
		ctx,
		&api.DeleteCertificateAuthorityRequest{CertificateAuthorityArn: &arn},
	); err != nil {
		t.Fatal(err)
	}

	_, err = f.svc.TagCertificateAuthority(ctx, &api.TagCertificateAuthorityRequest{
		CertificateAuthorityArn: &arn, Tags: []api.Tag{{Key: new("x")}},
	})
	wantCode(t, err, "InvalidStateException")

	if got := list(); len(got) != 1 {
		t.Fatalf("a deleted authority's tags changed: %v", got)
	}
}

func TestResources(t *testing.T) {
	f := newFixture()
	ctx := testContext()
	arn := f.create(t, ctx, api.Tag{Key: new("env"), Value: new("dev")})
	deleted := f.create(t, ctx)
	if _, err := f.svc.DeleteCertificateAuthority(
		ctx,
		&api.DeleteCertificateAuthorityRequest{CertificateAuthorityArn: &deleted},
	); err != nil {
		t.Fatal(err)
	}

	resources, err := f.svc.Resources(ctx)
	if err != nil || len(resources) != 1 || resources[0].ARN != arn || resources[0].Type != "certificate-authority" ||
		resources[0].Tags["env"] != "dev" || resources[0].Details.(map[string]any)["RecordsOnly"] != true {
		t.Fatalf("unexpected inventory %+v, %v", resources, err)
	}
}
