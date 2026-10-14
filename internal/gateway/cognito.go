package gateway

import (
	"html/template"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/cognitoidp"
)

// UseHosted serves the user pool's hosted endpoints in issuer-path and configured-domain form.
func (s *Server) UseHosted(hosted *cognitoidp.Service) { s.hosted = hosted }

func requestEndpoint(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}

	return scheme + "://" + r.Host
}

const (
	hostedLoginPath    = "/login"
	hostedTokenPath    = "/oauth2/token" // #nosec G101 -- OAuth route, not a credential.
	hostedUserInfoPath = "/oauth2/userInfo"
	hostedRevokePath   = "/oauth2/revoke"
)

// hostedPoolID is the shape of a user pool ID, the first path segment of the issuer-path form.
var hostedPoolID = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-\d_[0-9a-zA-Z]+$`)

var hostedPage = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width">
<title>Sign in</title></head><body><main><h1>Sign in</h1>
{{if .Result.Message}}<p role="alert">{{.Result.Message}}</p>{{end}}
{{if .Result.Transaction}}<form method="post" action="{{.Action}}">
<input type="hidden" name="transaction" value="{{.Result.Transaction}}">
<input type="hidden" name="csrf" value="{{.Result.CSRF}}">
{{if .Result.Challenge}}<p>{{.Result.Challenge}}</p>
{{if eq .Result.Challenge "NEW_PASSWORD_REQUIRED"}}
<label>New password <input type="password" name="password" autocomplete="new-password" required></label>
{{range .Result.Required}}<label>{{.}} <input name="{{.}}" required></label>{{end}}
{{else}}{{if .Result.TOTPSecret}}<p>Add this key to your authenticator: <code>{{.Result.TOTPSecret}}</code></p>{{end}}
<label>Code or MFA selection <input name="code" autocomplete="one-time-code" required></label>{{end}}
{{else}}
<label>Username <input name="username" autocomplete="username" value="{{.Result.Username}}" required></label>
<label>Password <input type="password" name="password" autocomplete="current-password" required></label>
{{end}}<button type="submit">Continue</button></form>
{{if not .Result.Challenge}}{{range .Result.Providers}}<form method="post" action="{{$.Action}}">
<input type="hidden" name="transaction" value="{{$.Result.Transaction}}">
<input type="hidden" name="csrf" value="{{$.Result.CSRF}}">
<button name="identity_provider" value="{{.}}">Sign in with {{.}}</button></form>{{end}}{{end}}{{end}}
</main></body></html>`))

func (s *Server) serveHosted(w http.ResponseWriter, r *http.Request) bool {
	if s.hosted == nil {
		return false
	}

	// The path decides first, so other services' requests never read storage here.
	poolID, path, base := "", r.URL.Path, ""
	if head, tail, split := strings.Cut(strings.TrimPrefix(path, "/"), "/"); split && hostedPoolID.MatchString(head) {
		poolID, path, base = head, "/"+tail, "/"+head
	}

	switch path {
	case "/oauth2/authorize",
		hostedLoginPath,
		"/logout",
		hostedTokenPath,
		hostedUserInfoPath,
		hostedRevokePath,
		"/oauth2/idpresponse":
	default:
		return false
	}

	if poolID == "" {
		var found bool
		var err error
		poolID, found, err = s.hosted.HostedPool(r.Context(), r.Host)
		if err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return true
		}

		if !found {
			return false
		}
	}

	endpoint := requestEndpoint(r)
	ctx := request.WithMetadata(r.Context(), request.Metadata{Endpoint: endpoint})
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().
		Set("Content-Security-Policy", "default-src 'none'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	oauth := path == hostedTokenPath || path == hostedUserInfoPath || path == hostedRevokePath
	if oauth {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return true
		}
	}

	get := r.Method == http.MethodGet
	post := r.Method == http.MethodPost
	if (!get && !post) || (path != hostedLoginPath && path != hostedUserInfoPath &&
		((oauth && !post) || (!oauth && !get))) {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return true
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", http.StatusBadRequest)
		return true
	}

	if oauth {
		var status int
		var body []byte
		var err error
		switch path {
		case hostedTokenPath:
			status, body, err = s.hosted.Token(ctx, poolID, endpoint, r.Header.Get("Authorization"), r.PostForm)
		case hostedUserInfoPath:
			status, body, err = s.hosted.UserInfo(ctx, poolID, r.Header.Get("Authorization"))
		case hostedRevokePath:
			status, body, err = s.hosted.Revoke(ctx, poolID, r.Header.Get("Authorization"), r.PostForm)
		}

		if err != nil {
			status, body = http.StatusInternalServerError, []byte(`{"error":"server_error"}`)
		}

		if status == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		}

		w.WriteHeader(status)
		_, _ = w.Write(body) // #nosec G705 -- JSON encoding and application/json prevent HTML interpretation.
		return true
	}

	browserName, sessionName := "tinystack-browser-"+poolID, "cognito-"+poolID
	browser, session := "", ""
	if cookie, err := r.Cookie(browserName); err == nil {
		browser = cookie.Value
	}

	if cookie, err := r.Cookie(sessionName); err == nil {
		session = cookie.Value
	}

	var result *cognitoidp.HostedResult
	var err error
	switch path {
	case "/oauth2/authorize", hostedLoginPath:
		if post {
			result, err = s.hosted.HostedLogin(ctx, poolID, r.PostForm, browser, session)
		} else {
			result, err = s.hosted.Authorize(ctx, poolID, r.URL.Query(), browser, session)
		}
	case "/logout":
		result, err = s.hosted.HostedLogout(ctx, poolID, r.URL.Query(), browser, session)
	case "/oauth2/idpresponse":
		result, err = s.hosted.IdPResponse(ctx, poolID, r.URL.Query(), browser, session)
	}

	if err != nil {
		result = &cognitoidp.HostedResult{Message: awserror.From(err).Message}
		w.WriteHeader(http.StatusBadRequest)
	}

	cookie := func(name, value string, age int) {
		// #nosec G124 -- Secure follows the actual TLS transport; local HTTP is supported.
		http.SetCookie(w, &http.Cookie{
			Name: name, Value: value, Path: base + "/", MaxAge: age,
			HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode,
		})
	}

	if result.Browser != "" {
		cookie(browserName, result.Browser, int(time.Hour/time.Second))
	}

	if result.Session != "" {
		cookie(sessionName, result.Session, int(time.Hour/time.Second))
	}

	if result.ClearSession {
		cookie(sessionName, "", -1)
	}

	if result.Redirect != "" {
		http.Redirect(
			w,
			r,
			result.Redirect,
			http.StatusFound,
		) // #nosec G710 -- Service checks client callback/logout allowlists.
		return true
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := hostedPage.Execute(w, struct {
		Result *cognitoidp.HostedResult
		Action string
	}{Result: result, Action: base + hostedLoginPath}); err != nil {
		s.logger.Error("render hosted sign-in page")
	}

	return true
}
