package redshiftdata

import (
	"testing"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/redshiftdata"
)

func TestSubstitution(t *testing.T) {
	sql := `select :id, :id2, ':id', "a:id", $$:id$$, $body$:id$body$, :id::text -- :missing
/* :missing */`
	got, err := substitute(
		sql,
		[]api.SqlParameter{{Name: new("id"), Value: new("O'Reilly")}, {Name: new("id2"), Value: new("2")}},
	)
	want := `select 'O''Reilly', '2', ':id', "a:id", $$:id$$, $body$:id$body$, 'O''Reilly'::text -- :missing
/* :missing */`
	if err != nil || got != want {
		t.Fatalf("%q %v", got, err)
	}

	if _, err := substitute("select :missing", nil); err == nil {
		t.Fatal("missing bind accepted")
	}

	if _, err := substitute(
		":a",
		[]api.SqlParameter{{Name: new("a"), Value: new("1")}, {Name: new("a"), Value: new("2")}},
	); err == nil {
		t.Fatal("duplicate accepted")
	}
}

func TestFixtureMatching(t *testing.T) {
	f := Fixture{
		Target:         &Target{WorkgroupName: "wg"},
		Database:       "dev",
		Regex:          `^select 'x'$`,
		ColumnMetadata: []api.ColumnMetadata{{Name: new("v")}},
		Records:        [][]api.Field{{{StringValue: new("answer")}}},
	}

	if err := f.validate(); err != nil {
		t.Fatal(err)
	}

	if !f.matches("select 'x'", "", "wg", "dev") || f.matches("select 'x'", "", "other", "dev") ||
		f.matches("select 'x'", "", "wg", "other") {
		t.Fatal("scope matching")
	}

	f.Regex = "["
	if err := f.validate(); err == nil {
		t.Fatal("invalid regex accepted")
	}

	f.Regex = ""
	f.Contains = "select"
	f.Records[0][0].LongValue = new(int64(1))
	if err := f.validate(); err == nil {
		t.Fatal("invalid union accepted")
	}
}

func TestFailedBatchHasNoResult(t *testing.T) {
	r := &record{
		Batch: true,
		Answers: []Fixture{
			{
				ColumnMetadata: []api.ColumnMetadata{{Name: new("v")}},
				Records:        [][]api.Field{{{LongValue: new(int64(1))}}},
			},
			{Error: new("fixture error")},
		},
		DescribeStatementResponse: api.DescribeStatementResponse{
			SubStatements: []api.SubStatementData{{Id: new("one")}, {Id: new("two")}},
		},
	}

	finish(r)
	if *r.Status != statusFailed || *r.HasResultSet || *r.SubStatements[0].Status != statusAborted ||
		*r.SubStatements[1].Status != statusFailed {
		t.Fatal(r)
	}
}
