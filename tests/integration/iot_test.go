package integration_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iot"
	iottypes "github.com/aws/aws-sdk-go-v2/service/iot/types"
)

func iotClient(s *httptest.Server, key string) *iot.Client {
	return iot.New(iot.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: key, SecretAccessKey: "test"}, nil
		}),
		Retryer: aws.NopRetryer{},
	})
}

func TestIoTThingsAndTypes(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := iotClient(s, "test")

	if _, err := c.CreateThingType(ctx, &iot.CreateThingTypeInput{
		ThingTypeName: aws.String("sensor"),
		ThingTypeProperties: &iottypes.ThingTypeProperties{
			ThingTypeDescription: aws.String("temperature sensors"), SearchableAttributes: []string{"floor"},
		},
		Tags: []iottypes.Tag{{Key: aws.String("team"), Value: aws.String("a")}},
	}); err != nil {
		t.Fatal(err)
	}

	created, err := c.CreateThing(ctx, &iot.CreateThingInput{
		ThingName: aws.String("probe-1"), ThingTypeName: aws.String("sensor"),
		AttributePayload: &iottypes.AttributePayload{Attributes: map[string]string{"floor": "3", "room": "a"}},
	})
	if err != nil || aws.ToString(created.ThingArn) != "arn:aws:iot:us-east-1:000000000000:thing/probe-1" ||
		len(aws.ToString(created.ThingId)) != 36 {
		t.Fatalf("%+v %v", created, err)
	}

	// The same thing again is no change; different attributes are a conflict.
	again, err := c.CreateThing(ctx, &iot.CreateThingInput{
		ThingName: aws.String("probe-1"), ThingTypeName: aws.String("sensor"),
		AttributePayload: &iottypes.AttributePayload{Attributes: map[string]string{"floor": "3", "room": "a"}},
	})
	if err != nil || aws.ToString(again.ThingId) != aws.ToString(created.ThingId) {
		t.Fatalf("%+v %v", again, err)
	}

	_, err = c.CreateThing(ctx, &iot.CreateThingInput{ThingName: aws.String("probe-1")})
	wantCode(t, err, "ResourceAlreadyExistsException")
	_, err = c.CreateThing(ctx, &iot.CreateThingInput{ThingName: aws.String("bad name")})
	wantCode(t, err, "InvalidRequestException")
	_, err = c.CreateThing(ctx, &iot.CreateThingInput{ThingName: aws.String("x"), ThingTypeName: aws.String("none")})
	wantCode(t, err, "ResourceNotFoundException")

	if _, err = c.CreateThing(ctx, &iot.CreateThingInput{ThingName: aws.String("probe-2")}); err != nil {
		t.Fatal(err)
	}

	described, err := c.DescribeThing(ctx, &iot.DescribeThingInput{ThingName: aws.String("probe-1")})
	if err != nil || aws.ToString(described.ThingTypeName) != "sensor" || described.Version != 1 ||
		described.Attributes["room"] != "a" || aws.ToString(described.DefaultClientId) != "probe-1" {
		t.Fatalf("%+v %v", described, err)
	}

	// Merging removes attributes set to "" and keeps the rest.
	if _, err = c.UpdateThing(ctx, &iot.UpdateThingInput{
		ThingName: aws.String("probe-1"), ExpectedVersion: aws.Int64(1),
		AttributePayload: &iottypes.AttributePayload{
			Attributes: map[string]string{"room": "", "rack": "7"}, Merge: true,
		},
	}); err != nil {
		t.Fatal(err)
	}

	_, err = c.UpdateThing(ctx, &iot.UpdateThingInput{ThingName: aws.String("probe-1"), ExpectedVersion: aws.Int64(1)})
	wantCode(t, err, "VersionConflictException")
	described, err = c.DescribeThing(ctx, &iot.DescribeThingInput{ThingName: aws.String("probe-1")})
	if err != nil || described.Version != 2 || len(described.Attributes) != 2 || described.Attributes["rack"] != "7" {
		t.Fatalf("%+v %v", described, err)
	}

	listed, err := c.ListThings(ctx, &iot.ListThingsInput{ThingTypeName: aws.String("sensor")})
	if err != nil || len(listed.Things) != 1 || aws.ToString(listed.Things[0].ThingName) != "probe-1" {
		t.Fatalf("%+v %v", listed, err)
	}

	listed, err = c.ListThings(ctx, &iot.ListThingsInput{
		AttributeName: aws.String("rack"), AttributeValue: aws.String("7"),
	})
	if err != nil || len(listed.Things) != 1 {
		t.Fatalf("%+v %v", listed, err)
	}

	var names []string
	for p := iot.NewListThingsPaginator(c, &iot.ListThingsInput{MaxResults: aws.Int32(1)}); p.HasMorePages(); {
		out, pageErr := p.NextPage(ctx)
		if pageErr != nil {
			t.Fatal(pageErr)
		}

		for _, th := range out.Things {
			names = append(names, aws.ToString(th.ThingName))
		}
	}

	if !slices.Equal(names, []string{"probe-1", "probe-2"}) {
		t.Fatal(names)
	}

	// A thing type must be deprecated, and unused, before it is deleted; new
	// things cannot use a deprecated type.
	_, err = c.DeleteThingType(ctx, &iot.DeleteThingTypeInput{ThingTypeName: aws.String("sensor")})
	wantCode(t, err, "InvalidRequestException")
	if _, err = c.DeprecateThingType(ctx, &iot.DeprecateThingTypeInput{
		ThingTypeName: aws.String("sensor"),
	}); err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateThing(ctx, &iot.CreateThingInput{ThingName: aws.String("p3"), ThingTypeName: aws.String("sensor")})
	wantCode(t, err, "InvalidRequestException")
	typ, err := c.DescribeThingType(ctx, &iot.DescribeThingTypeInput{ThingTypeName: aws.String("sensor")})
	if err != nil || !typ.ThingTypeMetadata.Deprecated || typ.ThingTypeMetadata.DeprecationDate == nil ||
		aws.ToString(typ.ThingTypeProperties.ThingTypeDescription) != "temperature sensors" {
		t.Fatalf("%+v %v", typ, err)
	}

	_, err = c.DeleteThingType(ctx, &iot.DeleteThingTypeInput{ThingTypeName: aws.String("sensor")})
	wantCode(t, err, "InvalidRequestException")
	if _, err = c.UpdateThing(ctx, &iot.UpdateThingInput{
		ThingName: aws.String("probe-1"), RemoveThingType: true,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeleteThingType(ctx, &iot.DeleteThingTypeInput{ThingTypeName: aws.String("sensor")}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeleteThing(ctx, &iot.DeleteThingInput{ThingName: aws.String("probe-2")}); err != nil {
		t.Fatal(err)
	}

	_, err = c.DescribeThing(ctx, &iot.DescribeThingInput{ThingName: aws.String("probe-2")})
	wantCode(t, err, "ResourceNotFoundException")

	// Billing groups are not supported, and fail rather than being ignored.
	_, err = c.CreateThing(ctx, &iot.CreateThingInput{ThingName: aws.String("b"), BillingGroupName: aws.String("g")})
	wantCode(t, err, "NotImplemented")

	// Another account sees none of this account's things.
	other, err := iotClient(s, "team-b").ListThings(ctx, &iot.ListThingsInput{})
	if err != nil || len(other.Things) != 0 {
		t.Fatalf("%+v %v", other, err)
	}
}

func TestIoTThingGroups(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := iotClient(s, "test")
	for _, g := range []struct{ name, parent string }{{"site", ""}, {"floor", "site"}, {"room", "floor"}} {
		in := &iot.CreateThingGroupInput{
			ThingGroupName: aws.String(g.name),
			ThingGroupProperties: &iottypes.ThingGroupProperties{
				AttributePayload: &iottypes.AttributePayload{Attributes: map[string]string{"level": g.name}},
			},
		}
		if g.parent != "" {
			in.ParentGroupName = aws.String(g.parent)
		}

		if _, err := c.CreateThingGroup(ctx, in); err != nil {
			t.Fatal(err)
		}
	}

	room, err := c.DescribeThingGroup(ctx, &iot.DescribeThingGroupInput{ThingGroupName: aws.String("room")})
	if err != nil || aws.ToString(room.ThingGroupMetadata.ParentGroupName) != "floor" || room.Version != 1 ||
		len(room.ThingGroupMetadata.RootToParentThingGroups) != 2 ||
		aws.ToString(room.ThingGroupMetadata.RootToParentThingGroups[0].GroupName) != "site" ||
		room.ThingGroupProperties.AttributePayload.Attributes["level"] != "room" {
		t.Fatalf("%+v %v", room, err)
	}

	updated, err := c.UpdateThingGroup(ctx, &iot.UpdateThingGroupInput{
		ThingGroupName: aws.String("room"), ExpectedVersion: aws.Int64(1),
		ThingGroupProperties: &iottypes.ThingGroupProperties{ThingGroupDescription: aws.String("server room")},
	})
	if err != nil || updated.Version != 2 {
		t.Fatalf("%+v %v", updated, err)
	}

	children, err := c.ListThingGroups(ctx, &iot.ListThingGroupsInput{
		ParentGroup: aws.String("site"), Recursive: aws.Bool(false),
	})
	if err != nil || len(children.ThingGroups) != 1 || aws.ToString(children.ThingGroups[0].GroupName) != "floor" {
		t.Fatalf("%+v %v", children, err)
	}

	below, err := c.ListThingGroups(ctx, &iot.ListThingGroupsInput{ParentGroup: aws.String("site")})
	if err != nil || len(below.ThingGroups) != 2 {
		t.Fatalf("%+v %v", below, err)
	}

	for _, th := range []string{"a", "b"} {
		if _, err = c.CreateThing(ctx, &iot.CreateThingInput{ThingName: aws.String(th)}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err = c.AddThingToThingGroup(ctx, &iot.AddThingToThingGroupInput{
		ThingName: aws.String("a"), ThingGroupName: aws.String("room"),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.AddThingToThingGroup(ctx, &iot.AddThingToThingGroupInput{
		ThingArn:      aws.String("arn:aws:iot:us-east-1:000000000000:thing/b"),
		ThingGroupArn: aws.String("arn:aws:iot:us-east-1:000000000000:thinggroup/site"),
	}); err != nil {
		t.Fatal(err)
	}

	direct, err := c.ListThingsInThingGroup(ctx, &iot.ListThingsInThingGroupInput{ThingGroupName: aws.String("site")})
	if err != nil || !slices.Equal(direct.Things, []string{"b"}) {
		t.Fatalf("%+v %v", direct, err)
	}

	all, err := c.ListThingsInThingGroup(ctx, &iot.ListThingsInThingGroupInput{
		ThingGroupName: aws.String("site"), Recursive: true,
	})
	if err != nil || !slices.Equal(all.Things, []string{"a", "b"}) {
		t.Fatalf("%+v %v", all, err)
	}

	groups, err := c.ListThingGroupsForThing(ctx, &iot.ListThingGroupsForThingInput{ThingName: aws.String("a")})
	if err != nil || len(groups.ThingGroups) != 1 || aws.ToString(groups.ThingGroups[0].GroupName) != "room" {
		t.Fatalf("%+v %v", groups, err)
	}

	// A group with children cannot be deleted; deleting a leaf removes its
	// things from it.
	_, err = c.DeleteThingGroup(ctx, &iot.DeleteThingGroupInput{ThingGroupName: aws.String("floor")})
	wantCode(t, err, "InvalidRequestException")
	if _, err = c.DeleteThingGroup(ctx, &iot.DeleteThingGroupInput{ThingGroupName: aws.String("room")}); err != nil {
		t.Fatal(err)
	}

	groups, err = c.ListThingGroupsForThing(ctx, &iot.ListThingGroupsForThingInput{ThingName: aws.String("a")})
	if err != nil || len(groups.ThingGroups) != 0 {
		t.Fatalf("%+v %v", groups, err)
	}

	if _, err = c.RemoveThingFromThingGroup(ctx, &iot.RemoveThingFromThingGroupInput{
		ThingName: aws.String("b"), ThingGroupName: aws.String("site"),
	}); err != nil {
		t.Fatal(err)
	}

	// Dynamic groups are not supported.
	_, err = c.AddThingToThingGroup(ctx, &iot.AddThingToThingGroupInput{
		ThingName: aws.String("a"), ThingGroupName: aws.String("site"), OverrideDynamicGroups: true,
	})
	wantCode(t, err, "NotImplemented")
}

// iotCA is a certificate authority a test registers with IoT.
type iotCA struct {
	key  *ecdsa.PrivateKey
	cert *x509.Certificate
	pem  string
}

func newIoTCA(t *testing.T) *iotCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}

	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}

	return &iotCA{key: key, cert: cert, pem: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
}

// sign issues a certificate for commonName and returns its PEM.
func (ca *iotCA) sign(t *testing.T, commonName string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: commonName},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}

	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func pemID(t *testing.T, data string) string {
	t.Helper()
	block, _ := pem.Decode([]byte(data))
	if block == nil {
		t.Fatalf("no PEM in %q", data)
	}

	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:])
}

func TestIoTCertificates(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := iotClient(s, "test")

	created, err := c.CreateKeysAndCertificate(ctx, &iot.CreateKeysAndCertificateInput{SetAsActive: true})
	if err != nil {
		t.Fatal(err)
	}

	id := aws.ToString(created.CertificateId)
	if id != pemID(t, aws.ToString(created.CertificatePem)) ||
		!strings.Contains(aws.ToString(created.KeyPair.PrivateKey), "BEGIN RSA PRIVATE KEY") ||
		aws.ToString(created.CertificateArn) != "arn:aws:iot:us-east-1:000000000000:cert/"+id {
		t.Fatalf("%+v", created)
	}

	described, err := c.DescribeCertificate(ctx, &iot.DescribeCertificateInput{CertificateId: &id})
	if err != nil || described.CertificateDescription.Status != iottypes.CertificateStatusActive ||
		aws.ToString(described.CertificateDescription.OwnedBy) != "000000000000" ||
		described.CertificateDescription.CertificateMode != iottypes.CertificateModeDefault {
		t.Fatalf("%+v %v", described, err)
	}

	// An active certificate cannot be deleted, and a revoked one stays revoked.
	_, err = c.DeleteCertificate(ctx, &iot.DeleteCertificateInput{CertificateId: &id})
	wantCode(t, err, "CertificateStateException")
	if _, err = c.UpdateCertificate(ctx, &iot.UpdateCertificateInput{
		CertificateId: &id, NewStatus: iottypes.CertificateStatusRevoked,
	}); err != nil {
		t.Fatal(err)
	}

	_, err = c.UpdateCertificate(ctx, &iot.UpdateCertificateInput{
		CertificateId: &id, NewStatus: iottypes.CertificateStatusActive,
	})
	wantCode(t, err, "CertificateStateException")
	_, err = c.UpdateCertificate(ctx, &iot.UpdateCertificateInput{
		CertificateId: &id, NewStatus: iottypes.CertificateStatusPendingTransfer,
	})
	wantCode(t, err, "InvalidRequestException")

	// A CSR for an ECDSA key is signed; an Ed25519 key is refused.
	csrKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	fromCSR, err := c.CreateCertificateFromCsr(ctx, &iot.CreateCertificateFromCsrInput{
		CertificateSigningRequest: aws.String(csrPEM(t, csrKey, "device-7")),
	})
	if err != nil {
		t.Fatal(err)
	}

	block, _ := pem.Decode([]byte(aws.ToString(fromCSR.CertificatePem)))
	issued, err := x509.ParseCertificate(block.Bytes)
	if err != nil || issued.Subject.CommonName != "device-7" {
		t.Fatalf("%+v %v", issued, err)
	}

	_, edKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateCertificateFromCsr(ctx, &iot.CreateCertificateFromCsrInput{
		CertificateSigningRequest: aws.String(csrPEM(t, edKey, "x")),
	})
	wantCode(t, err, "InvalidRequestException")

	listed, err := c.ListCertificates(ctx, &iot.ListCertificatesInput{AscendingOrder: true})
	if err != nil || len(listed.Certificates) != 2 || aws.ToString(listed.Certificates[0].CertificateId) != id {
		t.Fatalf("%+v %v", listed, err)
	}

	if _, err = c.DeleteCertificate(ctx, &iot.DeleteCertificateInput{CertificateId: &id}); err != nil {
		t.Fatal(err)
	}

	_, err = c.DescribeCertificate(ctx, &iot.DescribeCertificateInput{CertificateId: &id})
	wantCode(t, err, "ResourceNotFoundException")
}

func csrPEM(t *testing.T, key any, commonName string) string {
	t.Helper()
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: commonName},
	}, key)
	if err != nil {
		t.Fatal(err)
	}

	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
}

func TestIoTCACertificates(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := iotClient(s, "test")
	ca := newIoTCA(t)

	code, err := c.GetRegistrationCode(ctx, &iot.GetRegistrationCodeInput{})
	if err != nil || len(aws.ToString(code.RegistrationCode)) != 64 {
		t.Fatalf("%+v %v", code, err)
	}

	again, err := c.GetRegistrationCode(ctx, &iot.GetRegistrationCodeInput{})
	if err != nil || aws.ToString(again.RegistrationCode) != aws.ToString(code.RegistrationCode) {
		t.Fatalf("%+v %v", again, err)
	}

	// The verification certificate must name the registration code.
	_, err = c.RegisterCACertificate(ctx, &iot.RegisterCACertificateInput{
		CaCertificate: &ca.pem, VerificationCertificate: aws.String(ca.sign(t, "wrong")),
	})
	wantCode(t, err, "RegistrationCodeValidationException")
	_, err = c.RegisterCACertificate(ctx, &iot.RegisterCACertificateInput{CaCertificate: &ca.pem})
	wantCode(t, err, "InvalidRequestException")
	_, err = c.RegisterCACertificate(ctx, &iot.RegisterCACertificateInput{
		CaCertificate: aws.String(ca.sign(t, "leaf")), CertificateMode: iottypes.CertificateModeSniOnly,
	})
	wantCode(t, err, "CertificateValidationException")

	registered, err := c.RegisterCACertificate(ctx, &iot.RegisterCACertificateInput{
		CaCertificate: &ca.pem, VerificationCertificate: aws.String(ca.sign(t, aws.ToString(code.RegistrationCode))),
		SetAsActive: true, Tags: []iottypes.Tag{{Key: aws.String("env"), Value: aws.String("test")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	caID := aws.ToString(registered.CertificateId)
	if caID != pemID(t, ca.pem) {
		t.Fatalf("%+v", registered)
	}

	// A device certificate the CA signed is registered under it, found
	// without naming the CA.
	device, err := c.RegisterCertificate(ctx, &iot.RegisterCertificateInput{
		CertificatePem: aws.String(ca.sign(t, "device")), Status: iottypes.CertificateStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}

	described, err := c.DescribeCertificate(ctx, &iot.DescribeCertificateInput{CertificateId: device.CertificateId})
	if err != nil || aws.ToString(described.CertificateDescription.CaCertificateId) != caID ||
		described.CertificateDescription.Status != iottypes.CertificateStatusActive {
		t.Fatalf("%+v %v", described, err)
	}

	byCA, err := c.ListCertificatesByCA(ctx, &iot.ListCertificatesByCAInput{CaCertificateId: &caID})
	if err != nil || len(byCA.Certificates) != 1 {
		t.Fatalf("%+v %v", byCA, err)
	}

	// A certificate no registered CA signed needs RegisterCertificateWithoutCA.
	stranger := newIoTCA(t).sign(t, "stranger")
	_, err = c.RegisterCertificate(ctx, &iot.RegisterCertificateInput{CertificatePem: &stranger})
	wantCode(t, err, "CertificateValidationException")
	sni, err := c.RegisterCertificateWithoutCA(ctx, &iot.RegisterCertificateWithoutCAInput{CertificatePem: &stranger})
	if err != nil {
		t.Fatal(err)
	}

	described, err = c.DescribeCertificate(ctx, &iot.DescribeCertificateInput{CertificateId: sni.CertificateId})
	if err != nil || described.CertificateDescription.CertificateMode != iottypes.CertificateModeSniOnly ||
		described.CertificateDescription.Status != iottypes.CertificateStatusInactive {
		t.Fatalf("%+v %v", described, err)
	}

	_, err = c.RegisterCertificateWithoutCA(ctx, &iot.RegisterCertificateWithoutCAInput{CertificatePem: &stranger})
	wantCode(t, err, "ResourceAlreadyExistsException")

	tags, err := c.ListTagsForResource(ctx, &iot.ListTagsForResourceInput{ResourceArn: registered.CertificateArn})
	if err != nil || len(tags.Tags) != 1 || aws.ToString(tags.Tags[0].Value) != "test" {
		t.Fatalf("%+v %v", tags, err)
	}

	// A CA must be inactive, and sign no registered certificate, to be deleted.
	_, err = c.DeleteCACertificate(ctx, &iot.DeleteCACertificateInput{CertificateId: &caID})
	wantCode(t, err, "CertificateStateException")
	if _, err = c.UpdateCACertificate(ctx, &iot.UpdateCACertificateInput{
		CertificateId: &caID, NewStatus: iottypes.CACertificateStatusInactive,
	}); err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteCACertificate(ctx, &iot.DeleteCACertificateInput{CertificateId: &caID})
	wantCode(t, err, "CertificateStateException")
	if _, err = c.UpdateCertificate(ctx, &iot.UpdateCertificateInput{
		CertificateId: device.CertificateId, NewStatus: iottypes.CertificateStatusInactive,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeleteCertificate(ctx, &iot.DeleteCertificateInput{CertificateId: device.CertificateId}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeleteCACertificate(ctx, &iot.DeleteCACertificateInput{CertificateId: &caID}); err != nil {
		t.Fatal(err)
	}

	cas, err := c.ListCACertificates(ctx, &iot.ListCACertificatesInput{})
	if err != nil || len(cas.Certificates) != 0 {
		t.Fatalf("%+v %v", cas, err)
	}

	// Automatic registration needs the broker, so it is not supported yet.
	_, err = c.RegisterCACertificate(ctx, &iot.RegisterCACertificateInput{
		CaCertificate: &ca.pem, CertificateMode: iottypes.CertificateModeSniOnly, AllowAutoRegistration: true,
	})
	wantCode(t, err, "NotImplemented")
}

const iotPolicy = `{
  "Version": "2012-10-17",
  "Statement": [
    {"Effect": "Allow", "Action": "iot:Connect",
     "Resource": "arn:aws:iot:us-east-1:000000000000:client/${iot:Connection.Thing.ThingName}"},
    {"Effect": "Allow", "Action": ["iot:Publish", "iot:Receive"], "Resource": "*",
     "Condition": {"Bool": {"iot:Connection.Thing.IsAttached": "true"}}}
  ]
}`

func TestIoTPolicies(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := iotClient(s, "test")

	created, err := c.CreatePolicy(ctx, &iot.CreatePolicyInput{
		PolicyName: aws.String("devices"), PolicyDocument: aws.String(iotPolicy),
	})
	if err != nil || aws.ToString(created.PolicyVersionId) != "1" ||
		aws.ToString(created.PolicyArn) != "arn:aws:iot:us-east-1:000000000000:policy/devices" {
		t.Fatalf("%+v %v", created, err)
	}

	_, err = c.CreatePolicy(ctx, &iot.CreatePolicyInput{
		PolicyName: aws.String("devices"), PolicyDocument: aws.String(iotPolicy),
	})
	wantCode(t, err, "ResourceAlreadyExistsException")

	// Documents outside the evaluated subset are refused.
	for _, doc := range []string{
		`{"Statement": {"Effect": "Allow", "Action": "iot:*", "Resource": "*", "Principal": "*"}}`,
		`{"Statement": {"Effect": "Allow", "Action": "iot:*", "Resource": "*",
		  "Condition": {"IpAddress": {"aws:SourceIp": "10.0.0.0/8"}}}}`,
		`{"Statement": {"Effect": "Allow", "Action": "iot:*", "Resource": "${iot:Unknown}"}}`,
		`{"Statement": {"Effect": "Maybe", "Action": "iot:*", "Resource": "*"}}`,
		`{"Statement": []}`,
		`not json`,
	} {
		_, err = c.CreatePolicy(ctx, &iot.CreatePolicyInput{PolicyName: aws.String("bad"), PolicyDocument: &doc})
		wantCode(t, err, "MalformedPolicyException")
	}

	for i := 2; i <= 5; i++ {
		v, versionErr := c.CreatePolicyVersion(ctx, &iot.CreatePolicyVersionInput{
			PolicyName: aws.String("devices"), PolicyDocument: aws.String(iotPolicy), SetAsDefault: i == 3,
		})
		if versionErr != nil || aws.ToString(v.PolicyVersionId) != strconv.Itoa(i) || v.IsDefaultVersion != (i == 3) {
			t.Fatalf("%+v %v", v, versionErr)
		}
	}

	_, err = c.CreatePolicyVersion(ctx, &iot.CreatePolicyVersionInput{
		PolicyName: aws.String("devices"), PolicyDocument: aws.String(iotPolicy),
	})
	wantCode(t, err, "VersionsLimitExceededException")

	got, err := c.GetPolicy(ctx, &iot.GetPolicyInput{PolicyName: aws.String("devices")})
	if err != nil || aws.ToString(got.DefaultVersionId) != "3" || aws.ToString(got.PolicyDocument) != iotPolicy {
		t.Fatalf("%+v %v", got, err)
	}

	_, err = c.DeletePolicyVersion(ctx, &iot.DeletePolicyVersionInput{
		PolicyName: aws.String("devices"), PolicyVersionId: aws.String("3"),
	})
	wantCode(t, err, "DeleteConflictException")
	if _, err = c.SetDefaultPolicyVersion(ctx, &iot.SetDefaultPolicyVersionInput{
		PolicyName: aws.String("devices"), PolicyVersionId: aws.String("5"),
	}); err != nil {
		t.Fatal(err)
	}

	versions, err := c.ListPolicyVersions(ctx, &iot.ListPolicyVersionsInput{PolicyName: aws.String("devices")})
	if err != nil || len(versions.PolicyVersions) != 5 || aws.ToString(versions.PolicyVersions[0].VersionId) != "5" ||
		!versions.PolicyVersions[0].IsDefaultVersion {
		t.Fatalf("%+v %v", versions, err)
	}

	version, err := c.GetPolicyVersion(ctx, &iot.GetPolicyVersionInput{
		PolicyName: aws.String("devices"), PolicyVersionId: aws.String("2"),
	})
	if err != nil || version.IsDefaultVersion || aws.ToString(version.PolicyDocument) != iotPolicy {
		t.Fatalf("%+v %v", version, err)
	}

	// A policy is deleted only once its other versions are.
	_, err = c.DeletePolicy(ctx, &iot.DeletePolicyInput{PolicyName: aws.String("devices")})
	wantCode(t, err, "DeleteConflictException")
	for _, v := range []string{"1", "2", "3", "4"} {
		if _, err = c.DeletePolicyVersion(ctx, &iot.DeletePolicyVersionInput{
			PolicyName: aws.String("devices"), PolicyVersionId: aws.String(v),
		}); err != nil {
			t.Fatal(err)
		}
	}

	listed, err := c.ListPolicies(ctx, &iot.ListPoliciesInput{AscendingOrder: true})
	if err != nil || len(listed.Policies) != 1 {
		t.Fatalf("%+v %v", listed, err)
	}

	if _, err = c.DeletePolicy(ctx, &iot.DeletePolicyInput{PolicyName: aws.String("devices")}); err != nil {
		t.Fatal(err)
	}
}

func TestIoTAttachments(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := iotClient(s, "test")

	cert, err := c.CreateKeysAndCertificate(ctx, &iot.CreateKeysAndCertificateInput{})
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"direct", "inherited"} {
		if _, err = c.CreatePolicy(ctx, &iot.CreatePolicyInput{
			PolicyName: aws.String(name), PolicyDocument: aws.String(iotPolicy),
		}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err = c.CreateThingGroup(ctx, &iot.CreateThingGroupInput{ThingGroupName: aws.String("fleet")}); err != nil {
		t.Fatal(err)
	}

	for _, th := range []string{"one", "two"} {
		if _, err = c.CreateThing(ctx, &iot.CreateThingInput{ThingName: aws.String(th)}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err = c.AddThingToThingGroup(ctx, &iot.AddThingToThingGroupInput{
		ThingName: aws.String("one"), ThingGroupName: aws.String("fleet"),
	}); err != nil {
		t.Fatal(err)
	}

	groupARN := "arn:aws:iot:us-east-1:000000000000:thinggroup/fleet"
	for _, attach := range []struct{ policy, target string }{
		{"direct", aws.ToString(cert.CertificateArn)}, {"inherited", groupARN},
	} {
		if _, err = c.AttachPolicy(ctx, &iot.AttachPolicyInput{
			PolicyName: aws.String(attach.policy), Target: aws.String(attach.target),
		}); err != nil {
			t.Fatal(err)
		}
	}

	_, err = c.AttachPolicy(ctx, &iot.AttachPolicyInput{
		PolicyName: aws.String("direct"), Target: aws.String("arn:aws:iam::000000000000:role/r"),
	})
	wantCode(t, err, "InvalidRequestException")

	if _, err = c.AttachThingPrincipal(ctx, &iot.AttachThingPrincipalInput{
		ThingName: aws.String("one"), Principal: cert.CertificateArn,
		ThingPrincipalType: iottypes.ThingPrincipalTypeExclusiveThing,
	}); err != nil {
		t.Fatal(err)
	}

	// An exclusive principal is attached to one thing only.
	_, err = c.AttachThingPrincipal(ctx, &iot.AttachThingPrincipalInput{
		ThingName: aws.String("two"), Principal: cert.CertificateArn,
	})
	wantCode(t, err, "InvalidRequestException")

	principals, err := c.ListThingPrincipals(ctx, &iot.ListThingPrincipalsInput{ThingName: aws.String("one")})
	if err != nil || !slices.Equal(principals.Principals, []string{aws.ToString(cert.CertificateArn)}) {
		t.Fatalf("%+v %v", principals, err)
	}

	things, err := c.ListPrincipalThings(ctx, &iot.ListPrincipalThingsInput{Principal: cert.CertificateArn})
	if err != nil || !slices.Equal(things.Things, []string{"one"}) {
		t.Fatalf("%+v %v", things, err)
	}

	// The certificate's own policies, and with recursive, its thing's group's.
	attached, err := c.ListAttachedPolicies(ctx, &iot.ListAttachedPoliciesInput{Target: cert.CertificateArn})
	if err != nil || len(attached.Policies) != 1 || aws.ToString(attached.Policies[0].PolicyName) != "direct" {
		t.Fatalf("%+v %v", attached, err)
	}

	attached, err = c.ListAttachedPolicies(ctx, &iot.ListAttachedPoliciesInput{
		Target: cert.CertificateArn, Recursive: true,
	})
	if err != nil || len(attached.Policies) != 2 {
		t.Fatalf("%+v %v", attached, err)
	}

	targets, err := c.ListTargetsForPolicy(ctx, &iot.ListTargetsForPolicyInput{PolicyName: aws.String("inherited")})
	if err != nil || !slices.Equal(targets.Targets, []string{groupARN}) {
		t.Fatalf("%+v %v", targets, err)
	}

	// Attachments stop deletes until they are removed.
	_, err = c.DeleteThing(ctx, &iot.DeleteThingInput{ThingName: aws.String("one")})
	wantCode(t, err, "InvalidRequestException")
	_, err = c.DeletePolicy(ctx, &iot.DeletePolicyInput{PolicyName: aws.String("direct")})
	wantCode(t, err, "DeleteConflictException")
	_, err = c.DeleteCertificate(ctx, &iot.DeleteCertificateInput{CertificateId: cert.CertificateId})
	wantCode(t, err, "DeleteConflictException")

	if _, err = c.DetachThingPrincipal(ctx, &iot.DetachThingPrincipalInput{
		ThingName: aws.String("one"), Principal: cert.CertificateArn,
	}); err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteCertificate(ctx, &iot.DeleteCertificateInput{CertificateId: cert.CertificateId})
	wantCode(t, err, "DeleteConflictException")

	// forceDelete detaches the certificate's policies.
	if _, err = c.DeleteCertificate(ctx, &iot.DeleteCertificateInput{
		CertificateId: cert.CertificateId, ForceDelete: true,
	}); err != nil {
		t.Fatal(err)
	}

	targets, err = c.ListTargetsForPolicy(ctx, &iot.ListTargetsForPolicyInput{PolicyName: aws.String("direct")})
	if err != nil || len(targets.Targets) != 0 {
		t.Fatalf("%+v %v", targets, err)
	}

	if _, err = c.DeletePolicy(ctx, &iot.DeletePolicyInput{PolicyName: aws.String("direct")}); err != nil {
		t.Fatal(err)
	}

	// Deleting the group detaches its policy.
	if _, err = c.DeleteThingGroup(ctx, &iot.DeleteThingGroupInput{ThingGroupName: aws.String("fleet")}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeletePolicy(ctx, &iot.DeletePolicyInput{PolicyName: aws.String("inherited")}); err != nil {
		t.Fatal(err)
	}

	// Cognito identities are principals too.
	identity := "us-east-1:0f8f1e0c-1111-4222-8333-444455556666"
	if _, err = c.AttachThingPrincipal(ctx, &iot.AttachThingPrincipalInput{
		ThingName: aws.String("two"), Principal: aws.String(identity),
	}); err != nil {
		t.Fatal(err)
	}

	things, err = c.ListPrincipalThings(ctx, &iot.ListPrincipalThingsInput{Principal: aws.String(identity)})
	if err != nil || !slices.Equal(things.Things, []string{"two"}) {
		t.Fatalf("%+v %v", things, err)
	}
}

func TestIoTEndpointsAndTags(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := iotClient(s, "test")

	ats, err := c.DescribeEndpoint(ctx, &iot.DescribeEndpointInput{EndpointType: aws.String("iot:Data-ATS")})
	if err != nil {
		t.Fatal(err)
	}

	prefix, ok := strings.CutSuffix(aws.ToString(ats.EndpointAddress), "-ats.iot.us-east-1.localhost")
	if !ok || len(prefix) != 14 {
		t.Fatalf("%+v", ats)
	}

	legacy, err := c.DescribeEndpoint(ctx, &iot.DescribeEndpointInput{})
	if err != nil || aws.ToString(legacy.EndpointAddress) != prefix+".iot.us-east-1.localhost" {
		t.Fatalf("%+v %v", legacy, err)
	}

	_, err = c.DescribeEndpoint(ctx, &iot.DescribeEndpointInput{EndpointType: aws.String("iot:Other")})
	wantCode(t, err, "InvalidRequestException")
	other, err := iotClient(s, "team-b").DescribeEndpoint(ctx, &iot.DescribeEndpointInput{
		EndpointType: aws.String("iot:Data-ATS"),
	})
	if err != nil || strings.HasPrefix(aws.ToString(other.EndpointAddress), prefix) {
		t.Fatalf("%+v %v", other, err)
	}

	group, err := c.CreateThingGroup(ctx, &iot.CreateThingGroupInput{
		ThingGroupName: aws.String("tagged"), Tags: []iottypes.Tag{{Key: aws.String("a"), Value: aws.String("1")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = c.TagResource(ctx, &iot.TagResourceInput{
		ResourceArn: group.ThingGroupArn, Tags: []iottypes.Tag{{Key: aws.String("b"), Value: aws.String("2")}},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.UntagResource(ctx, &iot.UntagResourceInput{
		ResourceArn: group.ThingGroupArn, TagKeys: []string{"a"},
	}); err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListTagsForResource(ctx, &iot.ListTagsForResourceInput{ResourceArn: group.ThingGroupArn})
	if err != nil || len(tags.Tags) != 1 || aws.ToString(tags.Tags[0].Key) != "b" {
		t.Fatalf("%+v %v", tags, err)
	}

	_, err = c.ListTagsForResource(ctx, &iot.ListTagsForResourceInput{
		ResourceArn: aws.String("arn:aws:iot:us-east-1:000000000000:thing/none"),
	})
	wantCode(t, err, "InvalidRequestException")
}
