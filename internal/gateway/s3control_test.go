package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/config"
)

func TestS3ControlDiscriminator(t *testing.T) {
	s := &Server{cfg: config.Default(), services: map[string]*catalog.Service{
		"s3":        {Name: "s3", SigningName: "s3", Protocols: []string{catalog.RESTXML}},
		"s3control": {Name: "s3control", SigningName: "s3", Protocols: []string{catalog.RESTXML}},
	}}
	for _, tc := range []struct{ path, account, want string }{
		{"/v20180820/tags/arn:aws:s3:::assets", "000000000000", "s3control"},
		{"/v20180820/tags/arn:aws:s3:::assets", "", "s3"},
		{"/v20180820/object", "", "s3"},
		{"/v20180820", "000000000000", "s3"},
		{"/assets/object", "000000000000", "s3"},
	} {
		r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1"+tc.path, http.NoBody)
		r.Header.Set("x-amz-account-id", tc.account)
		got := s.restServices(r)
		if len(got) != 1 || got[0].Name != tc.want {
			t.Fatalf("%s account=%s: %+v", tc.path, tc.account, got)
		}
	}
}
