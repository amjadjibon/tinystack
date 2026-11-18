package gateway

import (
	"html/template"
	"net/http"

	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/services/ssooidc"
)

// UseSignIn serves IAM Identity Center's sign-in pages: device code approval and the authorization endpoint.
func (s *Server) UseSignIn(signIn *ssooidc.Service) {
	s.signIn = signIn
	pages := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.serveSignIn(w, r) })
	s.HandlePath(ssooidc.DevicePath, pages)
	s.HandlePath(ssooidc.ApprovePath, pages)
}

var signInPage = template.Must(template.New("signin").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width">
<title>IAM Identity Center sign-in</title></head><body><main><h1>Sign in to the AWS access portal</h1>
{{if .Message}}<p role="alert">{{.Message}}</p>{{end}}
{{if .Prompt}}<p>{{.Prompt.Client}} is asking to use your AWS access.</p>
<form method="post" action="{{.Action}}">
{{if .Prompt.Value}}<input type="hidden" name="{{.Field}}" value="{{.Prompt.Value}}">
{{if eq .Field "user_code"}}<p>Code: <code>{{.Prompt.Value}}</code></p>{{end}}
{{else}}<label>Code <input name="user_code" autocomplete="one-time-code" required></label>{{end}}
<label>Identity Store user name <input name="username" autocomplete="username" required></label>
<button type="submit">Allow access</button></form>{{end}}
{{if .Done}}<p>Access allowed. You can close this window and return to the application.</p>{{end}}
</main></body></html>`))

type signInView struct {
	Prompt  *ssooidc.Prompt
	Message string
	Action  string
	Field   string
	Done    bool
}

// serveSignIn handles the sign-in pages. GET /authorize belongs to it only with an authorization request's
// parameters, so an S3 bucket named authorize keeps its path.
func (s *Server) serveSignIn(w http.ResponseWriter, r *http.Request) bool {
	if s.signIn == nil {
		return false
	}

	query := r.URL.Query()
	switch {
	case r.URL.Path == ssooidc.DevicePath, r.URL.Path == ssooidc.ApprovePath:
	case r.URL.Path == ssooidc.AuthorizePath && r.Method == http.MethodGet && query.Has("client_id") &&
		query.Has("response_type"):
	default:
		return false
	}

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; form-action 'self'; frame-ancestors 'none'")
	view := signInView{Action: ssooidc.DevicePath, Field: "user_code"}
	render := func(status int) bool {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		if err := signInPage.Execute(w, view); err != nil {
			s.logger.Error("render Identity Center sign-in page")
		}

		return true
	}

	// Identity Store users have no passwords, so approving by user name would let anyone act as any user.
	if s.cfg.Identity.Mode == config.IdentityVerify {
		view.Message = "Identity Center sign-in is unavailable in identity.mode verify: Identity Store users " +
			"have no passwords to sign in with."
		return render(http.StatusForbidden)
	}

	post := r.Method == http.MethodPost
	if !post && r.Method != http.MethodGet || post && r.URL.Path == ssooidc.AuthorizePath ||
		!post && r.URL.Path == ssooidc.ApprovePath {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return true
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", http.StatusBadRequest)
		return true
	}

	ctx := r.Context()
	var err error
	switch {
	case r.URL.Path == ssooidc.AuthorizePath:
		view.Action, view.Field = ssooidc.ApprovePath, "transaction"
		view.Prompt, err = s.signIn.Authorize(ctx, query)
	case r.URL.Path == ssooidc.ApprovePath:
		view.Action, view.Field = ssooidc.ApprovePath, "transaction"
		var redirect string
		redirect, err = s.signIn.ApproveAuthorization(ctx, r.PostForm.Get("transaction"), r.PostForm.Get("username"))
		if err == nil {
			// #nosec G710 -- the redirect URI is one the client registered, and only loopback URIs register.
			http.Redirect(w, r, redirect, http.StatusFound)
			return true
		}
	case post:
		err = s.signIn.ApproveDevice(ctx, r.PostForm.Get("user_code"), r.PostForm.Get("username"))
		view.Done = err == nil
		if err != nil {
			// Show the form again so the user can correct the user name.
			view.Prompt, _ = s.signIn.DevicePrompt(ctx, r.PostForm.Get("user_code"))
		}
	case query.Get("user_code") != "":
		view.Prompt, err = s.signIn.DevicePrompt(ctx, query.Get("user_code"))
	default:
		view.Prompt = &ssooidc.Prompt{Client: "An application"}
	}

	if err == nil {
		return render(http.StatusOK)
	}

	view.Message = ssooidc.SignInMessage(err)
	if view.Message == "" {
		s.logger.Error("Identity Center sign-in failed", "error", err)
		view.Message = "Sign-in failed."
		return render(http.StatusInternalServerError)
	}

	return render(http.StatusBadRequest)
}
