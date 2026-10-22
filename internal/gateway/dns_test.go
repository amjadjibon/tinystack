package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/config"
)

func TestAWSHostScope(t *testing.T) {
	s := &Server{cfg: config.Default()}
	for _, tc := range []struct{ host, service, region, bucket string }{
		{"s3.amazonaws.com", "s3", "us-east-1", ""},
		{"iam.amazonaws.com", "iam", "us-east-1", ""},
		{"ec2.eu-west-1.amazonaws.com:1234", "ec2", "eu-west-1", ""},
		{"my.bucket.s3.eu-west-1.amazonaws.com", "s3", "eu-west-1", "my.bucket"},
		{"mybucket.s3.amazonaws.com", "s3", "us-east-1", "mybucket"},
		{"mybucket.s3.cn-north-1.amazonaws.com.cn", "s3", "cn-north-1", "mybucket"},
		{"s3.eu-west-1.api.aws", "s3", "eu-west-1", ""},
	} {
		t.Run(tc.host, func(t *testing.T) {
			r := httptest.NewRequest("GET", "http://"+tc.host+"/", http.NoBody)
			m := request.Metadata{Scope: request.Scope{Region: "us-east-1"}}
			_, err := s.identify(r, &m)
			if err != nil {
				t.Fatal(err)
			}

			if m.Service != tc.service || m.Region != tc.region || virtualBucket(r.Host) != tc.bucket {
				t.Fatalf("scope=%v bucket=%q", m, virtualBucket(r.Host))
			}
		})
	}
}
