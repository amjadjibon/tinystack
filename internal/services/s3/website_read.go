package s3

import (
	"cmp"
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	iampolicy "github.com/amjadjibon/tinystack/internal/services/iam/policy"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// WebsiteRequest describes a public GET or HEAD on an S3 website endpoint.
type WebsiteRequest struct {
	Bucket, Region, Key string
	Host, Scheme        string
	RemoteIP            string
	Head                bool
}

// WebsiteResponse owns Body, when present. The caller closes it after streaming.
type WebsiteResponse struct {
	LastModified time.Time
	Body         io.ReadCloser
	Location     string
	ContentType  string
	ETag         string
	Length       int64
	Status       int
}

func validWebsiteRedirect(value *string) error {
	if value == nil {
		return nil
	}

	parsed, err := url.Parse(*value)
	if err == nil && len(*value) <= 2048 && !strings.Contains(*value, "\\") {
		if (parsed.Scheme == "http" || parsed.Scheme == websiteHTTPS) && parsed.Host != "" && parsed.User == nil {
			return nil
		}

		if parsed.Scheme == "" && strings.HasPrefix(*value, "/") && !strings.HasPrefix(*value, "//") {
			return nil
		}
	}

	return invalidArgument("Website redirect location must be an absolute path or an HTTP(S) URL")
}

type websiteBucket struct {
	req          *WebsiteRequest
	now          time.Time
	policy, base string
}

func (b websiteBucket) permits(key string) bool {
	principal := &request.Principal{Kind: request.PrincipalAnonymous}
	doc := iampolicy.Document{JSON: b.policy}
	values := map[string][]string{
		"aws:SourceIp":        {b.req.RemoteIP},
		"aws:SecureTransport": {strconv.FormatBool(b.req.Scheme == websiteHTTPS)},
	}
	decision := iampolicy.EvaluateResource(doc, "s3:GetObject", bucketARNPrefix+b.req.Bucket+"/"+key,
		iampolicy.Context{Values: values}, principal)
	return decision.Allowed && !decision.ExplicitDeny && len(decision.Unsupported) == 0
}

func (b websiteBucket) lookup(tx storage.ReadTx, key string) (*object, int, error) {
	if !b.permits(key) {
		return nil, http.StatusForbidden, nil
	}

	obj, found, err := storage.Read[object](tx, b.base+"/o/"+key)
	if err != nil {
		return nil, 0, err
	}

	if !found {
		return nil, http.StatusNotFound, nil
	}

	if obj.CustomerKeyMD5 != "" || !obj.readable(b.now) {
		return nil, http.StatusForbidden, nil
	}

	return obj, http.StatusOK, nil
}

func (b websiteBucket) redirect(host, scheme, key string, status int) *WebsiteResponse {
	u := &url.URL{
		Host: cmp.Or(host, b.req.Host), Scheme: cmp.Or(scheme, b.req.Scheme), Path: "/" + key,
	}

	return &WebsiteResponse{Status: status, Location: u.String()}
}

func (b websiteBucket) routing(rules []api.RoutingRule, status int) *WebsiteResponse {
	for _, rule := range rules {
		key := b.req.Key
		prefix := ""
		if c := rule.Condition; c != nil {
			prefix = common.Value(c.KeyPrefixEquals)
			if !strings.HasPrefix(key, prefix) ||
				(c.HttpErrorCodeReturnedEquals != nil &&
					*c.HttpErrorCodeReturnedEquals != strconv.Itoa(status)) {
				continue
			}
		}

		r := rule.Redirect
		if r == nil {
			continue
		}

		switch {
		case r.ReplaceKeyWith != nil:
			key = *r.ReplaceKeyWith
		case r.ReplaceKeyPrefixWith != nil:
			key = *r.ReplaceKeyPrefixWith + strings.TrimPrefix(key, prefix)
		}

		code, _ := strconv.Atoi(common.Value(r.HttpRedirectCode))
		return b.redirect(
			common.Value(r.HostName),
			common.Value(r.Protocol),
			key,
			cmp.Or(code, http.StatusMovedPermanently),
		)
	}

	return nil
}

func (b websiteBucket) resolve(tx storage.ReadTx, cfg *api.WebsiteConfiguration) (
	*WebsiteResponse, *object, string, error,
) {
	if r := cfg.RedirectAllRequestsTo; r != nil {
		if !b.permits(b.req.Key) {
			return &WebsiteResponse{Status: http.StatusForbidden}, nil, "", nil
		}

		redirect := b.redirect(
			common.Value(r.HostName),
			common.Value(r.Protocol),
			b.req.Key,
			http.StatusMovedPermanently,
		)
		return redirect, nil, "", nil
	}

	suffix := common.Value(cfg.IndexDocument.Suffix)
	key, trailingSlash := b.req.Key, b.req.Key == "" || strings.HasSuffix(b.req.Key, "/")
	if trailingSlash {
		key += suffix
	}

	obj, status, err := b.lookup(tx, key)
	if err != nil {
		return nil, nil, "", err
	}

	if status != http.StatusForbidden {
		if redirect := b.routing(cfg.RoutingRules, status); redirect != nil {
			return redirect, nil, "", nil
		}
	}

	if status == http.StatusNotFound && !trailingSlash {
		dir := key + "/"
		index, _, lookupErr := b.lookup(tx, dir+suffix)
		if lookupErr != nil {
			return nil, nil, "", lookupErr
		}

		if index != nil {
			return b.redirect("", "", dir, http.StatusFound), nil, "", nil
		}
	}

	if obj != nil && obj.WebsiteRedirect != "" {
		return &WebsiteResponse{Status: http.StatusMovedPermanently, Location: obj.WebsiteRedirect}, nil, "", nil
	}

	if obj == nil && cfg.ErrorDocument != nil {
		key = common.Value(cfg.ErrorDocument.Key)
		obj, _, err = b.lookup(tx, key)
	}

	return &WebsiteResponse{Status: status}, obj, key, err
}

// Website looks up a public object and streams its immutable body after the read transaction.
func (s *Service) Website(ctx context.Context, in *WebsiteRequest) (*WebsiteResponse, error) {
	var out *WebsiteResponse
	var data []byte
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		index, found, err := storage.Read[bucketIndex](tx, indexPrefix+in.Bucket)
		if err != nil {
			return err
		}

		if !found {
			out = &WebsiteResponse{Status: http.StatusNotFound}
			return nil
		}

		if index.Region != in.Region {
			out = &WebsiteResponse{Status: http.StatusBadRequest}
			return nil
		}

		b := websiteBucket{req: in, now: s.now(), base: index.Account + "/" + index.Region + "/s3/" + in.Bucket}
		meta, _, err := storage.Read[bucketMeta](tx, b.base)
		if err != nil {
			return err
		}

		switch {
		case meta.Website == nil:
			out = &WebsiteResponse{Status: http.StatusNotFound}
			return nil
		case meta.Payer == "Requester":
			out = &WebsiteResponse{Status: http.StatusForbidden}
			return nil
		}

		scope := request.Scope{AccountID: index.Account, Region: index.Region}
		b.policy, err = restrictPublicPolicy(tx, scope, nil, meta.Policy, meta.PublicAccessBlock)
		if err != nil {
			return err
		}

		response, obj, key, err := b.resolve(tx, meta.Website)
		if err != nil {
			return err
		}

		out = response
		if obj != nil {
			out.ContentType, out.ETag, out.Length = cmp.Or(obj.ContentType, defaultType), obj.ETag, obj.Size
			out.LastModified = time.UnixMilli(obj.LastModified).UTC()
			if !in.Head {
				data, _ = tx.Get(b.base + "/db/" + key)
				if blob, ok := storage.BlobReference(data); ok {
					s.blobs.Pin(blob)
				}
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	if out.ContentType != "" && !in.Head {
		body, err := s.openBody(data)
		if err != nil {
			s.releaseBody(data)
			return nil, err
		}

		out.Body = &websiteBody{bodyReader: body, release: func() { s.releaseBody(data) }}
	}

	return out, nil
}

type websiteBody struct {
	bodyReader
	release func()
}

func (r *websiteBody) Close() error {
	defer r.release()
	return r.bodyReader.Close()
}
