// Package lambda implements Lambda functions and SQS and DynamoDB stream event
// source mappings without HTTP dependencies. A Runtime runs function code.
//
// Each function is one record under "<account>/<region>/lambda/function/<name>"
// with its deployment package under ".../lambda/code/<name>"; event source
// mappings are records under ".../lambda/mapping/<uuid>".
package lambda

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/delivery"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/services/dynamodb"
	"github.com/amjadjibon/tinystack/internal/services/kinesis"
	"github.com/amjadjibon/tinystack/internal/services/logs"
	"github.com/amjadjibon/tinystack/internal/services/sqs"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	latest         = "$LATEST"
	packageZip     = "Zip"
	packageImage   = "Image"
	archX86        = "x86_64"
	archARM        = "arm64"
	defaultTimeout = 3
	maxTimeout     = 900
	defaultMemory  = 128
	maxMemory      = 10240
	maxEnvironment = 4096
	maxUnzipped    = 262144000
	listLimit      = 50
	timeFormat     = "2006-01-02T15:04:05.000-0700"
	// placeholderKey signs SDK calls made by function code whose creator was
	// unsigned or has no configured secret key; TinyStack maps unknown access
	// keys to the default account when it does not verify signatures.
	placeholderKey = "tinystack"
)

var _ api.API = (*Service)(nil)

type Service struct {
	store   storage.Store
	queues  *sqs.Service
	streams *dynamodb.Streams
	// kinesis reads Kinesis streams for event source mappings; see UseKinesis.
	kinesis *kinesis.Service
	logs    *logs.Service
	runtime Runtime
	logger  *slog.Logger
	// secretKeys are the configured SigV4 secrets by access key; function code
	// signs its SDK calls with its creator's.
	secretKeys map[string]string
	// deliveries records asynchronous invocations and mapping batches.
	deliveries *delivery.Log
	// caller sends asynchronous invocation results to destinations; see
	// UseCaller.
	caller *catalog.Caller
	// objects reads deployment packages from S3; see UseObjects.
	objects ObjectReader
	now     func() time.Time
	// bg runs background work (async invocations and pollers) until Stop.
	bg      *background.Group
	pollers map[string]*poller
	// running counts each function's invocations in flight, which its
	// reserved concurrency caps; slots guards it.
	running map[string]int
	mu      sync.Mutex
	slots   sync.Mutex
}

// New returns a Lambda service that runs code with runtime, reads SQS and
// DynamoDB stream event sources from queues and streams, and writes each
// invocation's log to logGroups.
// secretKeys holds the configured SigV4 secrets by access key, if any.
func New(
	store storage.Store,
	queues *sqs.Service,
	streams *dynamodb.Streams,
	logGroups *logs.Service,
	runtime Runtime,
	logger *slog.Logger,
	secretKeys map[string]string,
	deliveries *delivery.Log,
) *Service {
	if logger == nil {
		logger = slog.Default()
	}

	return &Service{
		store:      store,
		queues:     queues,
		streams:    streams,
		logs:       logGroups,
		runtime:    runtime,
		logger:     logger,
		secretKeys: secretKeys,
		deliveries: deliveries,
		now:        time.Now,
		bg:         background.New(),
		pollers:    map[string]*poller{},
		running:    map[string]int{},
	}
}

// ObjectReader returns the bytes of an S3 object, or of one version when
// version is not empty, in the caller's account and region.
type ObjectReader func(ctx context.Context, bucket, key, version string) ([]byte, error)

// UseObjects lets functions take their code from S3. S3 notifies Lambda, so it
// is created after Lambda and registered here; call it before serving requests.
func (s *Service) UseObjects(read ObjectReader) { s.objects = read }

// deploymentPackage returns the Zip package a request gives: ZipFile, or the
// S3 object that S3Bucket, S3Key, and S3ObjectVersion name, which is copied
// when the request is made, so later changes to the object do not affect the
// function.
func (s *Service) deploymentPackage(ctx context.Context, zipFile []byte, bucket, key, version *string) ([]byte, error) {
	if common.Value(bucket) == hotReloadBucket {
		return nil, invalid("Hot reload is not supported for layers")
	}

	if bucket == nil && key == nil && version == nil {
		return zipFile, nil
	}

	switch {
	case len(zipFile) > 0:
		return nil, invalid("Please don't provide both S3 and ZipFile")
	case common.Value(bucket) == "" || common.Value(key) == "":
		return nil, invalid("Please provide both S3Bucket and S3Key")
	case s.objects == nil:
		return nil, invalid("Code from S3 is not available")
	}

	data, err := s.objects(ctx, *bucket, *key, common.Value(version))
	if s3Err, ok := errors.AsType[*awserror.Error](err); ok {
		return nil, invalid("Error occurred while GetObject. S3 Error Code: %s. S3 Error Message: %s",
			s3Err.Code, s3Err.Message)
	}

	return data, err
}

// Stop ends pollers and async invocations, waits for them, and then releases
// every runtime environment. Concurrent and later calls wait for the first.
func (s *Service) Stop() {
	if s.bg.Stop() {
		s.runtime.Close()
	}
}

// Reset ends every event source mapping's poller and removes the warm
// environments, so nothing keeps running for functions that no longer exist.
// Call it before the store is cleared.
func (s *Service) Reset(context.Context) {
	s.mu.Lock()
	ids := slices.Collect(maps.Keys(s.pollers))
	s.mu.Unlock()
	for _, id := range ids {
		<-s.stopPoller(id)
	}

	common.ResetRuntime(s.runtime)
}

// function is the stored configuration of one function: $LATEST, or a
// published version, which never changes.
type function struct {
	Environment   map[string]string `json:",omitempty"`
	Tags          map[string]string `json:",omitempty"`
	Name          string
	Runtime       string `json:",omitempty"`
	Role          string
	Handler       string `json:",omitempty"`
	Description   string `json:",omitempty"`
	PackageType   string
	ImageURI      string `json:",omitempty"`
	HotReloadPath string `json:",omitempty"`
	Architecture  string `json:",omitempty"`
	CodeSha256    string
	LastModified  string
	// Version is the number of a published version, or empty for $LATEST.
	Version string `json:",omitempty"`
	// Source is the $LATEST revision a version was published from.
	Source string `json:",omitempty"`
	// RevisionID changes with every update of the code or configuration.
	RevisionID string `json:",omitempty"`
	AccessKey  string `json:",omitempty"`
	// Layers are the layer versions the function uses, in order.
	Layers     []functionLayer `json:",omitempty"`
	CodeSize   int64
	Timeout    int64
	MemorySize int64
	// Published is the last version number published, kept on $LATEST.
	Published int64 `json:",omitempty"`
}

// version returns the function's version: its number or $LATEST.
func (fn *function) version() string {
	if fn.Version == "" {
		return latest
	}

	return fn.Version
}

// arn is the ARN of the function's version: unqualified for $LATEST.
func (fn *function) arn(scope request.Scope) string {
	return qualifiedARN(scope, fn.Name, fn.Version)
}

func invalid(format string, args ...any) error {
	return awserror.New("InvalidParameterValueException", fmt.Sprintf(format, args...))
}

func notFound(what string) error {
	return awserror.New("ResourceNotFoundException", what+" not found")
}

func accessDenied(message string) error {
	return &awserror.Error{Code: "AccessDeniedException", Message: message, StatusCode: http.StatusForbidden}
}

func conflict(message string) error { return awserror.New("ResourceConflictException", message) }

// Storage kinds: functions are "<scope>/lambda/function/<name>" with their
// code under "code/<name>", published versions "version/<name>/<number>" with
// their code under "version-code/<name>/<number>", aliases
// "alias/<name>/<alias>", and event source mappings "mapping/<uuid>".
const (
	kindFunction    = "function"
	kindCode        = "code"
	kindVersion     = "version"
	kindVersionCode = "version-code"
	kindAlias       = "alias"
	kindMapping     = "mapping"
)

func functionARN(scope request.Scope, name string) string {
	return "arn:aws:lambda:" + scope.Region + ":" + scope.AccountID + ":function:" + name
}

// qualifiedARN is the ARN of a function at a version or alias; an empty
// qualifier leaves it unqualified.
func qualifiedARN(scope request.Scope, name, qualifier string) string {
	if qualifier == "" {
		return functionARN(scope, name)
	}

	return functionARN(scope, name) + ":" + qualifier
}

var (
	functionName     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	qualifierPattern = regexp.MustCompile(`^(\$LATEST|[a-zA-Z0-9_-]{1,128})$`)
	roleARN          = regexp.MustCompile(`^arn:(aws[a-zA-Z-]*)?:iam::\d{12}:role/?[a-zA-Z0-9+=,.@_/-]+$`)
	accountID        = regexp.MustCompile(`^\d{12}$`)
	envName          = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*$`)
)

// reserved lists environment variables Lambda sets itself.
var reserved = []string{
	"_HANDLER", "_X_AMZN_TRACE_ID", "AWS_DEFAULT_REGION", "AWS_REGION", "AWS_EXECUTION_ENV",
	"AWS_LAMBDA_FUNCTION_NAME", "AWS_LAMBDA_FUNCTION_MEMORY_SIZE", "AWS_LAMBDA_FUNCTION_VERSION",
	"AWS_LAMBDA_INITIALIZATION_TYPE", "AWS_LAMBDA_LOG_GROUP_NAME", "AWS_LAMBDA_LOG_STREAM_NAME",
	"AWS_ACCESS_KEY", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
	"AWS_LAMBDA_RUNTIME_API", "LAMBDA_TASK_ROOT", "LAMBDA_RUNTIME_DIR",
}

// resolveName maps a function name, partial ARN ("<account>:function:<name>"),
// or ARN, any of them optionally qualified with a version or alias, to a
// function in the caller's scope and the qualifier.
//
//nolint:gocritic // Metadata is an immutable request snapshot.
func resolveName(m request.Metadata, raw string) (name, qualifier string, err error) {
	parts := strings.Split(raw, ":")
	switch {
	case len(parts) <= 2:
		name = parts[0]
		if len(parts) == 2 {
			qualifier = parts[1]
		}
	case len(parts) >= 7 && len(parts) <= 8 && parts[0] == "arn" && parts[2] == "lambda" && parts[5] == "function":
		if parts[3] != m.Region {
			return "", "", invalid("The function ARN is not in the %s region", m.Region)
		}

		if parts[4] != m.AccountID {
			return "", "", accessDenied("The function belongs to another account")
		}

		name = parts[6]
		if len(parts) == 8 {
			qualifier = parts[7]
		}
	case len(parts) >= 3 && len(parts) <= 4 && accountID.MatchString(parts[0]) && parts[1] == "function":
		if parts[0] != m.AccountID {
			return "", "", accessDenied("The function belongs to another account")
		}

		name = parts[2]
		if len(parts) == 4 {
			qualifier = parts[3]
		}
	default:
		return "", "", invalid("Invalid function name or ARN: %s", raw)
	}

	if !functionName.MatchString(name) {
		return "", "", invalid("Invalid function name: %s", raw)
	}

	if len(parts) == 2 || len(parts) == 4 || len(parts) == 8 {
		if !qualifierPattern.MatchString(qualifier) {
			return "", "", invalid("Invalid qualifier: %s", raw)
		}
	}

	return name, qualifier, nil
}

// withQualifier combines the qualifier of a function name with a Qualifier
// parameter, which must agree when both are set.
func withQualifier(fromName string, param *string) (string, error) {
	if param == nil {
		return fromName, nil
	}

	if !qualifierPattern.MatchString(*param) {
		return "", invalid("Invalid qualifier: %s", *param)
	}

	if fromName != "" && fromName != *param {
		return "", invalid("The derived qualifier from the function name does not match the specified qualifier.")
	}

	return *param, nil
}

func loadFunction(tx storage.ReadTx, key string) (*function, bool, error) {
	data, ok := tx.Get(key)
	if !ok {
		return nil, false, nil
	}

	var fn function
	if err := json.Unmarshal(data, &fn); err != nil {
		return nil, false, fmt.Errorf("decode function: %w", err)
	}

	return &fn, true, nil
}

// readJSON decodes the record under key into v, reporting whether there is one.
func readJSON(tx storage.ReadTx, key string, v any) (bool, error) {
	data, ok := tx.Get(key)
	if !ok {
		return false, nil
	}

	if err := json.Unmarshal(data, v); err != nil {
		return false, fmt.Errorf("decode %s: %w", key, err)
	}

	return true, nil
}

func writeJSON(tx storage.WriteTx, key string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode %s: %w", key, err)
	}

	tx.Put(key, data)
	return nil
}

// configuration renders the stored function as the API reports it.
func (fn *function) configuration(scope request.Scope) api.FunctionConfiguration {
	cfg := api.FunctionConfiguration{
		FunctionName:     new(fn.Name),
		FunctionArn:      new(fn.arn(scope)),
		Role:             new(fn.Role),
		CodeSize:         new(fn.CodeSize),
		Description:      new(fn.Description),
		Timeout:          new(fn.Timeout),
		MemorySize:       new(fn.MemorySize),
		LastModified:     new(fn.LastModified),
		CodeSha256:       new(fn.CodeSha256),
		Version:          new(fn.version()),
		State:            new("Active"),
		LastUpdateStatus: new("Successful"),
		PackageType:      new(fn.PackageType),
		Architectures:    []string{archX86},
	}

	if fn.Runtime != "" {
		cfg.Runtime, cfg.Handler = new(fn.Runtime), new(fn.Handler)
	}

	if fn.Architecture != "" {
		cfg.Architectures = []string{fn.Architecture}
	}

	if fn.RevisionID != "" {
		cfg.RevisionId = new(fn.RevisionID)
	}

	if len(fn.Environment) > 0 {
		cfg.Environment = &api.EnvironmentResponse{Variables: fn.Environment}
	}

	for _, l := range fn.Layers {
		cfg.Layers = append(cfg.Layers, api.Layer{Arn: new(l.ARN), CodeSize: new(l.CodeSize)})
	}

	return cfg
}

func checkEnvironment(env *api.Environment) error {
	if env == nil {
		return nil
	}

	size := 0
	var used []string
	for k, v := range env.Variables {
		if !envName.MatchString(k) {
			return invalid("Environment variable name %s is invalid", k)
		}

		if slices.Contains(reserved, k) {
			used = append(used, k)
		}

		size += len(k) + len(v)
	}

	if len(used) > 0 {
		sort.Strings(used)
		return invalid(
			"Lambda was unable to configure your environment variables because the environment variables you "+
				"have provided contains reserved keys that are currently not supported for modification. "+
				"Reserved keys used in this request: %s",
			strings.Join(used, ","),
		)
	}

	if size > maxEnvironment {
		return invalid("Lambda was unable to configure your environment variables because the environment "+
			"variables you have provided exceeded the 4KB limit. String measured: %d", size)
	}

	return nil
}

// checkZip validates a deployment package.
func checkZip(code []byte) error {
	archive, err := zip.NewReader(bytes.NewReader(code), int64(len(code)))
	if err != nil {
		return invalid("Could not unzip uploaded file. Please check your file, then try to upload again.")
	}

	var size uint64
	for _, f := range archive.File {
		size += f.UncompressedSize64
	}

	if size > maxUnzipped {
		return invalid("Unzipped size must be smaller than %d bytes", maxUnzipped)
	}

	return nil
}

// settings are the configuration fields CreateFunction and
// UpdateFunctionConfiguration share; a nil field keeps its current value.
type settings struct {
	Environment *api.Environment
	Role        *string
	Handler     *string
	Description *string
	Runtime     *string
	Timeout     *int64
	MemorySize  *int64
}

// configure validates and applies c to fn.
func (fn *function) configure(c *settings) error {
	if c.Role != nil {
		fn.Role = *c.Role
	}

	if !roleARN.MatchString(fn.Role) {
		return invalid("The role defined for the function cannot be assumed by Lambda.")
	}

	if c.Description != nil {
		if fn.Description = *c.Description; len(fn.Description) > 256 {
			return invalid("Description must be at most 256 characters")
		}
	}

	if c.Timeout != nil {
		if fn.Timeout = *c.Timeout; fn.Timeout < 1 || fn.Timeout > maxTimeout {
			return invalid("Timeout must be between 1 and %d seconds", maxTimeout)
		}
	}

	if c.MemorySize != nil {
		if fn.MemorySize = *c.MemorySize; fn.MemorySize < defaultMemory || fn.MemorySize > maxMemory {
			return invalid("MemorySize must be between %d and %d MB", defaultMemory, maxMemory)
		}
	}

	if c.Environment != nil {
		if err := checkEnvironment(c.Environment); err != nil {
			return err
		}

		fn.Environment = nil
		if len(c.Environment.Variables) > 0 {
			fn.Environment = c.Environment.Variables
		}
	}

	if c.Runtime != nil {
		fn.Runtime = *c.Runtime
	}

	if c.Handler != nil {
		fn.Handler = *c.Handler
	}

	if fn.PackageType == packageImage {
		if fn.Runtime != "" || fn.Handler != "" {
			return invalid("Runtime and Handler are not supported for Image functions")
		}

		return nil
	}

	if _, ok := Image(fn.Runtime); !ok {
		return invalid("The runtime parameter of %s is not supported", fn.Runtime)
	}

	if fn.Handler == "" || strings.ContainsAny(fn.Handler, " \t\r\n") || len(fn.Handler) > 128 {
		return invalid("Handler is required for Zip functions and must not contain whitespace")
	}

	return nil
}

// setArchitecture applies an Architectures parameter, which holds at most one.
func (fn *function) setArchitecture(architectures []string) error {
	switch {
	case len(architectures) > 1:
		return invalid("Architectures must hold one architecture")
	case len(architectures) == 1 && architectures[0] != archX86 && architectures[0] != archARM:
		return invalid("Architecture %s is not supported", architectures[0])
	case len(architectures) == 1:
		fn.Architecture = architectures[0]
	}

	return nil
}

// setCode validates a Zip package or image URI for fn's package type and
// records its size and hash. It returns the package bytes of a Zip function.
func (fn *function) setCode(zipFile []byte, imageURI *string, directory string) ([]byte, error) {
	sum := sha256.New()
	fn.HotReloadPath = directory
	if directory != "" {
		if fn.PackageType != packageZip || imageURI != nil {
			return nil, invalid("Hot reload requires a Zip function without ImageUri")
		}

		sum.Write([]byte(directory))
		fn.CodeSize = 0
		fn.CodeSha256 = base64.StdEncoding.EncodeToString(sum.Sum(nil))
		return nil, nil
	}

	if fn.PackageType == packageImage {
		if common.Value(imageURI) == "" || len(zipFile) > 0 {
			return nil, invalid("Image functions need Code.ImageUri")
		}

		fn.ImageURI = *imageURI
		sum.Write([]byte(fn.ImageURI))
		fn.CodeSha256 = base64.StdEncoding.EncodeToString(sum.Sum(nil))
		return nil, nil
	}

	if imageURI != nil || len(zipFile) == 0 {
		return nil, invalid("Zip functions need Code.ZipFile")
	}

	if err := checkZip(zipFile); err != nil {
		return nil, err
	}

	sum.Write(zipFile)
	fn.CodeSize = int64(len(zipFile))
	fn.CodeSha256 = base64.StdEncoding.EncodeToString(sum.Sum(nil))
	return zipFile, nil
}

// newFunction validates CreateFunction input into a function record and its
// deployment package; zipFile is the Zip package the request gives.
func newFunction(in *api.CreateFunctionRequest, zipFile []byte, directory string) (*function, []byte, error) {
	fn := &function{
		Name:        common.Value(in.FunctionName),
		PackageType: common.Value(in.PackageType),
		Timeout:     defaultTimeout,
		MemorySize:  defaultMemory,
		RevisionID:  uuid.NewV4().String(),
	}

	if !functionName.MatchString(fn.Name) {
		return nil, nil, invalid("Invalid function name: %s", fn.Name)
	}

	switch fn.PackageType {
	case "":
		fn.PackageType = packageZip
	case packageZip, packageImage:
	default:
		return nil, nil, invalid("PackageType must be Zip or Image")
	}

	err := fn.configure(&settings{
		Role:        in.Role,
		Handler:     in.Handler,
		Description: in.Description,
		Runtime:     in.Runtime,
		Timeout:     in.Timeout,
		MemorySize:  in.MemorySize,
		Environment: in.Environment,
	})
	if err != nil {
		return nil, nil, err
	}

	if guardErr := fn.setArchitecture(in.Architectures); guardErr != nil {
		return nil, nil, guardErr
	}

	if err = tagging.Check(in.Tags); err != nil {
		return nil, nil, invalid("Invalid tags: %v", err)
	}

	fn.Tags = in.Tags
	if in.Code == nil {
		return nil, nil, invalid("Code is required")
	}

	code, err := fn.setCode(zipFile, in.Code.ImageUri, directory)
	if err != nil {
		return nil, nil, err
	}

	return fn, code, nil
}

func (s *Service) CreateFunction(
	ctx context.Context,
	in *api.CreateFunctionRequest,
) (*api.FunctionConfiguration, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var zipFile []byte
	var directory string
	if in.Code != nil {
		zipFile, directory, err = s.functionCode(ctx, in.Code.ZipFile, in.Code.S3Bucket, in.Code.S3Key,
			in.Code.S3ObjectVersion)
		if err != nil {
			return nil, err
		}
	}

	fn, code, err := newFunction(in, zipFile, directory)
	if err != nil {
		return nil, err
	}

	if fn.Layers, err = s.layersOf(ctx, &m, fn, in.Layers); err != nil {
		return nil, err
	}

	fn.AccessKey = m.AccessKey
	fn.LastModified = s.now().UTC().Format(timeFormat)
	key := m.Key("lambda", kindFunction, fn.Name)
	created := fn
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, exists := tx.Get(key); exists {
			return conflict("Function already exist: " + fn.Name)
		}

		if code != nil {
			tx.Put(m.Key("lambda", kindCode, fn.Name), code)
		}

		if writeErr := writeJSON(tx, key, fn); writeErr != nil || !common.Value(in.Publish) {
			return writeErr
		}

		var publishErr error
		created, publishErr = publish(tx, m.Scope, fn, nil, fn.LastModified)
		return publishErr
	})
	if err != nil {
		return nil, err
	}

	cfg := created.configuration(m.Scope)
	return &cfg, nil
}

// load reads a function of the caller at the qualifier its name carries.
func (s *Service) load(ctx context.Context, raw *string) (request.Metadata, *function, error) {
	return s.loadAt(ctx, raw, nil)
}

// loadAt reads a function of the caller at the qualifier its name carries or
// qualifier gives. The tags of a version are its function's.
func (s *Service) loadAt(ctx context.Context, raw, qualifier *string) (request.Metadata, *function, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return m, nil, err
	}

	name, q, err := resolveName(m, common.Value(raw))
	if err != nil {
		return m, nil, err
	}

	if q, err = withQualifier(q, qualifier); err != nil {
		return m, nil, err
	}

	var fn *function
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var loadErr error
		if fn, _, loadErr = resolve(tx, m.Scope, name, q); loadErr != nil || fn.Version == "" {
			return loadErr
		}

		base, _, loadErr := resolve(tx, m.Scope, name, "")
		if loadErr == nil {
			fn.Tags = base.Tags
		}

		return loadErr
	})
	return m, fn, err
}

// GetFunctionConfiguration returns the configuration of $LATEST, a version,
// or the version an alias points to.
func (s *Service) GetFunctionConfiguration(
	ctx context.Context,
	in *api.GetFunctionConfigurationRequest,
) (*api.FunctionConfiguration, error) {
	m, fn, err := s.loadAt(ctx, in.FunctionName, in.Qualifier)
	if err != nil {
		return nil, err
	}

	cfg := fn.configuration(m.Scope)
	return &cfg, nil
}

func (s *Service) GetFunction(ctx context.Context, in *api.GetFunctionRequest) (*api.GetFunctionResponse, error) {
	m, fn, err := s.loadAt(ctx, in.FunctionName, in.Qualifier)
	if err != nil {
		return nil, err
	}

	cfg := fn.configuration(m.Scope)
	out := &api.GetFunctionResponse{
		Configuration: &cfg,
		Code:          &api.FunctionCodeLocation{RepositoryType: new("S3")},
		Tags:          fn.Tags,
	}
	if fn.HotReloadPath != "" {
		out.Code = &api.FunctionCodeLocation{RepositoryType: new("hot-reload")}
	}

	if fn.PackageType == packageImage {
		out.Code = &api.FunctionCodeLocation{RepositoryType: new("ECR"), ImageUri: new(fn.ImageURI)}
	}

	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		n, ok, readErr := reservedConcurrency(tx, m.Scope, fn.Name)
		if ok {
			out.Concurrency = &api.Concurrency{ReservedConcurrentExecutions: new(n)}
		}

		return readErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// after decodes a pagination marker, the last name of the previous page.
func after(marker *string) (string, error) {
	if marker == nil {
		return "", nil
	}

	b, err := base64.RawURLEncoding.DecodeString(*marker)
	if err != nil || len(b) == 0 {
		return "", invalid("Invalid Marker")
	}

	return string(b), nil
}

func nextMarker(last string) *string {
	return new(base64.RawURLEncoding.EncodeToString([]byte(last)))
}

func (s *Service) ListFunctions(
	ctx context.Context,
	in *api.ListFunctionsRequest,
) (*api.ListFunctionsResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	limit := int64(listLimit)
	if in.MaxItems != nil {
		if limit = *in.MaxItems; limit < 1 || limit > listLimit {
			return nil, invalid("MaxItems must be between 1 and %d", listLimit)
		}
	}

	start, err := after(in.Marker)
	if err != nil {
		return nil, err
	}

	var fns []*function
	prefix := m.Key("lambda", kindFunction, "")
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys(prefix) {
			if strings.TrimPrefix(k, prefix) <= start {
				continue
			}

			fn, _, loadErr := loadFunction(tx, k)
			if loadErr != nil {
				return loadErr
			}

			fns = append(fns, fn)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	out := &api.ListFunctionsResponse{}
	if int64(len(fns)) > limit {
		fns = fns[:limit]
		out.NextMarker = nextMarker(fns[limit-1].Name)
	}

	for _, fn := range fns {
		out.Functions = append(out.Functions, fn.configuration(m.Scope))
	}

	return out, nil
}
