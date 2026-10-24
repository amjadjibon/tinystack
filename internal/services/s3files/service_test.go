package s3files

import (
	"testing"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3files"
)

// The model's path lookahead is rendered as an equivalent RE2 expression by the generator.
func TestModelValidation(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"/", "/a", "/a/b/c/d", "/normal.dot", "/space here"} {
		if err := (&api.RootDirectory{Path: new(path)}).Validate(""); err != nil {
			t.Errorf("valid path %q: %v", path, err)
		}
	}

	for _, path := range []string{"", "relative", "/.hidden", "/a/.hidden", "/a/b/c/d/e", "/a/", "/a//b", "/a$", "/a\n"} {
		if err := (&api.RootDirectory{Path: new(path)}).Validate(""); err == nil {
			t.Errorf("invalid path %q accepted", path)
		}
	}

	in := &api.CreateAccessPointRequest{
		FileSystemId: new("fs-00000000000000000"), PosixUser: &api.PosixUser{Gid: new(int64(0))},
	}

	if err := in.Validate(""); err == nil {
		t.Error("missing POSIX UID accepted")
	}
}
