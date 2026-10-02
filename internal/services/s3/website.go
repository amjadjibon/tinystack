package s3

import (
	"context"
	"net/http"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
)

// checkWebsite validates a website configuration: it redirects every request
// to another host, or it names an index document, with optional routing rules.
func checkWebsite(cfg *api.WebsiteConfiguration) error {
	switch {
	case cfg == nil:
		return malformedXML()
	case cfg.RedirectAllRequestsTo != nil:
		if cfg.IndexDocument != nil || cfg.ErrorDocument != nil || len(cfg.RoutingRules) > 0 ||
			value(cfg.RedirectAllRequestsTo.HostName) == "" {
			return invalidArgument("RedirectAllRequestsTo cannot be provided in conjunction with other Routing Rules.")
		}

		return nil
	case cfg.IndexDocument == nil:
		return invalidArgument("A value for IndexDocument Suffix must be provided if RedirectAllRequestsTo is empty")
	}

	if suffix := value(cfg.IndexDocument.Suffix); suffix == "" || strings.Contains(suffix, "/") {
		return invalidArgument("The IndexDocument Suffix is not well formed")
	}

	for _, r := range cfg.RoutingRules {
		if r.Redirect == nil || (r.Redirect.ReplaceKeyPrefixWith != nil && r.Redirect.ReplaceKeyWith != nil) {
			return malformedXML()
		}
	}

	return nil
}

// PutBucketWebsite stores the bucket's website configuration. TinyStack has
// no website endpoint, so nothing serves it.
func (s *Service) PutBucketWebsite(
	ctx context.Context,
	in *api.PutBucketWebsiteRequest,
) (*api.PutBucketWebsiteOutput, error) {
	if err := checkWebsite(in.WebsiteConfiguration); err != nil {
		return nil, err
	}

	err := s.updateBucket(ctx, in.Bucket, in.ExpectedBucketOwner, func(meta *bucketMeta) {
		meta.Website = in.WebsiteConfiguration
	})
	if err != nil {
		return nil, err
	}

	return &api.PutBucketWebsiteOutput{}, nil
}

func (s *Service) GetBucketWebsite(
	ctx context.Context,
	in *api.GetBucketWebsiteRequest,
) (*api.GetBucketWebsiteOutput, error) {
	meta, err := s.bucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	w := meta.Website
	if w == nil {
		return nil, fail(http.StatusNotFound, "NoSuchWebsiteConfiguration",
			"The specified bucket does not have a website configuration")
	}

	return &api.GetBucketWebsiteOutput{
		RedirectAllRequestsTo: w.RedirectAllRequestsTo, IndexDocument: w.IndexDocument,
		ErrorDocument: w.ErrorDocument, RoutingRules: w.RoutingRules,
	}, nil
}

func (s *Service) DeleteBucketWebsite(
	ctx context.Context,
	in *api.DeleteBucketWebsiteRequest,
) (*api.DeleteBucketWebsiteOutput, error) {
	err := s.updateBucket(ctx, in.Bucket, in.ExpectedBucketOwner, func(meta *bucketMeta) { meta.Website = nil })
	if err != nil {
		return nil, err
	}

	return &api.DeleteBucketWebsiteOutput{}, nil
}
