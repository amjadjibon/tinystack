package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrtypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
)

func ecrClient(s *httptest.Server, key, region string) *ecr.Client {
	return ecr.New(ecr.Options{
		Region:       region,
		BaseEndpoint: aws.String(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: key, SecretAccessKey: "test"}, nil
		}),
		Retryer: aws.NopRetryer{},
	})
}

func imageDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// uploadLayer uploads data to repo through the layer API and returns its digest.
func uploadLayer(ctx context.Context, t *testing.T, c *ecr.Client, repo string, data []byte) string {
	t.Helper()
	started, err := c.InitiateLayerUpload(ctx, &ecr.InitiateLayerUploadInput{RepositoryName: aws.String(repo)})
	if err != nil {
		t.Fatal(err)
	}

	for first := 0; first < len(data); first += 4 {
		part := data[first:min(first+4, len(data))]
		_, err = c.UploadLayerPart(ctx, &ecr.UploadLayerPartInput{
			RepositoryName: aws.String(repo), UploadId: started.UploadId, LayerPartBlob: part,
			PartFirstByte: aws.Int64(int64(first)), PartLastByte: aws.Int64(int64(first + len(part) - 1)),
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	digest := imageDigest(data)
	_, err = c.CompleteLayerUpload(ctx, &ecr.CompleteLayerUploadInput{
		RepositoryName: aws.String(repo), UploadId: started.UploadId, LayerDigests: []string{digest},
	})
	if err != nil {
		t.Fatal(err)
	}

	return digest
}

// imageManifest returns an OCI image manifest for a config and one layer.
func imageManifest(config, layer string, layerSize int) string {
	data, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{
			"mediaType": "application/vnd.oci.image.config.v1+json",
			"digest":    config,
			"size":      2,
		},
		"layers": []map[string]any{{
			"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip", "digest": layer, "size": layerSize,
		}},
	})
	return string(data)
}

// pushImage uploads a config and a layer with content and puts an image
// tagged tag (none if empty), returning the manifest's digest.
func pushImage(ctx context.Context, t *testing.T, c *ecr.Client, repo, tag, content string) string {
	t.Helper()
	config := uploadLayer(ctx, t, c, repo, []byte(`{"c":"`+content+`"}`))
	layer := uploadLayer(ctx, t, c, repo, []byte("layer "+content))
	in := &ecr.PutImageInput{
		RepositoryName: aws.String(repo),
		ImageManifest:  aws.String(imageManifest(config, layer, len("layer "+content))),
	}
	if tag != "" {
		in.ImageTag = aws.String(tag)
	}

	out, err := c.PutImage(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	return aws.ToString(out.Image.ImageId.ImageDigest)
}

func createRepository(ctx context.Context, t *testing.T, c *ecr.Client, name string) {
	t.Helper()
	if _, err := c.CreateRepository(ctx, &ecr.CreateRepositoryInput{RepositoryName: aws.String(name)}); err != nil {
		t.Fatal(err)
	}
}

func TestECRRepositories(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := ecrClient(s, "test", "us-east-1")

	out, err := c.CreateRepository(ctx, &ecr.CreateRepositoryInput{
		RepositoryName:             aws.String("team/web"),
		ImageTagMutability:         ecrtypes.ImageTagMutabilityImmutable,
		ImageScanningConfiguration: &ecrtypes.ImageScanningConfiguration{ScanOnPush: true},
		Tags:                       []ecrtypes.Tag{{Key: aws.String("team"), Value: aws.String("web")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	repo := out.Repository
	host := strings.TrimPrefix(s.URL, "http://")
	if aws.ToString(repo.RepositoryArn) != "arn:aws:ecr:us-east-1:000000000000:repository/team/web" ||
		aws.ToString(repo.RepositoryUri) != host+"/team/web" || aws.ToString(repo.RegistryId) != "000000000000" ||
		repo.ImageTagMutability != ecrtypes.ImageTagMutabilityImmutable || !repo.ImageScanningConfiguration.ScanOnPush ||
		repo.EncryptionConfiguration.EncryptionType != ecrtypes.EncryptionTypeAes256 || repo.CreatedAt == nil {
		t.Fatalf("repository = %+v", repo)
	}

	_, err = c.CreateRepository(ctx, &ecr.CreateRepositoryInput{RepositoryName: aws.String("team/web")})
	code(t, err, "RepositoryAlreadyExistsException")
	for _, name := range []string{"Upper", "a", "trailing/", "double//slash", "dash-"} {
		_, err = c.CreateRepository(ctx, &ecr.CreateRepositoryInput{RepositoryName: aws.String(name)})
		code(t, err, "InvalidParameterException")
	}

	_, err = c.CreateRepository(ctx, &ecr.CreateRepositoryInput{
		RepositoryName:          aws.String("kms"),
		EncryptionConfiguration: &ecrtypes.EncryptionConfiguration{EncryptionType: ecrtypes.EncryptionTypeKms},
	})
	code(t, err, "InvalidParameterException")

	createRepository(ctx, t, c, "team")
	createRepository(ctx, t, c, "api")
	listed, err := c.DescribeRepositories(ctx, &ecr.DescribeRepositoriesInput{MaxResults: aws.Int32(2)})
	if err != nil {
		t.Fatal(err)
	}

	if len(listed.Repositories) != 2 || aws.ToString(listed.Repositories[0].RepositoryName) != "api" ||
		listed.NextToken == nil {
		t.Fatalf("first page = %+v", listed)
	}

	rest, err := c.DescribeRepositories(ctx, &ecr.DescribeRepositoriesInput{NextToken: listed.NextToken})
	if err != nil {
		t.Fatal(err)
	}

	if len(rest.Repositories) != 1 || aws.ToString(rest.Repositories[0].RepositoryName) != "team/web" ||
		rest.NextToken != nil {
		t.Fatalf("second page = %+v", rest)
	}

	_, err = c.DescribeRepositories(ctx, &ecr.DescribeRepositoriesInput{RepositoryNames: []string{"api", "missing"}})
	code(t, err, "RepositoryNotFoundException")

	// Repositories belong to a region and an account.
	west, err := ecrClient(s, "test", "us-west-2").DescribeRepositories(ctx, &ecr.DescribeRepositoriesInput{})
	if err != nil || len(west.Repositories) != 0 {
		t.Fatalf("us-west-2 = %+v, %v", west, err)
	}

	_, err = ecrClient(s, "team-b", "us-east-1").DescribeRepositories(ctx, &ecr.DescribeRepositoriesInput{
		RepositoryNames: []string{"api"},
	})
	code(t, err, "RepositoryNotFoundException")
	_, err = c.DescribeRepositories(ctx, &ecr.DescribeRepositoriesInput{RegistryId: aws.String("111111111111")})
	code(t, err, "InvalidParameterException")

	t.Run("tags", func(t *testing.T) {
		arn := repo.RepositoryArn
		_, err := c.TagResource(ctx, &ecr.TagResourceInput{ResourceArn: arn, Tags: []ecrtypes.Tag{
			{Key: aws.String("env"), Value: aws.String("dev")},
		}})
		if err != nil {
			t.Fatal(err)
		}

		if _, err = c.UntagResource(
			ctx,
			&ecr.UntagResourceInput{ResourceArn: arn, TagKeys: []string{"team"}},
		); err != nil {
			t.Fatal(err)
		}

		tags, err := c.ListTagsForResource(ctx, &ecr.ListTagsForResourceInput{ResourceArn: arn})
		if err != nil {
			t.Fatal(err)
		}

		if len(tags.Tags) != 1 || aws.ToString(tags.Tags[0].Key) != "env" {
			t.Fatalf("tags = %+v", tags.Tags)
		}

		many := make([]ecrtypes.Tag, 51)
		for i := range many {
			many[i] = ecrtypes.Tag{Key: aws.String(strings.Repeat("k", i+1)), Value: aws.String("v")}
		}

		_, err = c.TagResource(ctx, &ecr.TagResourceInput{ResourceArn: arn, Tags: many})
		code(t, err, "TooManyTagsException")
		_, err = c.ListTagsForResource(ctx, &ecr.ListTagsForResourceInput{
			ResourceArn: aws.String("arn:aws:ecr:us-east-1:000000000000:repository/missing"),
		})
		code(t, err, "RepositoryNotFoundException")
	})

	t.Run("settings", func(t *testing.T) {
		name := aws.String("api")
		_, err := c.PutImageTagMutability(ctx, &ecr.PutImageTagMutabilityInput{
			RepositoryName: name, ImageTagMutability: ecrtypes.ImageTagMutabilityImmutable,
		})
		if err != nil {
			t.Fatal(err)
		}

		_, err = c.PutImageScanningConfiguration(ctx, &ecr.PutImageScanningConfigurationInput{
			RepositoryName: name, ImageScanningConfiguration: &ecrtypes.ImageScanningConfiguration{ScanOnPush: true},
		})
		if err != nil {
			t.Fatal(err)
		}

		got, err := c.DescribeRepositories(ctx, &ecr.DescribeRepositoriesInput{RepositoryNames: []string{"api"}})
		if err != nil {
			t.Fatal(err)
		}

		if r := got.Repositories[0]; r.ImageTagMutability != ecrtypes.ImageTagMutabilityImmutable ||
			!r.ImageScanningConfiguration.ScanOnPush {
			t.Fatalf("repository = %+v", r)
		}

		_, err = c.PutImageTagMutability(ctx, &ecr.PutImageTagMutabilityInput{
			RepositoryName: name, ImageTagMutability: ecrtypes.ImageTagMutabilityImmutableWithExclusion,
		})
		code(t, err, "InvalidParameterException")
	})

	t.Run("policies", func(t *testing.T) {
		name := aws.String("api")
		_, err := c.GetRepositoryPolicy(ctx, &ecr.GetRepositoryPolicyInput{RepositoryName: name})
		code(t, err, "RepositoryPolicyNotFoundException")
		policy := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"ecr:BatchGetImage"}]}`
		if _, err = c.SetRepositoryPolicy(ctx, &ecr.SetRepositoryPolicyInput{
			RepositoryName: name, PolicyText: aws.String(policy),
		}); err != nil {
			t.Fatal(err)
		}

		got, err := c.GetRepositoryPolicy(ctx, &ecr.GetRepositoryPolicyInput{RepositoryName: name})
		if err != nil || aws.ToString(got.PolicyText) != policy {
			t.Fatalf("policy = %+v, %v", got, err)
		}

		_, err = c.SetRepositoryPolicy(ctx, &ecr.SetRepositoryPolicyInput{
			RepositoryName: name, PolicyText: aws.String("{}"),
		})
		code(t, err, "InvalidParameterException")
		if _, err = c.DeleteRepositoryPolicy(ctx, &ecr.DeleteRepositoryPolicyInput{RepositoryName: name}); err != nil {
			t.Fatal(err)
		}

		_, err = c.DeleteRepositoryPolicy(ctx, &ecr.DeleteRepositoryPolicyInput{RepositoryName: name})
		code(t, err, "RepositoryPolicyNotFoundException")
	})

	t.Run("delete", func(t *testing.T) {
		pushImage(ctx, t, c, "team", "v1", "one")
		_, err := c.DeleteRepository(ctx, &ecr.DeleteRepositoryInput{RepositoryName: aws.String("team")})
		code(t, err, "RepositoryNotEmptyException")
		deleted, err := c.DeleteRepository(ctx, &ecr.DeleteRepositoryInput{
			RepositoryName: aws.String("team"), Force: true,
		})
		if err != nil || aws.ToString(deleted.Repository.RepositoryName) != "team" {
			t.Fatalf("delete = %+v, %v", deleted, err)
		}

		// A repository named like a deleted one's prefix keeps its records.
		got, err := c.DescribeRepositories(ctx, &ecr.DescribeRepositoriesInput{RepositoryNames: []string{"team/web"}})
		if err != nil || len(got.Repositories) != 1 {
			t.Fatalf("team/web = %+v, %v", got, err)
		}

		createRepository(ctx, t, c, "team")
		images, err := c.ListImages(ctx, &ecr.ListImagesInput{RepositoryName: aws.String("team")})
		if err != nil || len(images.ImageIds) != 0 {
			t.Fatalf("recreated repository images = %+v, %v", images, err)
		}
	})
}

func TestECRImages(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := ecrClient(s, "test", "us-east-1")
	createRepository(ctx, t, c, "app")
	repo := aws.String("app")

	t.Run("layers", func(t *testing.T) {
		data := []byte("0123456789")
		digest := uploadLayer(ctx, t, c, "app", data)
		checked, err := c.BatchCheckLayerAvailability(ctx, &ecr.BatchCheckLayerAvailabilityInput{
			RepositoryName: repo, LayerDigests: []string{digest, imageDigest([]byte("missing")), "bad"},
		})
		if err != nil {
			t.Fatal(err)
		}

		if len(checked.Layers) != 1 || aws.ToInt64(checked.Layers[0].LayerSize) != 10 ||
			checked.Layers[0].LayerAvailability != ecrtypes.LayerAvailabilityAvailable || len(checked.Failures) != 2 ||
			checked.Failures[0].FailureCode != ecrtypes.LayerFailureCodeMissingLayerDigest ||
			checked.Failures[1].FailureCode != ecrtypes.LayerFailureCodeInvalidLayerDigest {
			t.Fatalf("availability = %+v", checked)
		}

		started, err := c.InitiateLayerUpload(ctx, &ecr.InitiateLayerUploadInput{RepositoryName: repo})
		if err != nil {
			t.Fatal(err)
		}

		_, err = c.UploadLayerPart(ctx, &ecr.UploadLayerPartInput{
			RepositoryName: repo, UploadId: started.UploadId, LayerPartBlob: []byte("ab"),
			PartFirstByte: aws.Int64(1), PartLastByte: aws.Int64(2),
		})
		code(t, err, "InvalidLayerPartException")
		_, err = c.CompleteLayerUpload(ctx, &ecr.CompleteLayerUploadInput{
			RepositoryName: repo, UploadId: started.UploadId, LayerDigests: []string{digest},
		})
		code(t, err, "EmptyUploadException")
		_, err = c.UploadLayerPart(ctx, &ecr.UploadLayerPartInput{
			RepositoryName: repo, UploadId: started.UploadId, LayerPartBlob: []byte("ab"),
			PartFirstByte: aws.Int64(0), PartLastByte: aws.Int64(1),
		})
		if err != nil {
			t.Fatal(err)
		}

		_, err = c.CompleteLayerUpload(ctx, &ecr.CompleteLayerUploadInput{
			RepositoryName: repo, UploadId: started.UploadId, LayerDigests: []string{digest},
		})
		code(t, err, "InvalidLayerException")
		_, err = c.CompleteLayerUpload(ctx, &ecr.CompleteLayerUploadInput{
			RepositoryName: repo, UploadId: aws.String("missing"), LayerDigests: []string{digest},
		})
		code(t, err, "UploadNotFoundException")

		// The download URL carries a password, so a plain GET works.
		url, err := c.GetDownloadUrlForLayer(ctx, &ecr.GetDownloadUrlForLayerInput{
			RepositoryName: repo, LayerDigest: aws.String(digest),
		})
		if err != nil {
			t.Fatal(err)
		}

		resp, err := http.Get(aws.ToString(url.DownloadUrl))
		if err != nil {
			t.Fatal(err)
		}

		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !bytes.Equal(body, data) {
			t.Fatalf("download = %d %q", resp.StatusCode, body)
		}

		_, err = c.GetDownloadUrlForLayer(ctx, &ecr.GetDownloadUrlForLayerInput{
			RepositoryName: repo, LayerDigest: aws.String(imageDigest([]byte("missing"))),
		})
		code(t, err, "LayersNotFoundException")
	})

	first := pushImage(ctx, t, c, "app", "v1", "one")
	t.Run("put", func(t *testing.T) {
		_, err := c.PutImage(ctx, &ecr.PutImageInput{
			RepositoryName: repo, ImageTag: aws.String("broken"),
			ImageManifest: aws.String(imageManifest(imageDigest([]byte("x")), imageDigest([]byte("y")), 1)),
		})
		code(t, err, "LayersNotFoundException")
		_, err = c.PutImage(ctx, &ecr.PutImageInput{RepositoryName: repo, ImageManifest: aws.String("not json")})
		code(t, err, "InvalidParameterException")

		got, err := c.BatchGetImage(ctx, &ecr.BatchGetImageInput{
			RepositoryName: repo, ImageIds: []ecrtypes.ImageIdentifier{{ImageTag: aws.String("v1")}},
		})
		if err != nil {
			t.Fatal(err)
		}

		manifest := got.Images[0].ImageManifest
		if imageDigest([]byte(aws.ToString(manifest))) != first {
			t.Fatalf("digest = %s, want %s", imageDigest([]byte(aws.ToString(manifest))), first)
		}

		_, err = c.PutImage(ctx, &ecr.PutImageInput{
			RepositoryName: repo, ImageManifest: manifest, ImageTag: aws.String("v1"),
		})
		code(t, err, "ImageAlreadyExistsException")
		_, err = c.PutImage(ctx, &ecr.PutImageInput{
			RepositoryName: repo, ImageManifest: manifest, ImageDigest: aws.String(imageDigest([]byte("other"))),
		})
		code(t, err, "ImageDigestDoesNotMatchException")

		// A second tag for the same manifest.
		if _, err = c.PutImage(ctx, &ecr.PutImageInput{
			RepositoryName: repo, ImageManifest: manifest, ImageTag: aws.String("latest"),
		}); err != nil {
			t.Fatal(err)
		}
	})

	second := pushImage(ctx, t, c, "app", "v2", "two")
	t.Run("retag", func(t *testing.T) {
		// Moving latest to the second image leaves the first with v1.
		got, err := c.BatchGetImage(ctx, &ecr.BatchGetImageInput{
			RepositoryName: repo, ImageIds: []ecrtypes.ImageIdentifier{{ImageDigest: aws.String(second)}},
		})
		if err != nil {
			t.Fatal(err)
		}

		if _, err = c.PutImage(ctx, &ecr.PutImageInput{
			RepositoryName: repo, ImageManifest: got.Images[0].ImageManifest, ImageTag: aws.String("latest"),
		}); err != nil {
			t.Fatal(err)
		}

		details, err := c.DescribeImages(
			ctx,
			&ecr.DescribeImagesInput{RepositoryName: repo, ImageIds: []ecrtypes.ImageIdentifier{
				{ImageDigest: aws.String(first)}, {ImageTag: aws.String("latest")},
			}},
		)
		if err != nil {
			t.Fatal(err)
		}

		if !slices.Equal(details.ImageDetails[0].ImageTags, []string{"v1"}) ||
			!slices.Equal(details.ImageDetails[1].ImageTags, []string{"latest", "v2"}) {
			t.Fatalf("tags = %v %v", details.ImageDetails[0].ImageTags, details.ImageDetails[1].ImageTags)
		}

		// The layer is the image's size; the config is not counted.
		d := details.ImageDetails[0]
		if aws.ToInt64(d.ImageSizeInBytes) != int64(len("layer one")) || d.ImagePushedAt == nil ||
			d.LastRecordedPullTime == nil || aws.ToString(d.ArtifactMediaType) != "application/vnd.oci.image.config.v1+json" ||
			aws.ToString(d.ImageManifestMediaType) != "application/vnd.oci.image.manifest.v1+json" {
			t.Fatalf("detail = %+v", d)
		}

		createRepository(ctx, t, c, "locked")
		_, err = c.PutImageTagMutability(ctx, &ecr.PutImageTagMutabilityInput{
			RepositoryName: aws.String("locked"), ImageTagMutability: ecrtypes.ImageTagMutabilityImmutable,
		})
		if err != nil {
			t.Fatal(err)
		}

		pushImage(ctx, t, c, "locked", "v1", "one")
		config := uploadLayer(ctx, t, c, "locked", []byte(`{"c":"two"}`))
		layer := uploadLayer(ctx, t, c, "locked", []byte("layer two"))
		_, err = c.PutImage(ctx, &ecr.PutImageInput{
			RepositoryName: aws.String("locked"), ImageTag: aws.String("v1"),
			ImageManifest: aws.String(imageManifest(config, layer, 9)),
		})
		code(t, err, "ImageTagAlreadyExistsException")
	})

	untagged := pushImage(ctx, t, c, "app", "", "three")
	t.Run("list", func(t *testing.T) {
		all, err := c.ListImages(ctx, &ecr.ListImagesInput{RepositoryName: repo})
		if err != nil {
			t.Fatal(err)
		}

		if len(all.ImageIds) != 4 {
			t.Fatalf("images = %+v", all.ImageIds)
		}

		only, err := c.ListImages(ctx, &ecr.ListImagesInput{
			RepositoryName: repo, Filter: &ecrtypes.ListImagesFilter{TagStatus: ecrtypes.TagStatusUntagged},
		})
		if err != nil {
			t.Fatal(err)
		}

		if len(only.ImageIds) != 1 || aws.ToString(only.ImageIds[0].ImageDigest) != untagged {
			t.Fatalf("untagged = %+v", only.ImageIds)
		}

		var pages int
		paginator := ecr.NewListImagesPaginator(c, &ecr.ListImagesInput{RepositoryName: repo, MaxResults: aws.Int32(3)})
		var seen []ecrtypes.ImageIdentifier
		for paginator.HasMorePages() {
			p, pageErr := paginator.NextPage(ctx)
			if pageErr != nil {
				t.Fatal(pageErr)
			}

			pages++
			seen = append(seen, p.ImageIds...)
		}

		if pages != 2 || len(seen) != 4 {
			t.Fatalf("pages = %d, images = %d", pages, len(seen))
		}

		failed, err := c.BatchGetImage(
			ctx,
			&ecr.BatchGetImageInput{RepositoryName: repo, ImageIds: []ecrtypes.ImageIdentifier{
				{ImageTag: aws.String("missing")}, {ImageDigest: aws.String(first), ImageTag: aws.String("v2")}, {},
			}},
		)
		if err != nil {
			t.Fatal(err)
		}

		codes := []ecrtypes.ImageFailureCode{}
		for _, f := range failed.Failures {
			codes = append(codes, f.FailureCode)
		}

		if !slices.Equal(codes, []ecrtypes.ImageFailureCode{
			ecrtypes.ImageFailureCodeImageNotFound, ecrtypes.ImageFailureCodeImageTagDoesNotMatchDigest,
			ecrtypes.ImageFailureCodeMissingDigestAndTag,
		}) {
			t.Fatalf("failures = %v", codes)
		}

		_, err = c.DescribeImages(ctx, &ecr.DescribeImagesInput{
			RepositoryName: repo, ImageIds: []ecrtypes.ImageIdentifier{{ImageTag: aws.String("missing")}},
		})
		code(t, err, "ImageNotFoundException")
	})

	t.Run("index", func(t *testing.T) {
		index, _ := json.Marshal(map[string]any{
			"schemaVersion": 2,
			"mediaType":     "application/vnd.oci.image.index.v1+json",
			"manifests": []map[string]any{
				{"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": second, "size": 1},
			},
		})
		out, err := c.PutImage(ctx, &ecr.PutImageInput{
			RepositoryName: repo, ImageManifest: aws.String(string(index)), ImageTag: aws.String("multi"),
		})
		if err != nil {
			t.Fatal(err)
		}

		missing, _ := json.Marshal(map[string]any{
			"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json",
			"manifests": []map[string]any{{"digest": imageDigest([]byte("none")), "size": 1}},
		})
		_, err = c.PutImage(ctx, &ecr.PutImageInput{RepositoryName: repo, ImageManifest: aws.String(string(missing))})
		code(t, err, "ReferencedImagesNotFoundException")

		deleted, err := c.BatchDeleteImage(
			ctx,
			&ecr.BatchDeleteImageInput{RepositoryName: repo, ImageIds: []ecrtypes.ImageIdentifier{
				{ImageDigest: aws.String(second)},
			}},
		)
		if err != nil {
			t.Fatal(err)
		}

		if len(deleted.ImageIds) != 0 || len(deleted.Failures) != 1 ||
			deleted.Failures[0].FailureCode != ecrtypes.ImageFailureCodeImageReferencedByManifestList {
			t.Fatalf("delete referenced = %+v", deleted)
		}

		if _, err = c.BatchDeleteImage(
			ctx,
			&ecr.BatchDeleteImageInput{RepositoryName: repo, ImageIds: []ecrtypes.ImageIdentifier{
				{ImageDigest: out.Image.ImageId.ImageDigest},
			}},
		); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("delete", func(t *testing.T) {
		// Removing one of two tags keeps the image; removing the last deletes it.
		deleted, err := c.BatchDeleteImage(
			ctx,
			&ecr.BatchDeleteImageInput{RepositoryName: repo, ImageIds: []ecrtypes.ImageIdentifier{
				{ImageTag: aws.String("latest")}, {ImageTag: aws.String("v1")}, {ImageTag: aws.String("gone")},
			}},
		)
		if err != nil {
			t.Fatal(err)
		}

		if len(deleted.ImageIds) != 2 || len(deleted.Failures) != 1 {
			t.Fatalf("deleted = %+v", deleted)
		}

		left, err := c.DescribeImages(ctx, &ecr.DescribeImagesInput{RepositoryName: repo})
		if err != nil {
			t.Fatal(err)
		}

		var digests []string
		for _, d := range left.ImageDetails {
			digests = append(digests, aws.ToString(d.ImageDigest))
		}

		slices.Sort(digests)
		want := []string{second, untagged}
		slices.Sort(want)
		if !slices.Equal(digests, want) {
			t.Fatalf("images = %v, want %v", digests, want)
		}
	})
}

func TestECRLifecyclePolicy(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := ecrClient(s, "test", "us-east-1")
	createRepository(ctx, t, c, "jobs")
	repo := aws.String("jobs")

	_, err := c.GetLifecyclePolicy(ctx, &ecr.GetLifecyclePolicyInput{RepositoryName: repo})
	code(t, err, "LifecyclePolicyNotFoundException")
	for _, bad := range []string{
		`{"rules":[]}`,
		`{"rules":[{"rulePriority":1,"selection":{"tagStatus":"tagged","countType":"imageCountMoreThan",` +
			`"countNumber":1},"action":{"type":"expire"}}]}`,
		`{"rules":[{"rulePriority":1,"selection":{"tagStatus":"any","countType":"imageCountMoreThan",` +
			`"countNumber":1},"action":{"type":"expire"}},{"rulePriority":2,"selection":{"tagStatus":"untagged",` +
			`"countType":"imageCountMoreThan","countNumber":1},"action":{"type":"expire"}}]}`,
		`{"rules":[{"rulePriority":1,"selection":{"tagStatus":"untagged","countType":"sinceImagePushed",` +
			`"countNumber":1},"action":{"type":"expire"}}]}`,
		`{"rules":[{"rulePriority":1,"selection":{"tagStatus":"untagged","countType":"imageCountMoreThan",` +
			`"countNumber":1,"extra":true},"action":{"type":"expire"}}]}`,
	} {
		_, err = c.PutLifecyclePolicy(ctx, &ecr.PutLifecyclePolicyInput{
			RepositoryName: repo, LifecyclePolicyText: aws.String(bad),
		})
		code(t, err, "InvalidParameterException")
	}

	for i, content := range []string{"a", "b", "c"} {
		pushImage(ctx, t, c, "jobs", "release-"+string(rune('1'+i)), content)
	}

	keep := pushImage(ctx, t, c, "jobs", "stable", "d")
	pushImage(ctx, t, c, "jobs", "", "e")
	newest := pushImage(ctx, t, c, "jobs", "", "f")

	// Keep two release images and one untagged image; stable matches no rule.
	policy := `{"rules":[` +
		`{"rulePriority":1,"selection":{"tagStatus":"tagged","tagPrefixList":["release-"],` +
		`"countType":"imageCountMoreThan","countNumber":2},"action":{"type":"expire"}},` +
		`{"rulePriority":2,"selection":{"tagStatus":"untagged","countType":"imageCountMoreThan","countNumber":1},` +
		`"action":{"type":"expire"}}]}`
	if _, err = c.PutLifecyclePolicy(ctx, &ecr.PutLifecyclePolicyInput{
		RepositoryName: repo, LifecyclePolicyText: aws.String(policy),
	}); err != nil {
		t.Fatal(err)
	}

	listed, err := c.ListImages(ctx, &ecr.ListImagesInput{RepositoryName: repo})
	if err != nil {
		t.Fatal(err)
	}

	var ids []string
	for _, id := range listed.ImageIds {
		if tag := aws.ToString(id.ImageTag); tag != "" {
			ids = append(ids, tag)
		} else {
			ids = append(ids, aws.ToString(id.ImageDigest))
		}
	}

	slices.Sort(ids)
	want := []string{"release-2", "release-3", "stable", newest}
	slices.Sort(want)
	if !slices.Equal(ids, want) {
		t.Fatalf("images = %v, want %v (stable %s)", ids, want, keep)
	}

	// The policy also runs when an image is pushed.
	pushImage(ctx, t, c, "jobs", "release-4", "g")
	got, err := c.DescribeImages(ctx, &ecr.DescribeImagesInput{
		RepositoryName: repo, ImageIds: []ecrtypes.ImageIdentifier{{ImageTag: aws.String("release-2")}},
	})
	code(t, err, "ImageNotFoundException")
	_ = got

	read, err := c.GetLifecyclePolicy(ctx, &ecr.GetLifecyclePolicyInput{RepositoryName: repo})
	if err != nil || aws.ToString(read.LifecyclePolicyText) != policy || read.LastEvaluatedAt == nil {
		t.Fatalf("policy = %+v, %v", read, err)
	}

	if _, err = c.DeleteLifecyclePolicy(ctx, &ecr.DeleteLifecyclePolicyInput{RepositoryName: repo}); err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteLifecyclePolicy(ctx, &ecr.DeleteLifecyclePolicyInput{RepositoryName: repo})
	code(t, err, "LifecyclePolicyNotFoundException")
}
