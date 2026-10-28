package dsql

import (
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/sigv4"
)

var errDatabaseToken = errors.New("invalid database authentication token")

// databaseToken checks token structure and lifetime even when signature verification is disabled.
// The caller resolves the signing identity and invokes the shared SigV4 verifier in verify mode.
func databaseToken(raw, endpoint, region, user string, now time.Time) (*http.Request, *sigv4.Authorization, error) {
	if strings.Contains(raw, "://") {
		return nil, nil, errDatabaseToken
	}

	u, err := url.Parse("https://" + raw)
	if err != nil || u.Host != endpoint || u.User != nil || (u.Path != "/" && u.Path != "") || u.Fragment != "" {
		return nil, nil, errDatabaseToken
	}

	q := u.Query()
	for _, values := range q {
		if len(values) != 1 {
			return nil, nil, errDatabaseToken
		}
	}

	action := "DbConnect"
	if user == "admin" {
		action = "DbConnectAdmin"
	}

	if user == "" || q.Get("Action") != action {
		return nil, nil, errDatabaseToken
	}

	a, err := sigv4.ParseQuery(q, 0)
	if err != nil || a.Algorithm != sigv4.Algorithm || a.Service != serviceName || a.Region != region ||
		len(a.SignedHeaders) != 1 || a.SignedHeaders[0] != "host" {
		return nil, nil, errDatabaseToken
	}

	signature, err := hex.DecodeString(a.Signature)
	if err != nil || len(signature) != 32 {
		return nil, nil, errDatabaseToken
	}

	signedAt, err := time.Parse("20060102T150405Z", q.Get("X-Amz-Date"))
	if err != nil || signedAt.Format("20060102") != a.Date || now.Before(signedAt.Add(-sigv4.MaxSkew)) ||
		!now.Before(signedAt.Add(a.Expires)) {
		return nil, nil, errDatabaseToken
	}

	return &http.Request{Method: http.MethodGet, URL: u, Host: u.Host, Header: http.Header{}}, a, nil
}
