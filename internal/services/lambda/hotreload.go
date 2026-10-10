package lambda

import (
	"context"
	"os"
	"path/filepath"

	"github.com/amjadjibon/tinystack/internal/common"
)

const hotReloadBucket = "hot-reload"

// functionCode resolves deployment input outside storage transactions. A live
// directory is stored in the function record rather than as deployment bytes.
func (s *Service) functionCode(
	ctx context.Context,
	zipFile []byte,
	bucket, key, version *string,
) (code []byte, directory string, err error) {
	if common.Value(bucket) != hotReloadBucket {
		code, err = s.deploymentPackage(ctx, zipFile, bucket, key, version)
		return code, "", err
	}

	if len(zipFile) > 0 || version != nil {
		return nil, "", invalid("Hot reload cannot be combined with ZipFile or S3ObjectVersion")
	}

	directory = common.Value(key)
	if !filepath.IsAbs(directory) {
		return nil, "", invalid("Hot reload S3Key must be an absolute host path to an existing directory")
	}

	info, err := os.Stat(directory)
	if err != nil || !info.IsDir() {
		return nil, "", invalid("Hot reload S3Key must name an existing host directory: %s", directory)
	}

	return nil, filepath.Clean(directory), nil
}
