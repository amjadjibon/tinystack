package config

import "testing"

func TestAuthorizationModes(t *testing.T) {
	if Default().Identity.Authorization != AuthorizationOff {
		t.Fatal("authorization must default off")
	}

	for _, mode := range []string{"", AuthorizationOff, AuthorizationSoft, AuthorizationEnforce, "bad"} {
		for _, authentication := range []string{IdentityParse, IdentityVerify} {
			cfg := Default()
			cfg.Identity.Authorization, cfg.Identity.Mode = mode, authentication
			if authentication == IdentityVerify {
				cfg.Identity.SecretKeys = map[string]string{"root": "secret"}
			}

			err := cfg.Validate()
			if (err != nil) != (mode == "bad") {
				t.Fatalf("%s/%s: %v", mode, authentication, err)
			}
		}
	}
}
