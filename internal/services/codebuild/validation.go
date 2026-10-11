package codebuild

import (
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/codebuild"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
)

const (
	noSource     = "NO_SOURCE"
	noArtifacts  = "NO_ARTIFACTS"
	s3Type       = "S3"
	githubSource = "GITHUB"
	commitSource = "CODECOMMIT"
	none         = "NONE"
	plaintext    = "PLAINTEXT"
)

var (
	namePattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{1,149}$`)
	projectARNPattern = regexp.MustCompile(
		`^arn:aws:codebuild:([a-z]{2}(?:-[a-z0-9]+)+-[0-9]+):([0-9]{12}):project/([A-Za-z0-9][A-Za-z0-9_-]{1,149})$`,
	)
	rolePattern = regexp.MustCompile(
		`^arn:aws:iam::([0-9]{12}):role/((?:[A-Za-z0-9_+=,.@-]+/)*[A-Za-z0-9_+=,.@-]{1,64})$`,
	)
	bucketPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,255}$`)
	tagPattern     = regexp.MustCompile(`^[\p{L}\p{Z}\p{N}_.:/=@+\-]*$`)
	imagePattern   = regexp.MustCompile(
		`^[A-Za-z0-9][A-Za-z0-9._/-]*(?::[A-Za-z0-9._-]{1,128}|@sha256:[a-f0-9]{64})$`,
	)
	repoPattern      = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	parameterPattern = regexp.MustCompile(`^/?[A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)*$`)
	secretPattern    = regexp.MustCompile(`^[A-Za-z0-9/_+=.@-]+(?::[A-Za-z0-9_./+=@-]*){0,3}$`)
)

func nameOK(v *string) error {
	if v == nil || !namePattern.MatchString(*v) {
		return invalid(
			"Project name requires 2-150 letters, digits, hyphens or underscores, starting with a letter or digit",
		)
	}
	return nil
}

func projectIdentifier(scope request.Scope, v string) (name string, local bool, err error) {
	if strings.HasPrefix(v, "arn:") {
		m := projectARNPattern.FindStringSubmatch(v)
		if m == nil {
			return "", false, invalid("Invalid project ARN")
		}
		return m[3], m[1] == scope.Region && m[2] == scope.AccountID, nil
	}
	if e := nameOK(new(v)); e != nil {
		return "", false, e
	}
	return v, true, nil
}

func checkString(v *string, minimum, maximum int, label string) error {
	if v == nil || utf8.RuneCountInString(*v) < minimum || utf8.RuneCountInString(*v) > maximum ||
		strings.ContainsRune(*v, 0) {
		return invalid(label + " is missing or outside the supported length")
	}
	return nil
}

func bucketOK(v string) bool {
	if !bucketPattern.MatchString(v) || strings.Contains(v, "..") || strings.Contains(v, ".-") ||
		strings.Contains(v, "-.") {
		return false
	}
	return !regexpIP.MatchString(v) && !strings.HasPrefix(v, "xn--") && !strings.HasSuffix(v, "-s3alias") &&
		!strings.HasSuffix(v, "--ol-s3") &&
		!strings.HasSuffix(v, ".mrap") &&
		!strings.HasSuffix(v, "--x-s3") &&
		!strings.HasSuffix(v, "--table-s3")
}

var regexpIP = regexp.MustCompile(`^\d+\.\d+\.\d+\.\d+$`)

func objectPathOK(v string) bool {
	if len(v) > 1024 || strings.ContainsAny(v, "\x00\r\n") || strings.HasPrefix(v, "arn:") ||
		strings.Contains(v, "://") {
		return false
	}
	for _, p := range strings.Split(v, "/") {
		if p == ".." || p == "." {
			return false
		}
	}
	return true
}

func validate(scope request.Scope, p *api.Project) error {
	if e := nameOK(p.Name); e != nil {
		return e
	}
	if p.Description != nil {
		if e := checkString(p.Description, 0, 255, "Description"); e != nil {
			return e
		}
	}
	if p.ServiceRole == nil {
		return invalid("ServiceRole is required")
	}
	role := rolePattern.FindStringSubmatch(*p.ServiceRole)
	if len(role) != 3 || role[1] != scope.AccountID || len(*p.ServiceRole) > 2048 ||
		strings.Contains("/"+rolePath(role)+"/", "/../") ||
		strings.Contains("/"+rolePath(role)+"/", "/./") {
		return invalid("ServiceRole requires a caller-account commercial IAM role ARN")
	}
	if e := sourceOK(scope, p.Source); e != nil {
		return e
	}
	if p.SourceVersion != nil {
		if e := checkString(p.SourceVersion, 0, 1024, "SourceVersion"); e != nil {
			return e
		}
		if *p.SourceVersion != "" && common.Value(p.Source.Type) == noSource {
			return invalid("NO_SOURCE cannot have SourceVersion")
		}
	}
	if e := artifactsOK(p.Artifacts); e != nil {
		return e
	}
	if e := environmentOK(p.Environment); e != nil {
		return e
	}
	p.TimeoutInMinutes = common.OrElse(p.TimeoutInMinutes, new(int64(60)))
	p.QueuedTimeoutInMinutes = common.OrElse(p.QueuedTimeoutInMinutes, new(int64(60)))
	if *p.TimeoutInMinutes < 5 || *p.TimeoutInMinutes > 2160 {
		return invalid("TimeoutInMinutes must be 5-2160")
	}
	if *p.QueuedTimeoutInMinutes < 5 || *p.QueuedTimeoutInMinutes > 480 {
		return invalid("QueuedTimeoutInMinutes must be 5-480")
	}
	tags, e := tagging.Map(
		p.Tags,
		func(t api.Tag) (*string, *string) { return t.Key, t.Value },
		invalid("Tag key and value are required"),
		func(string) error { return invalid("Duplicate tag key") },
	)
	if e != nil {
		return e
	}
	if e = tagging.CheckSet(tags); e != nil {
		return invalid(e.Error())
	}
	for k, v := range tags {
		if utf8.RuneCountInString(k) > 127 || utf8.RuneCountInString(v) > 255 || !tagPattern.MatchString(k) ||
			!tagPattern.MatchString(v) {
			return invalid("Tags require modeled characters, keys of 1-127 and values of 0-255 characters")
		}
	}
	p.Tags = tagList(tags)
	return nil
}

func sourceOK(scope request.Scope, s *api.ProjectSource) error {
	if s == nil {
		return invalid("Source is required")
	}
	typ := common.Value(s.Type)
	switch typ {
	case noSource, s3Type, githubSource, commitSource:
	default:
		return invalid("Supported source types are NO_SOURCE, GITHUB, CODECOMMIT and S3")
	}
	if typ == noSource {
		if s.Location != nil || s.GitCloneDepth != nil {
			return invalid("NO_SOURCE does not support location or GitCloneDepth")
		}
		if e := checkString(s.Buildspec, 1, 65536, "NO_SOURCE inline Buildspec"); e != nil {
			return e
		}
		if !strings.Contains(*s.Buildspec, "\n") {
			return invalid("NO_SOURCE requires an inline multiline Buildspec, not a file reference")
		}
	} else {
		if e := checkString(s.Location, 1, 2048, "Source Location"); e != nil {
			return e
		}
		if typ == s3Type {
			bucket, key, ok := strings.Cut(*s.Location, "/")
			if !ok || !bucketOK(bucket) || key == "" || !objectPathOK(key) {
				return invalid("S3 source location requires bucket/key or bucket/folder/")
			}
			if s.GitCloneDepth != nil {
				return invalid("S3 source cannot specify GitCloneDepth")
			}
		} else {
			u, e := url.Parse(*s.Location)
			if e != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
				u.Port() != "" {
				return invalid("Git source requires a supported HTTPS clone URL")
			}
			parts := strings.Split(strings.Trim(u.Path, "/"), "/")
			if typ == githubSource {
				if u.Host != "github.com" || len(parts) != 2 || !repoPattern.MatchString(parts[0]) ||
					!repoPattern.MatchString(parts[1]) {
					return invalid("GITHUB location requires https://github.com/owner/repository")
				}
			} else {
				if u.Host != "git-codecommit."+scope.Region+".amazonaws.com" || len(parts) != 3 || parts[0] != "v1" ||
					parts[1] != "repos" ||
					!repoPattern.MatchString(parts[2]) {
					return invalid("CODECOMMIT location requires a caller-Region HTTPS repository URL")
				}
			}
			if s.GitCloneDepth != nil && (*s.GitCloneDepth < 0 || *s.GitCloneDepth > 2147483647) {
				return invalid("GitCloneDepth must be 0-2147483647")
			}
		}
	}
	if s.Buildspec != nil {
		if e := checkString(s.Buildspec, 0, 65536, "Buildspec"); e != nil {
			return e
		}
		v := *s.Buildspec
		if strings.HasPrefix(v, "arn:") {
			bucket, key, ok := strings.Cut(strings.TrimPrefix(v, "arn:aws:s3:::"), "/")
			if !strings.HasPrefix(v, "arn:aws:s3:::") || !ok || !bucketOK(bucket) || key == "" || !objectPathOK(key) {
				return invalid("Buildspec S3 reference requires a commercial bucket/object ARN")
			}
		}
	}
	return nil
}

func artifactsOK(a *api.ProjectArtifacts) error {
	if a == nil {
		return invalid("Artifacts is required")
	}
	if common.Value(a.Type) == noArtifacts {
		if a.Location != nil || a.Name != nil || a.Path != nil || a.NamespaceType != nil || a.Packaging != nil ||
			a.OverrideArtifactName != nil ||
			a.EncryptionDisabled != nil {
			return invalid("NO_ARTIFACTS supports only Type in this metadata slice")
		}
		return nil
	}
	if common.Value(a.Type) != s3Type {
		return invalid("Artifacts supports NO_ARTIFACTS or S3")
	}
	if a.Location == nil || !bucketOK(*a.Location) {
		return invalid("S3 artifact Location requires a bucket name")
	}
	if a.Name == nil || *a.Name == "" || !objectPathOK(*a.Name) {
		return invalid("S3 artifact Name requires a finite object name or /")
	}
	if a.Path != nil && !objectPathOK(*a.Path) {
		return invalid("Invalid artifact Path")
	}
	a.NamespaceType = common.OrElse(a.NamespaceType, new(none))
	a.Packaging = common.OrElse(a.Packaging, new(none))
	if *a.NamespaceType != none && *a.NamespaceType != "BUILD_ID" {
		return invalid("Artifact NamespaceType supports NONE or BUILD_ID")
	}
	if *a.Packaging != none && *a.Packaging != "ZIP" {
		return invalid("Artifact Packaging supports NONE or ZIP")
	}
	a.OverrideArtifactName = common.OrElse(a.OverrideArtifactName, new(false))
	a.EncryptionDisabled = common.OrElse(a.EncryptionDisabled, new(false))
	return nil
}

func environmentOK(env *api.ProjectEnvironment) error {
	if env == nil {
		return invalid("Environment is required")
	}
	if common.Value(env.Type) != "LINUX_CONTAINER" {
		return invalid("Only LINUX_CONTAINER environment metadata is supported")
	}
	switch common.Value(env.ComputeType) {
	case "BUILD_GENERAL1_SMALL", "BUILD_GENERAL1_MEDIUM", "BUILD_GENERAL1_LARGE":
	default:
		return invalid("ComputeType supports BUILD_GENERAL1_SMALL, MEDIUM or LARGE")
	}
	if env.Image == nil || len(*env.Image) > 1024 || !imagePattern.MatchString(*env.Image) {
		return invalid("Image requires a finite Docker image tag or sha256 digest")
	}
	env.PrivilegedMode = common.OrElse(env.PrivilegedMode, new(false))
	if len(env.EnvironmentVariables) > 100 {
		return invalid("Local metadata supports at most 100 environment variables")
	}
	seen := map[string]bool{}
	for i := range env.EnvironmentVariables {
		v := &env.EnvironmentVariables[i]
		if v.Name == nil || !envNamePattern.MatchString(*v.Name) || strings.HasPrefix(*v.Name, "CODEBUILD_") ||
			seen[*v.Name] {
			return invalid(
				"Variable names require unique shell identifiers of 1-256 characters without CODEBUILD_ prefix",
			)
		}
		seen[*v.Name] = true
		if e := checkString(v.Value, 0, 4096, "Environment variable Value"); e != nil {
			return e
		}
		v.Type = common.OrElse(v.Type, new(plaintext))
		switch *v.Type {
		case plaintext:
		case "PARAMETER_STORE":
			if !parameterPattern.MatchString(*v.Value) {
				return invalid("PARAMETER_STORE supports parameter names only")
			}
		case "SECRETS_MANAGER":
			if !secretPattern.MatchString(*v.Value) {
				return invalid("SECRETS_MANAGER supports secret names with optional JSON key/version selectors only")
			}
		default:
			return invalid("Invalid environment variable Type")
		}
	}
	return nil
}

func rolePath(parts []string) string {
	if len(parts) < 3 {
		return ""
	}
	return parts[2]
}
