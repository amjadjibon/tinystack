package integration_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iot"
	iottypes "github.com/aws/aws-sdk-go-v2/service/iot/types"
)

const provisioningBody = `{
  "Parameters": {
    "SerialNumber": {"Type": "String"},
    "Floor": {"Type": "String", "Default": "1"},
    "AWS::IoT::Certificate::Id": {"Type": "String"}
  },
  "Resources": {
    "thing": {
      "Type": "AWS::IoT::Thing",
      "Properties": {
        "ThingName": {"Fn::Join": ["", ["sensor-", {"Ref": "SerialNumber"}]]},
        "AttributePayload": {"floor": {"Ref": "Floor"}},
        "ThingGroups": ["fleet"]
      }
    },
    "certificate": {
      "Type": "AWS::IoT::Certificate",
      "Properties": {"CertificateId": {"Ref": "AWS::IoT::Certificate::Id"}, "Status": "ACTIVE"}
    },
    "policy": {"Type": "AWS::IoT::Policy", "Properties": {"PolicyName": "device-policy"}}
  },
  "DeviceConfiguration": {"endpoint": {"Fn::Sub": "floor-${Floor}"}}
}`

func TestIoTFleetProvisioning(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := iotClient(s, "test")
	role := aws.String("arn:aws:iam::000000000000:role/provisioning")
	if _, err := c.CreateThingGroup(ctx, &iot.CreateThingGroupInput{ThingGroupName: aws.String("fleet")}); err != nil {
		t.Fatal(err)
	}

	if _, err := c.CreatePolicy(ctx, &iot.CreatePolicyInput{
		PolicyName: aws.String("device-policy"),
		PolicyDocument: aws.String(`{"Version":"2012-10-17","Statement":[` +
			`{"Effect":"Allow","Action":"iot:*","Resource":"*"}]}`),
	}); err != nil {
		t.Fatal(err)
	}

	// Bodies outside the subset, and just-in-time templates, fail.
	for _, body := range []string{
		`{"Resources": {}}`,
		`{"Conditions": {}, "Resources": {}}`,
		strings.Replace(provisioningBody, "Fn::Sub", "Fn::Base64", 1),
		strings.Replace(provisioningBody, "AWS::IoT::Policy", "AWS::IoT::Rule", 1),
	} {
		_, err := c.CreateProvisioningTemplate(ctx, &iot.CreateProvisioningTemplateInput{
			TemplateName: aws.String("bad"), TemplateBody: aws.String(body), ProvisioningRoleArn: role,
		})
		wantCode(t, err, "InvalidRequestException")
	}

	_, err := c.CreateProvisioningTemplate(ctx, &iot.CreateProvisioningTemplateInput{
		TemplateName: aws.String("bad"), TemplateBody: aws.String(provisioningBody), ProvisioningRoleArn: role,
		Type: iottypes.TemplateTypeJitp,
	})
	wantCode(t, err, "InvalidRequestException")

	created, err := c.CreateProvisioningTemplate(ctx, &iot.CreateProvisioningTemplateInput{
		TemplateName: aws.String("sensors"), TemplateBody: aws.String(provisioningBody), ProvisioningRoleArn: role,
		Enabled: aws.Bool(true),
	})
	if err != nil || aws.ToString(created.TemplateArn) !=
		"arn:aws:iot:us-east-1:000000000000:provisioningtemplate/sensors" || aws.ToInt32(created.DefaultVersionId) != 1 {
		t.Fatalf("created = %+v, %v", created, err)
	}

	// A device creates its keys and certificate, then provisions itself.
	device := connected(t, mqttURL(s), "claim-1")
	device.subscribe("$aws/certificates/create/json/+", "$aws/provisioning-templates/sensors/provision/json/+")
	if code := device.publish("$aws/certificates/create/json", `{}`, false); code != 0 {
		t.Fatalf("puback 0x%x", code)
	}

	var keys map[string]any
	if err = json.Unmarshal(device.message("$aws/certificates/create/json/accepted").Payload, &keys); err != nil ||
		keys["privateKey"] == nil || keys["certificateOwnershipToken"] == nil {
		t.Fatalf("keys = %v, %v", keys, err)
	}

	certID, _ := keys["certificateId"].(string)
	described, err := c.DescribeCertificate(ctx, &iot.DescribeCertificateInput{CertificateId: &certID})
	if err != nil || described.CertificateDescription.Status != iottypes.CertificateStatusInactive {
		t.Fatalf("new certificate = %+v, %v", described, err)
	}

	provision, err := json.Marshal(map[string]any{
		"certificateOwnershipToken": keys["certificateOwnershipToken"],
		"parameters":                map[string]string{"SerialNumber": "42"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if code := device.publish(
		"$aws/provisioning-templates/sensors/provision/json",
		string(provision),
		false,
	); code != 0 {
		t.Fatalf("puback 0x%x", code)
	}

	var done map[string]any
	if err = json.Unmarshal(
		device.message("$aws/provisioning-templates/sensors/provision/json/accepted").Payload,
		&done,
	); err != nil || done["thingName"] != "sensor-42" || at(done, "deviceConfiguration.endpoint") != "floor-1" {
		t.Fatalf("provisioned = %v, %v", done, err)
	}

	thing, err := c.DescribeThing(ctx, &iot.DescribeThingInput{ThingName: aws.String("sensor-42")})
	if err != nil || thing.Attributes["floor"] != "1" {
		t.Fatalf("thing = %+v, %v", thing, err)
	}

	groups, err := c.ListThingGroupsForThing(ctx, &iot.ListThingGroupsForThingInput{ThingName: aws.String("sensor-42")})
	if err != nil || len(groups.ThingGroups) != 1 {
		t.Fatalf("groups = %+v, %v", groups, err)
	}

	described, err = c.DescribeCertificate(ctx, &iot.DescribeCertificateInput{CertificateId: &certID})
	if err != nil || described.CertificateDescription.Status != iottypes.CertificateStatusActive {
		t.Fatalf("provisioned certificate = %+v, %v", described, err)
	}

	certARN := described.CertificateDescription.CertificateArn
	principals, err := c.ListThingPrincipals(ctx, &iot.ListThingPrincipalsInput{ThingName: aws.String("sensor-42")})
	if err != nil || len(principals.Principals) != 1 || principals.Principals[0] != aws.ToString(certARN) {
		t.Fatalf("principals = %+v, %v", principals, err)
	}

	policies, err := c.ListAttachedPolicies(ctx, &iot.ListAttachedPoliciesInput{Target: certARN})
	if err != nil || len(policies.Policies) != 1 {
		t.Fatalf("policies = %+v, %v", policies, err)
	}

	// A token provisions once.
	if code := device.publish(
		"$aws/provisioning-templates/sensors/provision/json",
		string(provision),
		false,
	); code != 0 {
		t.Fatalf("puback 0x%x", code)
	}

	var rejected map[string]any
	if err = json.Unmarshal(device.message("$aws/provisioning-templates/sensors/provision/json/rejected").Payload,
		&rejected); err != nil || rejected["errorCode"] != "InvalidCertificateOwnershipToken" {
		t.Fatalf("rejected = %v, %v", rejected, err)
	}

	// A pre-provisioning hook can refuse devices and override parameters.
	fn := createFunction(t, lambdaClient(s, "test"), "provision-hook", "app.provisionHook")
	if _, err = c.CreateProvisioningTemplate(ctx, &iot.CreateProvisioningTemplateInput{
		TemplateName: aws.String("hooked"), TemplateBody: aws.String(provisioningBody), ProvisioningRoleArn: role,
		Enabled: aws.Bool(true), PreProvisioningHook: &iottypes.ProvisioningHook{TargetArn: fn.FunctionArn},
	}); err != nil {
		t.Fatal(err)
	}

	device.subscribe("$aws/provisioning-templates/hooked/provision/json/+")
	for serial, want := range map[string]string{"0": "rejected", "7": "accepted"} {
		if code := device.publish("$aws/certificates/create/json", `{}`, false); code != 0 {
			t.Fatalf("puback 0x%x", code)
		}

		if err = json.Unmarshal(device.message("$aws/certificates/create/json/accepted").Payload, &keys); err != nil {
			t.Fatal(err)
		}

		request, marshalErr := json.Marshal(map[string]any{
			"certificateOwnershipToken": keys["certificateOwnershipToken"],
			"parameters":                map[string]string{"SerialNumber": serial},
		})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}

		if code := device.publish("$aws/provisioning-templates/hooked/provision/json", string(request),
			false); code != 0 {
			t.Fatalf("puback 0x%x", code)
		}

		reply := device.message("$aws/provisioning-templates/hooked/provision/json/" + want)
		if want == "accepted" {
			if err = json.Unmarshal(
				reply.Payload,
				&done,
			); err != nil ||
				at(done, "deviceConfiguration.endpoint") != "floor-9" {
				t.Fatalf("hooked = %v, %v", done, err)
			}
		}
	}

	// Template versions.
	version, err := c.CreateProvisioningTemplateVersion(ctx, &iot.CreateProvisioningTemplateVersionInput{
		TemplateName: aws.String("sensors"), TemplateBody: aws.String(provisioningBody), SetAsDefault: true,
	})
	if err != nil || aws.ToInt32(version.VersionId) != 2 || !version.IsDefaultVersion {
		t.Fatalf("version = %+v, %v", version, err)
	}

	versions, err := c.ListProvisioningTemplateVersions(ctx, &iot.ListProvisioningTemplateVersionsInput{
		TemplateName: aws.String("sensors"),
	})
	if err != nil || len(versions.Versions) != 2 {
		t.Fatalf("versions = %+v, %v", versions, err)
	}

	_, err = c.DeleteProvisioningTemplateVersion(ctx, &iot.DeleteProvisioningTemplateVersionInput{
		TemplateName: aws.String("sensors"), VersionId: aws.Int32(2),
	})
	wantCode(t, err, "DeleteConflictException")
	if _, err = c.DeleteProvisioningTemplateVersion(ctx, &iot.DeleteProvisioningTemplateVersionInput{
		TemplateName: aws.String("sensors"), VersionId: aws.Int32(1),
	}); err != nil {
		t.Fatal(err)
	}

	// A disabled template provisions nothing.
	if _, err = c.UpdateProvisioningTemplate(ctx, &iot.UpdateProvisioningTemplateInput{
		TemplateName: aws.String("sensors"), Enabled: aws.Bool(false),
	}); err != nil {
		t.Fatal(err)
	}

	template, err := c.DescribeProvisioningTemplate(ctx, &iot.DescribeProvisioningTemplateInput{
		TemplateName: aws.String("sensors"),
	})
	if err != nil || aws.ToBool(template.Enabled) || aws.ToInt32(template.DefaultVersionId) != 2 {
		t.Fatalf("template = %+v, %v", template, err)
	}

	list, err := c.ListProvisioningTemplates(ctx, &iot.ListProvisioningTemplatesInput{})
	if err != nil || len(list.Templates) != 2 {
		t.Fatalf("templates = %+v, %v", list, err)
	}

	if _, err = c.DeleteProvisioningTemplate(ctx, &iot.DeleteProvisioningTemplateInput{
		TemplateName: aws.String("sensors"),
	}); err != nil {
		t.Fatal(err)
	}

	_, err = c.DescribeProvisioningTemplate(ctx, &iot.DescribeProvisioningTemplateInput{
		TemplateName: aws.String("sensors"),
	})
	wantCode(t, err, "ResourceNotFoundException")
}
