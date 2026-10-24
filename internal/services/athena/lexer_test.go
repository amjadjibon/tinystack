package athena

import (
	"slices"
	"strings"
	"testing"
)

func TestRewrite(t *testing.T) {
	for query, want := range map[string]string{
		`SELECT * FROM awsdatacatalog.lake.t WHERE d > DATE '2024-01-01'`: `SELECT * FROM lake.t WHERE d > '2024-01-01'`,
		"SELECT CAST(x AS DATE), `a b` FROM t":                            `SELECT CAST(x AS TEXT), "a b" FROM t`,
		"SELECT json_extract_scalar(j, '$.a') FROM t WHERE id = ? -- ?": "SELECT json_extract(j, '$.a') FROM t " +
			"WHERE id = 7 -- ?",
		"SELECT 'it''s ?' AS date FROM t WHERE x = ?": "SELECT 'it''s ?' AS date FROM t WHERE x = 7",
	} {
		tokens, err := lex(query)
		if err != nil {
			t.Fatal(err)
		}

		var params []string
		if slices.ContainsFunc(tokens, func(t token) bool { return t.kind == tokenParam }) {
			params = []string{"7"}
		}

		if got, err := rewrite(tokens, params); err != nil || got != want {
			t.Errorf("rewrite(%s) = %q, %v; want %q", query, got, err, want)
		}
	}

	tokens, _ := lex("SELECT 1 FROM t WHERE a = ? AND b = ?")
	if _, err := rewrite(tokens, []string{"1"}); err == nil {
		t.Fatal("too few parameters passed")
	}

	if _, err := lex("SELECT 'open"); err == nil {
		t.Fatal("an open string lexed")
	}

	tokens, _ = lex(`SELECT s.id FROM "Sales" s JOIN other.events e ON 1=1`)
	refs := references(tokens, "lake")
	for _, want := range []tableRef{{"lake", "sales"}, {"other", "events"}, {"lake", "s"}} {
		if !slices.Contains(refs, want) {
			t.Errorf("references lack %v: %v", want, refs)
		}
	}

	if got := unescape(`\t|\001|\\`); got != "\t|\x01|\\" {
		t.Errorf("unescape = %q", got)
	}
}

func TestWriteQueryTokenSpacing(t *testing.T) {
	for _, query := range []string{"SELECT 1>=0", "SELECT 'a'||'b'", "SELECT 1 -- comment\n + 2"} {
		original, err := lex(query)
		if err != nil {
			t.Fatal(err)
		}

		got, err := rewrite(queryTokens(solid(original)), nil)
		if err != nil {
			t.Fatal(err)
		}

		if strings.Contains(got, "> =") || strings.Contains(got, "| |") ||
			!strings.HasPrefix(strings.TrimSpace(got), "SELECT ") {
			t.Fatalf("%q", got)
		}
	}
}
