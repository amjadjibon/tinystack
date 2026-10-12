package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"testing"

	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	sdk "github.com/aws/aws-sdk-go-v2/service/s3tables"
	"github.com/aws/aws-sdk-go-v2/service/s3tables/types"
)

func s3tablesCFTemplate(bucket string, p map[string]any, later bool) string {
	resources := map[string]any{
		"Bucket": map[string]any{
			"Type":       "AWS::S3Tables::TableBucket",
			"Properties": map[string]any{"TableBucketName": bucket},
		},
		"Namespace": map[string]any{
			"Type": "AWS::S3Tables::Namespace",
			"Properties": map[string]any{
				"TableBucketARN": map[string]any{"Fn::GetAtt": []any{"Bucket", "TableBucketARN"}},
				"Namespace":      "data",
			},
		},
		"Next": map[string]any{
			"Type": "AWS::S3Tables::Namespace",
			"Properties": map[string]any{
				"TableBucketARN": map[string]any{"Fn::GetAtt": []any{"Bucket", "TableBucketARN"}},
				"Namespace":      "next",
			},
		},
		"Table": map[string]any{"Type": "AWS::S3Tables::Table", "Properties": p},
	}
	if later {
		resources["Later"] = map[string]any{
			"Type":       "AWS::S3Tables::TableBucket",
			"DependsOn":  "Table",
			"Properties": map[string]any{"TableBucketName": "aws-invalid"},
		}
	}
	outputs := map[string]any{}
	for _, name := range []string{"Bucket", "Namespace", "Table"} {
		outputs[name+"Ref"] = map[string]any{"Value": map[string]any{"Ref": name}}
	}
	for name, ref := range map[string][]any{
		"BucketARN": {
			"Bucket",
			"TableBucketARN",
		},
		"TableARN": {
			"Table",
			"TableARN",
		},
		"VersionToken": {
			"Table",
			"VersionToken",
		},
	} {
		outputs[name] = map[string]any{"Value": map[string]any{"Fn::GetAtt": ref}}
	}
	raw, _ := json.Marshal(map[string]any{"Resources": resources, "Outputs": outputs})
	return string(raw)
}

func s3tablesCFProps() map[string]any {
	return map[string]any{
		"TableBucketARN":  map[string]any{"Fn::GetAtt": []any{"Bucket", "TableBucketARN"}},
		"Namespace":       map[string]any{"Ref": "Namespace"},
		"TableName":       "source",
		"OpenTableFormat": "ICEBERG",
		"WithoutMetadata": "Yes",
	}
}

func s3tablesCFUpdate(
	t *testing.T,
	cf *cfn.Client,
	bucket string,
	p map[string]any,
	later bool,
	status cfntypes.StackStatus,
) cfntypes.Stack {
	t.Helper()
	_, e := cf.UpdateStack(
		context.Background(),
		&cfn.UpdateStackInput{StackName: new("s3tables-cf"), TemplateBody: new(s3tablesCFTemplate(bucket, p, later))},
	)
	if e != nil {
		if status != cfntypes.StackStatusUpdateRollbackComplete {
			t.Fatal(e)
		}
		code(t, e, "ValidationError")
		read, e := cf.DescribeStacks(context.Background(), &cfn.DescribeStacksInput{StackName: new("s3tables-cf")})
		if e != nil {
			t.Fatal(e)
		}
		return read.Stacks[0]
	}
	return wantStatus(context.Background(), t, cf, "s3tables-cf", status)
}

func TestCloudFormationS3TablesHierarchyReferencesRenameAtomicTagsLiveCompensationAndReplacement(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	cf := cfnClient(s)
	c := s3tablesClient(s, "test", "us-east-1")
	p := s3tablesCFProps()
	p["Tags"] = locationCFTags("old")
	_, e := cf.CreateStack(
		ctx,
		&cfn.CreateStackInput{
			StackName:    new("s3tables-cf"),
			TemplateBody: new(s3tablesCFTemplate("cf-bucket", p, false)),
		},
	)
	if e != nil {
		t.Fatal(e)
	}
	st := wantStatus(ctx, t, cf, "s3tables-cf", cfntypes.StackStatusCreateComplete)
	out := efsStackOutputs(st)
	b, arn := out["BucketARN"], out["TableARN"]
	initial := s3tablesGet(t, c, arn)
	if out["BucketRef"] != "cf-bucket" || out["NamespaceRef"] != "data" || out["TableRef"] != "source" ||
		out["VersionToken"] != *initial.VersionToken ||
		len(s3tablesTags(t, c, arn)) != 50 ||
		initial.WarehouseLocation != nil {
		t.Fatal(out, initial)
	}
	p["Tags"] = locationCFTags("new")
	p["TableName"] = "renamed"
	p["Namespace"] = map[string]any{"Ref": "Next"}
	st = s3tablesCFUpdate(t, cf, "cf-bucket", p, false, cfntypes.StackStatusUpdateComplete)
	d := s3tablesGet(t, c, arn)
	out = efsStackOutputs(st)
	if out["TableRef"] != "renamed" || out["TableARN"] != arn || d.Namespace[0] != "next" ||
		*d.VersionToken == *initial.VersionToken ||
		len(s3tablesTags(t, c, arn)) != 50 ||
		s3tablesTags(t, c, arn)["new-00"] != "value" {
		t.Fatal(out, d)
	}
	// Capture actual external configuration/tags, not the previous template.
	_, e = c.RenameTable(
		ctx,
		&sdk.RenameTableInput{
			TableBucketARN: new(b),
			Namespace:      new("next"),
			Name:           new("renamed"),
			NewName:        new("external_name"),
			VersionToken:   d.VersionToken,
		},
	)
	if e != nil {
		t.Fatal(e)
	}
	_, e = c.UntagResource(ctx, &sdk.UntagResourceInput{ResourceArn: new(arn), TagKeys: []string{"new-00"}})
	if e != nil {
		t.Fatal(e)
	}
	_, e = c.TagResource(ctx, &sdk.TagResourceInput{ResourceArn: new(arn), Tags: map[string]string{"external": "live"}})
	if e != nil {
		t.Fatal(e)
	}
	live := s3tablesGet(t, c, arn)
	liveTags := s3tablesTags(t, c, arn)
	overflow := maps.Clone(p)
	overflow["TableName"] = "must_not_write"
	overflow["Tags"] = locationCFTags("overflow")
	s3tablesCFUpdate(t, cf, "cf-bucket", overflow, false, cfntypes.StackStatusUpdateRollbackComplete)
	if !reflect.DeepEqual(live, s3tablesGet(t, c, arn)) || !maps.Equal(liveTags, s3tablesTags(t, c, arn)) {
		t.Fatal("final tag overflow wrote live record")
	}
	// Later-resource failure restores live identity, timestamps and tags with fresh public token.
	candidate := maps.Clone(p)
	delete(candidate, "Tags")
	candidate["TableName"] = "attempt"
	candidate["Namespace"] = map[string]any{"Ref": "Namespace"}
	s3tablesCFUpdate(t, cf, "cf-bucket", candidate, true, cfntypes.StackStatusUpdateRollbackComplete)
	restored := s3tablesGet(t, c, arn)
	if *restored.VersionToken == *live.VersionToken || *restored.Name != *live.Name ||
		restored.Namespace[0] != live.Namespace[0] ||
		!restored.CreatedAt.Equal(*live.CreatedAt) ||
		!restored.ModifiedAt.Equal(*live.ModifiedAt) ||
		!maps.Equal(liveTags, s3tablesTags(t, c, arn)) {
		t.Fatal("live compensation", restored, live)
	}
	_, e = c.DeleteTable(
		ctx,
		&sdk.DeleteTableInput{
			TableBucketARN: new(b),
			Namespace:      new("next"),
			Name:           new("external_name"),
			VersionToken:   live.VersionToken,
		},
	)
	s3tablesStatus(t, e, "ConflictException", 409)
	for _, k := range []string{"TableName", "Namespace", "TableBucketARN", "OpenTableFormat", "WithoutMetadata"} {
		invalid := maps.Clone(p)
		delete(invalid, k)
		s3tablesCFUpdate(t, cf, "cf-bucket", invalid, false, cfntypes.StackStatusUpdateRollbackComplete)
		if !reflect.DeepEqual(restored, s3tablesGet(t, c, arn)) || !maps.Equal(liveTags, s3tablesTags(t, c, arn)) {
			t.Fatal("required removal wrote", k)
		}
	}
	for _, k := range []string{"IcebergMetadata", "Compaction", "SnapshotManagement", "StorageClassConfiguration"} {
		invalid := maps.Clone(p)
		invalid[k] = map[string]any{}
		s3tablesCFUpdate(t, cf, "cf-bucket", invalid, false, cfntypes.StackStatusUpdateRollbackComplete)
		if !reflect.DeepEqual(restored, s3tablesGet(t, c, arn)) {
			t.Fatal("unsupported change wrote", k)
		}
	}
	_, e = c.CreateTable(
		ctx,
		&sdk.CreateTableInput{
			TableBucketARN: new(b),
			Namespace:      new("next"),
			Name:           new("collision"),
			Format:         types.OpenTableFormatIceberg,
		},
	)
	if e != nil {
		t.Fatal(e)
	}
	collision := maps.Clone(p)
	collision["TableName"] = "collision"
	s3tablesCFUpdate(t, cf, "cf-bucket", collision, false, cfntypes.StackStatusUpdateRollbackComplete)
	if !reflect.DeepEqual(restored, s3tablesGet(t, c, arn)) {
		t.Fatal("collision wrote source")
	}
	_, e = c.DeleteTable(
		ctx,
		&sdk.DeleteTableInput{TableBucketARN: new(b), Namespace: new("next"), Name: new("collision")},
	)
	if e != nil {
		t.Fatal(e)
	}
	// Selected removal of Tags removes only stack-owned keys and retains external keys.
	delete(p, "Tags")
	p["TableName"] = "final"
	s3tablesCFUpdate(t, cf, "cf-bucket", p, false, cfntypes.StackStatusUpdateComplete)
	if !maps.Equal(s3tablesTags(t, c, arn), map[string]string{"external": "live"}) ||
		*s3tablesGet(t, c, arn).Name != "final" {
		t.Fatal("tag removal")
	}
	// A failed replacement must read the retired live record's current public token/identity.
	liveBeforeReplacement := s3tablesGet(t, c, arn)
	_, e = c.RenameTable(
		ctx,
		&sdk.RenameTableInput{
			TableBucketARN: new(b),
			Namespace:      new("next"),
			Name:           new("final"),
			NewName:        new("external_before_replacement"),
			VersionToken:   liveBeforeReplacement.VersionToken,
		},
	)
	if e != nil {
		t.Fatal(e)
	}
	liveBeforeReplacement = s3tablesGet(t, c, arn)
	rollbackStack := s3tablesCFUpdate(t, cf, "replacement-failed", p, true, cfntypes.StackStatusUpdateRollbackComplete)
	rollbackOutputs := efsStackOutputs(rollbackStack)
	if !reflect.DeepEqual(liveBeforeReplacement, s3tablesGet(t, c, arn)) ||
		rollbackOutputs["TableRef"] != "external_before_replacement" ||
		rollbackOutputs["VersionToken"] != *liveBeforeReplacement.VersionToken {
		t.Fatal("replacement rollback did not read live identity/token", rollbackOutputs)
	}
	_, e = c.GetTableBucket(
		ctx,
		&sdk.GetTableBucketInput{
			TableBucketARN: new("arn:aws:s3tables:us-east-1:000000000000:bucket/replacement-failed"),
		},
	)
	s3tablesStatus(t, e, "NotFoundException", 404)
	// Successful hierarchy replacement retires old same-named namespace/table records before parents.
	st = s3tablesCFUpdate(t, cf, "replacement-bucket", p, false, cfntypes.StackStatusUpdateComplete)
	next := efsStackOutputs(st)
	if next["TableARN"] == arn || next["BucketARN"] == b || next["TableRef"] != "final" {
		t.Fatal(next)
	}
	_, e = c.GetTableBucket(ctx, &sdk.GetTableBucketInput{TableBucketARN: new(b)})
	s3tablesStatus(t, e, "NotFoundException", 404)
	_, e = c.GetTable(ctx, &sdk.GetTableInput{TableArn: new(arn)})
	s3tablesStatus(t, e, "NotFoundException", 404)
	// Parent deletion refuses unrelated tables; delete retains that external record.
	unrelated, e := c.CreateTable(
		ctx,
		&sdk.CreateTableInput{
			TableBucketARN: new(next["BucketARN"]),
			Namespace:      new("data"),
			Name:           new("external"),
			Format:         types.OpenTableFormatIceberg,
		},
	)
	if e != nil {
		t.Fatal(e)
	}
	_, e = cf.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("s3tables-cf")})
	if e != nil {
		t.Fatal(e)
	}
	wantStatus(ctx, t, cf, "s3tables-cf", cfntypes.StackStatusDeleteFailed)
	s3tablesGet(t, c, *unrelated.TableARN)
	_, e = c.DeleteTable(
		ctx,
		&sdk.DeleteTableInput{TableBucketARN: new(next["BucketARN"]), Namespace: new("data"), Name: new("external")},
	)
	if e != nil {
		t.Fatal(e)
	}
	_, e = cf.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("s3tables-cf")})
	if e != nil {
		t.Fatal(e)
	}
	wantStatus(ctx, t, cf, *st.StackId, cfntypes.StackStatusDeleteComplete)
}

func TestCloudFormationS3TablesBucketFinalTagsStrictPropertiesAndCreateRollback(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	cf := cfnClient(s)
	c := s3tablesClient(s, "test", "us-east-1")
	template := func(p map[string]any, later bool) string {
		resources := map[string]any{"Bucket": map[string]any{"Type": "AWS::S3Tables::TableBucket", "Properties": p}}
		if later {
			resources["Later"] = map[string]any{
				"Type":      "AWS::S3Tables::Namespace",
				"DependsOn": "Bucket",
				"Properties": map[string]any{
					"TableBucketARN": map[string]any{"Fn::GetAtt": []any{"Bucket", "TableBucketARN"}},
					"Namespace":      "aws_invalid",
				},
			}
		}
		raw, _ := json.Marshal(map[string]any{"Resources": resources})
		return string(raw)
	}
	p := map[string]any{"TableBucketName": "tag-cf-bucket", "Tags": locationCFTags("old")}
	_, e := cf.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: new("bucket-tags"), TemplateBody: new(template(p, false))},
	)
	if e != nil {
		t.Fatal(e)
	}
	bucketStack := wantStatus(ctx, t, cf, "bucket-tags", cfntypes.StackStatusCreateComplete)
	arn := "arn:aws:s3tables:us-east-1:000000000000:bucket/tag-cf-bucket"
	update := func(p map[string]any, later bool, status cfntypes.StackStatus) {
		t.Helper()
		_, updateErr := cf.UpdateStack(
			ctx,
			&cfn.UpdateStackInput{StackName: new("bucket-tags"), TemplateBody: new(template(p, later))},
		)
		if updateErr != nil {
			if status != cfntypes.StackStatusUpdateRollbackComplete {
				t.Fatal(updateErr)
			}
			code(t, updateErr, "ValidationError")
			return
		}
		wantStatus(ctx, t, cf, "bucket-tags", status)
	}
	p["Tags"] = locationCFTags("new")
	update(p, false, cfntypes.StackStatusUpdateComplete)
	if len(s3tablesTags(t, c, arn)) != 50 || s3tablesTags(t, c, arn)["new-00"] != "value" {
		t.Fatal("disjoint final tags")
	}
	_, e = c.UntagResource(ctx, &sdk.UntagResourceInput{ResourceArn: new(arn), TagKeys: []string{"new-00"}})
	if e != nil {
		t.Fatal(e)
	}
	_, e = c.TagResource(ctx, &sdk.TagResourceInput{ResourceArn: new(arn), Tags: map[string]string{"external": "live"}})
	if e != nil {
		t.Fatal(e)
	}
	live := s3tablesTags(t, c, arn)
	invalid := maps.Clone(p)
	invalid["Tags"] = locationCFTags("overflow")
	update(invalid, false, cfntypes.StackStatusUpdateRollbackComplete)
	if !maps.Equal(live, s3tablesTags(t, c, arn)) {
		t.Fatal("overflow wrote bucket tags")
	}
	changed := maps.Clone(p)
	delete(changed, "Tags")
	update(changed, true, cfntypes.StackStatusUpdateRollbackComplete)
	if !maps.Equal(live, s3tablesTags(t, c, arn)) {
		t.Fatal("later failure did not restore live bucket tags")
	}
	for _, tags := range []any{
		[]any{map[string]any{"Key": "colon:key", "Value": "bad"}},
		[]any{map[string]any{"Key": "tab\tkey", "Value": "bad"}},
		nil,
		map[string]any{},
		[]any{map[string]any{
			"Key":   "x",
			"Value": "y",
			"Extra": true,
		}},
		[]any{
			map[string]any{
				"Key":   "x",
				"Value": "a",
			},
			map[string]any{
				"Key":   "x",
				"Value": "b",
			},
		},
		[]any{map[string]any{
			"Key":   "x",
			"Value": false,
		}},
	} {
		invalid := maps.Clone(p)
		invalid["Tags"] = tags
		update(invalid, false, cfntypes.StackStatusUpdateRollbackComplete)
		if !maps.Equal(live, s3tablesTags(t, c, arn)) {
			t.Fatal("strict tag candidate wrote")
		}
	}
	for _, property := range []string{
		"EncryptionConfiguration",
		"UnreferencedFileRemoval",
		"MetricsConfiguration",
		"ReplicationConfiguration",
		"StorageClassConfiguration",
	} {
		invalid := maps.Clone(p)
		invalid["TableBucketName"] = "must-not-create"
		invalid[property] = map[string]any{}
		update(invalid, false, cfntypes.StackStatusUpdateRollbackComplete)
		_, e = c.GetTableBucket(
			ctx,
			&sdk.GetTableBucketInput{
				TableBucketARN: new("arn:aws:s3tables:us-east-1:000000000000:bucket/must-not-create"),
			},
		)
		s3tablesStatus(t, e, "NotFoundException", 404)
	}
	delete(p, "Tags")
	update(p, false, cfntypes.StackStatusUpdateComplete)
	if !maps.Equal(s3tablesTags(t, c, arn), map[string]string{"external": "live"}) {
		t.Fatal("bucket tag removal")
	}
	_, e = cf.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("bucket-tags")})
	if e != nil {
		t.Fatal(e)
	}
	wantStatus(ctx, t, cf, *bucketStack.StackId, cfntypes.StackStatusDeleteComplete)
	for i := range 2 {
		name := fmt.Sprintf("create-rollback-%d", i)
		p := s3tablesCFProps()
		raw := s3tablesCFTemplate(name, p, true)
		_, e = cf.CreateStack(ctx, &cfn.CreateStackInput{StackName: new(name), TemplateBody: new(raw)})
		if e != nil {
			t.Fatal(e)
		}
		wantStatus(ctx, t, cf, name, cfntypes.StackStatusRollbackComplete)
		_, e = c.GetTableBucket(
			ctx,
			&sdk.GetTableBucketInput{TableBucketARN: new("arn:aws:s3tables:us-east-1:000000000000:bucket/" + name)},
		)
		s3tablesStatus(t, e, "NotFoundException", 404)
	}
}
