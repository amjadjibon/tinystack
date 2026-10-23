package amplify

import (
	api "github.com/amjadjibon/tinystack/internal/aws/generated/amplify"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
)

// The generated Validate methods enforce the model's constraint traits; these are the rules they cannot express.

func checkTags(tags map[string]string) error {
	if err := tagging.CheckSet(tags); err != nil {
		return bad("%s", err)
	}

	return nil
}

func checkCertificate(c *api.CertificateSettings) error {
	if c != nil && common.Value(c.Type) == certificateCustom && c.CustomCertificateArn == nil {
		return bad("customCertificateArn is required for CUSTOM certificates.")
	}

	return nil
}
