package lambda

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// A layer version is "layer/<name>/<zero-padded version>" with its package
// under "layer-code/<name>/<zero-padded version>"; "layer-head/<name>" holds
// the last version number published, which is never reused.
const (
	kindLayer     = "layer"
	kindLayerCode = "layer-code"
	kindLayerHead = "layer-head"
	// maxLayers is how many layers a function may use.
	maxLayers = 5
)

var (
	layerName = regexp.MustCompile(`^[a-zA-Z0-9-_]{1,140}$`)
	// layerARN matches a layer ARN, with its version when it has one.
	layerARN = regexp.MustCompile(
		`^arn:aws[a-zA-Z-]*:lambda:([a-z0-9-]+):(\d{12}):layer:([a-zA-Z0-9-_]{1,140})(?::(\d+))?$`,
	)
)

// layerVersion is one published, immutable version of a layer.
type layerVersion struct {
	Name         string
	Description  string `json:",omitempty"`
	License      string `json:",omitempty"`
	CreatedDate  string
	CodeSha256   string
	Runtimes     []string `json:",omitempty"`
	Architecture []string `json:",omitempty"`
	CodeSize     int64
	Version      int64
}

// functionLayer is a layer version a function uses.
type functionLayer struct {
	ARN      string
	CodeSize int64
}

func layerID(name string, version int64) string { return fmt.Sprintf("%s/%010d", name, version) }

func layerKey(scope request.Scope, name string, version int64) string {
	return scopeKey(scope, kindLayer, layerID(name, version))
}

func layerCodeKey(scope request.Scope, name string, version int64) string {
	return scopeKey(scope, kindLayerCode, layerID(name, version))
}

func (l *layerVersion) arn(scope request.Scope) string {
	return "arn:aws:lambda:" + scope.Region + ":" + scope.AccountID + ":layer:" + l.Name
}

func (l *layerVersion) versionARN(scope request.Scope) string {
	return l.arn(scope) + ":" + strconv.FormatInt(l.Version, 10)
}

func (l *layerVersion) listItem(scope request.Scope) *api.LayerVersionsListItem {
	return &api.LayerVersionsListItem{
		LayerVersionArn: new(l.versionARN(scope)), Version: new(l.Version), Description: new(l.Description),
		CreatedDate: new(l.CreatedDate), CompatibleRuntimes: l.Runtimes, CompatibleArchitectures: l.Architecture,
		LicenseInfo: optional(l.License),
	}
}

func (l *layerVersion) output(scope request.Scope) *api.GetLayerVersionResponse {
	return &api.GetLayerVersionResponse{
		Content:  &api.LayerVersionContentOutput{CodeSha256: new(l.CodeSha256), CodeSize: new(l.CodeSize)},
		LayerArn: new(l.arn(scope)), LayerVersionArn: new(l.versionARN(scope)), Description: new(l.Description),
		CreatedDate: new(l.CreatedDate), Version: new(l.Version), CompatibleRuntimes: l.Runtimes,
		CompatibleArchitectures: l.Architecture, LicenseInfo: optional(l.License),
	}
}

// matches reports whether the layer version suits the runtime and the
// architecture a listing filters by.
func (l *layerVersion) matches(runtime, architecture *string) bool {
	return (runtime == nil || slices.Contains(l.Runtimes, *runtime)) &&
		(architecture == nil || slices.Contains(l.Architecture, *architecture))
}

func optional(s string) *string {
	if s == "" {
		return nil
	}

	return &s
}

// layerTarget resolves a layer name or ARN in the caller's Region to its
// scope and name.
//
//nolint:gocritic // Metadata is an immutable request snapshot.
func layerTarget(m request.Metadata, raw string) (request.Scope, string, error) {
	if match := layerARN.FindStringSubmatch(raw); match != nil && match[4] == "" {
		if match[1] != m.Region {
			return m.Scope, "", invalid("The layer ARN is not in the %s region", m.Region)
		}

		return request.Scope{AccountID: match[2], Region: match[1]}, match[3], nil
	}

	if !layerName.MatchString(raw) {
		return m.Scope, "", invalid("Invalid layer name %s", raw)
	}

	return m.Scope, raw, nil
}

func loadLayer(tx storage.ReadTx, scope request.Scope, name string, version int64) (*layerVersion, error) {
	var l layerVersion
	found, err := readJSON(tx, layerKey(scope, name, version), &l)
	if err == nil && !found {
		err = notFound("Layer version arn:aws:lambda:" + scope.Region + ":" + scope.AccountID + ":layer:" + name +
			":" + strconv.FormatInt(version, 10))
	}

	return &l, err
}

// PublishLayerVersion publishes a Zip package, from ZipFile or S3, as the
// next version of a layer.
func (s *Service) PublishLayerVersion(
	ctx context.Context,
	in *api.PublishLayerVersionRequest,
) (*api.PublishLayerVersionResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	scope, name, err := layerTarget(m, common.Value(in.LayerName))
	if err != nil {
		return nil, err
	}

	if scope != m.Scope {
		return nil, accessDenied("The layer belongs to another account")
	}

	c := in.Content
	if c == nil {
		return nil, invalid("Content is required")
	}

	code, err := s.deploymentPackage(ctx, c.ZipFile, c.S3Bucket, c.S3Key, c.S3ObjectVersion)
	if err != nil {
		return nil, err
	}

	if zipErr := checkZip(code); zipErr != nil {
		return nil, zipErr
	}

	if len(in.CompatibleRuntimes) > 15 || len(in.CompatibleArchitectures) > 2 ||
		len(common.Value(in.Description)) > 256 {
		return nil, invalid("Too many CompatibleRuntimes or CompatibleArchitectures, or Description is too long")
	}

	for _, r := range in.CompatibleRuntimes {
		if _, ok := Image(r); !ok {
			return nil, invalid("The runtime parameter of %s is not supported", r)
		}
	}

	for _, a := range in.CompatibleArchitectures {
		if a != archX86 && a != archARM {
			return nil, invalid("The architecture %s is not supported", a)
		}
	}

	sum := sha256.Sum256(code)
	l := layerVersion{
		Name: name, Description: common.Value(in.Description), License: common.Value(in.LicenseInfo),
		CreatedDate: s.now().UTC().Format(timeFormat), CodeSha256: base64.StdEncoding.EncodeToString(sum[:]),
		Runtimes: in.CompatibleRuntimes, Architecture: in.CompatibleArchitectures, CodeSize: int64(len(code)),
	}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		head := scopeKey(m.Scope, kindLayerHead, name)
		if data, ok := tx.Get(head); ok {
			last, parseErr := strconv.ParseInt(string(data), 10, 64)
			if parseErr != nil {
				return parseErr
			}

			l.Version = last
		}

		l.Version++
		tx.Put(head, []byte(strconv.FormatInt(l.Version, 10)))
		tx.Put(layerCodeKey(m.Scope, name, l.Version), code)
		return writeJSON(tx, layerKey(m.Scope, name, l.Version), &l)
	})
	if err != nil {
		return nil, err
	}

	out := l.output(m.Scope)
	return &api.PublishLayerVersionResponse{
		Content: out.Content, LayerArn: out.LayerArn, LayerVersionArn: out.LayerVersionArn,
		Description: out.Description, CreatedDate: out.CreatedDate, Version: out.Version,
		CompatibleRuntimes: out.CompatibleRuntimes, CompatibleArchitectures: out.CompatibleArchitectures,
		LicenseInfo: out.LicenseInfo,
	}, nil
}

func (s *Service) getLayerVersion(
	ctx context.Context,
	scope request.Scope,
	name string,
	version int64,
) (*api.GetLayerVersionResponse, error) {
	var l *layerVersion
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		var loadErr error
		l, loadErr = loadLayer(tx, scope, name, version)
		return loadErr
	})
	if err != nil {
		return nil, err
	}

	return l.output(scope), nil
}

func (s *Service) GetLayerVersion(
	ctx context.Context,
	in *api.GetLayerVersionRequest,
) (*api.GetLayerVersionResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	scope, name, err := layerTarget(m, common.Value(in.LayerName))
	if err != nil {
		return nil, err
	}

	return s.getLayerVersion(ctx, scope, name, common.Value(in.VersionNumber))
}

func (s *Service) GetLayerVersionByArn(
	ctx context.Context,
	in *api.GetLayerVersionByArnRequest,
) (*api.GetLayerVersionResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	scope, name, version, err := parseLayerVersionARN(m, common.Value(in.Arn))
	if err != nil {
		return nil, err
	}

	return s.getLayerVersion(ctx, scope, name, version)
}

// parseLayerVersionARN splits a layer version ARN in the caller's Region.
//
//nolint:gocritic // Metadata is an immutable request snapshot.
func parseLayerVersionARN(m request.Metadata, arn string) (scope request.Scope, name string, version int64, err error) {
	match := layerARN.FindStringSubmatch(arn)
	if match == nil || match[4] == "" {
		return m.Scope, "", 0, invalid("Layer version ARN %s is not valid", arn)
	}

	if match[1] != m.Region {
		return m.Scope, "", 0, invalid("Layers must be in the function's region, %s", m.Region)
	}

	version, err = strconv.ParseInt(match[4], 10, 64)
	if err != nil {
		return m.Scope, "", 0, invalid("Layer version ARN %s is not valid", arn)
	}

	return request.Scope{AccountID: match[2], Region: match[1]}, match[3], version, nil
}

// DeleteLayerVersion deletes one version of a layer. Functions that use it
// fail to run until they stop using it.
func (s *Service) DeleteLayerVersion(
	ctx context.Context,
	in *api.DeleteLayerVersionRequest,
) (*api.DeleteLayerVersionOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	scope, name, err := layerTarget(m, common.Value(in.LayerName))
	if err != nil {
		return nil, err
	}

	if scope != m.Scope {
		return nil, accessDenied("The layer belongs to another account")
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		tx.Delete(layerKey(scope, name, common.Value(in.VersionNumber)))
		tx.Delete(layerCodeKey(scope, name, common.Value(in.VersionNumber)))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteLayerVersionOutput{}, nil
}

// layerPage reads a listing's page size and the key it continues after.
func layerPage(marker *string, maxItems *int64) (limit int64, start string, err error) {
	if limit, err = pageSize(maxItems, listLimit); err != nil {
		return 0, "", err
	}

	start, err = after(marker)
	return limit, start, err
}

// ListLayerVersions lists a layer's versions, newest first.
func (s *Service) ListLayerVersions(
	ctx context.Context,
	in *api.ListLayerVersionsRequest,
) (*api.ListLayerVersionsResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	scope, name, err := layerTarget(m, common.Value(in.LayerName))
	if err != nil {
		return nil, err
	}

	limit, start, err := layerPage(in.Marker, in.MaxItems)
	if err != nil {
		return nil, err
	}

	out := &api.ListLayerVersionsResponse{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		keys := tx.Keys(scopeKey(scope, kindLayer, name+"/"))
		slices.Reverse(keys)
		for _, k := range keys {
			if start != "" && k >= start {
				continue
			}

			var l layerVersion
			if _, readErr := readJSON(tx, k, &l); readErr != nil {
				return readErr
			}

			if !l.matches(in.CompatibleRuntime, in.CompatibleArchitecture) {
				continue
			}

			if int64(len(out.LayerVersions)) == limit {
				out.NextMarker = nextMarker(start)
				return nil
			}

			out.LayerVersions = append(out.LayerVersions, *l.listItem(scope))
			start = k
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// ListLayers lists the caller's layers by name, each with its newest version
// that matches the filters.
func (s *Service) ListLayers(ctx context.Context, in *api.ListLayersRequest) (*api.ListLayersResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	limit, start, err := layerPage(in.Marker, in.MaxItems)
	if err != nil {
		return nil, err
	}

	out := &api.ListLayersResponse{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, head := range tx.Keys(scopeKey(m.Scope, kindLayerHead, "")) {
			name := strings.TrimPrefix(head, scopeKey(m.Scope, kindLayerHead, ""))
			if name <= start {
				continue
			}

			keys := tx.Keys(scopeKey(m.Scope, kindLayer, name+"/"))
			slices.Reverse(keys)
			for _, k := range keys {
				var l layerVersion
				if _, readErr := readJSON(tx, k, &l); readErr != nil {
					return readErr
				}

				if !l.matches(in.CompatibleRuntime, in.CompatibleArchitecture) {
					continue
				}

				if int64(len(out.Layers)) == limit {
					out.NextMarker = nextMarker(start)
					return nil
				}

				out.Layers = append(out.Layers, api.LayersListItem{
					LayerName: new(name), LayerArn: new(l.arn(m.Scope)), LatestMatchingVersion: l.listItem(m.Scope),
				})
				start = name
				break
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// functionLayers resolves the layer version ARNs a function is given: at most
// five, each existing in the function's Region.
func (s *Service) functionLayers(ctx context.Context, m *request.Metadata, arns []string) ([]functionLayer, error) {
	if len(arns) > maxLayers {
		return nil, invalid("A function can have at most %d layers", maxLayers)
	}

	var layers []functionLayer
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, arn := range arns {
			scope, name, version, parseErr := parseLayerVersionARN(*m, arn)
			if parseErr != nil {
				return parseErr
			}

			l, loadErr := loadLayer(tx, scope, name, version)
			if e, ok := errors.AsType[*awserror.Error](loadErr); ok && e.Code == "ResourceNotFoundException" {
				return invalid("Layer version %s does not exist.", arn)
			}

			if loadErr != nil {
				return loadErr
			}

			layers = append(layers, functionLayer{ARN: arn, CodeSize: l.CodeSize})
		}

		return nil
	})
	return layers, err
}

// layerCode reads the packages of a function's layers, in order.
func layerCode(tx storage.ReadTx, m *request.Metadata, layers []functionLayer) ([][]byte, error) {
	codes := make([][]byte, 0, len(layers))
	for _, l := range layers {
		scope, name, version, err := parseLayerVersionARN(*m, l.ARN)
		if err != nil {
			return nil, err
		}

		code, ok := tx.Get(layerCodeKey(scope, name, version))
		if !ok {
			return nil, awserror.New("ResourceNotFoundException", "Layer version "+l.ARN+" no longer exists")
		}

		codes = append(codes, code)
	}

	return codes, nil
}

// layersOf resolves the layers a function is given. Image functions bring
// their dependencies in the image, so they take none.
func (s *Service) layersOf(
	ctx context.Context,
	m *request.Metadata,
	fn *function,
	arns []string,
) ([]functionLayer, error) {
	if len(arns) > 0 && fn.PackageType == packageImage {
		return nil, invalid("Layers are not supported for Image functions")
	}

	return s.functionLayers(ctx, m, arns)
}
