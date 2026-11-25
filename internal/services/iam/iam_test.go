package iam

import (
	"context"
	"encoding/base32"
	"strings"
	"testing"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/iam"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	iampolicy "github.com/amjadjibon/tinystack/internal/services/iam/policy"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestAWSManagedCatalogValid(t *testing.T) {
	seenARN := map[string]bool{}
	seenID := map[string]bool{}
	for _, entry := range awsManagedCatalog {
		if !policyPath.MatchString(entry.Path) {
			t.Fatalf("invalid path %q for %s", entry.Path, entry.Name)
		}

		if !policyName.MatchString(entry.Name) {
			t.Fatalf("invalid name %q", entry.Name)
		}

		policyARN := entry.arn()
		if seenARN[policyARN] {
			t.Fatalf("duplicate ARN %s", policyARN)
		}

		seenARN[policyARN] = true
		id := entry.id()
		if !strings.HasPrefix(id, "ANPA") || len(id) != 20 {
			t.Fatalf("invalid ID %q for %s", id, policyARN)
		}

		if seenID[id] {
			t.Fatalf("duplicate ID %s", id)
		}

		seenID[id] = true
		if err := iampolicy.ValidateSimulation(entry.Document, false); err != nil {
			t.Fatalf("%s failed policy validation: %v", policyARN, err)
		}
	}
}

func TestTOTPVectorAndClock(t *testing.T) {
	// RFC 6238 Appendix B SHA1 test vector: secret "12345678901234567890", T=59 -> step 1 -> 94287082 (6 digits: 287082).
	secret := []byte("12345678901234567890")
	if got := totpCode(secret, 1); got != "287082" {
		t.Fatalf("got %s, want 287082", got)
	}

	svc := New(memory.New(nil, nil))
	fixed := time.Unix(1_700_000_010, 0).UTC()
	svc.UseClock(func() time.Time { return fixed })

	ctx := request.WithMetadata(context.Background(), request.Metadata{
		Scope: request.Scope{AccountID: "000000000000", Region: "us-east-1"},
	})
	if _, err := svc.CreateUser(ctx, &api.CreateUserRequest{UserName: new("totp-user")}); err != nil {
		t.Fatal(err)
	}

	created, err := svc.CreateVirtualMFADevice(ctx, &api.CreateVirtualMFADeviceRequest{
		VirtualMFADeviceName: new("device-1"),
	})
	if err != nil {
		t.Fatal(err)
	}

	raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(
		string(created.VirtualMFADevice.Base32StringSeed),
	)
	if err != nil {
		t.Fatal(err)
	}

	step := fixed.Unix() / totpStepSeconds
	if _, err = svc.EnableMFADevice(ctx, &api.EnableMFADeviceRequest{
		UserName:            new("totp-user"),
		SerialNumber:        created.VirtualMFADevice.SerialNumber,
		AuthenticationCode1: new(totpCode(raw, step)),
		AuthenticationCode2: new(totpCode(raw, step+1)),
	}); err != nil {
		t.Fatal(err)
	}

	// Advance clock by 3 minutes and resync with drifted device codes.
	fixed = fixed.Add(3 * time.Minute)
	newStep := fixed.Unix() / totpStepSeconds
	if _, err = svc.ResyncMFADevice(ctx, &api.ResyncMFADeviceRequest{
		UserName:            new("totp-user"),
		SerialNumber:        created.VirtualMFADevice.SerialNumber,
		AuthenticationCode1: new(totpCode(raw, newStep+5)),
		AuthenticationCode2: new(totpCode(raw, newStep+6)),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestLastUsedClock(t *testing.T) {
	svc := New(memory.New(nil, nil))
	fixed := time.Date(2026, 6, 15, 12, 30, 0, 0, time.UTC)
	svc.UseClock(func() time.Time { return fixed })

	ctx := request.WithMetadata(context.Background(), request.Metadata{
		Scope: request.Scope{AccountID: "000000000000", Region: "us-east-1"},
	})
	if _, err := svc.CreateUser(ctx, &api.CreateUserRequest{UserName: new("clock-user")}); err != nil {
		t.Fatal(err)
	}

	keyOut, err := svc.CreateAccessKey(ctx, &api.CreateAccessKeyRequest{UserName: new("clock-user")})
	if err != nil {
		t.Fatal(err)
	}

	fixed = fixed.Add(time.Hour)
	if err = svc.RecordAccessKeyUsed(ctx, *keyOut.AccessKey.AccessKeyId, "s3", "eu-central-1"); err != nil {
		t.Fatal(err)
	}

	lastUsed, err := svc.GetAccessKeyLastUsed(ctx, &api.GetAccessKeyLastUsedRequest{
		AccessKeyId: keyOut.AccessKey.AccessKeyId,
	})
	if err != nil {
		t.Fatal(err)
	}

	if lastUsed.AccessKeyLastUsed.LastUsedDate == nil ||
		!lastUsed.AccessKeyLastUsed.LastUsedDate.Equal(fixed) ||
		*lastUsed.AccessKeyLastUsed.ServiceName != "s3" ||
		*lastUsed.AccessKeyLastUsed.Region != "eu-central-1" {
		t.Fatalf("unexpected last used: %+v", lastUsed.AccessKeyLastUsed)
	}
}
