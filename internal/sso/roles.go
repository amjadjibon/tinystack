package sso

import "net/http"

// roles renders the access portal and accepts a selection from the browser's
// server-owned allowed set. GET never mints credentials; POST has the same
// origin and cookie checks as the credential handoff.
func (s *Service) roles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && !s.sameOriginPost(w, r) {
		return
	}

	cookie, err := r.Cookie(s.cookieName(sessionCookie))
	if err != nil {
		fail(w, http.StatusUnauthorized, "Sign-in expired", "Sign in to choose a role.", "")
		return
	}

	person, found := s.sessions.get(cookie.Value)
	if !found {
		fail(w, http.StatusUnauthorized, "Sign-in expired", "Sign in to choose a role.", "")
		return
	}

	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = rolesPage.Execute(w, person)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid role selection", http.StatusBadRequest)
		return
	}

	for _, role := range person.Roles {
		if role.Role != r.PostForm.Get("role") {
			continue
		}

		selected, problem := s.mintRole(r.Context(), &person, role)
		if problem != nil {
			fail(w, problem.status, problem.title, problem.message, problem.detail)
			return
		}

		if !s.sessions.replace(cookie.Value, *selected) {
			fail(w, http.StatusUnauthorized, "Sign-in expired", "Sign in to choose a role.", "")
			return
		}

		http.Redirect(w, r, "/?signin=sso", http.StatusSeeOther)
		return
	}

	http.Error(w, "role is not allowed for this session", http.StatusForbidden)
}
