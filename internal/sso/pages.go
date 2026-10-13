package sso

import (
	"html/template"
	"net/http"
)

var errorPage = template.Must(template.New("error").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>TinyStack sign-in</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>body{font:16px/1.5 system-ui,sans-serif;max-width:36rem;margin:12vh auto;padding:0 1rem;color:#16191f}
h1{font-size:1.4rem}p.detail{color:#414d5c}a{color:#0972d3}</style></head>
<body><h1>{{.Title}}</h1><p>{{.Message}}</p><p class="detail">{{.Detail}}</p>
<p><a href="/_tinystack/auth/login">Try signing in again</a> · <a href="/">Back to the console</a></p>
</body></html>`))

// fail writes an error page. Its text is fixed by TinyStack: nothing the
// browser or the provider sent is reflected, except detail the caller has made safe.
func fail(w http.ResponseWriter, status int, title, message, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = errorPage.Execute(w, map[string]string{"Title": title, "Message": message, "Detail": detail})
}
