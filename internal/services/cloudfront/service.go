// Package cloudfront implements the Amazon CloudFront service without HTTP
// dependencies. CloudFront is a global service, so resources belong to an
// account:
//
//	<account>/cloudfront/distribution/<id>                          (distribution record and tags)
//	<account>/cloudfront/reference/<caller reference>               (distribution ID)
//	<account>/cloudfront/alias/<cname>                              (distribution ID)
//	<account>/cloudfront/invalidation/<id>/<invalidation ID>        (invalidation record)
//	<account>/cloudfront/invalidation-ref/<id>/<caller reference>   (invalidation ID)
//	<account>/cloudfront/oac/<id>                                   (OAC record)
//	<account>/cloudfront/oac-name/<name>                            (OAC ID)
//
// All operations are executed synchronously against the store.
package cloudfront

import (
	"fmt"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/cloudfront"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/s3"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	service          = "cloudfront"
	kindDistribution = "distribution"
	kindReference    = "reference"
	kindAlias        = "alias"
	kindOAC          = "oac"
	kindOACName      = "oac-name"
	idLength         = 13
)

var _ api.API = (*Service)(nil)

type Service struct {
	store   storage.Store
	buckets *s3.Service
	now     func() time.Time
}

func New(store storage.Store) *Service {
	return &Service{store: store, now: time.Now}
}

func key(account, kind, name string) string {
	return account + "/" + service + "/" + kind + "/" + name
}

func newID(prefix string) string {
	return prefix + common.RandomID(idLength, common.AlphabetUpperHex)
}

func noSuchDistribution(id string) error {
	msg := "The specified distribution does not exist."
	if id != "" {
		msg = "The specified distribution does not exist: " + id
	}
	return &awserror.Error{
		Code:       "NoSuchDistribution",
		Message:    msg,
		StatusCode: 404,
	}
}

func distributionAlreadyExists() error {
	return &awserror.Error{
		Code: "DistributionAlreadyExists",
		Message: "The caller reference you attempted to create the distribution with " +
			"is associated with another distribution.",
		StatusCode: 409,
	}
}

func cnameAlreadyExists(cname string) error {
	return &awserror.Error{
		Code:       "CNAMEAlreadyExists",
		Message:    fmt.Sprintf("The CNAME specified (%s) is already defined for CloudFront.", cname),
		StatusCode: 409,
	}
}

func distributionNotDisabled() error {
	return &awserror.Error{
		Code: "DistributionNotDisabled",
		Message: "The specified CloudFront distribution is not disabled. " +
			"You must disable the distribution before you can delete it.",
		StatusCode: 409,
	}
}

func invalidIfMatchVersion() error {
	return &awserror.Error{
		Code:       "InvalidIfMatchVersion",
		Message:    "The If-Match version is missing or not valid.",
		StatusCode: 400,
	}
}

func invalidArgument(msg string) error {
	return &awserror.Error{
		Code:       "InvalidArgument",
		Message:    msg,
		StatusCode: 400,
	}
}

func noSuchOriginAccessControl(id string) error {
	msg := "The origin access control does not exist."
	if id != "" {
		msg = "The origin access control does not exist: " + id
	}
	return &awserror.Error{
		Code:       "NoSuchOriginAccessControl",
		Message:    msg,
		StatusCode: 404,
	}
}

func originAccessControlAlreadyExists() error {
	return &awserror.Error{
		Code:       "OriginAccessControlAlreadyExists",
		Message:    "An origin access control with the specified parameters already exists.",
		StatusCode: 409,
	}
}

func originAccessControlInUse() error {
	return &awserror.Error{
		Code: "OriginAccessControlInUse",
		Message: "Cannot delete the origin access control because " +
			"it's in use by one or more distributions.",
		StatusCode: 409,
	}
}
