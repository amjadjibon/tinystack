package efs

import (
	"math"
	"regexp"
	"strings"
	"unicode/utf8"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/efs"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
)

var (
	keyID = regexp.MustCompile(
		`^([a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}|mrk-[a-fA-F0-9]{32})$`,
	)
	alias         = regexp.MustCompile(`^alias/[A-Za-z0-9/_-]{1,250}$`)
	permissions   = regexp.MustCompile(`^[0-7]{3,4}$`)
	tagCharacters = regexp.MustCompile(`^[\p{L}\p{Z}\p{N}_.:/=+\-@]*$`)
	subnetID      = regexp.MustCompile(`^subnet-[0-9a-f]{8,40}$`)
	groupID       = regexp.MustCompile(`^sg-[0-9a-f]{8,40}$`)
)

func tokenOK(p *string) error {
	if p == nil || len(*p) < 1 || len(*p) > 64 {
		return invalid("Creation/client token requires 1-64 ASCII characters")
	}
	for _, r := range *p {
		if r < 32 || r > 126 {
			return invalid("Creation/client token requires printable ASCII characters")
		}
	}
	return nil
}

func kmsOK(scope request.Scope, value *string, encrypted bool) error {
	if value == nil {
		return nil
	}
	if !encrypted {
		return invalid("KmsKeyId requires Encrypted=true")
	}
	key := *value
	if strings.HasPrefix(key, "arn:") {
		prefix := fmtKMSPrefix(scope)
		if !strings.HasPrefix(key, prefix) {
			return invalid("KMS ARN must belong to this account and Region")
		}
		key = strings.TrimPrefix(key, prefix)
		if !strings.HasPrefix(key, "key/") && !strings.HasPrefix(key, "alias/") {
			return invalid("Invalid KMS ARN resource")
		}
		key = strings.TrimPrefix(key, "key/")
	}
	if !keyID.MatchString(key) && !alias.MatchString(key) {
		return invalid("Invalid KMS key ID or alias")
	}
	return nil
}

func fmtKMSPrefix(scope request.Scope) string {
	return "arn:aws:kms:" + scope.Region + ":" + scope.AccountID + ":"
}

// The pinned model documents 1-3414 MiBps. TinyStack uses that finite metadata
// ceiling uniformly; no regional capacity, quota negotiation or cooldown runs.
func throughputOK(performance, mode string, amount *float64) error {
	if performance != generalPurpose && performance != maxIO {
		return invalid("Invalid PerformanceMode")
	}
	if mode != bursting && mode != provisioned && mode != elastic {
		return invalid("Invalid ThroughputMode")
	}
	if performance == maxIO && mode == elastic {
		return invalid("maxIO does not support elastic throughput")
	}
	if mode == provisioned {
		if amount == nil || math.IsNaN(*amount) || math.IsInf(*amount, 0) || *amount < 1 || *amount > 3414 {
			return invalid("ProvisionedThroughputInMibps must be between 1 and 3414")
		}
	} else if amount != nil {
		return invalid("ProvisionedThroughputInMibps requires provisioned throughput")
	}
	return nil
}

func tagsMap(tags []api.Tag) (map[string]string, error) {
	out, err := tagging.Map(tags, func(t api.Tag) (*string, *string) { return t.Key, t.Value },
		invalid("Tags require Key and Value"), func(string) error { return invalid("Duplicate tag key") })
	if err != nil {
		return nil, err
	}
	if err = tagging.CheckSet(out); err != nil {
		return nil, invalid(err.Error())
	}
	for k, v := range out {
		if !tagCharacters.MatchString(k) || !tagCharacters.MatchString(v) {
			return nil, invalid("Invalid tag characters")
		}
	}
	return out, nil
}

func tagList(tags map[string]string) []api.Tag {
	return tagging.List(tags, func(k, v string) api.Tag { return api.Tag{Key: new(k), Value: new(v)} })
}

func tagName(tags map[string]string) *string {
	if name, ok := tags["Name"]; ok {
		return new(name)
	}
	return nil
}

func posixID(p *int64) error {
	if p == nil || *p < 0 || *p > 4294967295 {
		return invalid("POSIX IDs require integers between 0 and 4294967295")
	}
	return nil
}

func accessOK(in *api.CreateAccessPointRequest) error {
	if err := tokenOK(in.ClientToken); err != nil {
		return err
	}
	if p := in.PosixUser; p != nil {
		if err := posixID(p.Uid); err != nil {
			return err
		}
		if err := posixID(p.Gid); err != nil {
			return err
		}
		if len(p.SecondaryGids) > 16 {
			return invalid("At most 16 secondary groups are supported")
		}
		for _, id := range p.SecondaryGids {
			if err := posixID(new(id)); err != nil {
				return err
			}
		}
	}
	if r := in.RootDirectory; r != nil {
		if r.Path != nil {
			path := *r.Path
			if len(path) < 1 || utf8.RuneCountInString(path) > 100 || !strings.HasPrefix(path, "/") ||
				strings.ContainsAny(path, "$#<>;`|&?{}^*\n\r\x00") {
				return invalid("Invalid RootDirectory.Path")
			}
			if path != "/" {
				parts := strings.Split(path[1:], "/")
				if len(parts) > 4 {
					return invalid("Root path has at most four subdirectories")
				}
				for _, part := range parts {
					if part == "" || strings.HasPrefix(part, ".") {
						return invalid("Invalid root path component")
					}
				}
			}
		}
		if c := r.CreationInfo; c != nil {
			if err := posixID(c.OwnerUid); err != nil {
				return err
			}
			if err := posixID(c.OwnerGid); err != nil {
				return err
			}
			if !permissions.MatchString(common.Value(c.Permissions)) {
				return invalid("Permissions must contain 3-4 octal digits")
			}
		}
	}
	return nil
}
