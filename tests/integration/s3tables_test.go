package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	ordinary "github.com/aws/aws-sdk-go-v2/service/s3"
	sdk "github.com/aws/aws-sdk-go-v2/service/s3tables"
	"github.com/aws/aws-sdk-go-v2/service/s3tables/types"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/amjadjibon/tinystack/internal/storage/archive"
)

func s3tablesClient(s *httptest.Server, key, region string) *sdk.Client {
	return sdk.New(
		sdk.Options{Region: region, BaseEndpoint: new(s.URL), Credentials: creds(key), Retryer: aws.NopRetryer{}},
	)
}

func s3tablesStatus(t *testing.T, e error, name string, status int) {
	t.Helper()
	code(t, e, name)
	var r *smithyhttp.ResponseError
	if !errors.As(e, &r) || r.HTTPStatusCode() != status {
		t.Fatal("status", e)
	}
	switch name {
	case "BadRequestException":
		var v *types.BadRequestException
		if !errors.As(e, &v) || aws.ToString(v.Message) == "" {
			t.Fatal("typed invalid", e)
		}
	case "NotFoundException":
		var v *types.NotFoundException
		if !errors.As(e, &v) || aws.ToString(v.Message) == "" {
			t.Fatal("typed missing", e)
		}
	case "ConflictException":
		var v *types.ConflictException
		if !errors.As(e, &v) || aws.ToString(v.Message) == "" {
			t.Fatal("typed conflict", e)
		}
	}
}

func s3tablesGet(t *testing.T, c *sdk.Client, arn string) *sdk.GetTableOutput {
	t.Helper()
	out, e := c.GetTable(context.Background(), &sdk.GetTableInput{TableArn: new(arn)})
	if e != nil {
		t.Fatal(e)
	}
	out.ResultMetadata = middleware.Metadata{}
	return out
}

func s3tablesTags(t *testing.T, c *sdk.Client, arn string) map[string]string {
	t.Helper()
	out, e := c.ListTagsForResource(context.Background(), &sdk.ListTagsForResourceInput{ResourceArn: new(arn)})
	if e != nil {
		t.Fatal(e)
	}
	return out.Tags
}

func s3tablesHierarchy(t *testing.T, c *sdk.Client, bucket, ns, name string) (bucketARN, tableARN string) {
	t.Helper()
	ctx := context.Background()
	b, e := c.CreateTableBucket(
		ctx,
		&sdk.CreateTableBucketInput{Name: new(bucket), Tags: map[string]string{"owner": "bucket"}},
	)
	if e != nil {
		t.Fatal(e)
	}
	_, e = c.CreateNamespace(ctx, &sdk.CreateNamespaceInput{TableBucketARN: b.Arn, Namespace: []string{ns}})
	if e != nil {
		t.Fatal(e)
	}
	r, e := c.CreateTable(
		ctx,
		&sdk.CreateTableInput{
			TableBucketARN: b.Arn,
			Namespace:      new(ns),
			Name:           new(name),
			Format:         types.OpenTableFormatIceberg,
			Tags:           map[string]string{"owner": "table"},
		},
	)
	if e != nil {
		t.Fatal(e)
	}
	return *b.Arn, *r.TableARN
}

func TestS3TablesSDKAllSixteenOperationsErrorsRGTAAndSignature(t *testing.T) {
	for _, verify := range []bool{false, true} {
		t.Run(fmt.Sprint(verify), func(t *testing.T) {
			s := server(t)
			if verify {
				s = verifyingServer(t)
			}
			ctx := context.Background()
			c := s3tablesClient(s, "test", "us-east-1")
			b, arn := s3tablesHierarchy(t, c, "sdk-bucket", "data", "source")
			normal, normalErr := s3Client(s, "test", "us-east-1").ListBuckets(ctx, &ordinary.ListBucketsInput{})
			if normalErr != nil || len(normal.Buckets) != 0 {
				t.Fatal("table bucket polluted ordinary S3", normal, normalErr)
			}
			bucket, e := c.GetTableBucket(ctx, &sdk.GetTableBucketInput{TableBucketARN: new(b)})
			if e != nil || *bucket.Arn != b || *bucket.Name != "sdk-bucket" ||
				*bucket.OwnerAccountId != "000000000000" ||
				bucket.Type != types.TableBucketTypeCustomer ||
				bucket.TableBucketId == nil {
				t.Fatal(bucket, e)
			}
			n, e := c.GetNamespace(ctx, &sdk.GetNamespaceInput{TableBucketARN: new(b), Namespace: new("data")})
			if e != nil || !reflect.DeepEqual(n.Namespace, []string{"data"}) ||
				*n.TableBucketId != *bucket.TableBucketId ||
				*n.CreatedBy != "000000000000" {
				t.Fatal(n, e)
			}
			table := s3tablesGet(t, c, arn)
			if *table.Name != "source" || table.Format != types.OpenTableFormatIceberg ||
				table.Type != types.TableTypeCustomer ||
				*table.NamespaceId != *n.NamespaceId ||
				!table.CreatedAt.Equal(*table.ModifiedAt) ||
				table.MetadataLocation != nil ||
				table.WarehouseLocation != nil ||
				table.ManagedByService != nil {
				t.Fatal(table)
			}
			nameGet, e := c.GetTable(
				ctx,
				&sdk.GetTableInput{TableBucketARN: new(b), Namespace: new("data"), Name: new("source")},
			)
			if e != nil {
				t.Fatal(e)
			}
			nameGet.ResultMetadata = middleware.Metadata{}
			if !reflect.DeepEqual(table, nameGet) {
				t.Fatal("identity alternatives differ")
			}
			_, e = c.CreateTableBucket(ctx, &sdk.CreateTableBucketInput{Name: new("sdk-bucket")})
			s3tablesStatus(t, e, "ConflictException", 409)
			_, e = c.CreateNamespace(
				ctx,
				&sdk.CreateNamespaceInput{TableBucketARN: new(b), Namespace: []string{"data"}},
			)
			s3tablesStatus(t, e, "ConflictException", 409)
			_, e = c.CreateTable(
				ctx,
				&sdk.CreateTableInput{
					TableBucketARN: new(b),
					Namespace:      new("data"),
					Name:           new("source"),
					Format:         types.OpenTableFormatIceberg,
				},
			)
			s3tablesStatus(t, e, "ConflictException", 409)
			_, e = c.CreateTable(
				ctx,
				&sdk.CreateTableInput{
					TableBucketARN: new(b),
					Namespace:      new("missing"),
					Name:           new("valid"),
					Format:         types.OpenTableFormatIceberg,
				},
			)
			s3tablesStatus(t, e, "NotFoundException", 404)
			_, e = c.CreateTableBucket(ctx, &sdk.CreateTableBucketInput{Name: new("aws-reserved")})
			s3tablesStatus(t, e, "BadRequestException", 400)
			_, e = c.DeleteTableBucket(ctx, &sdk.DeleteTableBucketInput{TableBucketARN: new(b)})
			s3tablesStatus(t, e, "ConflictException", 409)
			_, e = c.DeleteNamespace(ctx, &sdk.DeleteNamespaceInput{TableBucketARN: new(b), Namespace: new("data")})
			s3tablesStatus(t, e, "ConflictException", 409)
			buckets, e := c.ListTableBuckets(
				ctx,
				&sdk.ListTableBucketsInput{
					MaxBuckets: new(int32(1)),
					Prefix:     new("sdk"),
					Type:       types.TableBucketTypeCustomer,
				},
			)
			if e != nil || len(buckets.TableBuckets) != 1 {
				t.Fatal(buckets, e)
			}
			namespaces, e := c.ListNamespaces(
				ctx,
				&sdk.ListNamespacesInput{TableBucketARN: new(b), MaxNamespaces: new(int32(1)), Prefix: new("da")},
			)
			if e != nil || len(namespaces.Namespaces) != 1 {
				t.Fatal(namespaces, e)
			}
			tables, e := c.ListTables(
				ctx,
				&sdk.ListTablesInput{
					TableBucketARN: new(b),
					Namespace:      new("data"),
					Prefix:         new("sou"),
					MaxTables:      new(int32(1)),
				},
			)
			if e != nil || len(tables.Tables) != 1 || *tables.Tables[0].TableARN != arn {
				t.Fatal(tables, e)
			}
			_, e = c.CreateNamespace(
				ctx,
				&sdk.CreateNamespaceInput{TableBucketARN: new(b), Namespace: []string{"next"}},
			)
			if e != nil {
				t.Fatal(e)
			}
			_, e = c.RenameTable(
				ctx,
				&sdk.RenameTableInput{
					TableBucketARN:   new(b),
					Namespace:        new("data"),
					Name:             new("source"),
					NewName:          new("renamed"),
					NewNamespaceName: new("next"),
					VersionToken:     table.VersionToken,
				},
			)
			if e != nil {
				t.Fatal(e)
			}
			renamed := s3tablesGet(t, c, arn)
			if *renamed.Name != "renamed" || renamed.Namespace[0] != "next" ||
				*renamed.VersionToken == *table.VersionToken ||
				!renamed.CreatedAt.Equal(*table.CreatedAt) ||
				!renamed.ModifiedAt.After(*table.ModifiedAt) {
				t.Fatal(renamed)
			}
			_, e = c.RenameTable(
				ctx,
				&sdk.RenameTableInput{
					TableBucketARN: new(b),
					Namespace:      new("next"),
					Name:           new("renamed"),
					NewName:        new("stale"),
					VersionToken:   table.VersionToken,
				},
			)
			s3tablesStatus(t, e, "ConflictException", 409)
			_, e = c.DeleteTable(
				ctx,
				&sdk.DeleteTableInput{
					TableBucketARN: new(b),
					Namespace:      new("next"),
					Name:           new("renamed"),
					VersionToken:   table.VersionToken,
				},
			)
			s3tablesStatus(t, e, "ConflictException", 409)
			for _, resource := range []string{b, arn} {
				_, e = c.TagResource(
					ctx,
					&sdk.TagResourceInput{
						ResourceArn: new(resource),
						Tags:        map[string]string{"stage": "demo", "empty": ""},
					},
				)
				if e != nil {
					t.Fatal(e)
				}
				if got := s3tablesTags(t, c, resource); got["stage"] != "demo" || got["empty"] != "" {
					t.Fatal(got)
				}
				_, e = c.UntagResource(
					ctx,
					&sdk.UntagResourceInput{ResourceArn: new(resource), TagKeys: []string{"stage", "missing"}},
				)
				if e != nil {
					t.Fatal(e)
				}
				rg := resourcegroupstaggingapi.New(
					resourcegroupstaggingapi.Options{
						Region:       "us-east-1",
						BaseEndpoint: new(s.URL),
						Credentials:  creds("test"),
						Retryer:      aws.NopRetryer{},
					},
				)
				tagged, tagErr := rg.TagResources(
					ctx,
					&resourcegroupstaggingapi.TagResourcesInput{
						ResourceARNList: []string{resource},
						Tags:            map[string]string{"rgta": "real"},
					},
				)
				if tagErr != nil || len(tagged.FailedResourcesMap) != 0 ||
					s3tablesTags(t, c, resource)["rgta"] != "real" {
					t.Fatal(tagged, tagErr)
				}
				mappings, mappingErr := rg.GetResources(
					ctx,
					&resourcegroupstaggingapi.GetResourcesInput{ResourceARNList: []string{resource}},
				)
				if mappingErr != nil || len(mappings.ResourceTagMappingList) != 1 {
					t.Fatal(mappings, mappingErr)
				}
				untagged, untagErr := rg.UntagResources(
					ctx,
					&resourcegroupstaggingapi.UntagResourcesInput{
						ResourceARNList: []string{resource},
						TagKeys:         []string{"rgta"},
					},
				)
				if untagErr != nil || len(untagged.FailedResourcesMap) != 0 {
					t.Fatal(untagged, untagErr)
				}
			}
			if !reflect.DeepEqual(renamed, s3tablesGet(t, c, arn)) {
				t.Fatal("tags changed public table metadata")
			}
			if verify {
				_, e = s3tablesClient(s, "wrong", "us-east-1").ListTableBuckets(ctx, &sdk.ListTableBucketsInput{})
				code(t, e, "InvalidSignatureException")
			}
			_, e = c.DeleteTable(
				ctx,
				&sdk.DeleteTableInput{
					TableBucketARN: new(b),
					Namespace:      new("next"),
					Name:           new("renamed"),
					VersionToken:   renamed.VersionToken,
				},
			)
			if e != nil {
				t.Fatal(e)
			}
			_, e = c.DeleteTable(
				ctx,
				&sdk.DeleteTableInput{TableBucketARN: new(b), Namespace: new("next"), Name: new("renamed")},
			)
			s3tablesStatus(t, e, "NotFoundException", 404)
			for _, ns := range []string{"data", "next"} {
				_, e = c.DeleteNamespace(ctx, &sdk.DeleteNamespaceInput{TableBucketARN: new(b), Namespace: new(ns)})
				if e != nil {
					t.Fatal(e)
				}
			}
			_, e = c.DeleteTableBucket(ctx, &sdk.DeleteTableBucketInput{TableBucketARN: new(b)})
			if e != nil {
				t.Fatal(e)
			}
			_, e = c.DeleteTableBucket(ctx, &sdk.DeleteTableBucketInput{TableBucketARN: new(b)})
			s3tablesStatus(t, e, "NotFoundException", 404)
		})
	}
}

func s3tablesWire(t *testing.T, s *httptest.Server, verb, path, body string) (status int, output map[string]any) {
	t.Helper()
	r, e := http.NewRequestWithContext(context.Background(), verb, s.URL+path, strings.NewReader(body))
	if e != nil {
		t.Fatal(e)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(
		"Authorization",
		"AWS4-HMAC-SHA256 Credential=test/20261012/us-east-1/s3tables/aws4_request, SignedHeaders=host, Signature=abcd",
	)
	res, e := s.Client().Do(r)
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	raw, e := io.ReadAll(res.Body)
	if e != nil {
		t.Fatal(e)
	}
	out := map[string]any{}
	if len(raw) > 0 {
		if e = json.Unmarshal(raw, &out); e != nil {
			t.Fatal(string(raw), e)
		}
	}
	return res.StatusCode, out
}

func TestS3TablesRawRESTBindingsCasingLabelsQueryAndExcludedRoutes(t *testing.T) {
	s := server(t)
	status, out := s3tablesWire(t, s, "PUT", "/buckets", `{"name":"wire-bucket","tags":{"owner":"wire"}}`)
	if status != 200 {
		t.Fatal(status, out)
	}
	b := out["arn"].(string)
	encoded := url.PathEscape(b)
	status, out = s3tablesWire(t, s, "GET", "/buckets/"+encoded, "")
	if status != 200 || out["name"] != "wire-bucket" || out["type"] != "customer" || out["createdAt"] == nil {
		t.Fatal(status, out)
	}
	status, out = s3tablesWire(t, s, "PUT", "/namespaces/"+encoded, `{"namespace":["data"]}`)
	if status != 200 || out["tableBucketARN"] != b {
		t.Fatal(status, out)
	}
	status, out = s3tablesWire(t, s, "GET", "/namespaces/"+encoded+"/data", "")
	if status != 200 || out["createdBy"] != "000000000000" {
		t.Fatal(status, out)
	}
	status, out = s3tablesWire(
		t,
		s,
		"PUT",
		"/tables/"+encoded+"/data",
		`{"name":"source","format":"ICEBERG","tags":{"empty":""}}`,
	)
	if status != 200 {
		t.Fatal(status, out)
	}
	arn, token := out["tableARN"].(string), out["versionToken"].(string)
	status, out = s3tablesWire(t, s, "GET", "/get-table?tableArn="+url.QueryEscape(arn), "")
	if status != 200 || out["format"] != "ICEBERG" || out["name"] != "source" || out["modifiedAt"] == nil {
		t.Fatal(status, out)
	}
	for _, k := range []string{"metadataLocation", "warehouseLocation", "managedByService"} {
		if _, ok := out[k]; ok {
			t.Fatal("fabricated", k)
		}
	}
	status, out = s3tablesWire(
		t,
		s,
		"GET",
		"/get-table?tableBucketARN="+url.QueryEscape(b)+"&namespace=data&name=source",
		"",
	)
	if status != 200 || out["tableARN"] != arn {
		t.Fatal(status, out)
	}
	for _, path := range []string{
		"/buckets?prefix=wire&maxBuckets=1&type=customer",
		"/namespaces/" + encoded + "?prefix=da&maxNamespaces=1",
		"/tables/" + encoded + "?namespace=data&prefix=sou&maxTables=1",
	} {
		status, out = s3tablesWire(t, s, "GET", path, "")
		if status != 200 {
			t.Fatal(status, out)
		}
	}
	status, out = s3tablesWire(t, s, "POST", "/tag/"+url.PathEscape(arn), `{"tags":{"changed":"yes"}}`)
	if status != 200 {
		t.Fatal(status, out)
	}
	status, out = s3tablesWire(t, s, "GET", "/tag/"+url.PathEscape(arn), "")
	if status != 200 || out["tags"].(map[string]any)["changed"] != "yes" {
		t.Fatal(status, out)
	}
	status, out = s3tablesWire(t, s, "DELETE", "/tag/"+url.PathEscape(arn)+"?tagKeys=changed&tagKeys=empty", "")
	if status != 204 || len(out) != 0 {
		t.Fatal(status, out)
	}
	status, out = s3tablesWire(
		t,
		s,
		"PUT",
		"/tables/"+encoded+"/data/source/rename",
		`{"newName":"renamed","versionToken":"`+token+`"}`,
	)
	if status != 204 || len(out) != 0 {
		t.Fatal(status, out)
	}
	status, out = s3tablesWire(
		t,
		s,
		"DELETE",
		"/tables/"+encoded+"/data/renamed?versionToken="+url.QueryEscape(token),
		"",
	)
	if status != 409 || out["__type"] != "ConflictException" || out["message"] == nil {
		t.Fatal(status, out)
	}
	status, out = s3tablesWire(t, s, "PUT", "/buckets", `{"name":"aws-reserved"}`)
	if status != 400 || out["__type"] != "BadRequestException" || out["message"] == nil {
		t.Fatal(status, out)
	}
	status, out = s3tablesWire(t, s, "GET", "/tables/"+encoded+"/missing", "")
	if status >= 200 && status < 300 {
		t.Fatal("unmodeled route succeeded", out)
	}
	for _, body := range []string{
		`{"name":"other","format":"ICEBERG","metadata":null}`,
		`{"name":"other","format":"ICEBERG","metadata":{"iceberg":{"schema":{"fields":[]}}}}`,
		`{"name":"other","format":"ICEBERG","encryptionConfiguration":{}}`,
		`{"name":"other","format":"ICEBERG","storageClassConfiguration":false}`,
	} {
		status, out = s3tablesWire(t, s, "PUT", "/tables/"+encoded+"/data", body)
		if status < 400 {
			t.Fatal("unsupported advanced input succeeded", body, status, out)
		}
	}
	for _, body := range []string{
		`{"name":"other","encryptionConfiguration":null}`,
		`{"name":"other","storageClassConfiguration":{}}`,
	} {
		status, out = s3tablesWire(t, s, "PUT", "/buckets", body)
		if status < 400 {
			t.Fatal("unsupported input succeeded", body, status, out)
		}
	}
	for _, r := range []struct{ verb, path, body string }{
		{"GET", "/tables/" + encoded + "/data/renamed/metadata-location", ""},
		{
			"PUT",
			"/tables/" + encoded + "/data/renamed/metadata-location",
			`{"metadataLocation":"s3://fake/file","versionToken":"x"}`,
		},

		{"GET", "/tables/" + encoded + "/data/renamed/maintenance", ""},
		{"PUT", "/tables/" + encoded + "/data/renamed/maintenance", `{}`},
		{"GET", "/iceberg/v1/config", ""},
	} {
		status, out = s3tablesWire(t, s, r.verb, r.path, r.body)
		if status < 400 {
			t.Fatal("unimplemented runtime/data route succeeded", r, status, out)
		}
	}
	status, out = s3tablesWire(t, s, "DELETE", "/tables/"+encoded+"/data/renamed", "")
	if status != 204 || len(out) != 0 {
		t.Fatal(status, out)
	}
	status, out = s3tablesWire(t, s, "GET", "/get-table?tableArn="+url.QueryEscape(arn), "")
	if status != 404 || out["__type"] != "NotFoundException" || out["message"] == nil {
		t.Fatal(status, out)
	}
	status, out = s3tablesWire(t, s, "DELETE", "/namespaces/"+encoded+"/data", "")
	if status != 204 {
		t.Fatal(status, out)
	}
	status, out = s3tablesWire(t, s, "DELETE", "/buckets/"+encoded, "")
	if status != 204 {
		t.Fatal(status, out)
	}
}

type s3tablesFixtureState struct {
	bucket                *sdk.GetTableBucketOutput
	namespace             *sdk.GetNamespaceOutput
	table                 *sdk.GetTableOutput
	bucketTags, tableTags map[string]string
}

func s3tablesFixtureRead(t *testing.T, c *sdk.Client, b, arn string) s3tablesFixtureState {
	t.Helper()
	ctx := context.Background()
	bucket, e := c.GetTableBucket(ctx, &sdk.GetTableBucketInput{TableBucketARN: new(b)})
	if e != nil {
		t.Fatal(e)
	}
	n, e := c.GetNamespace(ctx, &sdk.GetNamespaceInput{TableBucketARN: new(b), Namespace: new("next")})
	if e != nil {
		t.Fatal(e)
	}
	bucket.ResultMetadata = middleware.Metadata{}
	n.ResultMetadata = middleware.Metadata{}
	return s3tablesFixtureState{
		bucket:     bucket,
		namespace:  n,
		table:      s3tablesGet(t, c, arn),
		bucketTags: s3tablesTags(t, c, b),
		tableTags:  s3tablesTags(t, c, arn),
	}
}

func TestS3TablesSQLiteOfflineFixtureExactHierarchyReopenAndReset(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	c := s3tablesClient(s, "team-b", "ap-southeast-1")
	b, arn := s3tablesHierarchy(t, c, "fixture-bucket", "original", "source")
	_, e := c.CreateNamespace(ctx, &sdk.CreateNamespaceInput{TableBucketARN: new(b), Namespace: []string{"next"}})
	if e != nil {
		stop()
		t.Fatal(e)
	}
	first := s3tablesGet(t, c, arn)
	_, e = c.RenameTable(
		ctx,
		&sdk.RenameTableInput{
			TableBucketARN:   new(b),
			Namespace:        new("original"),
			Name:             new("source"),
			NewNamespaceName: new("next"),
			NewName:          new("renamed"),
			VersionToken:     first.VersionToken,
		},
	)
	if e != nil {
		stop()
		t.Fatal(e)
	}
	for _, resource := range []string{b, arn} {
		_, e = c.TagResource(
			ctx,
			&sdk.TagResourceInput{
				ResourceArn: new(resource),
				Tags:        map[string]string{"external": "persisted", "empty": ""},
			},
		)
		if e != nil {
			stop()
			t.Fatal(e)
		}
	}
	before := s3tablesFixtureRead(t, c, b, arn)
	if !strings.Contains(b, ":ap-southeast-1:111111111111:") ||
		!before.table.ModifiedAt.After(*before.table.CreatedAt) {
		stop()
		t.Fatal(before)
	}
	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	if !reflect.DeepEqual(before, s3tablesFixtureRead(t, s3tablesClient(s, "team-b", "ap-southeast-1"), b, arn)) {
		stop()
		t.Fatal("SQLite exact hierarchy")
	}
	stop()
	fixture := filepath.Join(t.TempDir(), "s3tables.tar.gz")
	if e = archive.Export(ctx, archive.ExportOptions{StorageDir: dir, OutputFile: fixture}); e != nil {
		t.Fatal(e)
	}
	child := filepath.Join(t.TempDir(), "child")
	if e = archive.Import(ctx, archive.ImportOptions{StorageDir: child, InputFile: fixture}); e != nil {
		t.Fatal(e)
	}
	for range 2 {
		s, stop = persistent(t, child, &fakeRuntime{})
		if !reflect.DeepEqual(before, s3tablesFixtureRead(t, s3tablesClient(s, "team-b", "ap-southeast-1"), b, arn)) {
			stop()
			t.Fatal("offline fixture normal startup/restart")
		}
		stop()
	}
	s, stop = persistent(t, child, &fakeRuntime{})
	defer stop()
	c = s3tablesClient(s, "team-b", "ap-southeast-1")
	for _, other := range []*sdk.Client{
		s3tablesClient(s,
			"test",
			"ap-southeast-1"),
		s3tablesClient(s,
			"team-b",
			"us-east-1"),
	} {
		_, e = other.GetTable(ctx, &sdk.GetTableInput{TableArn: new(arn)})
		s3tablesStatus(t, e, "BadRequestException", 400)
	}
	res, e := s.Client().Get(s.URL + "/_tinystack/resources")
	if e != nil {
		t.Fatal(e)
	}
	raw, e := io.ReadAll(res.Body)
	res.Body.Close()
	if e != nil || !strings.Contains(string(raw), arn) || !strings.Contains(string(raw), `"RecordsOnly":true`) ||
		!strings.Contains(string(raw), `"ExecutionReady":false`) {
		t.Fatal(string(raw), e)
	}
	req, _ := http.NewRequestWithContext(ctx, "POST", s.URL+"/_tinystack/reset", http.NoBody)
	res, e = s.Client().Do(req)
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal(res.StatusCode)
	}
	_, e = c.GetTable(ctx, &sdk.GetTableInput{TableArn: new(arn)})
	s3tablesStatus(t, e, "NotFoundException", 404)
	list, e := c.ListTableBuckets(ctx, &sdk.ListTableBucketsInput{})
	if e != nil || len(list.TableBuckets) != 0 {
		t.Fatal(list, e)
	}
	_, e = c.ListNamespaces(ctx, &sdk.ListNamespacesInput{TableBucketARN: new(b)})
	s3tablesStatus(t, e, "NotFoundException", 404)
	res, e = s.Client().Get(s.URL + "/_tinystack/resources")
	if e != nil {
		t.Fatal(e)
	}
	raw, e = io.ReadAll(res.Body)
	res.Body.Close()
	if e != nil || strings.Contains(string(raw), arn) || strings.Contains(string(raw), b) {
		t.Fatal("reset inventory", string(raw), e)
	}
}
