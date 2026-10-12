package s3tables

import (
	"fmt"
	"testing"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3tables"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestBucketNamespaceTablePagesFiltersScopeAndDeletedCursors(t *testing.T) {
	s := New(memory.New(nil, nil))
	ctx := local()
	for _, name := range []string{"bucket-a", "bucket-b", "bucket-c"} {
		createBucket(t, s, name)
	}
	first, e := s.ListTableBuckets(ctx, &api.ListTableBucketsRequest{MaxBuckets: new(int64(1)), Prefix: new("bucket-")})
	if e != nil || len(first.TableBuckets) != 1 || *first.TableBuckets[0].Name != "bucket-a" ||
		first.ContinuationToken == nil {
		t.Fatal(first, e)
	}
	for _, in := range []*api.ListTableBucketsRequest{
		{ContinuationToken: first.ContinuationToken},
		{ContinuationToken: new("bad")},
		{
			Prefix:            new("bucket-"),
			ContinuationToken: new(*first.ContinuationToken + "="),
		},
		{
			Prefix:            new("bucket-"),
			Type:              new("customer"),
			ContinuationToken: first.ContinuationToken,
		},
		{MaxBuckets: new(int64(0))},
		{MaxBuckets: new(int64(1001))},
		{Type: new("aws")},
	} {
		_, e = s.ListTableBuckets(ctx, in)
		wantError(t, e, "BadRequestException", 400)
	}
	_, e = s.ListTableBuckets(
		scoped("999999999999", "ap-southeast-1"),
		&api.ListTableBucketsRequest{Prefix: new("bucket-"), ContinuationToken: first.ContinuationToken},
	)
	wantError(t, e, "BadRequestException", 400)
	_, e = s.DeleteTableBucket(ctx, &api.DeleteTableBucketRequest{TableBucketARN: first.TableBuckets[0].Arn})
	if e != nil {
		t.Fatal(e)
	}
	next, e := s.ListTableBuckets(
		ctx,
		&api.ListTableBucketsRequest{
			Prefix:            new("bucket-"),
			ContinuationToken: first.ContinuationToken,
			MaxBuckets:        new(int64(1000)),
		},
	)
	if e != nil || len(next.TableBuckets) != 2 || next.ContinuationToken != nil {
		t.Fatal(next, e)
	}
	createBucket(t, s, "bucket-a")
	_, e = s.ListTableBuckets(
		ctx,
		&api.ListTableBucketsRequest{Prefix: new("bucket-"), ContinuationToken: first.ContinuationToken},
	)
	wantError(t, e, "BadRequestException", 400)
	b := createBucket(t, s, "hierarchy")
	for _, limit := range []int64{0, 1001} {
		_, err := s.ListNamespaces(ctx, &api.ListNamespacesRequest{TableBucketARN: new(b), MaxNamespaces: new(limit)})
		wantError(t, err, "BadRequestException", 400)
		_, err = s.ListTables(ctx, &api.ListTablesRequest{TableBucketARN: new(b), MaxTables: new(limit)})
		wantError(t, err, "BadRequestException", 400)
	}

	for _, ns := range []string{"a", "b", "c"} {
		createNS(t, s, b, ns)
	}
	nsPage, e := s.ListNamespaces(ctx, &api.ListNamespacesRequest{TableBucketARN: new(b), MaxNamespaces: new(int64(1))})
	if e != nil || len(nsPage.Namespaces) != 1 || nsPage.Namespaces[0].Namespace[0] != "a" ||
		nsPage.ContinuationToken == nil {
		t.Fatal(nsPage, e)
	}
	_, e = s.ListNamespaces(
		ctx,
		&api.ListNamespacesRequest{
			TableBucketARN:    new(b),
			Prefix:            new("b"),
			ContinuationToken: nsPage.ContinuationToken,
		},
	)
	wantError(t, e, "BadRequestException", 400)
	_, e = s.ListTableBuckets(ctx, &api.ListTableBucketsRequest{ContinuationToken: nsPage.ContinuationToken})
	wantError(t, e, "BadRequestException", 400)
	_, e = s.DeleteNamespace(ctx, &api.DeleteNamespaceRequest{TableBucketARN: new(b), Namespace: new("a")})
	if e != nil {
		t.Fatal(e)
	}
	nsEnd, e := s.ListNamespaces(
		ctx,
		&api.ListNamespacesRequest{TableBucketARN: new(b), ContinuationToken: nsPage.ContinuationToken},
	)
	if e != nil || len(nsEnd.Namespaces) != 2 {
		t.Fatal(nsEnd, e)
	}
	createNS(t, s, b, "a")
	_, e = s.ListNamespaces(
		ctx,
		&api.ListNamespacesRequest{TableBucketARN: new(b), ContinuationToken: nsPage.ContinuationToken},
	)
	wantError(t, e, "BadRequestException", 400)
	for _, ns := range []string{"a", "b"} {
		for _, name := range []string{"p_a", "p_b", "other"} {
			createTable(t, s, b, ns, name)
		}
	}
	tables, e := s.ListTables(
		ctx,
		&api.ListTablesRequest{TableBucketARN: new(b), Prefix: new("p_"), MaxTables: new(int64(1))},
	)
	if e != nil || len(tables.Tables) != 1 || tables.Tables[0].Namespace[0] != "a" || *tables.Tables[0].Name != "p_a" ||
		tables.ContinuationToken == nil {
		t.Fatal(tables, e)
	}
	_, e = s.ListTables(
		ctx,
		&api.ListTablesRequest{
			TableBucketARN:    new(b),
			Prefix:            new("p_"),
			Namespace:         new("a"),
			ContinuationToken: tables.ContinuationToken,
		},
	)
	wantError(t, e, "BadRequestException", 400)
	_, e = s.ListTables(
		ctx,
		&api.ListTablesRequest{
			TableBucketARN:    new(b),
			Prefix:            new("other"),
			ContinuationToken: tables.ContinuationToken,
		},
	)
	wantError(t, e, "BadRequestException", 400)
	_, e = s.DeleteTable(ctx, &api.DeleteTableRequest{TableBucketARN: new(b), Namespace: new("a"), Name: new("p_a")})
	if e != nil {
		t.Fatal(e)
	}
	tail, e := s.ListTables(
		ctx,
		&api.ListTablesRequest{TableBucketARN: new(b), Prefix: new("p_"), ContinuationToken: tables.ContinuationToken},
	)
	if e != nil || len(tail.Tables) != 3 || tail.Tables[0].Namespace[0] != "a" || tail.Tables[1].Namespace[0] != "b" {
		t.Fatal(tail, e)
	}
	createTable(t, s, b, "a", "p_a")
	_, e = s.ListTables(
		ctx,
		&api.ListTablesRequest{TableBucketARN: new(b), Prefix: new("p_"), ContinuationToken: tables.ContinuationToken},
	)
	wantError(t, e, "BadRequestException", 400)
	only, e := s.ListTables(ctx, &api.ListTablesRequest{TableBucketARN: new(b), Namespace: new("b"), Prefix: new("p_")})
	if e != nil || len(only.Tables) != 2 {
		t.Fatal(only, e)
	}
	only.Tables[0].Namespace[0] = "mutated"
	*only.Tables[0].Name = "mutated"
	getTable(t, s, b, "b", "p_a")
	// A filter namespace's incarnation participates even if the cursor record was deleted.
	filtered, e := s.ListTables(
		ctx,
		&api.ListTablesRequest{TableBucketARN: new(b), Namespace: new("b"), MaxTables: new(int64(1))},
	)
	if e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"other", "p_a", "p_b"} {
		_, e = s.DeleteTable(ctx, &api.DeleteTableRequest{TableBucketARN: new(b), Namespace: new("b"), Name: new(name)})
		if e != nil {
			t.Fatal(e)
		}
	}
	_, e = s.DeleteNamespace(ctx, &api.DeleteNamespaceRequest{TableBucketARN: new(b), Namespace: new("b")})
	if e != nil {
		t.Fatal(e)
	}
	createNS(t, s, b, "b")
	_, e = s.ListTables(
		ctx,
		&api.ListTablesRequest{
			TableBucketARN:    new(b),
			Namespace:         new("b"),
			ContinuationToken: filtered.ContinuationToken,
		},
	)
	wantError(t, e, "BadRequestException", 400)
	// Recreated parent bucket cannot reuse an earlier namespace page.
	for _, ns := range []string{"a", "b", "c"} {
		if ns == "a" {
			for _, name := range []string{"other", "p_a", "p_b"} {
				_, e = s.DeleteTable(
					ctx,
					&api.DeleteTableRequest{TableBucketARN: new(b), Namespace: new(ns), Name: new(name)},
				)
				if e != nil {
					t.Fatal(e)
				}
			}
		}
		_, e = s.DeleteNamespace(ctx, &api.DeleteNamespaceRequest{TableBucketARN: new(b), Namespace: new(ns)})
		if e != nil {
			t.Fatal(e)
		}
	}
	_, e = s.DeleteTableBucket(ctx, &api.DeleteTableBucketRequest{TableBucketARN: new(b)})
	if e != nil {
		t.Fatal(e)
	}
	createBucket(t, s, "hierarchy")
	_, e = s.ListNamespaces(
		ctx,
		&api.ListNamespacesRequest{TableBucketARN: new(b), ContinuationToken: nsPage.ContinuationToken},
	)
	wantError(t, e, "BadRequestException", 400)
}

func TestMeaningfulDefaultAndBoundaryPages(t *testing.T) {
	s := New(memory.New(nil, nil))
	b := createBucket(t, s, "default-page")
	createNS(t, s, b, "data")
	for i := range 1001 {
		createTable(t, s, b, "data", fmt.Sprintf("record_%04d", i))
	}
	first, e := s.ListTables(local(), &api.ListTablesRequest{TableBucketARN: new(b)})
	if e != nil || len(first.Tables) != 1000 || first.ContinuationToken == nil {
		t.Fatal(len(first.Tables), e)
	}
	next, e := s.ListTables(
		local(),
		&api.ListTablesRequest{TableBucketARN: new(b), ContinuationToken: first.ContinuationToken},
	)
	if e != nil || len(next.Tables) != 1 || *next.Tables[0].Name != "record_1000" || next.ContinuationToken != nil {
		t.Fatal(next, e)
	}
	_, e = s.ListTables(local(), &api.ListTablesRequest{TableBucketARN: new(b), Prefix: new(string([]byte{255}))})
	wantError(t, e, "BadRequestException", 400)
}
