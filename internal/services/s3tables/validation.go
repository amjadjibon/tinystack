package s3tables

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	bucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)
	namePattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9_]{0,254}$`)
)

// ValidateBucketName enforces customer creation rules, not ordinary S3 rules.
func ValidateBucketName(n string) error {
	if !bucketPattern.MatchString(n) {
		return invalid("Table bucket name requires 3-63 lowercase letters, digits or hyphens, with alphanumeric ends")
	}
	for _, p := range []string{"xn--", "sthree-", "amzn-s3-demo-", "aws"} {
		if strings.HasPrefix(n, p) {
			return invalid("Table bucket name uses a reserved prefix")
		}
	}
	for _, p := range []string{"-s3alias", "--ol-s3", "--x-s3", "--table-s3"} {
		if strings.HasSuffix(n, p) {
			return invalid("Table bucket name uses a reserved suffix")
		}
	}
	return nil
}

// ValidateNamespaceName admits a single customer namespace component.
func ValidateNamespaceName(n string) error {
	if !namePattern.MatchString(n) || strings.HasPrefix(n, "aws") {
		return invalid(
			"Namespace requires 1-255 lowercase letters, digits or underscores, an alphanumeric start and no aws prefix",
		)
	}
	return nil
}

// ValidateTableName enforces the table's own name rules (aws is allowed).
func ValidateTableName(n string) error {
	if !namePattern.MatchString(n) {
		return invalid("Table name requires 1-255 lowercase letters, digits or underscores and an alphanumeric start")
	}
	return nil
}

func checkKey(k string) error {
	if !tagText(k, 1, 128) || strings.HasPrefix(strings.ToLower(k), "aws:") {
		return invalid("Tag key requires 1-128 permitted Unicode characters and cannot use the aws: prefix")
	}
	return nil
}

func tagText(s string, minimum, maximum int) bool {
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) < minimum || utf8.RuneCountInString(s) > maximum {
		return false
	}
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsNumber(r) && !unicode.IsSpace(r) && !strings.ContainsRune("_.:/=+@-", r) {
			return false
		}
	}
	return true
}

// ValidateTags checks the final tag set, so merge overflow fails before writes.
func ValidateTags(tags map[string]string) error {
	if len(tags) > 50 {
		return invalid("A resource can have at most 50 user-defined tags")
	}
	for k, v := range tags {
		if e := checkKey(k); e != nil {
			return e
		}
		if !tagText(v, 0, 256) {
			return invalid("Tag value requires 0-256 permitted Unicode characters")
		}
	}
	return nil
}

func checkToken(p *string) error {
	if p != nil && (!utf8.ValidString(*p) || utf8.RuneCountInString(*p) < 1 || utf8.RuneCountInString(*p) > 2048) {
		return invalid("versionToken requires 1-2048 Unicode characters")
	}
	return nil
}
