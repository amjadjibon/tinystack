package integration_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/iot"
	iottypes "github.com/aws/aws-sdk-go-v2/service/iot/types"
)

const iotTemplate = `AWSTemplateFormatVersion: "2010-09-09"
Parameters:
  Csr: {Type: String}
  Floor: {Type: String, Default: "3"}
  Status: {Type: String, Default: ACTIVE}
  Action: {Type: String, Default: "iot:Connect"}
  Deprecated: {Type: String, Default: "false"}
Resources:
  Type:
    Type: AWS::IoT::ThingType
    Properties:
      ThingTypeName: sensor
      DeprecateThingType: !Ref Deprecated
      ThingTypeProperties: {ThingTypeDescription: sensors, SearchableAttributes: [floor]}
      Tags: [{Key: team, Value: a}]
  Fleet:
    Type: AWS::IoT::ThingGroup
    Properties:
      ThingGroupProperties: {ThingGroupDescription: !Sub "floor ${Floor}"}
      Tags: [{Key: team, Value: a}]
  Device:
    Type: AWS::IoT::Thing
    Properties:
      ThingName: probe
      AttributePayload:
        Attributes: {floor: !Ref Floor, room: a}
  Policy:
    Type: AWS::IoT::Policy
    Properties:
      PolicyDocument:
        Version: "2012-10-17"
        Statement: [{Effect: Allow, Action: !Ref Action, Resource: "*"}]
  Cert:
    Type: AWS::IoT::Certificate
    Properties:
      CertificateSigningRequest: !Ref Csr
      Status: !Ref Status
  Attach:
    Type: AWS::IoT::PolicyPrincipalAttachment
    Properties:
      PolicyName: !Ref Policy
      Principal: !GetAtt Cert.Arn
  Bind:
    Type: AWS::IoT::ThingPrincipalAttachment
    Properties:
      ThingName: !Ref Device
      Principal: !GetAtt Cert.Arn
  Rule:
    Type: AWS::IoT::TopicRule
    Properties:
      TopicRulePayload:
        Sql: !Sub "SELECT * FROM 'floor/${Floor}'"
        RuleDisabled: false
        Actions:
          - Republish: {Topic: alerts, RoleArn: "arn:aws:iam::000000000000:role/iot"}
      Tags: [{Key: team, Value: a}]
  Authorizer:
    Type: AWS::IoT::Authorizer
    Properties:
      AuthorizerFunctionArn: arn:aws:lambda:us-east-1:000000000000:function:auth
      SigningDisabled: true
      Status: !Ref Status
      Tags: [{Key: team, Value: a}]
Outputs:
  Authorizer: {Value: !Ref Authorizer}
  Rule: {Value: !Ref Rule}
  RuleArn: {Value: !GetAtt Rule.Arn}
  Thing: {Value: !GetAtt Device.Arn}
  Group: {Value: !Ref Fleet}
  Policy: {Value: !Ref Policy}
  Cert: {Value: !Ref Cert}
`

func TestCloudFormationIoT(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := cfnClient(s)
	devices := iotClient(s, "test")

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	csr := aws.String(csrPEM(t, key, "probe"))
	if _, err = c.CreateStack(ctx, &cfn.CreateStackInput{
		StackName: aws.String("things"), TemplateBody: aws.String(iotTemplate),
		Parameters: []cfntypes.Parameter{{ParameterKey: aws.String("Csr"), ParameterValue: csr}},
	}); err != nil {
		t.Fatal(err)
	}

	st := wantStatus(ctx, t, c, "things", cfntypes.StackStatusCreateComplete)
	outputs := outputsOf(&st)
	if outputs["Thing"] != "arn:aws:iot:us-east-1:000000000000:thing/probe" ||
		!strings.HasPrefix(outputs["Group"], "things-Fleet-") {
		t.Fatalf("outputs = %+v", outputs)
	}

	thing, err := devices.DescribeThing(ctx, &iot.DescribeThingInput{ThingName: aws.String("probe")})
	if err != nil || thing.Attributes["floor"] != "3" || thing.Attributes["room"] != "a" {
		t.Fatalf("%+v %v", thing, err)
	}

	rule, err := devices.GetTopicRule(ctx, &iot.GetTopicRuleInput{RuleName: aws.String(outputs["Rule"])})
	if err != nil || aws.ToString(rule.Rule.Sql) != "SELECT * FROM 'floor/3'" ||
		aws.ToString(rule.RuleArn) != outputs["RuleArn"] || strings.Contains(outputs["Rule"], "-") {
		t.Fatalf("%+v %v", rule, err)
	}

	cert := outputs["Cert"]
	described, err := devices.DescribeCertificate(ctx, &iot.DescribeCertificateInput{CertificateId: &cert})
	if err != nil || described.CertificateDescription.Status != iottypes.CertificateStatusActive {
		t.Fatalf("%+v %v", described, err)
	}

	certARN := aws.ToString(described.CertificateDescription.CertificateArn)
	attached, err := devices.ListAttachedPolicies(ctx, &iot.ListAttachedPoliciesInput{Target: &certARN})
	if err != nil || len(attached.Policies) != 1 || aws.ToString(attached.Policies[0].PolicyName) != outputs["Policy"] {
		t.Fatalf("%+v %v", attached, err)
	}

	principals, err := devices.ListThingPrincipals(ctx, &iot.ListThingPrincipalsInput{ThingName: aws.String("probe")})
	if err != nil || len(principals.Principals) != 1 || principals.Principals[0] != certARN {
		t.Fatalf("%+v %v", principals, err)
	}

	// An update changes every resource in place.
	if _, err = c.UpdateStack(ctx, &cfn.UpdateStackInput{
		StackName: aws.String("things"), TemplateBody: aws.String(strings.Replace(iotTemplate, ", room: a", "", 1)),
		Parameters: []cfntypes.Parameter{
			{ParameterKey: aws.String("Csr"), ParameterValue: csr},
			{ParameterKey: aws.String("Floor"), ParameterValue: aws.String("4")},
			{ParameterKey: aws.String("Status"), ParameterValue: aws.String("INACTIVE")},
			{ParameterKey: aws.String("Action"), ParameterValue: aws.String("iot:*")},
			{ParameterKey: aws.String("Deprecated"), ParameterValue: aws.String("true")},
		},
	}); err != nil {
		t.Fatal(err)
	}

	st = wantStatus(ctx, t, c, "things", cfntypes.StackStatusUpdateComplete)
	if updated := outputsOf(&st); updated["Cert"] != cert || updated["Group"] != outputs["Group"] {
		t.Fatalf("outputs = %+v, were %+v", updated, outputs)
	}

	thing, err = devices.DescribeThing(ctx, &iot.DescribeThingInput{ThingName: aws.String("probe")})
	if err != nil || len(thing.Attributes) != 1 || thing.Attributes["floor"] != "4" {
		t.Fatalf("%+v %v", thing, err)
	}

	group, err := devices.DescribeThingGroup(ctx, &iot.DescribeThingGroupInput{
		ThingGroupName: aws.String(outputs["Group"]),
	})
	if err != nil || aws.ToString(group.ThingGroupProperties.ThingGroupDescription) != "floor 4" {
		t.Fatalf("%+v %v", group, err)
	}

	policy, err := devices.GetPolicy(ctx, &iot.GetPolicyInput{PolicyName: aws.String(outputs["Policy"])})
	if err != nil || aws.ToString(policy.DefaultVersionId) != "2" ||
		!strings.Contains(aws.ToString(policy.PolicyDocument), `"iot:*"`) {
		t.Fatalf("%+v %v", policy, err)
	}

	described, err = devices.DescribeCertificate(ctx, &iot.DescribeCertificateInput{CertificateId: &cert})
	if err != nil || described.CertificateDescription.Status != iottypes.CertificateStatusInactive {
		t.Fatalf("%+v %v", described, err)
	}

	rule, err = devices.GetTopicRule(ctx, &iot.GetTopicRuleInput{RuleName: aws.String(outputs["Rule"])})
	if err != nil || aws.ToString(rule.Rule.Sql) != "SELECT * FROM 'floor/4'" {
		t.Fatalf("%+v %v", rule, err)
	}

	authorizer, err := devices.DescribeAuthorizer(ctx, &iot.DescribeAuthorizerInput{
		AuthorizerName: aws.String(outputs["Authorizer"]),
	})
	if err != nil || authorizer.AuthorizerDescription.Status != iottypes.AuthorizerStatusInactive ||
		!aws.ToBool(authorizer.AuthorizerDescription.SigningDisabled) {
		t.Fatalf("%+v %v", authorizer, err)
	}

	typ, err := devices.DescribeThingType(ctx, &iot.DescribeThingTypeInput{ThingTypeName: aws.String("sensor")})
	if err != nil || !typ.ThingTypeMetadata.Deprecated {
		t.Fatalf("%+v %v", typ, err)
	}

	// Deleting the stack removes everything, attachments first.
	if _, err = c.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: aws.String("things")}); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "stack things to be deleted", func() bool {
		out, describeErr := c.DescribeStacks(ctx, &cfn.DescribeStacksInput{StackName: aws.String("things")})
		return describeErr != nil || out.Stacks[0].StackStatus == cfntypes.StackStatusDeleteComplete
	})

	things, err := devices.ListThings(ctx, &iot.ListThingsInput{})
	if err != nil || len(things.Things) != 0 {
		t.Fatalf("%+v %v", things, err)
	}

	certs, err := devices.ListCertificates(ctx, &iot.ListCertificatesInput{})
	if err != nil || len(certs.Certificates) != 0 {
		t.Fatalf("%+v %v", certs, err)
	}

	policies, err := devices.ListPolicies(ctx, &iot.ListPoliciesInput{})
	if err != nil || len(policies.Policies) != 0 {
		t.Fatalf("%+v %v", policies, err)
	}

	authorizers, err := devices.ListAuthorizers(ctx, &iot.ListAuthorizersInput{})
	if err != nil || len(authorizers.Authorizers) != 0 {
		t.Fatalf("%+v %v", authorizers, err)
	}

	rules, err := devices.ListTopicRules(ctx, &iot.ListTopicRulesInput{})
	if err != nil || len(rules.Rules) != 0 {
		t.Fatalf("%+v %v", rules, err)
	}

	types, err := devices.ListThingTypes(ctx, &iot.ListThingTypesInput{})
	if err != nil || len(types.ThingTypes) != 0 {
		t.Fatalf("%+v %v", types, err)
	}
}
