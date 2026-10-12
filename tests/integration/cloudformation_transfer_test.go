package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"testing"

	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/cloudtrail"
	sdk "github.com/aws/aws-sdk-go-v2/service/transfer"
	"github.com/aws/aws-sdk-go-v2/service/transfer/types"
)

func transferCFServer() map[string]any {
	return map[string]any{"Protocols": []any{"SFTP"}, "EndpointType": "PUBLIC"}
}

func transferCFUser(name string) map[string]any {
	return map[string]any{
		"ServerId":      map[string]any{"Fn::GetAtt": []any{"Server", "ServerId"}},
		"UserName":      name,
		"Role":          "arn:aws:iam::000000000000:role/transfer",
		"HomeDirectory": "/bucket/home/" + name,
	}
}

func transferCFTemplate(server, user map[string]any, later bool) string {
	resources := map[string]any{"Server": map[string]any{"Type": "AWS::Transfer::Server", "Properties": server}}
	outputs := map[string]any{
		"ServerRef": map[string]any{"Value": map[string]any{"Ref": "Server"}},
		"ServerId":  map[string]any{"Value": map[string]any{"Fn::GetAtt": []any{"Server", "ServerId"}}},
		"ServerArn": map[string]any{"Value": map[string]any{"Fn::GetAtt": []any{"Server", "Arn"}}},
	}
	if user != nil {
		resources["User"] = map[string]any{"Type": "AWS::Transfer::User", "Properties": user}
		outputs["UserRef"] = map[string]any{"Value": map[string]any{"Ref": "User"}}
		for _, attr := range []string{"Arn", "ServerId", "UserName"} {
			outputs["User"+attr] = map[string]any{"Value": map[string]any{"Fn::GetAtt": []any{"User", attr}}}
		}
	}
	if later {
		dependency := "Server"
		if user != nil {
			dependency = "User"
		}
		resources["Later"] = map[string]any{
			"Type":       "AWS::Transfer::Server",
			"DependsOn":  dependency,
			"Properties": map[string]any{"Protocols": []any{"FTPS"}, "EndpointType": "PUBLIC"},
		}
	}
	raw, _ := json.Marshal(map[string]any{"Resources": resources, "Outputs": outputs})
	return string(raw)
}

func transferCFUpdate(
	t *testing.T,
	cf *cfn.Client,
	server, user map[string]any,
	later bool,
	status cfntypes.StackStatus,
) cfntypes.Stack {
	t.Helper()
	name := "transfer-cf"
	_, e := cf.UpdateStack(
		context.Background(),
		&cfn.UpdateStackInput{StackName: new(name), TemplateBody: new(transferCFTemplate(server, user, later))},
	)
	if e != nil {
		t.Fatal(e)
	}
	return wantStatus(context.Background(), t, cf, name, status)
}

func transferCFTags(prefix string) []any {
	out := []any{}
	for n := range 50 {
		out = append(out, map[string]any{"Key": fmt.Sprintf("%s-%02d", prefix, n), "Value": "value"})
	}
	return out
}

func TestCloudFormationTransferLifecycleReferencesDefaultsAtomicTagsExternalSnapshotAndReplacement(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	cf := cfnClient(s)
	c := transferClient(s, "test", "us-east-1")
	p, u := transferCFServer(), transferCFUser("alice")
	p["Tags"] = transferCFTags("old-server")
	u["Tags"] = transferCFTags("old-user")
	name := "transfer-cf"
	_, e := cf.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: new(name), TemplateBody: new(transferCFTemplate(p, u, false))},
	)
	if e != nil {
		t.Fatal(e)
	}
	st := wantStatus(ctx, t, cf, name, cfntypes.StackStatusCreateComplete)
	out := efsStackOutputs(st)
	id := new(out["ServerId"])
	if out["ServerRef"] != out["ServerArn"] ||
		out["ServerRef"] != "arn:aws:transfer:us-east-1:000000000000:server/"+*id ||
		out["UserRef"] != "alice" ||
		out["UserUserName"] != "alice" ||
		out["UserServerId"] != *id ||
		out["UserArn"] != "arn:aws:transfer:us-east-1:000000000000:user/"+*id+"/alice" {
		t.Fatal(out)
	}
	if *transferServer(t, c, id).SecurityPolicyName != "TransferSecurityPolicy-2018-11" ||
		transferUser(t, c, id, "alice").HomeDirectoryType != types.HomeDirectoryTypePath {
		t.Fatal("CF defaults")
	}
	p["Tags"] = transferCFTags("new-server")
	u["Tags"] = transferCFTags("new-user")
	u["HomeDirectory"] = "/cf/changed"
	p["PreAuthenticationLoginBanner"] = "changed"
	transferCFUpdate(t, cf, p, u, false, cfntypes.StackStatusUpdateComplete)
	srv, usr := transferServer(t, c, id), transferUser(t, c, id, "alice")
	if len(srv.Tags) != 50 || len(usr.Tags) != 50 || *usr.HomeDirectory != "/cf/changed" ||
		*srv.PreAuthenticationLoginBanner != "changed" {
		t.Fatal(srv, usr)
	}
	// A final overflow fails the whole user candidate; prior server mutations are compensated.
	_, e = c.UntagResource(ctx, &sdk.UntagResourceInput{Arn: usr.Arn, TagKeys: []string{"new-user-00"}})
	if e != nil {
		t.Fatal(e)
	}
	_, e = c.TagResource(
		ctx,
		&sdk.TagResourceInput{Arn: usr.Arn, Tags: []types.Tag{{Key: new("external"), Value: new("actual")}}},
	)
	if e != nil {
		t.Fatal(e)
	}
	overflowUser := maps.Clone(u)
	overflowUser["Tags"] = transferCFTags("overflow")
	overflowUser["HomeDirectory"] = "/must-not-write"
	liveS, liveU := transferServer(t, c, id), transferUser(t, c, id, "alice")
	transferCFUpdate(t, cf, p, overflowUser, false, cfntypes.StackStatusUpdateRollbackComplete)
	if !reflect.DeepEqual(liveS, transferServer(t, c, id)) ||
		!reflect.DeepEqual(liveU, transferUser(t, c, id, "alice")) {
		t.Fatal("overflow wrote")
	}
	// Remove owned tags while retaining the external tag, then capture actual external config/key metadata.
	delete(p, "Tags")
	delete(u, "Tags")
	u["HomeDirectory"] = "/cf/cleared"
	p["SecurityPolicyName"] = "TransferSecurityPolicy-2024-01"
	transferCFUpdate(t, cf, p, u, false, cfntypes.StackStatusUpdateComplete)
	usr = transferUser(t, c, id, "alice")
	if len(usr.Tags) != 1 || *usr.Tags[0].Key != "external" || len(transferServer(t, c, id).Tags) != 0 {
		t.Fatal("tag ownership", usr)
	}
	_, e = c.UpdateUser(
		ctx,
		&sdk.UpdateUserInput{
			ServerId:      id,
			UserName:      new("alice"),
			HomeDirectory: new("/external/live"),
			Role:          new("arn:aws:iam::000000000000:role/external"),
		},
	)
	if e != nil {
		t.Fatal(e)
	}
	_, e = c.UpdateServer(
		ctx,
		&sdk.UpdateServerInput{ServerId: id, PreAuthenticationLoginBanner: new("external server banner")},
	)
	if e != nil {
		t.Fatal(e)
	}
	_, e = c.ImportSshPublicKey(
		ctx,
		&sdk.ImportSshPublicKeyInput{ServerId: id, UserName: new("alice"), SshPublicKeyBody: new(transferKey(7))},
	)
	if e != nil {
		t.Fatal(e)
	}
	liveS, liveU = transferServer(t, c, id), transferUser(t, c, id, "alice")
	changedServer, changedUser := maps.Clone(p), maps.Clone(u)
	changedServer["PreAuthenticationLoginBanner"] = "new template"
	changedUser["HomeDirectory"] = "/new/template"
	transferCFUpdate(t, cf, changedServer, changedUser, true, cfntypes.StackStatusUpdateRollbackComplete)
	if !reflect.DeepEqual(liveS, transferServer(t, c, id)) ||
		!reflect.DeepEqual(liveU, transferUser(t, c, id, "alice")) {
		t.Fatal("live compensation lost external key/config/tags")
	}
	// Required removal, and unsupported banner removal combined with identity change, fail before writing.
	required := maps.Clone(u)
	delete(required, "Role")
	required["UserName"] = "renamed"
	transferCFUpdate(t, cf, p, required, false, cfntypes.StackStatusUpdateRollbackComplete)
	if !reflect.DeepEqual(liveU, transferUser(t, c, id, "alice")) {
		t.Fatal("required removal wrote")
	}
	badServer := maps.Clone(p)
	delete(badServer, "PreAuthenticationLoginBanner")
	badServer["Domain"] = "S3"
	transferCFUpdate(t, cf, badServer, u, false, cfntypes.StackStatusUpdateRollbackComplete)
	if !reflect.DeepEqual(liveS, transferServer(t, c, id)) {
		t.Fatal("combined removal/replacement wrote")
	}
	// Create-first user collision preserves the original and unrelated user.
	collision := transferUserInput(id, "collision")
	_, e = c.CreateUser(ctx, collision)
	if e != nil {
		t.Fatal(e)
	}
	unrelated := transferUser(t, c, id, "collision")
	renamed := maps.Clone(u)
	renamed["UserName"] = "collision"
	transferCFUpdate(t, cf, p, renamed, false, cfntypes.StackStatusUpdateRollbackComplete)
	if !reflect.DeepEqual(liveU, transferUser(t, c, id, "alice")) ||
		!reflect.DeepEqual(unrelated, transferUser(t, c, id, "collision")) {
		t.Fatal("collision damaged records")
	}
	renamed["UserName"] = "failed-rename"
	transferCFUpdate(t, cf, p, renamed, true, cfntypes.StackStatusUpdateRollbackComplete)
	if !reflect.DeepEqual(liveU, transferUser(t, c, id, "alice")) {
		t.Fatal("failed replacement lost live incarnation")
	}
	_, e = c.DescribeUser(ctx, &sdk.DescribeUserInput{ServerId: id, UserName: new("failed-rename")})
	transferStatus(t, e, "ResourceNotFoundException", 404)
	renamed["UserName"] = "bob"
	st = transferCFUpdate(t, cf, p, renamed, false, cfntypes.StackStatusUpdateComplete)
	if efsStackOutputs(st)["UserRef"] != "bob" {
		t.Fatal("user Ref after replacement")
	}
	_, e = c.DescribeUser(ctx, &sdk.DescribeUserInput{ServerId: id, UserName: new("alice")})
	transferStatus(t, e, "ResourceNotFoundException", 404)
	if !reflect.DeepEqual(unrelated, transferUser(t, c, id, "collision")) {
		t.Fatal("replacement deleted unrelated user")
	}
	// Generated-ID server Domain replacement creates a parent and dependent user before retired cleanup.
	replacement := maps.Clone(p)
	replacement["Domain"] = "S3"
	st = transferCFUpdate(t, cf, replacement, renamed, false, cfntypes.StackStatusUpdateComplete)
	next := new(efsStackOutputs(st)["ServerId"])
	if *next == *id {
		t.Fatal("expected Domain property replacement")
	}
	_ = transferUser(t, c, next, "bob")
	_, e = c.DescribeServer(ctx, &sdk.DescribeServerInput{ServerId: id})
	transferStatus(t, e, "ResourceNotFoundException", 404)
	_, e = c.DescribeUser(ctx, &sdk.DescribeUserInput{ServerId: id, UserName: new("collision")})
	transferStatus(t, e, "ResourceNotFoundException", 404)
	// Removing optional policy restores the CF-specific fixed default.
	delete(replacement, "SecurityPolicyName")
	transferCFUpdate(t, cf, replacement, renamed, false, cfntypes.StackStatusUpdateComplete)
	if *transferServer(t, c, next).SecurityPolicyName != "TransferSecurityPolicy-2018-11" {
		t.Fatal("CF policy removal default")
	}
	_, e = cf.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new(name)})
	if e != nil {
		t.Fatal(e)
	}
	wantStatus(ctx, t, cf, *st.StackId, cfntypes.StackStatusDeleteComplete)
	_, e = c.DescribeServer(ctx, &sdk.DescribeServerInput{ServerId: next})
	transferStatus(t, e, "ResourceNotFoundException", 404)
	list, e := c.ListServers(ctx, &sdk.ListServersInput{})
	if e != nil || len(list.Servers) != 0 {
		t.Fatal("leftover server", list, e)
	}
}

func TestCloudFormationTransferCreateRollbackStrictPropertiesAndRuntimeAttributes(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	cf := cfnClient(s)
	c := transferClient(s, "test", "us-east-1")
	p, u := transferCFServer(), transferCFUser("alice")
	_, e := cf.CreateStack(
		ctx,
		&cfn.CreateStackInput{
			StackName:    new("transfer-create-rollback"),
			TemplateBody: new(transferCFTemplate(p, u, true)),
		},
	)
	if e != nil {
		t.Fatal(e)
	}
	wantStatus(ctx, t, cf, "transfer-create-rollback", cfntypes.StackStatusRollbackComplete)
	list, e := c.ListServers(ctx, &sdk.ListServersInput{})
	if e != nil || len(list.Servers) != 0 {
		t.Fatal("create rollback orphaned parent", list, e)
	}
	for _, value := range []any{false, nil, "", []any{}, map[string]any{}} {
		for _, field := range []string{
			"SshPublicKeys",
			"Policy",
			"HomeDirectoryMappings",
			"PosixProfile",
			"SshPublicKeyBody",
		} {
			bad := maps.Clone(u)
			bad[field] = value
			_, e = cf.CreateStack(
				ctx,
				&cfn.CreateStackInput{
					StackName:    new("transfer-excluded"),
					TemplateBody: new(transferCFTemplate(p, bad, false)),
				},
			)
			if e == nil {
				t.Fatal("excluded CF property", field, value)
			}
		}
	}

	for _, value := range []any{false, nil, "", []any{}, map[string]any{}} {
		bad := maps.Clone(u)
		bad["Tags"] = []any{map[string]any{"Key": "k", "Value": "v", "Future": value}}
		_, createErr := cf.CreateStack(
			ctx,
			&cfn.CreateStackInput{
				StackName:    new("transfer-nested-tag"),
				TemplateBody: new(transferCFTemplate(p, bad, false)),
			},
		)
		if createErr != nil {
			t.Fatal("selected top-level tags should reach resource validation", createErr)
		}
		rolledBack := wantStatus(ctx, t, cf, "transfer-nested-tag", cfntypes.StackStatusRollbackComplete)
		_, deleteErr := cf.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("transfer-nested-tag")})
		if deleteErr != nil {
			t.Fatal(deleteErr)
		}
		wantStatus(ctx, t, cf, *rolledBack.StackId, cfntypes.StackStatusDeleteComplete)
	}
	for _, attribute := range []string{"State", "Endpoint", "HostKeyFingerprint"} {
		raw := transferCFTemplate(p, u, false)
		var tmpl map[string]any
		json.Unmarshal([]byte(raw), &tmpl)
		tmpl["Outputs"].(map[string]any)["Bad"] = map[string]any{
			"Value": map[string]any{"Fn::GetAtt": []any{"Server", attribute}},
		}
		b, _ := json.Marshal(tmpl)
		_, e = cf.CreateStack(
			ctx,
			&cfn.CreateStackInput{StackName: new("transfer-runtime-attribute"), TemplateBody: new(string(b))},
		)
		if e == nil {
			t.Fatal("runtime GetAtt", attribute)
		}
	}
}

func TestCloudFormationTransferRejectedPrivateInputsDoNotLeakOrReplaceTemplate(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	cf := cfnClient(s)
	c := transferClient(s, "test", "us-east-1")
	p, u := transferCFServer(), transferCFUser("alice")
	name := "transfer-private"
	original := transferCFTemplate(p, u, false)
	_, e := cf.CreateStack(ctx, &cfn.CreateStackInput{StackName: new(name), TemplateBody: new(original)})
	if e != nil {
		t.Fatal(e)
	}
	st := wantStatus(ctx, t, cf, name, cfntypes.StackStatusCreateComplete)
	id := new(efsStackOutputs(st)["ServerId"])
	beforeS, beforeU := transferServer(t, c, id), transferUser(t, c, id, "alice")
	secret := "PRIVATE-MATERIAL-MUST-NOT-BE-STORED"
	for _, field := range []string{"HostKey", "Password", "SshPublicKeys"} {
		badS, badU := maps.Clone(p), maps.Clone(u)
		if field == "HostKey" {
			badS[field] = secret
		} else {
			badU[field] = secret
		}
		body := transferCFTemplate(badS, badU, false)
		_, createErr := cf.CreateStack(
			ctx,
			&cfn.CreateStackInput{StackName: new("transfer-rejected"), TemplateBody: new(body)},
		)
		if createErr == nil || strings.Contains(createErr.Error(), secret) {
			t.Fatal("private create", createErr)
		}
		_, readErr := cf.GetTemplate(ctx, &cfn.GetTemplateInput{StackName: new("transfer-rejected")})
		if readErr == nil {
			t.Fatal("rejected template persisted")
		}
		_, updateErr := cf.UpdateStack(ctx, &cfn.UpdateStackInput{StackName: new(name), TemplateBody: new(body)})
		if updateErr == nil || strings.Contains(updateErr.Error(), secret) {
			t.Fatal("private update", updateErr)
		}
	}
	retrieved, e := cf.GetTemplate(ctx, &cfn.GetTemplateInput{StackName: new(name)})
	if e != nil || *retrieved.TemplateBody != original {
		t.Fatal("rejected private update replaced template", retrieved, e)
	}
	events, e := cf.DescribeStackEvents(ctx, &cfn.DescribeStackEventsInput{StackName: new(name)})
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(events.StackEvents)
	if strings.Contains(string(raw), secret) {
		t.Fatal("event leak")
	}
	_, records := inspect(t, s, "GET", "/_tinystack/requests")
	for _, record := range records {
		if strings.Contains(record.RequestBody, secret) || strings.Contains(record.ResponseBody, secret) ||
			strings.Contains(record.ErrorMessage, secret) {
			t.Fatal("CF diagnostic leak")
		}
	}
	history, e := cloudtrailClient(s, "test", "us-east-1").LookupEvents(ctx, &cloudtrail.LookupEventsInput{})
	if e != nil {
		t.Fatal(e)
	}
	raw, _ = json.Marshal(history.Events)
	if strings.Contains(string(raw), secret) {
		t.Fatal("management event leak")
	}
	if !reflect.DeepEqual(beforeS, transferServer(t, c, id)) ||
		!reflect.DeepEqual(beforeU, transferUser(t, c, id, "alice")) {
		t.Fatal("rejected private input wrote")
	}
}
