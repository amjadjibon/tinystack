package integration_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/acmpca"
	"github.com/aws/aws-sdk-go-v2/service/acmpca/types"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
)

func privateCATemplate(commonName string, ocsp bool, envTag string) string {
	properties := map[string]any{
		"Type":             "ROOT",
		"KeyAlgorithm":     "RSA_2048",
		"SigningAlgorithm": "SHA256WITHRSA",
		"Subject":          map[string]any{"CommonName": commonName},
		"Tags":             []any{map[string]any{"Key": "Env", "Value": envTag}},
	}
	if ocsp {
		properties["RevocationConfiguration"] = map[string]any{"OcspConfiguration": map[string]any{"Enabled": true}}
	}

	raw, _ := json.Marshal(map[string]any{
		"Resources": map[string]any{
			"Authority": map[string]any{"Type": "AWS::ACMPCA::CertificateAuthority", "Properties": properties},
		},
		"Outputs": map[string]any{
			"Ref": map[string]any{"Value": map[string]any{"Ref": "Authority"}},
			"Arn": map[string]any{"Value": map[string]any{"Fn::GetAtt": []any{"Authority", "Arn"}}},
		},
	})

	return string(raw)
}

func TestCloudFormationPrivateCALifecycle(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	cf := cfnClient(s)
	c := acmpcaClient(s, "test")
	describe := func(arn string) types.CertificateAuthority {
		t.Helper()

		out, err := c.DescribeCertificateAuthority(
			ctx,
			&sdk.DescribeCertificateAuthorityInput{CertificateAuthorityArn: &arn},
		)
		if err != nil {
			t.Fatal(err)
		}

		return *out.CertificateAuthority
	}

	if _, err := cf.CreateStack(ctx, &cfn.CreateStackInput{
		StackName: aws.String("pca"), TemplateBody: aws.String(privateCATemplate("root.example.com", false, "Dev")),
	}); err != nil {
		t.Fatal(err)
	}

	stack := wantStatus(ctx, t, cf, "pca", cfntypes.StackStatusCreateComplete)
	outputs := outputsOf(&stack)
	arn := outputs["Arn"]
	if outputs["Ref"] != arn || !strings.HasPrefix(arn, "arn:aws:acm-pca:us-east-1:") {
		t.Fatalf("unexpected outputs %+v", outputs)
	}

	if ca := describe(arn); ca.Status != types.CertificateAuthorityStatusPendingCertificate ||
		aws.ToString(ca.CertificateAuthorityConfiguration.Subject.CommonName) != "root.example.com" {
		t.Fatalf("unexpected authority %+v", ca)
	}

	// The revocation configuration and tags change in place.
	if _, err := cf.UpdateStack(ctx, &cfn.UpdateStackInput{
		StackName: aws.String("pca"), TemplateBody: aws.String(privateCATemplate("root.example.com", true, "Prod")),
	}); err != nil {
		t.Fatal(err)
	}

	stack = wantStatus(ctx, t, cf, "pca", cfntypes.StackStatusUpdateComplete)
	updated := outputsOf(&stack)
	if updated["Arn"] != arn {
		t.Fatalf("the authority was replaced by an in-place update: %s", updated["Arn"])
	}

	ca := describe(arn)
	tags, err := c.ListTags(ctx, &sdk.ListTagsInput{CertificateAuthorityArn: &arn})
	if err != nil || len(tags.Tags) != 1 || aws.ToString(tags.Tags[0].Value) != "Prod" ||
		!aws.ToBool(ca.RevocationConfiguration.OcspConfiguration.Enabled) {
		t.Fatalf("unexpected update: %+v, %+v, %v", ca, tags, err)
	}

	// Dropping the revocation configuration disables it.
	if _, err = cf.UpdateStack(ctx, &cfn.UpdateStackInput{
		StackName: aws.String("pca"), TemplateBody: aws.String(privateCATemplate("root.example.com", false, "Prod")),
	}); err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, cf, "pca", cfntypes.StackStatusUpdateComplete)
	if ca = describe(arn); aws.ToBool(ca.RevocationConfiguration.OcspConfiguration.Enabled) {
		t.Fatalf("OCSP is still enabled: %+v", ca.RevocationConfiguration)
	}

	// A change to the subject replaces the authority; the old one is deleted.
	if _, err = cf.UpdateStack(ctx, &cfn.UpdateStackInput{
		StackName: aws.String("pca"), TemplateBody: aws.String(privateCATemplate("renamed.example.com", false, "Prod")),
	}); err != nil {
		t.Fatal(err)
	}

	stack = wantStatus(ctx, t, cf, "pca", cfntypes.StackStatusUpdateComplete)
	replaced := outputsOf(&stack)
	if replaced["Arn"] == arn || describe(arn).Status != types.CertificateAuthorityStatusDeleted ||
		aws.ToString(
			describe(replaced["Arn"]).CertificateAuthorityConfiguration.Subject.CommonName,
		) != "renamed.example.com" {
		t.Fatalf("the subject change did not replace the authority: %+v", replaced)
	}

	if _, err = cf.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: aws.String("pca")}); err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, cf, aws.ToString(stack.StackId), cfntypes.StackStatusDeleteComplete)
	if describe(replaced["Arn"]).Status != types.CertificateAuthorityStatusDeleted {
		t.Fatalf("the stack delete did not delete the authority")
	}
}

func TestCloudFormationPrivateCACertificateSigningRequestFails(t *testing.T) {
	s := server(t)
	template := strings.Replace(
		privateCATemplate("root.example.com", false, "Dev"),
		`"Arn"]`,
		`"CertificateSigningRequest"]`,
		1,
	)

	_, err := cfnClient(s).CreateStack(context.Background(), &cfn.CreateStackInput{
		StackName: aws.String("pca-csr"), TemplateBody: aws.String(template),
	})
	if err == nil || !strings.Contains(err.Error(), "CertificateSigningRequest") {
		t.Fatalf("a template reading the unavailable CertificateSigningRequest was accepted: %v", err)
	}
}
