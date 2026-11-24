package integration_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func TestS3Replication(t *testing.T) {
	ctx := context.Background()
	srv := server(t)
	source := s3Client(srv, "test", "us-east-1")
	other := s3Client(srv, "team-b", "us-east-1")
	src, dst, cross := "replication-source", "replication-target", "replication-cross-account"
	versioned(t, source, src)
	versioned(t, source, dst)
	versioned(t, other, cross)
	old := putVersion(t, source, src, "live/old", "already existed")

	role := "arn:aws:iam::000000000000:role/replication"
	rule := func(id, bucket, prefix string, priority int32) s3types.ReplicationRule {
		return s3types.ReplicationRule{
			ID: new(id), Status: s3types.ReplicationRuleStatusEnabled, Priority: new(priority),
			Filter:      &s3types.ReplicationRuleFilter{Prefix: new(prefix)},
			Destination: &s3types.Destination{Bucket: new("arn:aws:s3:::" + bucket)},
			DeleteMarkerReplication: &s3types.DeleteMarkerReplication{
				Status: s3types.DeleteMarkerReplicationStatusEnabled,
			},
		}
	}

	low := rule("low", dst, "live/", 1)
	low.Destination.StorageClass = s3types.StorageClassStandardIa
	high := rule("high", dst, "live/faster", 2)
	high.Destination.StorageClass = s3types.StorageClassGlacierIr
	_, err := source.PutBucketReplication(ctx, &s3.PutBucketReplicationInput{
		Bucket: &src, ReplicationConfiguration: &s3types.ReplicationConfiguration{
			Role: &role, Rules: []s3types.ReplicationRule{low, high, rule("cross", cross, "cross/", 3)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := source.GetBucketReplication(ctx, &s3.GetBucketReplicationInput{Bucket: &src})
	if err != nil || len(got.ReplicationConfiguration.Rules) != 3 {
		t.Fatalf("get replication: %+v %v", got, err)
	}

	first := putVersion(t, source, src, "live/faster", "first")
	second := putVersion(t, source, src, "live/faster", "second")
	marker, err := source.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &src, Key: new("live/faster")})
	if err != nil {
		t.Fatal(err)
	}

	waitStatus := func(key string, version *string, want s3types.ReplicationStatus) {
		t.Helper()
		waitS3(t, string(want), func() bool {
			out, headErr := source.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &src, Key: &key, VersionId: version})
			return headErr == nil && out.ReplicationStatus == want
		})
	}
	waitStatus("live/faster", &first, s3types.ReplicationStatusCompleted)
	waitS3(t, "replicated delete marker", func() bool {
		list, listErr := source.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: &dst})
		return listErr == nil && len(list.DeleteMarkers) == 1 && *list.DeleteMarkers[0].VersionId == *marker.VersionId
	})

	replica, err := source.GetObject(ctx, &s3.GetObjectInput{Bucket: &dst, Key: new("live/faster"), VersionId: &second})
	if err != nil {
		t.Fatal(err)
	}

	_ = replica.Body.Close()
	if replica.ReplicationStatus != s3types.ReplicationStatusReplica ||
		replica.StorageClass != s3types.StorageClassGlacierIr {
		t.Fatalf("replica status and class = %q %q", replica.ReplicationStatus, replica.StorageClass)
	}

	if body, _ := readVersion(t, source, dst, "live/faster", first); body != "first" {
		t.Fatalf("first replica body = %q", body)
	}

	_, err = source.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &dst, Key: new("live/old"), VersionId: &old})
	if _, status := s3Code(err); status != http.StatusNotFound {
		t.Fatalf("unexpected backfill: %v", err)
	}

	putVersion(t, source, src, "cross/denied", "private")
	waitStatus("cross/denied", nil, s3types.ReplicationStatusFailed)
	policy := fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":%q},
"Action":["s3:ReplicateObject","s3:ReplicateDelete"],"Resource":"arn:aws:s3:::%s/*"}]}`, role, cross)
	_, err = other.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{Bucket: &cross, Policy: &policy})
	if err != nil {
		t.Fatal(err)
	}

	putVersion(t, source, src, "cross/allowed", "granted")
	waitStatus("cross/allowed", nil, s3types.ReplicationStatusCompleted)
	if body, _ := readVersion(t, other, cross, "cross/allowed", ""); body != "granted" {
		t.Fatalf("cross-account replica body = %q", body)
	}

	if _, deleteErr := source.DeleteBucketReplication(
		ctx,
		&s3.DeleteBucketReplicationInput{Bucket: &src},
	); deleteErr != nil {
		t.Fatal(deleteErr)
	}

	_, err = source.GetBucketReplication(ctx, &s3.GetBucketReplicationInput{Bucket: &src})
	if code, _ := s3Code(err); code != "ReplicationConfigurationNotFoundError" {
		t.Fatalf("get deleted configuration: %v", err)
	}
}
