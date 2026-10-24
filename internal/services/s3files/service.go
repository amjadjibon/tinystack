// Package s3files implements the S3 Files control plane as records. A file system names an existing S3 bucket
// and IAM role; mount targets name existing EC2 subnets and security groups. No NFS server runs, no bytes are
// synchronized with S3, and mount target addresses are metadata only.
package s3files

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	ec2api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3files"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/services/iam"
	"github.com/amjadjibon/tinystack/internal/services/kms"
	"github.com/amjadjibon/tinystack/internal/services/s3"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName = "s3files"
	kindFS      = "file-system"
	kindMT      = "mount-target"
	kindAP      = "access-point"
	indexPrefix = "aws/s3files/index/"

	available = "available"
	creating  = "creating"
	deleting  = "deleting"

	// listPageSize is the page size of a list that the caller does not limit.
	listPageSize = 100
	// tagPageSize is the page size of ListTagsForResource, which the model leaves without a default.
	tagPageSize = 50

	// transitionDelay is how long a resource stays creating or deleting before the worker settles it.
	transitionDelay = 150 * time.Millisecond
)

var _ api.API = (*Service)(nil)

// Network reads existing EC2 records inside the caller's transaction.
type Network interface {
	MountSubnet(storage.ReadTx, request.Scope, string) (*ec2api.Subnet, error)
	MountGroups(storage.ReadTx, request.Scope, string, []string) ([]string, error)
}

// Service stores S3 Files resources and settles their lifecycle states in the background.
type Service struct {
	store   storage.Store
	network Network
	buckets *s3.Service
	roles   *iam.Service
	keys    *kms.Service
	workers *background.Group
}

// New constructs the service and starts its lifecycle worker; Stop ends it.
func New(store storage.Store, network Network, buckets *s3.Service, roles *iam.Service, keys *kms.Service) *Service {
	s := &Service{store: store, network: network, buckets: buckets, roles: roles, keys: keys, workers: background.New()}
	s.workers.Every(100*time.Millisecond, s.advance)

	return s
}

// Stop waits for the lifecycle worker to finish its current pass.
func (s *Service) Stop() { s.workers.Stop() }

// fileSystem is a file system record. Policy and Sync are nil until set.
type fileSystem struct {
	Created     time.Time
	Due         time.Time
	Tags        map[string]string
	Policy      *string
	Sync        *syncConfig
	ID          string
	Bucket      string
	Prefix      string
	ClientToken string
	Fingerprint string
	KMSKeyID    string
	RoleARN     string
	State       string
}

type syncConfig struct {
	Import     []api.ImportDataRule
	Expiration []api.ExpirationDataRule
	Version    int64
}

type mountTarget struct {
	Due           time.Time
	FileSystemID  string
	ID            string
	SubnetID      string
	VpcID         string
	ZoneID        string
	Zone          string
	IPv4          string
	IPv6          string
	State         string
	SecurityGroup []string
}

type accessPoint struct {
	Due           time.Time
	Tags          map[string]string
	PosixUser     *api.PosixUser
	RootDirectory *api.RootDirectory
	ID            string
	FileSystemID  string
	ClientToken   string
	State         string
}

// work indexes a resource whose state the worker has yet to settle.
type work struct {
	Scope request.Scope
	Kind  string
	ID    string
}

func invalid(format string, args ...any) error { return awserror.Validation(format, args...) }

func conflict(format string, args ...any) error {
	return awserror.New("ConflictException", fmt.Sprintf(format, args...))
}

func notFound(format string, args ...any) error {
	return awserror.New("ResourceNotFoundException", fmt.Sprintf(format, args...))
}

var errBadToken = awserror.Validation("nextToken is not valid")

func pageLimit(n *int64, fallback int) int {
	if n == nil {
		return fallback
	}

	return int(*n)
}

func fsKey(scope request.Scope, id string) string { return scope.Key(serviceName, kindFS, id) }

func mtKey(scope request.Scope, id string) string { return scope.Key(serviceName, kindMT, id) }

func apKey(scope request.Scope, id string) string { return scope.Key(serviceName, kindAP, id) }

func indexKey(scope request.Scope, kind, id string) string {
	return indexPrefix + scope.AccountID + "/" + scope.Region + "/" + kind + "/" + id
}

func fsARN(scope request.Scope, id string) string {
	return fmt.Sprintf("arn:aws:s3files:%s:%s:file-system/%s", scope.Region, scope.AccountID, id)
}

func apARN(scope request.Scope, fsID, id string) string {
	return fsARN(scope, fsID) + "/access-point/" + id
}

// resourceID returns the ID that ref (an ID or an ARN of the given ID kind) names in scope.
func resourceID(scope request.Scope, ref, idPrefix string) (string, bool) {
	if !strings.HasPrefix(ref, "arn:") {
		return ref, strings.HasPrefix(ref, idPrefix)
	}

	// arn:aws:s3files:<region>:<account>:file-system/<fs>[/access-point/<ap>]
	parts := strings.SplitN(ref, ":", 6)
	if len(parts) != 6 || parts[1] != "aws" || parts[2] != serviceName || parts[3] != scope.Region ||
		parts[4] != scope.AccountID {
		return "", false
	}

	segments := strings.Split(parts[5], "/")
	switch {
	case idPrefix == "fs-" && len(segments) == 2 && segments[0] == kindFS:
		return segments[1], true
	case idPrefix == "fsap-" && len(segments) == 4 && segments[0] == kindFS && segments[2] == kindAP:
		return segments[3], true
	}

	return "", false
}

func readFS(tx storage.ReadTx, scope request.Scope, ref string) (*fileSystem, error) {
	id, ok := resourceID(scope, ref, "fs-")
	if !ok {
		return nil, notFound("File system %s does not exist", ref)
	}

	r, found, err := storage.Read[fileSystem](tx, fsKey(scope, id))
	if err != nil {
		return nil, err
	}

	if !found {
		return nil, notFound("File system %s does not exist", ref)
	}

	return r, nil
}

func readMT(tx storage.ReadTx, scope request.Scope, id string) (*mountTarget, error) {
	r, found, err := storage.Read[mountTarget](tx, mtKey(scope, id))
	if err != nil {
		return nil, err
	}

	if !found {
		return nil, notFound("Mount target %s does not exist", id)
	}

	return r, nil
}

func readAP(tx storage.ReadTx, scope request.Scope, ref string) (*accessPoint, error) {
	id, ok := resourceID(scope, ref, "fsap-")
	if !ok {
		return nil, notFound("Access point %s does not exist", ref)
	}

	r, found, err := storage.Read[accessPoint](tx, apKey(scope, id))
	if err != nil {
		return nil, err
	}

	if !found || (strings.HasPrefix(ref, "arn:") && ref != apARN(scope, r.FileSystemID, r.ID)) {
		return nil, notFound("Access point %s does not exist", ref)
	}

	return r, nil
}

// queue stores r and indexes it for the worker to settle after transitionDelay.
func queue(tx storage.WriteTx, scope request.Scope, kind, id string, r any) error {
	if err := storage.Write(tx, scope.Key(serviceName, kind, id), r); err != nil {
		return err
	}

	return storage.Write(tx, indexKey(scope, kind, id), work{Scope: scope, Kind: kind, ID: id})
}

func nameTag(tags map[string]string) *string {
	if name, ok := tags["Name"]; ok {
		return new(name)
	}

	return nil
}

func newToken(token *string) string {
	if token != nil {
		return *token
	}

	return common.RandomID(32, "0123456789abcdef")
}

func (s *Service) view(ctx context.Context, fn func(storage.ReadTx, request.Scope) error) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	return s.store.View(ctx, func(tx storage.ReadTx) error { return fn(tx, scope) })
}

func (s *Service) update(ctx context.Context, fn func(storage.WriteTx, request.Scope) error) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error { return fn(tx, scope) })
}
