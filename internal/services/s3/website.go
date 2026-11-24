package s3

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/common"
)

const websiteHTTPS = "https"

// checkWebsite validates a website configuration: it redirects every request
// to another host, or it names an index document, with optional routing rules.
func checkWebsite(cfg *api.WebsiteConfiguration) error {
	switch {
	case cfg == nil:
		return malformedXML()
	case cfg.RedirectAllRequestsTo != nil:
		if cfg.IndexDocument != nil || cfg.ErrorDocument != nil || len(cfg.RoutingRules) > 0 ||
			common.Value(cfg.RedirectAllRequestsTo.HostName) == "" {
			return invalidArgument("RedirectAllRequestsTo cannot be provided in conjunction with other Routing Rules.")
		}

		return validateWebsiteDestination(cfg.RedirectAllRequestsTo.HostName, cfg.RedirectAllRequestsTo.Protocol)
	case cfg.IndexDocument == nil:
		return invalidArgument("A value for IndexDocument Suffix must be provided if RedirectAllRequestsTo is empty")
	}

	if suffix := common.Value(cfg.IndexDocument.Suffix); suffix == "" || strings.Contains(suffix, "/") {
		return invalidArgument("The IndexDocument Suffix is not well formed")
	}

	if len(cfg.RoutingRules) > 50 {
		return invalidArgument("A website configuration can have at most 50 routing rules")
	}

	if cfg.ErrorDocument != nil {
		if err := validKey(cfg.ErrorDocument.Key); err != nil {
			return err
		}
	}

	for _, r := range cfg.RoutingRules {
		if r.Redirect == nil || (r.Redirect.ReplaceKeyPrefixWith != nil && r.Redirect.ReplaceKeyWith != nil) {
			return malformedXML()
		}
		if err := validateWebsiteDestination(r.Redirect.HostName, r.Redirect.Protocol); err != nil {
			return err
		}

		if code := r.Redirect.HttpRedirectCode; code != nil {
			n, err := strconv.Atoi(*code)
			if err != nil || n < 300 || n >= 400 {
				return malformedXML()
			}
		}

		if c := r.Condition; c != nil && c.HttpErrorCodeReturnedEquals != nil {
			n, err := strconv.Atoi(*c.HttpErrorCodeReturnedEquals)
			if err != nil || n < 400 || n >= 600 {
				return malformedXML()
			}
		}
	}

	return nil
}

func validateWebsiteDestination(host, protocol *string) error {
	if protocol != nil && *protocol != "http" && *protocol != websiteHTTPS {
		return malformedXML()
	}

	if host != nil {
		u, err := url.Parse("http://" + *host)
		if err != nil || u.Hostname() == "" || u.Host != *host || u.User != nil || strings.Contains(*host, "\\") {
			return malformedXML()
		}
	}

	return nil
}

// PutBucketWebsite stores the configuration for the bucket's website endpoint.
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
