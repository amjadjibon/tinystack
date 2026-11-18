package mlfixtures

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"image"
	_ "image/jpeg" // Register the JPEG header decoder.
	_ "image/png"  // Register the PNG header decoder.
	"io"
	"regexp"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	s3api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/s3"
)

var sha256Hex = regexp.MustCompile(`^[a-f0-9]{64}$`)

// ValidDigest reports whether a fixture key is a lowercase SHA-256 hex digest.
func ValidDigest(value string) bool { return sha256Hex.MatchString(value) }

// ImageDigest reads exactly one inline or local S3 image. Reading is bounded even for disk-backed S3 objects.
func ImageDigest(
	ctx context.Context, buckets *s3.Service, data []byte, object *s3api.GetObjectRequest,
	limit int, tooLarge, badFormat string,
) (string, error) {
	if (data == nil) == (object == nil) {
		return "", awserror.New("InvalidParameterException", "specify exactly one of Bytes or S3Object")
	}

	sizeError := awserror.New(tooLarge, "image exceeds the supported size limit")
	if object != nil {
		if object.Bucket == nil || *object.Bucket == "" || object.Key == nil || *object.Key == "" {
			return "", awserror.New("InvalidParameterException", "S3Object requires Bucket and Name")
		}

		payload := &request.Payload{}
		info, err := buckets.GetObject(request.WithPayload(ctx, payload), object)
		if err != nil {
			return "", awserror.New("InvalidS3ObjectException", "cannot read local S3 object: "+err.Error())
		}

		defer func() { _ = payload.Body.Close() }()
		if *info.ContentLength > int64(limit) {
			return "", sizeError
		}

		data, err = io.ReadAll(io.LimitReader(payload.Body, int64(limit)+1))
		if err != nil {
			return "", awserror.New("InvalidS3ObjectException", "cannot read local S3 object: "+err.Error())
		}
	}

	if len(data) > limit {
		return "", sizeError
	}

	_, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "png" && format != "jpeg") {
		return "", awserror.New(badFormat, "expected a PNG or JPEG image")
	}

	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}
