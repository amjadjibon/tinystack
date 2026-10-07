package iot

import (
	"cmp"
	"encoding/json"
	"testing"
	"time"
)

func TestSQL(t *testing.T) {
	payload := `{"temp": 25, "ratio": 0.5, "name": "Probe", "tags": ["a", "b"], "nested": {"x": {"y": 7}},
		"flag": true, "none": null, "readings": [{"v": 1}, {"v": 2}]}`
	for _, tc := range []struct {
		sql, want, payload string
		matches            bool
	}{
		{"SELECT * FROM 'a/+'", payload, "", true},
		{"SELECT temp FROM 'a/+' WHERE temp > 20", `{"temp":25}`, "", true},
		{"SELECT temp FROM 'a/+' WHERE temp > 30", ``, "", false},
		{"SELECT nested.x.y, tags[1] AS second, readings[0].v AS v FROM 'a/b'", `{"second":"b","v":1,"y":7}`, "", true},
		{
			"SELECT temp / 2 AS half, temp / 2.0 AS exact, temp % 7 AS rest FROM 'a/b'",
			`{"exact":12.5,"half":12,"rest":4}`, "", true,
		},
		{"SELECT missing, missing.deeper FROM 'a/b'", `{}`, "", true},
		{"SELECT * FROM 'a/b' WHERE missing = 1 OR NOT (missing <> 1)", ``, "", false},
		{"SELECT * FROM 'a/b' WHERE missing = 1 OR temp = 25", payload, "", true},
		{"SELECT topic() AS t, topic(2) AS second, topic(9) AS none FROM 'a/b'", `{"second":"b","t":"a/b"}`, "", true},
		{
			"SELECT upper(name) AS u, lower(name) AS l, substring(name, 1, 3) AS s, length(tags) AS n FROM 'a/b'",
			`{"l":"probe","n":2,"s":"ro","u":"PROBE"}`, "", true,
		},
		{
			"SELECT concat(name, '-', temp) AS c, concat(tags, ['c']) AS all FROM 'a/b'",
			`{"all":["a","b","c"],"c":"Probe-25"}`, "", true,
		},
		{
			"SELECT cast(temp AS String) AS s, cast('3.7' AS Int) AS i, cast(ratio AS Decimal) AS d FROM 'a/b'",
			`{"d":0.5,"i":3,"s":"25"}`, "", true,
		},
		{"SELECT CASE temp WHEN 25 THEN 'hot' ELSE 'cold' END AS feel FROM 'a/b'", `{"feel":"hot"}`, "", true},
		{
			"SELECT CASE WHEN temp < 0 THEN 'ice' WHEN flag THEN 'flagged' END AS state FROM 'a/b'",
			`{"state":"flagged"}`, "", true,
		},
		{
			"SELECT isNull(none) AS n, isUndefined(missing) AS u, startswith(name, 'Pr') AS p FROM 'a/b'",
			`{"n":true,"p":true,"u":true}`, "", true,
		},
		{"SELECT * FROM 'a/b' WHERE 'b' IN tags AND name = 'Probe'", payload, "", true},
		{"SELECT {'k': temp, 'list': [1, missing, 2]} AS o FROM 'a/b'", `{"o":{"k":25,"list":[1,2]}}`, "", true},
		{
			"SELECT encode(*, 'base64') AS raw FROM 'a/b' WHERE get(tags, 0) = 'a'",
			`{"raw":"eyJ0YWdzIjpbImEiXX0="}`, `{"tags":["a"]}`, true,
		},
		{
			"SELECT abs(-3) AS a, round(2.5) AS r, floor(ratio) AS f, ceil(ratio) AS c, mod(7, 3) AS m FROM 'a/b'",
			`{"a":3,"c":1,"f":0,"m":1,"r":3}`, "", true,
		},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			q, err := parseQuery(tc.sql)
			if err != nil {
				t.Fatal(err)
			}

			raw := []byte(cmp.Or(tc.payload, payload))

			e := &env{doc: decodePayload(raw), raw: raw, topic: "a/b", now: time.Unix(0, 0)}
			if got := q.matches(e); got != tc.matches {
				t.Fatalf("matches = %v, want %v", got, tc.matches)
			}

			if !tc.matches {
				return
			}

			got, want := normalize(t, encodeResult(q.selectResult(e))), normalize(t, []byte(tc.want))
			if got != want {
				t.Fatalf("result = %s, want %s", got, want)
			}
		})
	}
}

// normalize re-encodes JSON so documents compare regardless of spacing.
func normalize(t *testing.T, b []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("%s: %v", b, err)
	}

	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}

	return string(out)
}

func TestSQLParseErrors(t *testing.T) {
	for _, sql := range []string{
		"",
		"SELECT",
		"SELECT * FROM",
		"SELECT * FROM a",
		"SELECT * FROM 'a/#/b'",
		"SELECT nosuch() FROM 'a'",
		"SELECT topic(1, 2) FROM 'a'",
		"SELECT cast(x AS Thing) FROM 'a'",
		"SELECT * FROM 'a' WHERE",
		"SELECT * FROM 'a' trailing",
		"SELECT CASE END FROM 'a'",
		"SELECT 'unterminated FROM 'a'",
		"SELECT x FROM 'a' WHERE y = #",
	} {
		if _, err := parseQuery(sql); err == nil {
			t.Errorf("%q parsed", sql)
		}
	}
}

func TestTopicMatches(t *testing.T) {
	for _, tc := range []struct {
		filter, topic string
		want          bool
	}{
		{"a/+/c", "a/b/c", true},
		{"a/#", "a", true},
		{"a/#", "a/b/c", true},
		{"#", "$aws/things/x/shadow/update", false},
		{"+/things/#", "$aws/things/x", false},
		{"$aws/things/+/shadow/#", "$aws/things/x/shadow/update/accepted", true},
		{"a/+", "a/b/c", false},
		{"a/b", "a/b", true},
	} {
		if got := topicMatches(tc.filter, tc.topic); got != tc.want {
			t.Errorf("topicMatches(%q, %q) = %v", tc.filter, tc.topic, got)
		}
	}
}

func TestTemplates(t *testing.T) {
	tmpl, err := parseTemplate("out/${topic(2)}/${level * 2}/${missing}")
	if err != nil {
		t.Fatal(err)
	}

	e := &env{doc: map[string]any{"level": int64(4)}, topic: "in/abc"}
	if got := tmpl.render(e); got != "out/abc/8/" {
		t.Fatalf("render = %q", got)
	}

	if _, err := parseTemplate("${unterminated"); err == nil {
		t.Fatal("an unterminated template parsed")
	}
}
