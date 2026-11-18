package apigateway

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/execute"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigateway"
)

func TestVelocityRequestMapping(t *testing.T) {
	c := &call{
		req:   &execute.Request{Header: http.Header{"X-Test": {"UPPER"}}, Query: url.Values{"id": {"query"}}},
		stage: &api.Stage{Variables: map[string]string{"name": "prod"}}, params: map[string]string{"id": "path"},
	}
	body := []byte(`{"ids":["ONE","TWO"],"weird.key":{"n":1.00}}`)
	variables := map[string]any{
		"authorizer": map[string]any{"name": "sam"},
		"identity":   map[string]any{"sourceIp": "::1"},
	}
	cases := []struct{ name, expression, want string }{
		{"body", `$input.body`, string(body)},
		{
			"JSONPath",
			`$input.json("$['weird.key'].n")/$input.path('$.ids[1]')/$input.json('$.absent')`,
			"1.00/TWO/null",
		},
		{"params", `$input.params('id')/$input.params().get('header').get('X-Test')`, "path/UPPER"},
		{
			"context",
			`$context.authorizer.name/$context.identity.sourceIp/$stageVariables.name/$!{unused}`,
			"sam/::1/prod/",
		},
		{
			"encoding",
			`$util.urlEncode('x /y')/$util.urlDecode('x+%2Fy')/$util.base64Decode($util.base64Encode('abc'))`,
			"x+%2Fy/x /y/abc",
		},
		{"escaping", `#set($s = "I'm")$util.escapeJavaScript($s)`, `I\'m`},
		{
			"parse and methods",
			`#set($v = $util.parseJson($input.body))$v.ids.size()/$v.ids.contains('ONE')/$v.ids[1].length()`,
			"2/true/3",
		},
		{
			"directives",
			"## line comment\n" + `#* block *##set($a = $input.path('$.ids'))` +
				`#if(false)bad#elseif($a.size() == 2)#foreach($v in $a)$v.toLowerCase()` +
				`#if($foreach.hasNext),#end#end#else bad#end`,
			"one,two",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := c.renderMapping(tc.expression, body, variables)
			if err != nil || string(got) != tc.want {
				t.Fatalf("got %q, %v, want %q", got, err, tc.want)
			}
		})
	}
}

func TestVelocityMappingErrors(t *testing.T) {
	c := &call{stage: &api.Stage{}}
	for _, tc := range []struct{ template, message string }{
		{`$input.path('$..x')`, "unsupported JSONPath"},
		{`$util.unknown('x')`, "has no method"},
		{`#macro(test)no#end`, "unsupported directive"},
		{`$input.path('$.n')`, "not valid JSON"},
	} {
		_, err := c.renderMapping(tc.template, []byte("bad JSON"), nil)
		if err == nil || !strings.Contains(err.Error(), tc.message) {
			t.Errorf("%q: %v", tc.template, err)
		}
	}
}
