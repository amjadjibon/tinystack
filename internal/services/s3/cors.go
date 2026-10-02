package s3

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// maxCORSRules is how many rules one CORS configuration may have.
const maxCORSRules = 100

var corsMethods = map[string]bool{
	http.MethodGet: true, http.MethodPut: true, http.MethodHead: true, http.MethodPost: true, http.MethodDelete: true,
}

// checkCORS validates a CORS configuration: every rule needs origins and
// methods S3 knows, and an origin or header may hold at most one "*".
func checkCORS(cfg *api.CORSConfiguration) error {
	if cfg == nil || len(cfg.CORSRules) == 0 || len(cfg.CORSRules) > maxCORSRules {
		return malformedXML()
	}

	for i := range cfg.CORSRules {
		r := &cfg.CORSRules[i]
		if len(r.AllowedOrigins) == 0 || len(r.AllowedMethods) == 0 || value(r.MaxAgeSeconds) < 0 {
			return malformedXML()
		}

		for _, m := range r.AllowedMethods {
			if !corsMethods[m] {
				return fail(http.StatusBadRequest, "InvalidRequest",
					"Found unsupported HTTP method in CORS config. Unsupported method is "+m)
			}
		}

		for _, o := range r.AllowedOrigins {
			if strings.Count(o, "*") > 1 {
				return fail(http.StatusBadRequest, "InvalidRequest",
					`AllowedOrigin "`+o+`" can not have more than one wildcard.`)
			}
		}

		for _, h := range r.AllowedHeaders {
			if strings.Count(h, "*") > 1 {
				return fail(http.StatusBadRequest, "InvalidRequest",
					`AllowedHeader "`+h+`" can not have more than one wildcard.`)
			}
		}
	}

	return nil
}

func (s *Service) PutBucketCors(ctx context.Context, in *api.PutBucketCorsRequest) (*api.PutBucketCorsOutput, error) {
	if err := checkCORS(in.CORSConfiguration); err != nil {
		return nil, err
	}

	err := s.updateBucket(ctx, in.Bucket, in.ExpectedBucketOwner, func(meta *bucketMeta) {
		meta.CORS = in.CORSConfiguration.CORSRules
	})
	if err != nil {
		return nil, err
	}

	return &api.PutBucketCorsOutput{}, nil
}

func (s *Service) GetBucketCors(ctx context.Context, in *api.GetBucketCorsRequest) (*api.GetBucketCorsOutput, error) {
	meta, err := s.bucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	if len(meta.CORS) == 0 {
		return nil, fail(http.StatusNotFound, "NoSuchCORSConfiguration", "The CORS configuration does not exist")
	}

	return &api.GetBucketCorsOutput{CORSRules: meta.CORS}, nil
}

func (s *Service) DeleteBucketCors(
	ctx context.Context,
	in *api.DeleteBucketCorsRequest,
) (*api.DeleteBucketCorsOutput, error) {
	err := s.updateBucket(ctx, in.Bucket, in.ExpectedBucketOwner, func(meta *bucketMeta) { meta.CORS = nil })
	if err != nil {
		return nil, err
	}

	return &api.DeleteBucketCorsOutput{}, nil
}

// wildcard reports whether s matches pattern, which may hold one "*".
func wildcard(pattern, s string) bool {
	before, after, ok := strings.Cut(pattern, "*")
	if !ok {
		return pattern == s
	}

	return len(s) >= len(before)+len(after) && strings.HasPrefix(s, before) && strings.HasSuffix(s, after)
}

// allows reports whether a rule allows origin to send method with headers.
func allows(r *api.CORSRule, origin, method string, headers []string) bool {
	if !slices.ContainsFunc(r.AllowedOrigins, func(o string) bool { return wildcard(o, origin) }) ||
		!slices.Contains(r.AllowedMethods, method) {
		return false
	}

	for _, h := range headers {
		if !slices.ContainsFunc(r.AllowedHeaders, func(a string) bool {
			return wildcard(strings.ToLower(a), strings.ToLower(h))
		}) {
			return false
		}
	}

	return true
}

// CORS returns the response headers the first rule of bucket's CORS
// configuration that allows the request gives it, or false when none does.
// headers are the ones a preflight asks to send, which actual requests leave
// empty. Preflights are anonymous, so this reads any account's bucket.
func (s *Service) CORS(
	ctx context.Context, bucket, origin, method string, headers []string,
) (allowed map[string]string, ok bool, err error) {
	var meta bucketMeta
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var index bucketIndex
		found, readErr := readJSON(tx, indexPrefix+bucket, &index)
		if readErr != nil || !found {
			return readErr
		}

		_, readErr = readJSON(tx, index.Account+"/"+index.Region+"/s3/"+bucket, &meta)
		return readErr
	})
	if err != nil {
		return nil, false, err
	}

	for i := range meta.CORS {
		r := &meta.CORS[i]
		if !allows(r, origin, method, headers) {
			continue
		}

		out := map[string]string{
			"Access-Control-Allow-Origin":      origin,
			"Access-Control-Allow-Methods":     strings.Join(r.AllowedMethods, ", "),
			"Access-Control-Allow-Credentials": "true",
			"Vary":                             "Origin, Access-Control-Request-Headers, Access-Control-Request-Method",
		}
		if slices.Contains(r.AllowedOrigins, "*") {
			out["Access-Control-Allow-Origin"] = "*"
			delete(out, "Access-Control-Allow-Credentials")
		}

		if len(headers) > 0 {
			out["Access-Control-Allow-Headers"] = strings.ToLower(strings.Join(headers, ", "))
		}

		if len(r.ExposeHeaders) > 0 {
			out["Access-Control-Expose-Headers"] = strings.Join(r.ExposeHeaders, ", ")
		}

		if r.MaxAgeSeconds != nil {
			out["Access-Control-Max-Age"] = strconv.FormatInt(*r.MaxAgeSeconds, 10)
		}

		return out, true, nil
	}

	return nil, false, nil
}
