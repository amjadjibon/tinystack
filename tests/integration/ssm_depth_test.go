package integration_test

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

func TestSSMSecureStringUsesKMS(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := ssmClient(s, "test", "us-east-1")
	keys := kmsClient(s, "test", "us-east-1")

	_, err := c.PutParameter(ctx, &ssm.PutParameterInput{
		Name: aws.String("/secure/a"), Value: aws.String("hunter2"), Type: ssmtypes.ParameterTypeSecureString,
	})
	if err != nil {
		t.Fatal(err)
	}

	// The AWS managed key appears on first use and is not a customer key.
	managed, err := keys.DescribeKey(ctx, &kms.DescribeKeyInput{KeyId: aws.String("alias/aws/ssm")})
	if err != nil || managed.KeyMetadata.KeyManager != kmstypes.KeyManagerTypeAws {
		t.Fatalf("got %+v, %v", managed, err)
	}

	raw, err := c.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String("/secure/a")})
	if err != nil {
		t.Fatal(err)
	}

	blob, err := base64.StdEncoding.DecodeString(aws.ToString(raw.Parameter.Value))
	if err != nil {
		t.Fatalf("ciphertext is not base64: %v", err)
	}

	// The ciphertext is what KMS makes: KMS can decrypt it with the parameter's encryption context.
	plain, err := keys.Decrypt(ctx, &kms.DecryptInput{
		CiphertextBlob:    blob,
		EncryptionContext: map[string]string{"PARAMETER_ARN": aws.ToString(raw.Parameter.ARN)},
	})
	if err != nil || string(plain.Plaintext) != "hunter2" {
		t.Fatalf("got %+v, %v", plain, err)
	}

	// Decryption through every read path.
	got, err := c.GetParameters(
		ctx,
		&ssm.GetParametersInput{Names: []string{"/secure/a"}, WithDecryption: aws.Bool(true)},
	)
	if err != nil || aws.ToString(got.Parameters[0].Value) != "hunter2" {
		t.Fatalf("got %+v, %v", got, err)
	}

	byPath, err := c.GetParametersByPath(ctx, &ssm.GetParametersByPathInput{
		Path: aws.String("/secure"), WithDecryption: aws.Bool(true),
	})
	if err != nil || aws.ToString(byPath.Parameters[0].Value) != "hunter2" {
		t.Fatalf("got %+v, %v", byPath, err)
	}

	history, err := c.GetParameterHistory(ctx, &ssm.GetParameterHistoryInput{
		Name: aws.String("/secure/a"), WithDecryption: aws.Bool(true),
	})
	if err != nil || aws.ToString(history.Parameters[0].Value) != "hunter2" {
		t.Fatalf("got %+v, %v", history, err)
	}

	// An overwrite without a new key keeps the key; a long value is chunked.
	long := strings.Repeat("x", 6000)
	_, err = c.PutParameter(ctx, &ssm.PutParameterInput{
		Name: aws.String("/secure/a"), Value: aws.String(long), Overwrite: aws.Bool(true),
		Tier: ssmtypes.ParameterTierAdvanced,
	})
	if err != nil {
		t.Fatal(err)
	}

	again, err := c.GetParameter(
		ctx,
		&ssm.GetParameterInput{Name: aws.String("/secure/a"), WithDecryption: aws.Bool(true)},
	)
	if err != nil || aws.ToString(again.Parameter.Value) != long ||
		again.Parameter.Type != ssmtypes.ParameterTypeSecureString {
		t.Fatalf("long value: %v", err)
	}
}

func TestSSMSecureStringKeyStates(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := ssmClient(s, "test", "us-east-1")
	keys := kmsClient(s, "test", "us-east-1")
	id := aliasedKey(t, keys, "states")

	if _, err := c.PutParameter(ctx, &ssm.PutParameterInput{
		Name: aws.String("/k"), Value: aws.String("v"), Type: ssmtypes.ParameterTypeSecureString, KeyId: aws.String(id),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := keys.DisableKey(ctx, &kms.DisableKeyInput{KeyId: aws.String(id)}); err != nil {
		t.Fatal(err)
	}

	_, err := c.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String("/k"), WithDecryption: aws.Bool(true)})
	wantCode(t, err, "InvalidKeyId")

	_, err = c.PutParameter(ctx, &ssm.PutParameterInput{
		Name: aws.String("/k"), Value: aws.String("w"), Overwrite: aws.Bool(true),
	})
	wantCode(t, err, "InvalidKeyId")

	if _, err = keys.EnableKey(ctx, &kms.EnableKeyInput{KeyId: aws.String(id)}); err != nil {
		t.Fatal(err)
	}

	if _, err = keys.ScheduleKeyDeletion(ctx, &kms.ScheduleKeyDeletionInput{
		KeyId: aws.String(id), PendingWindowInDays: aws.Int32(7),
	}); err != nil {
		t.Fatal(err)
	}

	_, err = c.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String("/k"), WithDecryption: aws.Bool(true)})
	wantCode(t, err, "InvalidKeyId")

	// Reading the ciphertext needs no key.
	if _, err = c.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String("/k")}); err != nil {
		t.Fatal(err)
	}
}

func TestSSMParameterPolicies(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := ssmClient(s, "test", "us-east-1")

	future := time.Now().Add(24 * time.Hour).UTC().Format("2006-01-02T15:04:05.000Z")
	policies := `[{"Type":"Expiration","Version":"1.0","Attributes":{"Timestamp":"` + future + `"}},` +
		`{"Type":"ExpirationNotification","Version":"1.0","Attributes":{"Before":"2","Unit":"Days"}},` +
		`{"Type":"NoChangeNotification","Version":"1.0","Attributes":{"After":"20","Unit":"Days"}}]`

	// Policies need the advanced tier.
	_, err := c.PutParameter(ctx, &ssm.PutParameterInput{
		Name: aws.String(
			"/pol",
		),
		Value:    aws.String("v"),
		Type:     ssmtypes.ParameterTypeString,
		Policies: aws.String(policies),
	})
	wantCode(t, err, "ValidationException")

	out, err := c.PutParameter(ctx, &ssm.PutParameterInput{
		Name: aws.String("/pol"), Value: aws.String("v"), Type: ssmtypes.ParameterTypeString,
		Tier: ssmtypes.ParameterTierIntelligentTiering, Policies: aws.String(policies),
	})
	if err != nil || out.Tier != ssmtypes.ParameterTierAdvanced {
		t.Fatalf("got %+v, %v", out, err)
	}

	described, err := c.DescribeParameters(ctx, &ssm.DescribeParametersInput{})
	if err != nil || len(described.Parameters) != 1 || len(described.Parameters[0].Policies) != 3 {
		t.Fatalf("got %+v, %v", described, err)
	}

	first := described.Parameters[0].Policies[0]
	if aws.ToString(first.PolicyType) != "Expiration" || aws.ToString(first.PolicyStatus) != "Pending" ||
		!strings.Contains(aws.ToString(first.PolicyText), future) {
		t.Fatalf("got %+v", first)
	}

	history, err := c.GetParameterHistory(ctx, &ssm.GetParameterHistoryInput{Name: aws.String("/pol")})
	if err != nil || len(history.Parameters[0].Policies) != 3 {
		t.Fatalf("got %+v, %v", history, err)
	}

	invalid := map[string]string{
		"InvalidPolicyTypeException": `[{"Type":"Other","Version":"1.0","Attributes":{}}]`,
		"InvalidPolicyAttributeException": `[{"Type":"Expiration","Version":"1.0",` +
			`"Attributes":{"Timestamp":"2001-01-01T00:00:00Z"}}]`,
		"IncompatiblePolicyException": `[{"Type":"ExpirationNotification","Version":"1.0",` +
			`"Attributes":{"Before":"2","Unit":"Days"}}]`,
	}
	for code, text := range invalid {
		_, err = c.PutParameter(ctx, &ssm.PutParameterInput{
			Name: aws.String("/bad"), Value: aws.String("v"), Type: ssmtypes.ParameterTypeString,
			Tier: ssmtypes.ParameterTierAdvanced, Policies: aws.String(text),
		})
		wantCode(t, err, code)
	}

	// An overwrite without Policies keeps them, and an empty list clears them.
	_, err = c.PutParameter(ctx, &ssm.PutParameterInput{
		Name: aws.String("/pol"), Value: aws.String("w"), Overwrite: aws.Bool(true),
	})
	if err != nil {
		t.Fatal(err)
	}

	described, _ = c.DescribeParameters(ctx, &ssm.DescribeParametersInput{})
	if len(described.Parameters[0].Policies) != 3 {
		t.Fatalf("policies lost: %+v", described.Parameters[0])
	}

	_, err = c.PutParameter(ctx, &ssm.PutParameterInput{
		Name: aws.String("/pol"), Value: aws.String("x"), Overwrite: aws.Bool(true), Policies: aws.String("[]"),
	})
	if err != nil {
		t.Fatal(err)
	}

	described, _ = c.DescribeParameters(ctx, &ssm.DescribeParametersInput{})
	if len(described.Parameters[0].Policies) != 0 {
		t.Fatalf("policies kept: %+v", described.Parameters[0])
	}
}

func TestSSMParameterExpiration(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := ssmClient(s, "test", "us-east-1")

	soon := time.Now().Add(1500 * time.Millisecond).UTC().Format("2006-01-02T15:04:05.000Z")
	_, err := c.PutParameter(ctx, &ssm.PutParameterInput{
		Name: aws.String("/temp"), Value: aws.String("v"), Type: ssmtypes.ParameterTypeString,
		Tier:     ssmtypes.ParameterTierAdvanced,
		Policies: aws.String(`[{"Type":"Expiration","Version":"1.0","Attributes":{"Timestamp":"` + soon + `"}}]`),
	})
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err = c.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String("/temp")})
		if err != nil {
			wantCode(t, err, "ParameterNotFound")
			return
		}

		if time.Now().After(deadline) {
			t.Fatal("the expired parameter was not deleted")
		}

		time.Sleep(200 * time.Millisecond)
	}
}

const shellDocument = `{"schemaVersion":"2.2","description":"Say hello","parameters":{"who":{"type":"String",` +
	`"default":"world"},"name":{"type":"String"}},"mainSteps":[{"action":"aws:runShellScript","name":"hi",` +
	`"inputs":{"runCommand":["echo {{ who }}"]}}]}`

func TestSSMDocuments(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := ssmClient(s, "test", "us-east-1")

	created, err := c.CreateDocument(ctx, &ssm.CreateDocumentInput{
		Name: aws.String("hello"), Content: aws.String(shellDocument), DocumentType: ssmtypes.DocumentTypeCommand,
		VersionName: aws.String("first"), Tags: []ssmtypes.Tag{{Key: aws.String("team"), Value: aws.String("core")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	d := created.DocumentDescription
	if aws.ToString(d.DocumentVersion) != "1" || d.Status != ssmtypes.DocumentStatusActive ||
		aws.ToString(d.SchemaVersion) != "2.2" || len(d.Parameters) != 2 || len(d.PlatformTypes) != 2 ||
		aws.ToString(d.Hash) == "" || aws.ToString(d.Owner) == "" {
		t.Fatalf("got %+v", d)
	}

	_, err = c.CreateDocument(
		ctx,
		&ssm.CreateDocumentInput{Name: aws.String("hello"), Content: aws.String(shellDocument)},
	)
	wantCode(t, err, "DocumentAlreadyExists")

	_, err = c.CreateDocument(
		ctx,
		&ssm.CreateDocumentInput{Name: aws.String("aws-mine"), Content: aws.String(shellDocument)},
	)
	wantCode(t, err, "InvalidDocument")

	_, err = c.CreateDocument(ctx, &ssm.CreateDocumentInput{Name: aws.String("broken"), Content: aws.String("{")})
	wantCode(t, err, "InvalidDocumentContent")

	updated, err := c.UpdateDocument(ctx, &ssm.UpdateDocumentInput{
		Name: aws.String("hello"), Content: aws.String(strings.Replace(shellDocument, "Say hello", "Say hi", 1)),
		DocumentVersion: aws.String("$LATEST"),
	})
	if err != nil || aws.ToString(updated.DocumentDescription.DocumentVersion) != "2" ||
		aws.ToString(updated.DocumentDescription.DefaultVersion) != "1" {
		t.Fatalf("got %+v, %v", updated, err)
	}

	_, err = c.UpdateDocument(ctx, &ssm.UpdateDocumentInput{
		Name: aws.String("hello"), Content: aws.String(strings.Replace(shellDocument, "Say hello", "Say hi", 1)),
	})
	wantCode(t, err, "DuplicateDocumentContent")

	got, err := c.GetDocument(ctx, &ssm.GetDocumentInput{Name: aws.String("hello")})
	if err != nil || aws.ToString(got.DocumentVersion) != "1" ||
		!strings.Contains(aws.ToString(got.Content), "Say hello") {
		t.Fatalf("got %+v, %v", got, err)
	}

	yaml, err := c.GetDocument(ctx, &ssm.GetDocumentInput{
		Name: aws.String("hello"), DocumentVersion: aws.String("$LATEST"), DocumentFormat: ssmtypes.DocumentFormatYaml,
	})
	if err != nil || !strings.Contains(aws.ToString(yaml.Content), "schemaVersion:") ||
		strings.HasPrefix(aws.ToString(yaml.Content), "{") {
		t.Fatalf("got %+v, %v", yaml, err)
	}

	byName, err := c.GetDocument(
		ctx,
		&ssm.GetDocumentInput{Name: aws.String("hello"), VersionName: aws.String("first")},
	)
	if err != nil || aws.ToString(byName.DocumentVersion) != "1" {
		t.Fatalf("got %+v, %v", byName, err)
	}

	_, err = c.GetDocument(ctx, &ssm.GetDocumentInput{Name: aws.String("hello"), DocumentVersion: aws.String("9")})
	wantCode(t, err, "InvalidDocumentVersion")

	_, err = c.GetDocument(ctx, &ssm.GetDocumentInput{Name: aws.String("missing")})
	wantCode(t, err, "InvalidDocument")

	versions, err := c.ListDocumentVersions(ctx, &ssm.ListDocumentVersionsInput{Name: aws.String("hello")})
	if err != nil || len(versions.DocumentVersions) != 2 || !versions.DocumentVersions[0].IsDefaultVersion ||
		versions.DocumentVersions[1].IsDefaultVersion {
		t.Fatalf("got %+v, %v", versions, err)
	}

	def, err := c.UpdateDocumentDefaultVersion(ctx, &ssm.UpdateDocumentDefaultVersionInput{
		Name: aws.String("hello"), DocumentVersion: aws.String("2"),
	})
	if err != nil || aws.ToString(def.Description.DefaultVersion) != "2" {
		t.Fatalf("got %+v, %v", def, err)
	}

	_, err = c.DeleteDocument(
		ctx,
		&ssm.DeleteDocumentInput{Name: aws.String("hello"), DocumentVersion: aws.String("2")},
	)
	wantCode(t, err, "InvalidDocumentOperation")

	if _, err = c.DeleteDocument(ctx, &ssm.DeleteDocumentInput{
		Name: aws.String("hello"), DocumentVersion: aws.String("1"),
	}); err != nil {
		t.Fatal(err)
	}

	versions, _ = c.ListDocumentVersions(ctx, &ssm.ListDocumentVersionsInput{Name: aws.String("hello")})
	if len(versions.DocumentVersions) != 1 {
		t.Fatalf("got %+v", versions)
	}

	// Sharing is recorded.
	_, err = c.ModifyDocumentPermission(ctx, &ssm.ModifyDocumentPermissionInput{
		Name: aws.String("hello"), PermissionType: ssmtypes.DocumentPermissionTypeShare,
		AccountIdsToAdd: []string{"111122223333"},
	})
	if err != nil {
		t.Fatal(err)
	}

	perms, err := c.DescribeDocumentPermission(ctx, &ssm.DescribeDocumentPermissionInput{
		Name: aws.String("hello"), PermissionType: ssmtypes.DocumentPermissionTypeShare,
	})
	if err != nil || len(perms.AccountIds) != 1 || perms.AccountIds[0] != "111122223333" {
		t.Fatalf("got %+v, %v", perms, err)
	}

	if _, err = c.DeleteDocument(ctx, &ssm.DeleteDocumentInput{Name: aws.String("hello")}); err != nil {
		t.Fatal(err)
	}

	_, err = c.DescribeDocument(ctx, &ssm.DescribeDocumentInput{Name: aws.String("hello")})
	wantCode(t, err, "InvalidDocument")
}

func TestSSMBuiltinDocuments(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := ssmClient(s, "test", "us-east-1")

	amazon, err := c.ListDocuments(ctx, &ssm.ListDocumentsInput{
		Filters: []ssmtypes.DocumentKeyValuesFilter{{Key: aws.String("Owner"), Values: []string{"Amazon"}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	found := map[string]ssmtypes.DocumentIdentifier{}
	for _, d := range amazon.DocumentIdentifiers {
		found[aws.ToString(d.Name)] = d
	}

	shell, power := found["AWS-RunShellScript"], found["AWS-RunPowerShellScript"]
	if aws.ToString(shell.Owner) != "Amazon" || aws.ToString(power.Owner) != "Amazon" ||
		len(
			shell.PlatformTypes,
		) != 2 || len(power.PlatformTypes) != 1 || power.PlatformTypes[0] != ssmtypes.PlatformTypeWindows {
		t.Fatalf("got %+v %+v", shell, power)
	}

	self, err := c.ListDocuments(ctx, &ssm.ListDocumentsInput{
		Filters: []ssmtypes.DocumentKeyValuesFilter{{Key: aws.String("Owner"), Values: []string{"Self"}}},
	})
	if err != nil || len(self.DocumentIdentifiers) != 0 {
		t.Fatalf("got %+v, %v", self, err)
	}

	windows, err := c.ListDocuments(ctx, &ssm.ListDocumentsInput{
		Filters: []ssmtypes.DocumentKeyValuesFilter{{Key: aws.String("PlatformTypes"), Values: []string{"Windows"}}},
	})
	if err != nil || len(windows.DocumentIdentifiers) != 1 {
		t.Fatalf("got %+v, %v", windows, err)
	}

	_, err = c.ListDocuments(ctx, &ssm.ListDocumentsInput{
		Filters: []ssmtypes.DocumentKeyValuesFilter{{Key: aws.String("Nope"), Values: []string{"x"}}},
	})
	wantCode(t, err, "InvalidFilterKey")

	desc, err := c.DescribeDocument(ctx, &ssm.DescribeDocumentInput{Name: aws.String("AWS-RunShellScript")})
	if err != nil || len(desc.Document.Parameters) != 3 {
		t.Fatalf("got %+v, %v", desc, err)
	}

	_, err = c.DeleteDocument(ctx, &ssm.DeleteDocumentInput{Name: aws.String("AWS-RunShellScript")})
	wantCode(t, err, "InvalidDocumentOperation")

	_, err = c.AddTagsToResource(ctx, &ssm.AddTagsToResourceInput{
		ResourceType: ssmtypes.ResourceTypeForTaggingDocument, ResourceId: aws.String("AWS-RunShellScript"),
		Tags: []ssmtypes.Tag{{Key: aws.String("a"), Value: aws.String("b")}},
	})
	wantCode(t, err, "InvalidResourceId")
}

func TestSSMDocumentTags(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := ssmClient(s, "test", "us-east-1")
	if _, err := c.CreateDocument(ctx, &ssm.CreateDocumentInput{
		Name: aws.String("tagged"), Content: aws.String(shellDocument),
	}); err != nil {
		t.Fatal(err)
	}

	kind := ssmtypes.ResourceTypeForTaggingDocument
	if _, err := c.AddTagsToResource(ctx, &ssm.AddTagsToResourceInput{
		ResourceType: kind, ResourceId: aws.String("tagged"),
		Tags: []ssmtypes.Tag{
			{Key: aws.String("env"), Value: aws.String("dev")}, {Key: aws.String("x"), Value: aws.String("y")},
		},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := c.RemoveTagsFromResource(ctx, &ssm.RemoveTagsFromResourceInput{
		ResourceType: kind, ResourceId: aws.String("tagged"), TagKeys: []string{"x"},
	}); err != nil {
		t.Fatal(err)
	}

	listed, err := c.ListTagsForResource(
		ctx,
		&ssm.ListTagsForResourceInput{ResourceType: kind, ResourceId: aws.String("tagged")},
	)
	if err != nil || len(listed.TagList) != 1 || aws.ToString(listed.TagList[0].Key) != "env" {
		t.Fatalf("got %+v, %v", listed, err)
	}

	byTag, err := c.ListDocuments(ctx, &ssm.ListDocumentsInput{
		Filters: []ssmtypes.DocumentKeyValuesFilter{{Key: aws.String("TagKey"), Values: []string{"env"}}},
	})
	if err != nil || len(byTag.DocumentIdentifiers) != 1 ||
		aws.ToString(byTag.DocumentIdentifiers[0].Name) != "tagged" {
		t.Fatalf("got %+v, %v", byTag, err)
	}

	_, err = c.ListTagsForResource(
		ctx,
		&ssm.ListTagsForResourceInput{ResourceType: kind, ResourceId: aws.String("none")},
	)
	wantCode(t, err, "InvalidResourceId")
}

func TestSSMRunCommand(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := ssmClient(s, "test", "us-east-1")
	machines := ec2Client(s, "test", "us-east-1")

	launched, err := machines.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId: latestImage(t, s), InstanceType: ec2types.InstanceTypeT3Micro,
		MinCount: aws.Int32(2), MaxCount: aws.Int32(2),
		TagSpecifications: []ec2types.TagSpecification{{
			ResourceType: ec2types.ResourceTypeInstance, Tags: ec2Tags("role", "web"),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	first, second := aws.ToString(launched.Instances[0].InstanceId), aws.ToString(launched.Instances[1].InstanceId)

	_, err = c.SendCommand(ctx, &ssm.SendCommandInput{
		DocumentName: aws.String("AWS-RunShellScript"), InstanceIds: []string{"i-0000000000000000"},
		Parameters: map[string][]string{"commands": {"uptime"}},
	})
	wantCode(t, err, "InvalidInstanceId")

	_, err = c.SendCommand(ctx, &ssm.SendCommandInput{
		DocumentName: aws.String("AWS-RunShellScript"), InstanceIds: []string{first},
	})
	wantCode(t, err, "InvalidParameters")

	_, err = c.SendCommand(ctx, &ssm.SendCommandInput{
		DocumentName: aws.String("AWS-RunShellScript"), InstanceIds: []string{first, first},
		Parameters: map[string][]string{"commands": {"uptime"}},
	})
	wantCode(t, err, "DuplicateInstanceId")

	_, err = c.SendCommand(ctx, &ssm.SendCommandInput{
		DocumentName: aws.String("missing"), InstanceIds: []string{first},
	})
	wantCode(t, err, "InvalidDocument")

	sent, err := c.SendCommand(ctx, &ssm.SendCommandInput{
		DocumentName: aws.String("AWS-RunShellScript"), Comment: aws.String("check"),
		Targets:    []ssmtypes.Target{{Key: aws.String("tag:role"), Values: []string{"web"}}},
		Parameters: map[string][]string{"commands": {"uptime"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	id := aws.ToString(sent.Command.CommandId)
	if sent.Command.TargetCount != 2 || sent.Command.Status != ssmtypes.CommandStatusPending {
		t.Fatalf("got %+v", sent.Command)
	}

	// The invocations finish as Success with empty output.
	waiter := ssm.NewCommandExecutedWaiter(c)
	for _, instance := range []string{first, second} {
		out, waitErr := waiter.WaitForOutput(ctx, &ssm.GetCommandInvocationInput{
			CommandId: aws.String(id), InstanceId: aws.String(instance),
		}, 20*time.Second)
		if waitErr != nil || out.Status != ssmtypes.CommandInvocationStatusSuccess ||
			aws.ToString(out.StandardOutputContent) != "" ||
			out.ResponseCode != 0 {
			t.Fatalf("got %+v, %v", out, waitErr)
		}
	}

	listed, err := c.ListCommands(ctx, &ssm.ListCommandsInput{CommandId: aws.String(id)})
	if err != nil || len(listed.Commands) != 1 || listed.Commands[0].Status != ssmtypes.CommandStatusSuccess ||
		listed.Commands[0].CompletedCount != 2 {
		t.Fatalf("got %+v, %v", listed, err)
	}

	byInstance, err := c.ListCommands(ctx, &ssm.ListCommandsInput{
		InstanceId: aws.String(first),
		Filters:    []ssmtypes.CommandFilter{{Key: ssmtypes.CommandFilterKeyStatus, Value: aws.String("Success")}},
	})
	if err != nil || len(byInstance.Commands) != 1 {
		t.Fatalf("got %+v, %v", byInstance, err)
	}

	invocations, err := c.ListCommandInvocations(ctx, &ssm.ListCommandInvocationsInput{
		CommandId: aws.String(id), Details: true,
	})
	if err != nil || len(invocations.CommandInvocations) != 2 ||
		len(invocations.CommandInvocations[0].CommandPlugins) != 1 ||
		invocations.CommandInvocations[0].Status != ssmtypes.CommandInvocationStatusSuccess {
		t.Fatalf("got %+v, %v", invocations, err)
	}

	_, err = c.GetCommandInvocation(ctx, &ssm.GetCommandInvocationInput{
		CommandId: aws.String(id), InstanceId: aws.String("i-0000000000000000"),
	})
	wantCode(t, err, "InvocationDoesNotExist")

	_, err = c.GetCommandInvocation(ctx, &ssm.GetCommandInvocationInput{
		CommandId: aws.String("nope"), InstanceId: aws.String(first),
	})
	wantCode(t, err, "InvalidCommandId")

	// A command is cancelled while it is still pending.
	again, err := c.SendCommand(ctx, &ssm.SendCommandInput{
		DocumentName: aws.String("AWS-RunShellScript"), InstanceIds: []string{first},
		Parameters: map[string][]string{"commands": {"sleep 60"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = c.CancelCommand(ctx, &ssm.CancelCommandInput{CommandId: again.Command.CommandId}); err != nil {
		t.Fatal(err)
	}

	cancelled, err := c.GetCommandInvocation(ctx, &ssm.GetCommandInvocationInput{
		CommandId: again.Command.CommandId, InstanceId: aws.String(first),
	})
	if err != nil || cancelled.Status != ssmtypes.CommandInvocationStatusCancelled {
		t.Fatalf("got %+v, %v", cancelled, err)
	}

	_, err = c.CancelCommand(ctx, &ssm.CancelCommandInput{CommandId: aws.String("nope")})
	wantCode(t, err, "InvalidCommandId")
}
