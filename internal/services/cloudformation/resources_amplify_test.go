package cloudformation

import "testing"

func TestAmplifyPropertyConversions(t *testing.T) {
	for _, p := range []props{
		{"EnvironmentVariables": "invalid"},
		{"EnvironmentVariables": []any{map[string]any{"Name": "TEAM"}}},
		{"EnvironmentVariables": []any{
			map[string]any{"Name": "TEAM", "Value": "a"},
			map[string]any{"Name": "TEAM", "Value": "b"},
		}},
		{"BasicAuthConfig": map[string]any{"Username": "user"}},
		{"BasicAuthConfig": map[string]any{"Unsupported": true}},
		{"CertificateSettings": map[string]any{"Unsupported": true}},
	} {
		if _, err := amplifyParams(p); err == nil {
			t.Fatal("accepted malformed properties", p)
		}
	}

	got, err := amplifyParams(props{
		"EnvironmentVariables": []any{map[string]any{"Name": "TEAM", "Value": "core"}},
		"BasicAuthConfig":      map[string]any{"EnableBasicAuth": true, "Username": "user", "Password": "pass"},
		"CertificateSettings":  map[string]any{"CertificateType": "CUSTOM", "CustomCertificateArn": "arn"},
	})
	if err != nil || got["BasicAuthCredentials"] != "dXNlcjpwYXNz" || got["EnableBasicAuth"] != true {
		t.Fatal(got, err)
	}
}
