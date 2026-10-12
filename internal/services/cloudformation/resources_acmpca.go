package cloudformation

import (
	"context"
	"reflect"
)

const (
	acmpcaAuthorityType = "AWS::ACMPCA::CertificateAuthority"
	memberAuthorityArn  = "CertificateAuthorityArn"
	blockCrl            = "CrlConfiguration"
	blockOcsp           = "OcspConfiguration"
)

// revocationUpdate is the RevocationConfiguration to send when it goes from old to p: a block the template no longer
// declares is disabled, as an absent block is in a new authority. It is nil when nothing changed.
func revocationUpdate(old, p props) map[string]any {
	oldRev, _ := old["RevocationConfiguration"].(map[string]any)
	newRev, _ := p["RevocationConfiguration"].(map[string]any)
	if reflect.DeepEqual(oldRev, newRev) {
		return nil
	}

	out := map[string]any{}
	for _, block := range []string{blockCrl, blockOcsp} {
		switch {
		case newRev[block] != nil:
			out[block] = newRev[block]
		case oldRev[block] != nil:
			out[block] = map[string]any{memberEnabled: false}
		}
	}

	return out
}

// retagAuthority moves an authority's tags from old's Tags property to p's. Its untag operation names the tags to
// remove by key, as tags without a value, rather than listing keys.
func retagAuthority(ctx context.Context, c *client, arn string, old, p props) error {
	set, removed := tagChanges(old.tags(), p.tags())
	if len(removed) > 0 {
		keys := make([]any, len(removed))
		for i, k := range removed {
			keys[i] = map[string]any{memberKey: k}
		}

		params := map[string]any{memberAuthorityArn: arn, memberTags: keys}
		if _, err := c.call(ctx, "acmpca", "UntagCertificateAuthority", params); err != nil {
			return err
		}
	}

	if len(set) == 0 {
		return nil
	}

	params := map[string]any{memberAuthorityArn: arn, memberTags: tagList(set, memberKey, memberValue)}
	_, err := c.call(ctx, "acmpca", "TagCertificateAuthority", params)

	return err
}

// Only the revocation configuration and tags change in place; every other property is fixed when the authority is
// created, so changing one replaces it. The CertificateSigningRequest attribute is not listed, because no
// certificate signing request is produced, so a template that reads it fails validation instead of getting an
// empty value.
func init() {
	register(acmpcaAuthorityType, &resourceType{
		properties: []string{
			memberType,
			"KeyAlgorithm",
			"SigningAlgorithm",
			"Subject",
			"RevocationConfiguration",
			"KeyStorageSecurityStandard",
			"UsageMode",
			memberTags,
		},
		replace: []string{
			memberType,
			"KeyAlgorithm",
			"SigningAlgorithm",
			"Subject",
			"KeyStorageSecurityStandard",
			"UsageMode",
		},
		attributes: []string{memberArn},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := map[string]any{
				"CertificateAuthorityConfiguration": pick(
					map[string]any{},
					p,
					"KeyAlgorithm",
					"SigningAlgorithm",
					"Subject",
				),
				"CertificateAuthorityType": p[memberType],
			}
			pick(params, p, "RevocationConfiguration", "KeyStorageSecurityStandard", "UsageMode", memberTags)

			res, err := c.call(ctx, "acmpca", "CreateCertificateAuthority", params)
			if err != nil {
				return "", nil, err
			}

			arn := outText(res, memberAuthorityArn)

			return arn, map[string]any{memberArn: arn}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if rev := revocationUpdate(r.Properties, p); rev != nil {
				params := map[string]any{memberAuthorityArn: r.Physical, "RevocationConfiguration": rev}
				if _, err := c.call(ctx, "acmpca", "UpdateCertificateAuthority", params); err != nil {
					return nil, err
				}
			}

			if err := retagAuthority(ctx, c, r.Physical, r.Properties, p); err != nil {
				return nil, err
			}

			return r.Attributes, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(
				ctx,
				"acmpca",
				"DeleteCertificateAuthority",
				map[string]any{memberAuthorityArn: r.Physical},
			)
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})
}
