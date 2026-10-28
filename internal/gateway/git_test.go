package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGitPath(t *testing.T) {
	for _, tc := range []struct {
		path, repository, rest string
		ok                     bool
	}{
		{"/v1/repos/demo/info/refs", "demo", "info/refs", true},
		{"/v1/repos/demo/git-upload-pack", "demo", "git-upload-pack", true},
		{"/v1/repos/demo", "demo", "", true},
		{"/v1/repos/demo/", "demo", "", true},
		{"/v1/repos/my.repo_1-x/git-receive-pack", "my.repo_1-x", "git-receive-pack", true},
		{"/v1/repos/", "", "", false},
		{"/v1/repos", "", "", false},
		{"/v2/repos/demo", "", "", false},
		{"/v1/other/demo", "", "", false},
		{"/", "", "", false},
	} {
		repository, rest, ok := gitPath(tc.path)
		if repository != tc.repository || rest != tc.rest || ok != tc.ok {
			t.Errorf("gitPath(%q) = %q, %q, %v", tc.path, repository, rest, ok)
		}
	}
}

func TestGitRegion(t *testing.T) {
	for host, region := range map[string]string{
		"git-codecommit.us-east-1.amazonaws.com":                         "us-east-1",
		"git-codecommit.eu-west-2.amazonaws.com:443":                     "eu-west-2",
		"GIT-CODECOMMIT.us-west-2.amazonaws.com.":                        "us-west-2",
		"git-codecommit-fips.us-gov-west-1.amazonaws.com":                "us-gov-west-1",
		"git-codecommit.cn-north-1.amazonaws.com.cn":                     "cn-north-1",
		"vpce-0abc.git-codecommit.us-east-1.vpce.amazonaws.com":          "us-east-1",
		"vpce-0abc-xyz.git-codecommit.ap-south-1.vpce.amazonaws.com:443": "ap-south-1",
	} {
		got, ok := gitRegion(host)
		if !ok || got != region {
			t.Errorf("gitRegion(%q) = %q, %v, want %q", host, got, ok, region)
		}
	}

	for _, host := range []string{
		"localhost:4566", "127.0.0.1", "codecommit.us-east-1.amazonaws.com", "git-codecommit.amazonaws.com",
		"git-codecommit.us-east-1.example.com", "evil-git-codecommit.us-east-1.amazonaws.com",
		"s3.us-east-1.amazonaws.com", "",
	} {
		if region, ok := gitRegion(host); ok {
			t.Errorf("gitRegion(%q) = %q", host, region)
		}
	}
}

func TestGitAction(t *testing.T) {
	for _, tc := range []struct {
		method, target, rest, action string
	}{
		{"GET", "/v1/repos/d/info/refs?service=git-upload-pack", "info/refs", "GitPull"},
		{"GET", "/v1/repos/d/info/refs?service=git-receive-pack", "info/refs", "GitPush"},
		{"POST", "/v1/repos/d/git-upload-pack", "git-upload-pack", "GitPull"},
		{"POST", "/v1/repos/d/git-receive-pack", "git-receive-pack", "GitPush"},
		{"GET", "/v1/repos/d/info/refs", "info/refs", ""},
		{"GET", "/v1/repos/d/info/refs?service=other", "info/refs", ""},
		{"GET", "/v1/repos/d/git-receive-pack", "git-receive-pack", ""},
		{"POST", "/v1/repos/d/info/refs?service=git-receive-pack", "info/refs", ""},
		{"POST", "/v1/repos/d/other", "other", ""},
	} {
		r := httptest.NewRequestWithContext(context.Background(), tc.method, tc.target, http.NoBody)
		if got := gitAction(r, tc.rest); got != tc.action {
			t.Errorf("%s %s: %q, want %q", tc.method, tc.target, got, tc.action)
		}
	}
}
