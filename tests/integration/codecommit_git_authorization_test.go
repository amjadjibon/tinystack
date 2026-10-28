package integration_test

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/amjadjibon/tinystack/internal/config"
)

const repoARN = "arn:aws:codecommit:us-east-1:000000000000:demo"

func gitPolicy(actions, resource string) string {
	return `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":[` + actions +
		`],"Resource":"` + resource + `"}]}`
}

// basic returns git arguments that send Basic credentials up front, as git otherwise waits for a challenge.
func basic(user, password string) []string {
	token := base64.StdEncoding.EncodeToString([]byte(user + ":" + password))
	return []string{"-c", "http.extraHeader=Authorization: Basic " + token}
}

func gitWithAuth(g *gitHome, dir string, auth []string, args ...string) (string, error) {
	return g.run(dir, append(auth, args...)...)
}

// seed pushes one commit to demo as the account's root.
func seedGit(t *testing.T, g *gitHome, s *httptest.Server) string {
	t.Helper()
	work := filepath.Join(g.home, "seed")
	g.must(g.home, "init", "-q", work)
	g.write(work, "a.txt", "a\n")
	g.must(work, "add", ".")
	g.must(work, "commit", "-m", "seed")
	g.must(work, append(basic("test", "x"), "push", s.URL+"/v1/repos/demo", "HEAD:refs/heads/main")...)
	return work
}

func TestCodeCommitGitAuthorization(t *testing.T) {
	for _, verify := range []bool{false, true} {
		name := "parse"
		if verify {
			name = "verify"
		}

		t.Run(name, func(t *testing.T) {
			s := authorizationServer(t, config.AuthorizationEnforce, verify)
			g := newGitHome(t)
			createRepo(t, codecommitClient(s, "test", "us-east-1"), "demo")
			createRepo(t, codecommitClient(s, "test", "us-east-1"), "other")
			host, _, _ := strings.Cut(strings.TrimPrefix(s.URL, "http://"), ":")
			work := filepath.Join(g.home, "seed")
			g.must(g.home, "init", "-q", work)
			g.write(work, "a.txt", "a\n")
			g.must(work, "add", ".")
			g.must(work, "commit", "-m", "seed")

			// Credentials as a client sends them: the access key, and a password that only verify mode checks.
			credentials := func(secret, key, repo string) []string {
				if verify {
					return basic(key, gitPassword(secret, time.Now(), "us-east-1", repo, host))
				}

				return basic(key, "x")
			}

			// The account's configured key acts as root and may push and pull.
			if out, err := gitWithAuth(g, work, credentials("test", "test", "demo"),
				"push", s.URL+"/v1/repos/demo", "HEAD:refs/heads/main"); err != nil {
				t.Fatalf("root push: %v\n%s", err, out)
			}

			alice := authorizationUser(t, s)
			key, secret := aws.ToString(alice.AccessKeyId), aws.ToString(alice.SecretAccessKey)
			cloneAs := func(dir, repo string) (string, error) {
				return gitWithAuth(g, g.home, credentials(secret, key, repo), "clone", s.URL+"/v1/repos/"+repo, dir)
			}
			pushAs := func(repo string) (string, error) {
				return gitWithAuth(g, work, credentials(secret, key, repo),
					"push", s.URL+"/v1/repos/"+repo, "HEAD:refs/heads/feature")
			}

			// No policy: nothing is allowed.
			if out, err := cloneAs(filepath.Join(g.home, "c0"), "demo"); err == nil || !strings.Contains(out, "403") {
				t.Fatalf("clone without a policy: %v\n%s", err, out)
			}

			// GitPull on the repository clones but does not push.
			authorizationPolicy(t, s, gitPolicy(`"codecommit:GitPull"`, repoARN))
			if out, err := cloneAs(filepath.Join(g.home, "c1"), "demo"); err != nil {
				t.Fatalf("clone with GitPull: %v\n%s", err, out)
			}

			if out, err := pushAs("demo"); err == nil || !strings.Contains(out, "403") {
				t.Fatalf("push with GitPull only: %v\n%s", err, out)
			}

			// The policy names one repository.
			if out, err := cloneAs(filepath.Join(g.home, "c2"), "other"); err == nil || !strings.Contains(out, "403") {
				t.Fatalf("clone of another repository: %v\n%s", err, out)
			}

			// GitPush as well allows the push.
			authorizationPolicy(t, s, gitPolicy(`"codecommit:GitPull","codecommit:GitPush"`, repoARN))
			if out, err := pushAs("demo"); err != nil {
				t.Fatalf("push with GitPush: %v\n%s", err, out)
			}

			// Without a key the caller is anonymous and denied; in verify mode the missing credentials are refused first.
			out, err := g.run(g.home, "clone", s.URL+"/v1/repos/demo", filepath.Join(g.home, "c3"))
			if err == nil || !strings.Contains(out, "403") && !strings.Contains(out, "Username") {
				t.Fatalf("anonymous clone: %v\n%s", err, out)
			}

			// A key nobody issued is anonymous too.
			if verify {
				return
			}

			out, err = gitWithAuth(g, g.home, basic("AKIAUNKNOWN", "x"),
				"clone", s.URL+"/v1/repos/demo", filepath.Join(g.home, "c4"))
			if err == nil || !strings.Contains(out, "403") {
				t.Fatalf("unknown key clone: %v\n%s", err, out)
			}

			// The decisions are in the request inspector.
			resp, err := http.Get(s.URL + "/_tinystack/requests")
			if err != nil {
				t.Fatal(err)
			}

			defer func() { _ = resp.Body.Close() }()
			body, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(body), `"codecommit:GitPush"`) || !strings.Contains(string(body), "GitPull") {
				t.Fatalf("the inspector lacks the git decisions: %s", body)
			}
		})
	}
}

func TestCodeCommitGitAuthorizationSoftAndOff(t *testing.T) {
	for _, mode := range []string{config.AuthorizationSoft, config.AuthorizationOff} {
		t.Run(mode, func(t *testing.T) {
			s := authorizationServer(t, mode, false)
			g := newGitHome(t)
			createRepo(t, codecommitClient(s, "test", "us-east-1"), "demo")
			work := seedGit(t, g, s)
			authorizationUser(t, s)

			// Anonymous callers are not stopped: soft mode records what enforce would deny.
			clone := filepath.Join(g.home, "anon")
			g.must(g.home, "clone", s.URL+"/v1/repos/demo", clone)
			g.must(work, "push", s.URL+"/v1/repos/demo", "HEAD:refs/heads/feature")

			resp, err := http.Get(s.URL + "/_tinystack/requests")
			if err != nil {
				t.Fatal(err)
			}

			defer func() { _ = resp.Body.Close() }()
			var inspected struct {
				Requests []struct {
					Authorization *struct {
						Message string `json:"message"`
						Allowed bool   `json:"allowed"`
					} `json:"authorization"`
					Operation string `json:"operation"`
				} `json:"requests"`
			}

			if err = json.NewDecoder(resp.Body).Decode(&inspected); err != nil {
				t.Fatal(err)
			}

			var denied int
			for _, r := range inspected.Requests {
				if r.Operation != "GitPull" && r.Operation != "GitPush" {
					continue
				}

				if mode == config.AuthorizationOff || r.Authorization == nil {
					t.Fatalf("%s: unexpected git record %+v", mode, r)
				}

				if !r.Authorization.Allowed && strings.HasPrefix(r.Authorization.Message, "would deny:") {
					denied++
				}
			}

			if mode == config.AuthorizationSoft && denied == 0 {
				t.Fatal("soft mode recorded no would-deny decision")
			}
		})
	}
}
