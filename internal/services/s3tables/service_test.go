package s3tables

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3tables"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func local() context.Context { return scoped("123456789012", "ap-southeast-1") }
func scoped(account, region string) context.Context {
	return request.WithMetadata(
		context.Background(),
		request.Metadata{Scope: request.Scope{AccountID: account, Region: region}},
	)
}

func wantError(t *testing.T, e error, code string, status int) {
	t.Helper()
	var a *awserror.Error
	if !errors.As(e, &a) || a.Code != code || a.StatusCode != status || a.Message == "" {
		t.Fatalf("wanted %s/%d: %v", code, status, e)
	}
}

func createBucket(t *testing.T, s *Service, name string) string {
	t.Helper()
	r, e := s.CreateTableBucket(local(), &api.CreateTableBucketRequest{Name: new(name)})
	if e != nil {
		t.Fatal(e)
	}
	return *r.Arn
}

func createNS(t *testing.T, s *Service, b, ns string) {
	t.Helper()
	_, e := s.CreateNamespace(local(), &api.CreateNamespaceRequest{TableBucketARN: new(b), Namespace: []string{ns}})
	if e != nil {
		t.Fatal(e)
	}
}

func createTable(t *testing.T, s *Service, b, ns, name string) *api.GetTableResponse {
	t.Helper()
	_, e := s.CreateTable(
		local(),
		&api.CreateTableRequest{TableBucketARN: new(b), Namespace: new(ns), Name: new(name), Format: new("ICEBERG")},
	)
	if e != nil {
		t.Fatal(e)
	}
	return getTable(t, s, b, ns, name)
}

func getTable(t *testing.T, s *Service, b, ns, name string) *api.GetTableResponse {
	t.Helper()
	r, e := s.GetTable(local(), &api.GetTableRequest{TableBucketARN: new(b), Namespace: new(ns), Name: new(name)})
	if e != nil {
		t.Fatal(e)
	}
	return r
}

func stored(t *testing.T, s *Service) map[string]string {
	t.Helper()
	out := map[string]string{}
	e := s.store.View(local(), func(tx storage.ReadTx) error {
		for _, k := range tx.Keys("") {
			v, _ := tx.Get(k)
			out[k] = string(v)
		}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	return out
}

func tags(t *testing.T, s *Service, arn string) map[string]string {
	t.Helper()
	r, e := s.ListTagsForResource(local(), &api.ListTagsForResourceRequest{ResourceArn: new(arn)})
	if e != nil {
		t.Fatal(e)
	}
	return r.Tags
}

func TestHierarchyDefaultsScopeIdentityDeepOwnershipAndDependencies(t *testing.T) {
	s := New(memory.New(nil, nil))
	ctx := local()
	input := &api.CreateTableBucketRequest{Name: new("record-bucket"), Tags: map[string]string{"owner": "caller"}}
	out, e := s.CreateTableBucket(ctx, input)
	if e != nil {
		t.Fatal(e)
	}
	b := *out.Arn
	*input.Name = "mutated"
	input.Tags["owner"] = "mutated"
	*out.Arn = "mutated"
	bucket, e := s.GetTableBucket(ctx, &api.GetTableBucketRequest{TableBucketARN: new(b)})
	if e != nil {
		t.Fatal(e)
	}
	if b != "arn:aws:s3tables:ap-southeast-1:123456789012:bucket/record-bucket" || *bucket.Name != "record-bucket" ||
		*bucket.Type != customer ||
		*bucket.OwnerAccountId != "123456789012" ||
		bucket.CreatedAt == nil ||
		bucket.TableBucketId == nil ||
		tags(t, s, b)["owner"] != "caller" {
		t.Fatal(bucket)
	}
	for _, other := range []context.Context{
		scoped("999999999999",
			"ap-southeast-1"),
		scoped("123456789012",
			"us-east-1"),
	} {
		_, e = s.GetTableBucket(other, &api.GetTableBucketRequest{TableBucketARN: new(b)})
		wantError(t, e, "BadRequestException", 400)
		_, e = s.CreateTableBucket(other, &api.CreateTableBucketRequest{Name: new("record-bucket")})
		if e != nil {
			t.Fatal(e)
		}
	}
	nsIn := &api.CreateNamespaceRequest{TableBucketARN: new(b), Namespace: []string{"data"}}
	nsOut, e := s.CreateNamespace(ctx, nsIn)
	if e != nil {
		t.Fatal(e)
	}
	nsIn.Namespace[0] = "changed"
	*nsIn.TableBucketARN = "changed"
	nsOut.Namespace[0] = "changed"
	n, e := s.GetNamespace(ctx, &api.GetNamespaceRequest{TableBucketARN: new(b), Namespace: new("data")})
	if e != nil {
		t.Fatal(e)
	}
	if n.Namespace[0] != "data" || *n.CreatedBy != "123456789012" || *n.TableBucketId != *bucket.TableBucketId {
		t.Fatal(n)
	}
	n.Namespace[0] = "changed"
	*n.NamespaceId = "changed"
	in := &api.CreateTableRequest{
		TableBucketARN: new(b),
		Namespace:      new("data"),
		Name:           new("aws_table"),
		Format:         new("ICEBERG"),
		Tags:           map[string]string{"empty": ""},
	}
	made, e := s.CreateTable(ctx, in)
	if e != nil {
		t.Fatal(e)
	}
	*in.TableBucketARN = "changed"
	*in.Namespace = "changed"
	*in.Name = "changed"
	*in.Format = "changed"
	in.Tags["empty"] = "changed"
	r := getTable(t, s, b, "data", "aws_table")
	if *r.TableARN != *made.TableARN || *r.VersionToken != *made.VersionToken || *r.Type != customer ||
		*r.Format != "ICEBERG" ||
		!r.CreatedAt.Equal(*r.ModifiedAt) ||
		*r.OwnerAccountId != "123456789012" ||
		*r.CreatedBy != *r.ModifiedBy ||
		*r.TableBucketId != *bucket.TableBucketId ||
		tags(t, s, *r.TableARN)["empty"] != "" {
		t.Fatal(r)
	}
	arn := *r.TableARN
	r.Namespace[0] = "changed"
	*r.Name = "changed"
	*r.VersionToken = "changed"
	byARN, e := s.GetTable(ctx, &api.GetTableRequest{TableArn: new(arn)})
	if e != nil || byARN.Namespace[0] != "data" || *byARN.Name != "aws_table" {
		t.Fatal(byARN, e)
	}
	before := stored(t, s)
	_, e = s.DeleteNamespace(ctx, &api.DeleteNamespaceRequest{TableBucketARN: new(b), Namespace: new("data")})
	wantError(t, e, "ConflictException", 409)
	_, e = s.DeleteTableBucket(ctx, &api.DeleteTableBucketRequest{TableBucketARN: new(b)})
	wantError(t, e, "ConflictException", 409)
	_, e = s.CreateTable(
		ctx,
		&api.CreateTableRequest{
			TableBucketARN: new(b),
			Namespace:      new("missing"),
			Name:           new("valid"),
			Format:         new("ICEBERG"),
		},
	)
	wantError(t, e, "NotFoundException", 404)
	_, e = s.CreateNamespace(
		ctx,
		&api.CreateNamespaceRequest{TableBucketARN: new(b), Namespace: []string{"data", "child"}},
	)
	wantError(t, e, "BadRequestException", 400)
	if !maps.Equal(before, stored(t, s)) {
		t.Fatal("failed operations wrote records")
	}
	_, e = s.DeleteTable(
		ctx,
		&api.DeleteTableRequest{
			TableBucketARN: new(b),
			Namespace:      new("data"),
			Name:           new("aws_table"),
			VersionToken:   byARN.VersionToken,
		},
	)
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.ListTagsForResource(ctx, &api.ListTagsForResourceRequest{ResourceArn: new(arn)})
	wantError(t, e, "NotFoundException", 404)
	_, e = s.DeleteTable(
		ctx,
		&api.DeleteTableRequest{TableBucketARN: new(b), Namespace: new("data"), Name: new("aws_table")},
	)
	wantError(t, e, "NotFoundException", 404)
	_, e = s.DeleteNamespace(ctx, &api.DeleteNamespaceRequest{TableBucketARN: new(b), Namespace: new("data")})
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.DeleteTableBucket(ctx, &api.DeleteTableBucketRequest{TableBucketARN: new(b)})
	if e != nil {
		t.Fatal(e)
	}
	createBucket(t, s, "record-bucket")
	recreated, e := s.GetTableBucket(ctx, &api.GetTableBucketRequest{TableBucketARN: new(b)})
	if e != nil || *recreated.TableBucketId == *bucket.TableBucketId || !recreated.CreatedAt.After(*bucket.CreatedAt) ||
		len(tags(t, s, b)) != 0 {
		t.Fatal(recreated, e)
	}
}

func TestNamesReferencesFormatsAtomicInvalidCandidatesAndTagRules(t *testing.T) {
	for _, region := range []string{"cn-north-1", "us-gov-east-1", "us-iso-east-1", "eu-isoe-west-1"} {
		_, e := New(
			memory.New(nil, nil),
		).CreateTableBucket(scoped("123456789012", region), &api.CreateTableBucketRequest{Name: new("valid-bucket")})
		wantError(t, e, "BadRequestException", 400)
	}

	s := New(memory.New(nil, nil))
	b := createBucket(t, s, "valid-bucket")
	createNS(t, s, b, "valid")
	createTable(t, s, b, "valid", "source")
	before := stored(t, s)
	for _, name := range []string{
		"",
		"ab",
		strings.Repeat("a",
			64),
		"a.b",
		"a_b",
		"UPPER",
		"-abc",
		"abc-",
		"awsbucket",
		"xn--abc",
		"sthree-abc",
		"amzn-s3-demo-abc",
		"abc-s3alias",
		"abc--ol-s3",
		"abc--x-s3",
		"abc--table-s3",
	} {
		_, e := s.CreateTableBucket(local(), &api.CreateTableBucketRequest{Name: new(name)})
		wantError(t, e, "BadRequestException", 400)
	}
	for _, ns := range []string{
		"",
		"_first",
		"bad-name",
		"bad.name",
		"UPPER",
		"aws",
		"awsdata",
		strings.Repeat("a",
			256),
	} {
		_, e := s.CreateNamespace(local(), &api.CreateNamespaceRequest{TableBucketARN: new(b), Namespace: []string{ns}})
		wantError(t, e, "BadRequestException", 400)
	}
	for _, name := range []string{"", "_first", "bad-name", "bad.name", "UPPER", strings.Repeat("a", 256)} {
		_, e := s.CreateTable(
			local(),
			&api.CreateTableRequest{
				TableBucketARN: new(b),
				Namespace:      new("valid"),
				Name:           new(name),
				Format:         new("ICEBERG"),
			},
		)
		wantError(t, e, "BadRequestException", 400)
	}
	for _, format := range []string{"", "iceberg", "PARQUET"} {
		_, e := s.CreateTable(
			local(),
			&api.CreateTableRequest{
				TableBucketARN: new(b),
				Namespace:      new("valid"),
				Name:           new("valid"),
				Format:         new(format),
			},
		)
		wantError(t, e, "BadRequestException", 400)
	}
	for _, arn := range []string{
		b + "/extra",
		strings.Replace(b,
			"s3tables:",
			"s3:",
			1),
		strings.Replace(b,
			"arn:aws:",
			"arn:aws-cn:",
			1),
		b + "/table/not-uuid",
		b + "/namespace/valid",
	} {
		_, e := s.ListTagsForResource(local(), &api.ListTagsForResourceRequest{ResourceArn: new(arn)})
		wantError(t, e, "BadRequestException", 400)
	}
	for _, input := range []*api.GetTableRequest{
		{},
		{TableArn: new(b)},
		{
			TableArn: new(b),
			Name:     new("source"),
		},
		{
			TableBucketARN: new(b),
			Name:           new("source"),
		},
	} {
		_, e := s.GetTable(local(), input)
		wantError(t, e, "BadRequestException", 400)
	}
	for _, tag := range []map[string]string{
		{"": "x"},
		{"aws:reserved": "x"},
		{"AWS:reserved": "x"},
		{strings.Repeat("é",
			129): "x"},
		{"ok": strings.Repeat("é",
			257)},
		{"bad!": "x"},
		{"ok": "bad!"},
		{"ok": string([]byte{255})},
	} {
		_, e := s.TagResource(local(), &api.TagResourceRequest{ResourceArn: new(b), Tags: tag})
		wantError(t, e, "BadRequestException", 400)
	}
	if !maps.Equal(before, stored(t, s)) {
		t.Fatal("invalid input wrote storage")
	}
	_, e := s.TagResource(
		local(),
		&api.TagResourceRequest{
			ResourceArn: new(b),
			Tags:        map[string]string{strings.Repeat("é", 128): strings.Repeat("é", 256), "empty": ""},
		},
	)
	if e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"a", strings.Repeat("a", 255), "9_end_"} {
		createNS(t, s, b, name)
		createTable(t, s, b, name, name)
	}
}

func TestRenameVersionTokensNoopsCollisionsAndCompensationABA(t *testing.T) {
	s := New(memory.New(nil, nil))
	b := createBucket(t, s, "rename-bucket")
	createNS(t, s, b, "old")
	createNS(t, s, b, "next")
	initial := createTable(t, s, b, "old", "source")
	createTable(t, s, b, "next", "occupied")
	before := stored(t, s)
	for _, in := range []*api.RenameTableRequest{
		{TableBucketARN: new(b), Namespace: new("old"), Name: new("source"), NewName: new("_invalid")},
		{TableBucketARN: new(b), Namespace: new("old"), Name: new("source"), NewNamespaceName: new("missing")},
		{
			TableBucketARN:   new(b),
			Namespace:        new("old"),
			Name:             new("source"),
			NewNamespaceName: new("next"),
			NewName:          new("occupied"),
		},

		{
			TableBucketARN: new(b),
			Namespace:      new("old"),
			Name:           new("source"),
			NewName:        new("renamed"),
			VersionToken:   new("stale"),
		},
	} {
		_, e := s.RenameTable(local(), in)
		if e == nil {
			t.Fatal("invalid rename succeeded")
		}
	}
	_, e := s.DeleteTable(
		local(),
		&api.DeleteTableRequest{
			TableBucketARN: new(b),
			Namespace:      new("old"),
			Name:           new("source"),
			VersionToken:   new("stale"),
		},
	)
	wantError(t, e, "ConflictException", 409)
	if !maps.Equal(before, stored(t, s)) {
		t.Fatal("failed rename/delete wrote")
	}
	_, e = s.RenameTable(
		local(),
		&api.RenameTableRequest{
			TableBucketARN: new(b),
			Namespace:      new("old"),
			Name:           new("source"),
			VersionToken:   initial.VersionToken,
		},
	)
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.TagResource(
		local(),
		&api.TagResourceRequest{ResourceArn: initial.TableARN, Tags: map[string]string{"owner": "live"}},
	)
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(initial, getTable(t, s, b, "old", "source")) {
		t.Fatal("noop/tagging advanced public table token or timestamps")
	}
	snapshot, after, e := s.CloudFormationUpdateTable(
		local(),
		&api.RenameTableRequest{
			TableBucketARN:   new(b),
			Namespace:        new("old"),
			Name:             new("source"),
			NewNamespaceName: new("next"),
			NewName:          new("renamed"),
			VersionToken:     initial.VersionToken,
		},
		map[string]string{},
		map[string]string{"owned": "new"},
	)
	if e != nil {
		t.Fatal(e)
	}
	if *after.Table.TableARN != *initial.TableARN || *after.Table.VersionToken == *initial.VersionToken ||
		!after.Table.ModifiedAt.After(*initial.ModifiedAt) ||
		!after.Table.CreatedAt.Equal(*initial.CreatedAt) ||
		after.Table.Namespace[0] != "next" {
		t.Fatal(after)
	}
	_, e = s.DeleteTable(
		local(),
		&api.DeleteTableRequest{
			TableBucketARN: new(b),
			Namespace:      new("next"),
			Name:           new("renamed"),
			VersionToken:   initial.VersionToken,
		},
	)
	wantError(t, e, "ConflictException", 409)
	restored, e := s.CloudFormationRestore(local(), snapshot, after.Revision)
	if e != nil {
		t.Fatal(e)
	}
	if *restored.Table.VersionToken == *initial.VersionToken ||
		*restored.Table.VersionToken == *after.Table.VersionToken ||
		!restored.Table.ModifiedAt.Equal(*initial.ModifiedAt) ||
		!maps.Equal(restored.Tags, map[string]string{"owner": "live"}) {
		t.Fatal(restored)
	}
	_, e = s.CloudFormationRestore(local(), snapshot, after.Revision)
	wantError(t, e, "ConflictException", 409)
	_, e = s.TagResource(
		local(),
		&api.TagResourceRequest{ResourceArn: initial.TableARN, Tags: map[string]string{"external": "guard"}},
	)
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.CloudFormationRestore(local(), snapshot, restored.Revision)
	wantError(t, e, "ConflictException", 409)
}

func fifty(prefix string) map[string]string {
	out := map[string]string{}
	for i := range 50 {
		out[fmt.Sprintf("%s-%02d", prefix, i)] = "value"
	}
	return out
}

func TestAtomicFinalTagsOverflowAndDeepSnapshotOwnership(t *testing.T) {
	s := New(memory.New(nil, nil))
	b := createBucket(t, s, "tags-bucket")
	createNS(t, s, b, "data")
	r := createTable(t, s, b, "data", "source")
	for _, arn := range []string{b, *r.TableARN} {
		_, e := s.TagResource(local(), &api.TagResourceRequest{ResourceArn: new(arn), Tags: fifty("old")})
		if e != nil {
			t.Fatal(e)
		}
		before := stored(t, s)
		_, e = s.TagResource(
			local(),
			&api.TagResourceRequest{ResourceArn: new(arn), Tags: map[string]string{"overflow": "x"}},
		)
		wantError(t, e, "BadRequestException", 400)
		if !maps.Equal(before, stored(t, s)) {
			t.Fatal("tag overflow wrote")
		}
	}
	_, after, e := s.CloudFormationUpdateBucket(local(), b, fifty("old"), fifty("new"))
	if e != nil || !maps.Equal(after.Tags, fifty("new")) {
		t.Fatal(after, e)
	}
	before, after, e := s.CloudFormationUpdateTable(
		local(),
		&api.RenameTableRequest{
			TableBucketARN: new(b),
			Namespace:      new("data"),
			Name:           new("source"),
			NewName:        new("renamed"),
		},
		fifty("old"),
		fifty("new"),
	)
	if e != nil {
		t.Fatal(e)
	}
	before.Tags["old-00"] = "mutated"
	after.Tags["new-00"] = "mutated"
	after.Table.Namespace[0] = "mutated"
	if !maps.Equal(tags(t, s, *r.TableARN), fifty("new")) {
		t.Fatal("snapshot aliases live")
	}
	_, e = s.UntagResource(local(), &api.UntagResourceRequest{ResourceArn: r.TableARN, TagKeys: []string{"new-00"}})
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.TagResource(
		local(),
		&api.TagResourceRequest{ResourceArn: r.TableARN, Tags: map[string]string{"external": "keep"}},
	)
	if e != nil {
		t.Fatal(e)
	}
	state := stored(t, s)
	_, _, e = s.CloudFormationUpdateTable(
		local(),
		&api.RenameTableRequest{
			TableBucketARN: new(b),
			Namespace:      new("data"),
			Name:           new("renamed"),
			NewName:        new("must_not_write"),
		},
		fifty("new"),
		fifty("overflow"),
	)
	wantError(t, e, "BadRequestException", 400)
	if !maps.Equal(state, stored(t, s)) {
		t.Fatal("final overflow changed rename")
	}
}

func TestConcurrentUniquenessParentsTagsAndVersionRaces(t *testing.T) {
	s := New(memory.New(nil, nil))
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 24 {
		wg.Go(func() {
			_, e := s.CreateTableBucket(local(), &api.CreateTableBucketRequest{Name: new("race-bucket")})
			if e == nil {
				wins.Add(1)
			} else {
				wantError(t, e, "ConflictException", 409)
			}
		})
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatal(wins.Load())
	}
	b := "arn:aws:s3tables:ap-southeast-1:123456789012:bucket/race-bucket"
	createNS(t, s, b, "data")
	wins.Store(0)
	for range 24 {
		wg.Go(func() {
			_, e := s.CreateTable(
				local(),
				&api.CreateTableRequest{
					TableBucketARN: new(b),
					Namespace:      new("data"),
					Name:           new("source"),
					Format:         new("ICEBERG"),
				},
			)
			if e == nil {
				wins.Add(1)
			} else {
				wantError(t, e, "ConflictException", 409)
			}
		})
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatal(wins.Load())
	}
	r := getTable(t, s, b, "data", "source")
	for i := range 24 {
		wg.Go(func() {
			_, e := s.TagResource(
				local(),
				&api.TagResourceRequest{
					ResourceArn: r.TableARN,
					Tags:        map[string]string{fmt.Sprintf("key-%02d", i): "value"},
				},
			)
			if e != nil {
				t.Error(e)
			}
		})
	}
	wg.Wait()
	if len(tags(t, s, *r.TableARN)) != 24 {
		t.Fatal("disjoint writes lost")
	}
	wins.Store(0)
	for i := range 24 {
		wg.Go(func() {
			_, e := s.RenameTable(
				local(),
				&api.RenameTableRequest{
					TableBucketARN: new(b),
					Namespace:      new("data"),
					Name:           new("source"),
					NewName:        new(fmt.Sprintf("winner_%02d", i)),
					VersionToken:   r.VersionToken,
				},
			)
			if e == nil {
				wins.Add(1)
			}
		})
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatal("rename winners", wins.Load())
	}
	// Rename and delete with the same version cannot both commit.
	r = createTable(t, s, b, "data", "race")
	wins.Store(0)
	wg.Go(func() {
		_, e := s.RenameTable(
			local(),
			&api.RenameTableRequest{
				TableBucketARN: new(b),
				Namespace:      new("data"),
				Name:           new("race"),
				NewName:        new("race_renamed"),
				VersionToken:   r.VersionToken,
			},
		)
		if e == nil {
			wins.Add(1)
		}
	})
	wg.Go(func() {
		_, e := s.DeleteTable(
			local(),
			&api.DeleteTableRequest{
				TableBucketARN: new(b),
				Namespace:      new("data"),
				Name:           new("race"),
				VersionToken:   r.VersionToken,
			},
		)
		if e == nil {
			wins.Add(1)
		}
	})
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatal("rename/delete winners", wins.Load())
	}
	for i := range 30 {
		parent := createBucket(t, s, fmt.Sprintf("parent-%02d", i))
		wg.Go(func() {
			_, _ = s.CreateNamespace(
				local(),
				&api.CreateNamespaceRequest{TableBucketARN: new(parent), Namespace: []string{"child"}},
			)
		})
		wg.Go(func() {
			_, _ = s.DeleteTableBucket(local(), &api.DeleteTableBucketRequest{TableBucketARN: new(parent)})
		})
		wg.Wait()
		n, e := s.ListNamespaces(local(), &api.ListNamespacesRequest{TableBucketARN: new(parent)})
		if e == nil && len(n.Namespaces) != 1 {
			t.Fatal("lost child")
		}
		if e != nil {
			wantError(t, e, "NotFoundException", 404)
		}
	}
	// Collision on one destination is checked within the same transaction.
	createTable(t, s, b, "data", "left")
	createTable(t, s, b, "data", "right")
	wins.Store(0)
	for _, name := range []string{"left", "right"} {
		wg.Go(func() {
			_, e := s.RenameTable(
				local(),
				&api.RenameTableRequest{
					TableBucketARN: new(b),
					Namespace:      new("data"),
					Name:           new(name),
					NewName:        new("destination"),
				},
			)
			if e == nil {
				wins.Add(1)
			} else {
				wantError(t, e, "ConflictException", 409)
			}
		})
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatal("collision winners", wins.Load())
	}
}

func TestRecreatedHierarchyFreshMetadataAndNamespaceTableParentRace(t *testing.T) {
	s := New(memory.New(nil, nil))
	b := createBucket(t, s, "recreate-bucket")
	createNS(t, s, b, "data")
	old := createTable(t, s, b, "data", "source")
	ns, e := s.GetNamespace(local(), &api.GetNamespaceRequest{TableBucketARN: new(b), Namespace: new("data")})
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.DeleteTable(
		local(),
		&api.DeleteTableRequest{TableBucketARN: new(b), Namespace: new("data"), Name: new("source")},
	)
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.DeleteNamespace(local(), &api.DeleteNamespaceRequest{TableBucketARN: new(b), Namespace: new("data")})
	if e != nil {
		t.Fatal(e)
	}
	createNS(t, s, b, "data")
	next := createTable(t, s, b, "data", "source")
	nextNS, e := s.GetNamespace(local(), &api.GetNamespaceRequest{TableBucketARN: new(b), Namespace: new("data")})
	if e != nil {
		t.Fatal(e)
	}
	if *next.TableARN == *old.TableARN || *next.VersionToken == *old.VersionToken ||
		!next.CreatedAt.After(*old.CreatedAt) ||
		*nextNS.NamespaceId == *ns.NamespaceId ||
		!nextNS.CreatedAt.After(*ns.CreatedAt) {
		t.Fatal("recreation reused public identity", old, next, ns, nextNS)
	}
	_, e = s.DeleteTable(
		local(),
		&api.DeleteTableRequest{
			TableBucketARN: new(b),
			Namespace:      new("data"),
			Name:           new("source"),
			VersionToken:   old.VersionToken,
		},
	)
	wantError(t, e, "ConflictException", 409)
	var wg sync.WaitGroup
	for i := range 30 {
		name := fmt.Sprintf("race_%02d", i)
		createNS(t, s, b, name)
		wg.Go(func() {
			_, _ = s.CreateTable(
				local(),
				&api.CreateTableRequest{
					TableBucketARN: new(b),
					Namespace:      new(name),
					Name:           new("child"),
					Format:         new("ICEBERG"),
				},
			)
		})
		wg.Go(func() {
			_, _ = s.DeleteNamespace(local(), &api.DeleteNamespaceRequest{TableBucketARN: new(b), Namespace: new(name)})
		})
		wg.Wait()
		_, e = s.GetNamespace(local(), &api.GetNamespaceRequest{TableBucketARN: new(b), Namespace: new(name)})
		all, err := s.ListTables(local(), &api.ListTablesRequest{TableBucketARN: new(b)})
		if err != nil {
			t.Fatal(err)
		}
		children := 0
		for _, r := range all.Tables {
			if r.Namespace[0] == name {
				children++
			}
		}
		if e == nil && children != 1 {
			t.Fatal("live parent lost child")
		}
		if e != nil {
			wantError(t, e, "NotFoundException", 404)
			if children != 0 {
				t.Fatal("orphan table")
			}
		}
	}
}
