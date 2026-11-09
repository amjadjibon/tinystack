package xray

import "testing"

func TestTraceFilter(t *testing.T) {
	trace := traceRecord{
		ID: "1-12345678-123456789012345678901234",
		Segments: map[string]storedSegment{
			"a": {
				Segment: &segmentDocument{
					Name:        "api",
					Start:       1,
					End:         new(float64(3)),
					Error:       true,
					Annotations: map[string]any{"env": "prod", "n": float64(2), "ok": true},
				},
			},
		},
	}

	for _, test := range []struct {
		expression string
		match      bool
	}{
		{`service("api")`, true},
		{`service("other")`, false},
		{`error OR fault AND throttle`, true},
		{`(error OR fault) AND throttle`, false},
		{`annotation.env = "prod" AND duration > 1`, true},
		{`annotation.n = 2 AND annotation.ok = true`, true},
		{`annotation.env = "AND OR"`, false},
		{`responsetime > 2`, false},
		{`service("api") AND (fault OR error)`, true},
	} {
		t.Run(test.expression, func(t *testing.T) {
			f, err := parseFilter(test.expression)
			if err != nil {
				t.Fatal(err)
			}

			if f(trace) != test.match {
				t.Fatal("unexpected match")
			}
		})
	}

	for _, expr := range []string{
		`foo`,
		`http.url = "x"`,
		`error garbage`,
		`annotation.a != 1`,
		`duration >= 1`,
		`NOT error`,
		`service("api") { fault }`,
		`(error`,
		`annotation.a =`,
		`service("unterminated)`,
		`error AND`,
		`http.status = "200"`,
	} {
		t.Run(expr, func(t *testing.T) {
			if _, err := parseFilter(expr); err == nil {
				t.Fatal("unsupported filter accepted")
			}
		})
	}
}
