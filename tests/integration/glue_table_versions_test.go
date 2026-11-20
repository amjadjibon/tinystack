package integration_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/glue"
	gt "github.com/aws/aws-sdk-go-v2/service/glue/types"
)

func TestGlueTableVersions(t *testing.T) {
	ctx := context.Background()
	c := glueClient(server(t))
	db, name := new("history"), new("events")
	createDB := func() {
		t.Helper()
		_, err := c.CreateDatabase(ctx, &glue.CreateDatabaseInput{DatabaseInput: &gt.DatabaseInput{Name: db}})
		if err != nil {
			t.Fatal(err)
		}
	}

	createTable := func() {
		t.Helper()
		_, err := c.CreateTable(ctx, &glue.CreateTableInput{
			DatabaseName: db, TableInput: &gt.TableInput{Name: name, Description: new("initial")},
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	update := func(description string, skip bool) {
		t.Helper()
		_, err := c.UpdateTable(ctx, &glue.UpdateTableInput{
			DatabaseName: db,
			SkipArchive:  new(skip),
			TableInput:   &gt.TableInput{Name: name, Description: new(description)},
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	check := func(id *string, wantID, description string) {
		t.Helper()
		result, err := c.GetTableVersion(ctx, &glue.GetTableVersionInput{
			DatabaseName: db, TableName: name, VersionId: id,
		})
		if err != nil {
			t.Fatal(err)
		}

		v := result.TableVersion
		if v == nil || v.Table == nil || *v.VersionId != wantID || *v.Table.VersionId != wantID ||
			*v.Table.Description != description {
			t.Fatalf("version %s: %+v", wantID, v)
		}
	}

	list := func() *glue.GetTableVersionsInput {
		return &glue.GetTableVersionsInput{DatabaseName: db, TableName: name, MaxResults: new(int32(1))}
	}

	createDB()
	createTable()
	check(nil, "0", "initial")
	update("discarded on the next update", false)
	update("third", true)
	update("current", false)
	check(nil, "3", "current")
	check(new("0"), "0", "initial")
	check(new("2"), "2", "third")
	_, err := c.GetTableVersion(ctx, &glue.GetTableVersionInput{DatabaseName: db, TableName: name, VersionId: new("1")})
	code(t, err, "EntityNotFoundException")

	paginator := glue.NewGetTableVersionsPaginator(c, list())
	for _, want := range []string{"3", "2", "0"} {
		page, pageErr := paginator.NextPage(ctx)
		if pageErr != nil || len(page.TableVersions) != 1 || *page.TableVersions[0].VersionId != want {
			t.Fatalf("page for version %s: %+v, %v", want, page, pageErr)
		}
	}

	if paginator.HasMorePages() {
		t.Fatal("unexpected extra page")
	}

	batch, err := c.BatchDeleteTableVersion(ctx, &glue.BatchDeleteTableVersionInput{
		DatabaseName: db, TableName: name, VersionIds: []string{"2", "1", "3", "invalid"},
	})
	if err != nil || len(batch.Errors) != 3 || *batch.Errors[0].VersionId != "1" ||
		*batch.Errors[0].ErrorDetail.ErrorCode != "EntityNotFoundException" ||
		*batch.Errors[1].ErrorDetail.ErrorCode != "InvalidInputException" ||
		*batch.Errors[2].ErrorDetail.ErrorCode != "InvalidInputException" {
		t.Fatalf("batch delete: %+v, %v", batch, err)
	}

	_, err = c.DeleteTableVersion(ctx, &glue.DeleteTableVersionInput{
		DatabaseName: db, TableName: name, VersionId: new("0"),
	})
	if err != nil {
		t.Fatal(err)
	}

	page, err := c.GetTableVersions(ctx, list())
	if err != nil || len(page.TableVersions) != 1 || *page.TableVersions[0].VersionId != "3" || page.NextToken != nil {
		t.Fatalf("remaining history: %+v, %v", page, err)
	}

	update("archived when deleting table", false)
	_, err = c.DeleteTable(ctx, &glue.DeleteTableInput{DatabaseName: db, Name: name})
	if err != nil {
		t.Fatal(err)
	}

	createTable()
	page, err = c.GetTableVersions(ctx, list())
	if err != nil || len(page.TableVersions) != 1 || *page.TableVersions[0].VersionId != "0" || page.NextToken != nil {
		t.Fatalf("history after recreating table: %+v, %v", page, err)
	}

	update("archived when deleting database", false)
	_, err = c.DeleteDatabase(ctx, &glue.DeleteDatabaseInput{Name: db})
	if err != nil {
		t.Fatal(err)
	}

	createDB()
	createTable()
	page, err = c.GetTableVersions(ctx, list())
	if err != nil || len(page.TableVersions) != 1 || *page.TableVersions[0].VersionId != "0" || page.NextToken != nil {
		t.Fatalf("history after recreating database: %+v, %v", page, err)
	}
}
