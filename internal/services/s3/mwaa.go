package s3

import (
	"fmt"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// CheckMWAASource reads bucket configuration in the caller's transaction. It
// checks references only: no objects, policies, IAM, encryption or DAG contents.
func (s *Service) CheckMWAASource(tx storage.ReadTx, scope request.Scope, arn string) error {
	name, ok := strings.CutPrefix(arn, "arn:aws:s3:::")
	if !ok || !validBucketName(name) {
		return fmt.Errorf("SourceBucketArn requires a commercial general-purpose S3 bucket ARN")
	}
	key, e := resolve(tx, request.Metadata{Scope: scope}, new(name), new(scope.AccountID))
	if e != nil {
		return fmt.Errorf("SourceBucketArn must reference a bucket in this account and Region: %w", e)
	}
	var meta bucketMeta
	found, e := readJSON(tx, key, &meta)
	if e != nil {
		return e
	}
	p := meta.PublicAccessBlock
	if !found || meta.Versioning != versioningEnabled || p == nil ||
		!common.Value(p.BlockPublicAcls) || !common.Value(p.IgnorePublicAcls) ||
		!common.Value(p.BlockPublicPolicy) || !common.Value(p.RestrictPublicBuckets) {
		return fmt.Errorf("SourceBucketArn requires enabled versioning and all four public-access blocks")
	}
	return nil
}
