package sts

import (
	"encoding/base64"
	"testing"
)

func TestParseTokenRejectsEmptyAudiences(t *testing.T) {
	for _, audience := range []string{`[]`, `null`, `[""]`, `["valid",""]`} {
		raw := `{"iss":"https://cognito-identity.amazonaws.com","sub":"subject","exp":9999999999,"aud":` + audience + `}`
		token := "e30." + base64.RawURLEncoding.EncodeToString([]byte(raw)) + ".c2ln"
		if _, _, err := parseToken(token); err == nil {
			t.Fatalf("accepted audience %s", audience)
		}
	}
}
