package appsync

import (
	"reflect"
	"strings"
	"testing"
)

func TestCompileModules(t *testing.T) {
	// esbuild's output: aliased and namespace imports, then an export list.
	bundle := `import { util as u, runtime } from "@aws-appsync/utils";
import * as ddb from "@aws-appsync/utils/dynamodb";

// src/resolver.ts
function request(ctx) {
  return ddb.get({ key: { id: u.defaultIfNull(ctx.args.id, "none") } });
}
const response = (ctx) => ({ seen: ctx.stash.seen, kind: typeof runtime.earlyReturn });
export {
  request,
  response
};
`
	var appended []*fieldError
	j, err := newJS(map[string]any{"arguments": map[string]any{}, "stash": map[string]any{"seen": 1}}, &appended)
	if err != nil {
		t.Fatal(err)
	}

	defer j.stop()
	h, err := j.load(bundle)
	if err != nil {
		t.Fatal(err)
	}

	req, err := j.call(h.request)
	if err != nil {
		t.Fatal(err)
	}

	got, err := j.export(req)
	want := map[string]any{"operation": "GetItem", "key": map[string]any{"id": map[string]any{"S": "none"}}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("request = %v, %v", got, err)
	}

	res, err := j.call(h.response)
	if err != nil {
		t.Fatal(err)
	}

	if got, _ = j.export(res); !reflect.DeepEqual(got, map[string]any{"seen": 1.0, "kind": "function"}) {
		t.Fatalf("response = %v", got)
	}

	if _, err = compile(`import axios from "axios"; export function request() {}`); err == nil ||
		!strings.Contains(err.Error(), "axios") {
		t.Fatalf("unknown module: %v", err)
	}

	if _, err = j.load(`export function request(ctx) { return {}; }`); err == nil {
		t.Fatal("code without a response function loaded")
	}
}
