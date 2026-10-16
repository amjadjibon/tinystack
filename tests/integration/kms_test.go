package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
)

func kmsClient(s *httptest.Server, key, region string) *kms.Client {
	return kms.New(kms.Options{
		Region:       region,
		BaseEndpoint: aws.String(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: key, SecretAccessKey: "test"}, nil
		}),
		Retryer: aws.NopRetryer{},
	})
}

func createKey(t *testing.T, c *kms.Client, description string) *kmstypes.KeyMetadata {
	t.Helper()
	out, err := c.CreateKey(context.Background(), &kms.CreateKeyInput{Description: aws.String(description)})
	if err != nil {
		t.Fatal(err)
	}

	return out.KeyMetadata
}

func TestKMSKeys(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := kmsClient(s, "test", "us-east-1")
	created, err := c.CreateKey(ctx, &kms.CreateKeyInput{
		Description: aws.String("orders"),
		Tags:        []kmstypes.Tag{{TagKey: aws.String("team"), TagValue: aws.String("core")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	m := created.KeyMetadata
	id := aws.ToString(m.KeyId)
	if m.KeyState != kmstypes.KeyStateEnabled || !m.Enabled || m.KeySpec != kmstypes.KeySpecSymmetricDefault ||
		m.KeyUsage != kmstypes.KeyUsageTypeEncryptDecrypt || aws.ToString(m.Description) != "orders" ||
		aws.ToString(m.Arn) != "arn:aws:kms:us-east-1:000000000000:key/"+id || m.CreationDate == nil ||
		!slices.Equal(m.EncryptionAlgorithms, []kmstypes.EncryptionAlgorithmSpec{"SYMMETRIC_DEFAULT"}) {
		t.Fatalf("metadata = %+v", m)
	}

	// Keys are described by ID, ARN, alias name, or alias ARN.
	if _, err = c.CreateAlias(ctx, &kms.CreateAliasInput{
		AliasName: aws.String("alias/orders"), TargetKeyId: aws.String(id),
	}); err != nil {
		t.Fatal(err)
	}

	for _, ref := range []string{
		id, aws.ToString(m.Arn), "alias/orders", "arn:aws:kms:us-east-1:000000000000:alias/orders",
	} {
		described, describeErr := c.DescribeKey(ctx, &kms.DescribeKeyInput{KeyId: aws.String(ref)})
		if describeErr != nil || aws.ToString(described.KeyMetadata.KeyId) != id {
			t.Fatalf("describe %s: %+v %v", ref, described, describeErr)
		}
	}

	for ref, want := range map[string]string{
		"not-a-key":        "NotFoundException",
		"alias/missing":    "NotFoundException",
		"arn:aws:s3:::key": "InvalidArnException",
		"arn:aws:kms:eu-west-1:000000000000:key/" + id:    "NotFoundException",
		"00000000-0000-4000-8000-000000000000":            "NotFoundException",
		"arn:aws:kms:us-east-1:111111111111:alias/orders": "NotFoundException",
	} {
		_, err = c.DescribeKey(ctx, &kms.DescribeKeyInput{KeyId: aws.String(ref)})
		code(t, err, want)
	}

	// Other regions and accounts do not see the key.
	for _, other := range []*kms.Client{kmsClient(s, "test", "eu-west-1"), kmsClient(s, "team-b", "us-east-1")} {
		listed, listErr := other.ListKeys(ctx, &kms.ListKeysInput{})
		if listErr != nil || len(listed.Keys) != 0 {
			t.Fatalf("other scope keys = %+v %v", listed, listErr)
		}
	}

	// ListKeys pages in key ID order.
	ids := []string{id}
	for range 2 {
		ids = append(ids, aws.ToString(createKey(t, c, "extra").KeyId))
	}

	slices.Sort(ids)
	var listed []string
	in := &kms.ListKeysInput{Limit: aws.Int32(2)}
	for {
		page, listErr := c.ListKeys(ctx, in)
		if listErr != nil {
			t.Fatal(listErr)
		}

		for _, k := range page.Keys {
			listed = append(listed, aws.ToString(k.KeyId))
		}

		if !page.Truncated {
			break
		}

		in.Marker = page.NextMarker
	}

	if !slices.Equal(listed, ids) {
		t.Fatalf("listed %v, want %v", listed, ids)
	}

	// Description, policy, rotation, and tags.
	if _, err = c.UpdateKeyDescription(ctx, &kms.UpdateKeyDescriptionInput{
		KeyId: aws.String(id), Description: aws.String("orders v2"),
	}); err != nil {
		t.Fatal(err)
	}

	policy, err := c.GetKeyPolicy(ctx, &kms.GetKeyPolicyInput{KeyId: aws.String(id)})
	if err != nil || !strings.Contains(aws.ToString(policy.Policy), "arn:aws:iam::000000000000:root") ||
		aws.ToString(policy.PolicyName) != "default" {
		t.Fatalf("policy = %+v %v", policy, err)
	}

	custom := `{"Version":"2012-10-17","Statement":{"Effect":"Allow","Principal":"*","Action":"kms:*","Resource":"*"}}`
	if _, err = c.PutKeyPolicy(
		ctx,
		&kms.PutKeyPolicyInput{KeyId: aws.String(id), Policy: aws.String(custom)},
	); err != nil {
		t.Fatal(err)
	}

	_, err = c.PutKeyPolicy(ctx, &kms.PutKeyPolicyInput{KeyId: aws.String(id), Policy: aws.String("{nope")})
	code(t, err, "MalformedPolicyDocumentException")
	policies, err := c.ListKeyPolicies(ctx, &kms.ListKeyPoliciesInput{KeyId: aws.String(id)})
	if err != nil || !slices.Equal(policies.PolicyNames, []string{"default"}) {
		t.Fatalf("policies = %+v %v", policies, err)
	}

	if _, err = c.EnableKeyRotation(ctx, &kms.EnableKeyRotationInput{
		KeyId: aws.String(id), RotationPeriodInDays: aws.Int32(180),
	}); err != nil {
		t.Fatal(err)
	}

	rotation, err := c.GetKeyRotationStatus(ctx, &kms.GetKeyRotationStatusInput{KeyId: aws.String(id)})
	if err != nil || !rotation.KeyRotationEnabled || aws.ToInt32(rotation.RotationPeriodInDays) != 180 ||
		rotation.NextRotationDate == nil {
		t.Fatalf("rotation = %+v %v", rotation, err)
	}

	_, err = c.EnableKeyRotation(ctx, &kms.EnableKeyRotationInput{
		KeyId: aws.String(id), RotationPeriodInDays: aws.Int32(30),
	})
	code(t, err, "ValidationException")
	if _, err = c.TagResource(ctx, &kms.TagResourceInput{
		KeyId: aws.String(id), Tags: []kmstypes.Tag{{TagKey: aws.String("env"), TagValue: aws.String("dev")}},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.UntagResource(ctx, &kms.UntagResourceInput{
		KeyId: aws.String(id), TagKeys: []string{"team"},
	}); err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListResourceTags(ctx, &kms.ListResourceTagsInput{KeyId: aws.String(id)})
	if err != nil || len(tags.Tags) != 1 || aws.ToString(tags.Tags[0].TagKey) != "env" {
		t.Fatalf("tags = %+v %v", tags, err)
	}

	// Unsupported key types fail explicitly.
	for _, in := range []*kms.CreateKeyInput{
		{KeySpec: kmstypes.KeySpecEccSecgP256k1, KeyUsage: kmstypes.KeyUsageTypeSignVerify},
		{KeyUsage: kmstypes.KeyUsageTypeSignVerify},
		{Origin: kmstypes.OriginTypeExternal},
		{MultiRegion: aws.Bool(true)},
	} {
		_, err = c.CreateKey(ctx, in)
		code(t, err, "UnsupportedOperationException")
	}

	// A key pending deletion is described but not usable; cancelling leaves it disabled.
	scheduled, err := c.ScheduleKeyDeletion(ctx, &kms.ScheduleKeyDeletionInput{
		KeyId: aws.String(id), PendingWindowInDays: aws.Int32(7),
	})
	if err != nil || scheduled.KeyState != kmstypes.KeyStatePendingDeletion ||
		aws.ToInt32(scheduled.PendingWindowInDays) != 7 ||
		time.Until(*scheduled.DeletionDate) < 6*24*time.Hour {
		t.Fatalf("scheduled = %+v %v", scheduled, err)
	}

	described, err := c.DescribeKey(ctx, &kms.DescribeKeyInput{KeyId: aws.String(id)})
	if err != nil || described.KeyMetadata.KeyState != kmstypes.KeyStatePendingDeletion ||
		described.KeyMetadata.Enabled || described.KeyMetadata.DeletionDate == nil {
		t.Fatalf("pending = %+v %v", described, err)
	}

	_, err = c.EnableKey(ctx, &kms.EnableKeyInput{KeyId: aws.String(id)})
	code(t, err, "KMSInvalidStateException")
	_, err = c.Encrypt(ctx, &kms.EncryptInput{KeyId: aws.String(id), Plaintext: []byte("x")})
	code(t, err, "KMSInvalidStateException")
	_, err = c.ScheduleKeyDeletion(ctx, &kms.ScheduleKeyDeletionInput{KeyId: aws.String(id)})
	code(t, err, "KMSInvalidStateException")
	if _, err = c.CancelKeyDeletion(ctx, &kms.CancelKeyDeletionInput{KeyId: aws.String(id)}); err != nil {
		t.Fatal(err)
	}

	described, err = c.DescribeKey(ctx, &kms.DescribeKeyInput{KeyId: aws.String(id)})
	if err != nil || described.KeyMetadata.KeyState != kmstypes.KeyStateDisabled {
		t.Fatalf("cancelled = %+v %v", described, err)
	}

	_, err = c.Encrypt(ctx, &kms.EncryptInput{KeyId: aws.String(id), Plaintext: []byte("x")})
	code(t, err, "DisabledException")
	if _, err = c.EnableKey(ctx, &kms.EnableKeyInput{KeyId: aws.String(id)}); err != nil {
		t.Fatal(err)
	}
}

func TestKMSAliases(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := kmsClient(s, "test", "us-east-1")
	first, second := createKey(t, c, "first"), createKey(t, c, "second")
	create := func(name string, target *string) error {
		_, err := c.CreateAlias(ctx, &kms.CreateAliasInput{AliasName: aws.String(name), TargetKeyId: target})
		return err
	}

	for _, name := range []string{"alias/b", "alias/a/nested"} {
		if err := create(name, first.KeyId); err != nil {
			t.Fatal(err)
		}
	}

	code(t, create("alias/b", second.KeyId), "AlreadyExistsException")
	code(t, create("orders", first.KeyId), "InvalidAliasNameException")
	code(t, create("alias/aws/s3", first.KeyId), "InvalidAliasNameException")
	code(t, create("alias/c", aws.String("alias/b")), "NotFoundException")

	if _, err := c.UpdateAlias(ctx, &kms.UpdateAliasInput{
		AliasName: aws.String("alias/b"), TargetKeyId: second.Arn,
	}); err != nil {
		t.Fatal(err)
	}

	_, err := c.UpdateAlias(ctx, &kms.UpdateAliasInput{AliasName: aws.String("alias/none"), TargetKeyId: first.KeyId})
	code(t, err, "NotFoundException")

	all, err := c.ListAliases(ctx, &kms.ListAliasesInput{})
	if err != nil || len(all.Aliases) != 2 || aws.ToString(all.Aliases[0].AliasName) != "alias/a/nested" ||
		aws.ToString(all.Aliases[1].TargetKeyId) != aws.ToString(second.KeyId) ||
		aws.ToString(all.Aliases[1].AliasArn) != "arn:aws:kms:us-east-1:000000000000:alias/b" {
		t.Fatalf("aliases = %+v %v", all, err)
	}

	byKey, err := c.ListAliases(ctx, &kms.ListAliasesInput{KeyId: first.KeyId})
	if err != nil || len(byKey.Aliases) != 1 || aws.ToString(byKey.Aliases[0].AliasName) != "alias/a/nested" {
		t.Fatalf("aliases of first = %+v %v", byKey, err)
	}

	if _, err = c.DeleteAlias(ctx, &kms.DeleteAliasInput{AliasName: aws.String("alias/b")}); err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteAlias(ctx, &kms.DeleteAliasInput{AliasName: aws.String("alias/b")})
	code(t, err, "NotFoundException")

	// Aliases cannot point at a key pending deletion.
	if _, err = c.ScheduleKeyDeletion(ctx, &kms.ScheduleKeyDeletionInput{KeyId: second.KeyId}); err != nil {
		t.Fatal(err)
	}

	code(t, create("alias/b", second.KeyId), "KMSInvalidStateException")
}

func TestKMSEncryption(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := kmsClient(s, "test", "us-east-1")
	k := createKey(t, c, "data")
	other := createKey(t, c, "other")
	if _, err := c.CreateAlias(ctx, &kms.CreateAliasInput{
		AliasName: aws.String("alias/data"), TargetKeyId: k.KeyId,
	}); err != nil {
		t.Fatal(err)
	}

	encryptionContext := map[string]string{"tenant": "a", "purpose": "test"}
	encrypted, err := c.Encrypt(ctx, &kms.EncryptInput{
		KeyId: aws.String("alias/data"), Plaintext: []byte("secret"), EncryptionContext: encryptionContext,
	})
	if err != nil || aws.ToString(encrypted.KeyId) != aws.ToString(k.Arn) ||
		bytes.Contains(encrypted.CiphertextBlob, []byte("secret")) {
		t.Fatalf("encrypted = %+v %v", encrypted, err)
	}

	// Decrypt finds the key in the ciphertext; the context must match.
	decrypted, err := c.Decrypt(ctx, &kms.DecryptInput{
		CiphertextBlob: encrypted.CiphertextBlob, EncryptionContext: encryptionContext,
	})
	if err != nil || string(decrypted.Plaintext) != "secret" || aws.ToString(decrypted.KeyId) != aws.ToString(k.Arn) {
		t.Fatalf("decrypted = %+v %v", decrypted, err)
	}

	decrypt := func(in *kms.DecryptInput) error {
		_, decryptErr := c.Decrypt(ctx, in)
		return decryptErr
	}

	code(t, decrypt(&kms.DecryptInput{CiphertextBlob: encrypted.CiphertextBlob}), "InvalidCiphertextException")
	code(t, decrypt(&kms.DecryptInput{
		CiphertextBlob: encrypted.CiphertextBlob, EncryptionContext: map[string]string{"tenant": "b"},
	}), "InvalidCiphertextException")
	code(t, decrypt(&kms.DecryptInput{
		CiphertextBlob: encrypted.CiphertextBlob, EncryptionContext: encryptionContext, KeyId: other.KeyId,
	}), "IncorrectKeyException")
	code(t, decrypt(&kms.DecryptInput{CiphertextBlob: []byte("garbage")}), "InvalidCiphertextException")
	tampered := slices.Clone(encrypted.CiphertextBlob)
	tampered[len(tampered)-1] ^= 1
	code(t, decrypt(&kms.DecryptInput{CiphertextBlob: tampered, EncryptionContext: encryptionContext}),
		"InvalidCiphertextException")

	// Another account cannot decrypt the ciphertext.
	_, err = kmsClient(s, "team-b", "us-east-1").Decrypt(ctx, &kms.DecryptInput{
		CiphertextBlob: encrypted.CiphertextBlob, EncryptionContext: encryptionContext,
	})
	code(t, err, "InvalidCiphertextException")

	// Data keys decrypt to the plaintext key.
	dataKey, err := c.GenerateDataKey(ctx, &kms.GenerateDataKeyInput{
		KeyId: k.KeyId, KeySpec: kmstypes.DataKeySpecAes256,
	})
	if err != nil || len(dataKey.Plaintext) != 32 {
		t.Fatalf("data key = %+v %v", dataKey, err)
	}

	decrypted, err = c.Decrypt(ctx, &kms.DecryptInput{CiphertextBlob: dataKey.CiphertextBlob})
	if err != nil || !bytes.Equal(decrypted.Plaintext, dataKey.Plaintext) {
		t.Fatalf("data key decrypted = %+v %v", decrypted, err)
	}

	withoutPlaintext, err := c.GenerateDataKeyWithoutPlaintext(ctx, &kms.GenerateDataKeyWithoutPlaintextInput{
		KeyId: k.KeyId, NumberOfBytes: aws.Int32(64),
	})
	if err != nil {
		t.Fatal(err)
	}

	decrypted, err = c.Decrypt(ctx, &kms.DecryptInput{CiphertextBlob: withoutPlaintext.CiphertextBlob})
	if err != nil || len(decrypted.Plaintext) != 64 {
		t.Fatalf("data key without plaintext = %+v %v", decrypted, err)
	}

	for _, in := range []*kms.GenerateDataKeyInput{
		{KeyId: k.KeyId},
		{KeyId: k.KeyId, KeySpec: kmstypes.DataKeySpecAes128, NumberOfBytes: aws.Int32(16)},
		{KeyId: k.KeyId, NumberOfBytes: aws.Int32(2000)},
	} {
		_, err = c.GenerateDataKey(ctx, in)
		code(t, err, "ValidationException")
	}

	// DryRun validates without encrypting.
	_, err = c.Encrypt(ctx, &kms.EncryptInput{KeyId: k.KeyId, Plaintext: []byte("x"), DryRun: aws.Bool(true)})
	code(t, err, "DryRunOperationException")
	_, err = c.Encrypt(ctx, &kms.EncryptInput{
		KeyId: k.KeyId, Plaintext: []byte("x"), EncryptionAlgorithm: kmstypes.EncryptionAlgorithmSpecRsaesOaepSha256,
	})
	code(t, err, "InvalidKeyUsageException")
	_, err = c.Encrypt(ctx, &kms.EncryptInput{KeyId: k.KeyId, Plaintext: make([]byte, 4097)})
	code(t, err, "ValidationException")

	// A disabled key keeps its ciphertexts but cannot decrypt them.
	if _, err = c.DisableKey(ctx, &kms.DisableKeyInput{KeyId: k.KeyId}); err != nil {
		t.Fatal(err)
	}

	code(t, decrypt(&kms.DecryptInput{CiphertextBlob: dataKey.CiphertextBlob}), "DisabledException")

	random, err := c.GenerateRandom(ctx, &kms.GenerateRandomInput{NumberOfBytes: aws.Int32(24)})
	if err != nil || len(random.Plaintext) != 24 {
		t.Fatalf("random = %+v %v", random, err)
	}
}

// jsonCall sends an unsigned AWS JSON 1.1 request and returns the body and status.
func jsonCall(t *testing.T, s *httptest.Server, target, body string) (out []byte, status int) {
	t.Helper()
	r, err := http.NewRequest(http.MethodPost, s.URL, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	r.Header.Set("X-Amz-Target", target)
	r.Header.Set("Content-Type", "application/x-amz-json-1.1")
	resp, err := s.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}

	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()

	out, err = io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	return out, resp.StatusCode
}

// TestKMSWire checks the AWS JSON 1.1 envelope of a raw request.
func TestKMSWire(t *testing.T) {
	s := server(t)
	body, status := jsonCall(t, s, "TrentService.CreateKey", `{"Description":"raw"}`)
	var out struct {
		KeyMetadata struct{ KeyId, Description string }
	}
	if status != 200 || json.Unmarshal(body, &out) != nil || out.KeyMetadata.Description != "raw" {
		t.Fatalf("CreateKey = %d %s", status, body)
	}

	body, status = jsonCall(t, s, "TrentService.DescribeKey", `{"KeyId":"alias/missing"}`)
	if status != http.StatusNotFound || !strings.Contains(string(body), `"__type":"NotFoundException"`) {
		t.Fatalf("DescribeKey = %d %s", status, body)
	}
}
