package acmpca

import (
	"maps"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/acmpca"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
)

const (
	typeRoot            = "ROOT"
	typeSubordinate     = "SUBORDINATE"
	usageGeneral        = "GENERAL_PURPOSE"
	usageShortLived     = "SHORT_LIVED_CERTIFICATE"
	securityDefault     = "FIPS_140_2_LEVEL_3_OR_HIGHER"
	securityFIPSLevel2  = "FIPS_140_2_LEVEL_2_OR_HIGHER"
	securityCCPC        = "CCPC_LEVEL_1_OR_HIGHER"
	ownerSelf           = "SELF"
	ownerOtherAccounts  = "OTHER_ACCOUNTS"
	aclPublicRead       = "PUBLIC_READ"
	aclBucketOwner      = "BUCKET_OWNER_FULL_CONTROL"
	maxPageSize         = 1000
	maxExpirationInDays = 5000
)

var (
	arnPattern        = regexp.MustCompile(`^arn:[\w+=/,.@-]+:acm-pca:[\w+=/,.@-]*:[0-9]*:[\w+=,.@-]+(/[\w+=,.@-]+)*$`)
	tokenPattern      = regexp.MustCompile(`^[\x{0009}\x{000A}\x{000D}\x{0020}-\x{00FF}]*$`)
	printablePattern  = regexp.MustCompile(`^[a-zA-Z0-9'()+,\-.?:/= ]*$`)
	countryPattern    = regexp.MustCompile(`^[A-Za-z]{2}$`)
	cnamePattern      = regexp.MustCompile(`^[-a-zA-Z0-9;/?:@&=+$,%_.!~*()']*$`)
	bucketPattern     = regexp.MustCompile(`^[-a-zA-Z0-9._/]+$`)
	tagPattern        = regexp.MustCompile(`^([\p{L}\p{Z}\p{N}_.:/=+\-@]*)$`)
	authorityTypes    = []string{typeRoot, typeSubordinate}
	usageModes        = []string{usageGeneral, usageShortLived}
	securityStandards = []string{securityFIPSLevel2, securityDefault, securityCCPC}
	owners            = []string{ownerSelf, ownerOtherAccounts}
	objectACLs        = []string{aclPublicRead, aclBucketOwner}
	rsaSigning        = []string{"SHA256WITHRSA", "SHA384WITHRSA", "SHA512WITHRSA"}
	ecdsaSigning      = []string{"SHA256WITHECDSA", "SHA384WITHECDSA", "SHA512WITHECDSA"}
	signingAlgorithms = map[string][]string{
		"RSA_2048":      rsaSigning,
		"RSA_3072":      rsaSigning,
		"RSA_4096":      rsaSigning,
		"EC_prime256v1": ecdsaSigning,
		"EC_secp384r1":  ecdsaSigning,
		"EC_secp521r1":  ecdsaSigning,
		"ML_DSA_44":     {"ML_DSA_44"},
		"ML_DSA_65":     {"ML_DSA_65"},
		"ML_DSA_87":     {"ML_DSA_87"},
		"SM2":           {"SM3WITHSM2"},
	}
)

// subjectField is one X.500 name part with the model's limits on it.
type subjectField struct {
	value   func(*api.ASN1Subject) *string
	pattern *regexp.Regexp
	name    string
	max     int
}

var subjectFields = []subjectField{
	{func(s *api.ASN1Subject) *string { return s.Country }, countryPattern, "Country", 2},
	{func(s *api.ASN1Subject) *string { return s.Organization }, nil, "Organization", 64},
	{func(s *api.ASN1Subject) *string { return s.OrganizationalUnit }, nil, "OrganizationalUnit", 64},
	{
		func(s *api.ASN1Subject) *string { return s.DistinguishedNameQualifier },
		printablePattern,
		"DistinguishedNameQualifier",
		64,
	},
	{func(s *api.ASN1Subject) *string { return s.State }, nil, "State", 128},
	{func(s *api.ASN1Subject) *string { return s.CommonName }, nil, "CommonName", 64},
	{func(s *api.ASN1Subject) *string { return s.SerialNumber }, printablePattern, "SerialNumber", 64},
	{func(s *api.ASN1Subject) *string { return s.Locality }, nil, "Locality", 128},
	{func(s *api.ASN1Subject) *string { return s.Title }, nil, "Title", 64},
	{func(s *api.ASN1Subject) *string { return s.Surname }, nil, "Surname", 40},
	{func(s *api.ASN1Subject) *string { return s.GivenName }, nil, "GivenName", 16},
	{func(s *api.ASN1Subject) *string { return s.Initials }, nil, "Initials", 5},
	{func(s *api.ASN1Subject) *string { return s.Pseudonym }, nil, "Pseudonym", 128},
	{func(s *api.ASN1Subject) *string { return s.GenerationQualifier }, nil, "GenerationQualifier", 3},
}

// parseARN returns the ID of the authority an ARN names in scope. An ARN that is not an authority ARN is invalid;
// one for another account or Region names an authority this scope cannot see.
func parseARN(scope request.Scope, arn *string) (string, error) {
	value := common.Value(arn)
	if n := len(value); n < 5 || n > 200 || !arnPattern.MatchString(value) {
		return "", invalidArn("%q is not a valid certificate authority ARN", value)
	}

	parts := strings.SplitN(value, ":", 6)
	kind, id, ok := strings.Cut(parts[5], "/")
	if kind != authorityKind || !ok || id == "" || strings.Contains(id, "/") {
		return "", invalidArn("%q is not a valid certificate authority ARN", value)
	}

	if value != authorityARN(scope, id) {
		return "", notFound("Certificate authority %s was not found", value)
	}

	return id, nil
}

func checkEnum(name string, value *string, allowed []string) error {
	if !slices.Contains(allowed, common.Value(value)) {
		return invalidArgs("%s must be one of %s", name, strings.Join(allowed, ", "))
	}

	return nil
}

func checkLength(name, value string, maxLength int) error {
	if utf8.RuneCountInString(value) > maxLength {
		return invalidArgs("%s must be at most %d characters long", name, maxLength)
	}

	return nil
}

// checkConfiguration validates the key, signing algorithm and subject. The signing algorithm family must match the
// key family, ML-DSA keys sign with the algorithm of their own name, and SM2 exists in the China Regions only.
func checkConfiguration(cfg *api.CertificateAuthorityConfiguration, region string) error {
	if cfg == nil {
		return invalidArgs("CertificateAuthorityConfiguration is required")
	}

	allowed, known := signingAlgorithms[common.Value(cfg.KeyAlgorithm)]
	if !known {
		return invalidArgs("KeyAlgorithm %q is not valid", common.Value(cfg.KeyAlgorithm))
	}

	if common.Value(cfg.KeyAlgorithm) == "SM2" && !strings.HasPrefix(region, "cn-") {
		return invalidArgs("KeyAlgorithm SM2 is available in the China Regions only")
	}

	signing := common.Value(cfg.SigningAlgorithm)
	if !slices.ContainsFunc(slices.Collect(maps.Values(signingAlgorithms)), func(list []string) bool {
		return slices.Contains(list, signing)
	}) {
		return invalidArgs("SigningAlgorithm %q is not valid", signing)
	}

	if !slices.Contains(allowed, signing) {
		return invalidArgs("SigningAlgorithm %s does not match KeyAlgorithm %s, which signs with %s",
			signing, *cfg.KeyAlgorithm, strings.Join(allowed, ", "))
	}

	if cfg.Subject == nil {
		return invalidArgs("Subject is required")
	}

	for _, f := range subjectFields {
		value := f.value(cfg.Subject)
		if value == nil {
			continue
		}

		if err := checkLength("Subject."+f.name, *value, f.max); err != nil {
			return err
		}

		if f.pattern != nil && !f.pattern.MatchString(*value) {
			return invalidArgs("Subject.%s does not match %s", f.name, f.pattern)
		}
	}

	return nil
}

func checkCname(name string, value *string) error {
	if value == nil {
		return nil
	}

	if err := checkLength(name, *value, 253); err != nil {
		return err
	}

	lower := strings.ToLower(*value)
	hasProtocol := strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
	if !cnamePattern.MatchString(*value) || hasProtocol {
		return invalidArgs("%s must follow RFC 2396 and must not include a protocol prefix", name)
	}

	return nil
}

// checkRevocation validates the CRL and OCSP blocks present in rev. A disabled block may carry nothing but
// Enabled=false, and an enabled CRL names the S3 bucket it is written to.
func checkRevocation(rev *api.RevocationConfiguration) error {
	if rev == nil {
		return nil
	}

	if rev.CrlConfiguration != nil {
		if err := checkCRL(rev.CrlConfiguration); err != nil {
			return err
		}
	}

	if rev.OcspConfiguration != nil {
		return checkOCSP(rev.OcspConfiguration)
	}

	return nil
}

func checkCRL(crl *api.CrlConfiguration) error {
	if crl.Enabled == nil {
		return invalidArgs("CrlConfiguration.Enabled is required")
	}

	if !*crl.Enabled {
		if crl.CustomCname != nil || crl.ExpirationInDays != nil || crl.S3BucketName != nil || crl.S3ObjectAcl != nil {
			return invalidArgs("A disabled CrlConfiguration must contain only Enabled=false")
		}

		return nil
	}

	if days := crl.ExpirationInDays; days != nil && (*days < 1 || *days > maxExpirationInDays) {
		return invalidArgs("CrlConfiguration.ExpirationInDays must be from 1 to %d", maxExpirationInDays)
	}

	if err := checkCname("CrlConfiguration.CustomCname", crl.CustomCname); err != nil {
		return err
	}

	bucket := common.Value(crl.S3BucketName)
	if n := len(bucket); n < 3 || n > 255 || !bucketPattern.MatchString(bucket) {
		return invalidArgs("An enabled CrlConfiguration requires an S3BucketName of 3 to 255 characters")
	}

	if crl.S3ObjectAcl != nil {
		return checkEnum("CrlConfiguration.S3ObjectAcl", crl.S3ObjectAcl, objectACLs)
	}

	return nil
}

func checkOCSP(ocsp *api.OcspConfiguration) error {
	if ocsp.Enabled == nil {
		return invalidArgs("OcspConfiguration.Enabled is required")
	}

	if !*ocsp.Enabled && ocsp.OcspCustomCname != nil {
		return invalidArgs("A disabled OcspConfiguration must contain only Enabled=false")
	}

	return checkCname("OcspConfiguration.OcspCustomCname", ocsp.OcspCustomCname)
}

// tagMap returns tags as a map. A tag needs a key of 1 to 128 characters and has an optional value of up to 256, and
// both use the characters the model allows; a repeated key keeps its last value.
func tagMap(tags []api.Tag) (map[string]string, error) {
	out, err := tagging.Map(tags, func(t api.Tag) (*string, *string) { return t.Key, t.Value }, nil, nil)
	if err != nil {
		return nil, err
	}

	for _, t := range tags {
		if common.Value(t.Key) == "" {
			return nil, invalidTag("Tag keys must be 1 to 128 characters long")
		}
	}

	for k, v := range out {
		if !tagPattern.MatchString(k) || !tagPattern.MatchString(v) {
			return nil, invalidTag("Tag %q contains characters that are not allowed", k)
		}
	}

	return out, nil
}
