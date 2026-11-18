package vtl

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRender(t *testing.T) {
	var ctx any
	if err := json.Unmarshal([]byte(`{"args":{"id":"a1","n":3,"tags":["x","y"],"price":2.5},"stash":{}}`),
		&ctx); err != nil {
		t.Fatal(err)
	}

	util := map[string]any{
		"toJson": Func(func(args []any) (any, error) {
			b, err := json.Marshal(ToJSON(args[0]))
			return string(b), err
		}),
		"qr": Func(func([]any) (any, error) { return "", nil }),
	}
	for _, c := range []struct{ src, want string }{
		{`{"id": "$ctx.args.id", "n": $ctx.args.n, "price": ${ctx.args.price}}`, `{"id": "a1", "n": 3, "price": 2.5}`},
		{`$ctx.args.missing|$!ctx.args.missing|$100`, `$ctx.args.missing||$100`},
		{
			`#set($x = $ctx.args.n * 2 + 1)$x #if($x > 6 && !$ctx.args.missing)big#elseif($x > 3)mid#{else}small#end`,
			`7 big`,
		},
		{`#foreach($t in $ctx.args.tags)$t#if($foreach.hasNext),#end#end`, `x,y`},
		{`#set($m = {})$util.qr($m.put("k", "v"))$util.toJson($m)`, `{"k":"v"}`},
		{`#set($l = [])$util.qr($l.add(1))$util.qr($l.add("two"))$l.size() $util.toJson($l)`, `2 [1,"two"]`},
		{`#set($ctx.stash.seen = true)$ctx.stash.seen`, `true`},
		{`"${ctx.args.id}-#if($ctx.args.n == 3)three#end"`, `"a1-three"`},
		{`#foreach($i in [1..3])$i#if($i == 2)#break#end#end`, `12`},
		{`$ctx.args.id.toUpperCase().substring(0, 1) $ctx.args.tags.get(1) $ctx.args.containsKey("n")`, `A y true`},
		{"## a comment\n#* block *#text\\$x", `text$x`},
		{`#set($s = "a,b,c")$s.split(",").size() $s.replaceAll("[a-b]", "z") ${s.length()}`, `3 z,z,c 5`},
		{`#if($ctx.args.n eq 3 and $ctx.args.id ne "b")yes#end`, `yes`},
	} {
		tpl, err := Parse(c.src)
		if err != nil {
			t.Fatalf("%s: %v", c.src, err)
		}

		vars := map[string]any{"ctx": FromJSON(ctx), "util": util}
		got, _, _, err := tpl.Render(vars)
		if err != nil || got != c.want {
			t.Errorf("%s = %q, %v; want %q", c.src, got, err, c.want)
		}
	}

	tpl, _ := Parse(`before#return({"early": $ctx.args.n})after`)
	out, v, ret, err := tpl.Render(map[string]any{"ctx": FromJSON(ctx)})
	if err != nil || !ret || out != "before" || ToJSON(v).(map[string]any)["early"] != int64(3) {
		t.Fatalf("#return = %q %v %v %v", out, v, ret, err)
	}

	for _, bad := range []string{`#if($x)`, `#end`, `#set($x)`, `#foreach($x)#end`, `${x`, `#else`} {
		if _, err = Parse(bad); err == nil {
			t.Errorf("%s parsed", bad)
		}
	}

	tpl, _ = Parse(`$ctx.args.id.frobnicate()`)
	if _, _, _, err = tpl.Render(map[string]any{"ctx": FromJSON(ctx)}); err == nil ||
		!strings.Contains(err.Error(), "frobnicate") {
		t.Fatalf("unknown method: %v", err)
	}
}
