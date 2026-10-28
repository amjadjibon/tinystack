package dsql

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/amjadjibon/tinystack/internal/aws/sigv4"
)

func TestDatabaseToken(t *testing.T) {
	const endpoint = "cluster.dsql.us-east-1.on.aws"
	const emptyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	now := time.Now().UTC().Truncate(time.Second)
	for _, user := range []string{"admin", "reader"} {
		t.Run(user, func(t *testing.T) {
			action := "DbConnect"
			if user == "admin" {
				action = "DbConnectAdmin"
			}

			u, err := url.Parse("https://" + endpoint + "/?Action=" + action + "&X-Amz-Expires=900")
			if err != nil {
				t.Fatal(err)
			}

			req, _, err := databaseToken("", endpoint, "us-east-1", user, now)
			if err == nil || req != nil {
				t.Fatal("accepted empty token")
			}

			request := &http.Request{Method: http.MethodGet, URL: u, Host: u.Host, Header: http.Header{}}
			signed, _, err := v4.NewSigner().PresignHTTP(context.Background(),
				aws.Credentials{AccessKeyID: "test", SecretAccessKey: "secret"}, request,
				emptyHash, "dsql", "us-east-1", now)
			if err != nil {
				t.Fatal(err)
			}

			raw := strings.TrimPrefix(signed, "https://")
			r, a, err := databaseToken(raw, endpoint, "us-east-1", user, now)
			if err != nil {
				t.Fatal(err)
			}

			if err := sigv4.Verify(r, a, "secret", emptyHash, false, now); err != nil {
				t.Fatal(err)
			}

			for name, token := range map[string]string{
				"host":      strings.Replace(raw, endpoint, "other.dsql.us-east-1.on.aws", 1),
				"action":    strings.Replace(raw, "Action="+action, "Action=Other", 1),
				"duplicate": raw + "&Action=" + action,
				"signature": strings.Replace(raw, "X-Amz-Signature=", "X-Amz-Signature=z", 1),
				"scheme":    signed,
			} {
				if _, _, err := databaseToken(token, endpoint, "us-east-1", user, now); err == nil {
					t.Errorf("accepted %s", name)
				}
			}

			if _, _, err := databaseToken(raw, endpoint, "us-east-1", user, now.Add(15*time.Minute)); err == nil {
				t.Error("accepted expired token")
			}

			if _, _, err := databaseToken(raw, endpoint, "us-west-2", user, now); err == nil {
				t.Error("accepted wrong region")
			}
		})
	}
}
