// Package lambda implements Lambda functions and SQS event source mappings
// without HTTP dependencies. A Runtime runs function code.
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
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/delivery"
	"github.com/amjadjibon/tinystack/internal/services/logs"
	"github.com/amjadjibon/tinystack/internal/services/sqs"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	latest         = "$LATEST"
	packageZip     = "Zip"
	packageImage   = "Image"
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
	logs    *logs.Service
	runtime Runtime
	logger  *slog.Logger
	// secretKeys are the configured SigV4 secrets by access key; function code
	// signs its SDK calls with its creator's.
	secretKeys map[string]string
	// deliveries records asynchronous invocations and mapping batches.
	deliveries *delivery.Log
	now        func() time.Time
	// ctx ends background work (async invocations and pollers) on Stop.
	ctx     context.Context
	cancel  context.CancelFunc
	pollers map[string]context.CancelFunc
	wg      sync.WaitGroup
	mu      sync.Mutex
	stop    sync.Once
}

// New returns a Lambda service that runs code with runtime, reads SQS event
// sources from queues, and writes each invocation's log to logGroups.
// secretKeys holds the configured SigV4 secrets by access key, if any.
func New(
	store storage.Store,
	queues *sqs.Service,
	logGroups *logs.Service,
	runtime Runtime,
	logger *slog.Logger,
	secretKeys map[string]string,
	deliveries *delivery.Log,
) *Service {
	if logger == nil {
		logger = slog.Default()
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &Service{
		store:      store,
		queues:     queues,
		logs:       logGroups,
		runtime:    runtime,
		logger:     logger,
		secretKeys: secretKeys,
		deliveries: deliveries,
		now:        time.Now,
		ctx:        ctx,
		cancel:     cancel,
		pollers:    map[string]context.CancelFunc{},
	}
}

// Stop ends pollers and async invocations, waits for them, and then releases
// every runtime environment. Concurrent and later calls wait for the first.
func (s *Service) Stop() {
	s.stop.Do(func() {
		s.mu.Lock()
		s.cancel()
		s.mu.Unlock()
		s.wg.Wait()
		s.runtime.Close()
	})
}

// spawn runs fn in the background until Stop and reports false once Stop has run.
func (s *Service) spawn(fn func(context.Context)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		return false
	}

	s.wg.Go(func() { fn(s.ctx) })
	return true
}

// function is the stored configuration of one function.
type function struct {
	Environment  map[string]string `json:",omitempty"`
	Tags         map[string]string `json:",omitempty"`
	Name         string
	Runtime      string `json:",omitempty"`
	Role         string
	Handler      string `json:",omitempty"`
	Description  string `json:",omitempty"`
	PackageType  string
	ImageURI     string `json:",omitempty"`
	Architecture string `json:",omitempty"`
	CodeSha256   string
	LastModified string
	// RevisionID changes with every update of the code or configuration.
	RevisionID string `json:",omitempty"`
	AccessKey  string `json:",omitempty"`
	CodeSize   int64
	Timeout    int64
	MemorySize int64
}

func value[T any](p *T) (v T) {
	if p != nil {
		return *p
	}

	return v
}

func invalid(format string, args ...any) error {
	return awserror.New("InvalidParameterValueException", fmt.Sprintf(format, args...))
}

func notFound(what string) error {
	return awserror.New("ResourceNotFoundException", what+" not found")
}

func conflict(message string) error { return awserror.New("ResourceConflictException", message) }

func identity(ctx context.Context) (request.Metadata, error) {
	m, ok := request.FromContext(ctx)
	if !ok || m.AccountID == "" || m.Region == "" {
		return m, fmt.Errorf("missing request identity")
	}

	return m, nil
}

// Storage kinds: functions are "<scope>/lambda/function/<name>" and event
// source mappings "<scope>/lambda/mapping/<uuid>".
const (
	kindFunction = "function"
	kindMapping  = "mapping"
)

func scopeKey(m request.Scope, kind, name string) string {
	return m.AccountID + "/" + m.Region + "/lambda/" + kind + "/" + name
}

func functionARN(scope request.Scope, name string) string {
	return "arn:aws:lambda:" + scope.Region + ":" + scope.AccountID + ":function:" + name
}

var (
	functionName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	roleARN      = regexp.MustCompile(`^arn:(aws[a-zA-Z-]*)?:iam::\d{12}:role/?[a-zA-Z0-9+=,.@_/-]+$`)
	accountID    = regexp.MustCompile(`^\d{12}$`)
	envName      = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*$`)
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
// or ARN to a function in the caller's scope. Only the $LATEST qualifier
// exists because TinyStack does not publish versions or aliases.
//
//nolint:gocritic // Metadata is an immutable request snapshot.
func resolveName(m request.Metadata, raw string) (string, error) {
	parts := strings.Split(raw, ":")
	var name, qualifier string
	switch {
	case len(parts) <= 2:
		name = parts[0]
		if len(parts) == 2 {
			qualifier = parts[1]
		}
	case len(parts) >= 7 && len(parts) <= 8 && parts[0] == "arn" && parts[2] == "lambda" && parts[5] == "function":
		if parts[3] != m.Region {
			return "", invalid("The function ARN is not in the %s region", m.Region)
		}

		if parts[4] != m.AccountID {
			return "", &awserror.Error{
				Code:       "AccessDeniedException",
				Message:    "The function belongs to another account",
				StatusCode: http.StatusForbidden,
			}
		}

		name = parts[6]
		if len(parts) == 8 {
			qualifier = parts[7]
		}
	case len(parts) >= 3 && len(parts) <= 4 && accountID.MatchString(parts[0]) && parts[1] == "function":
		if parts[0] != m.AccountID {
			return "", &awserror.Error{
				Code:       "AccessDeniedException",
				Message:    "The function belongs to another account",
				StatusCode: http.StatusForbidden,
			}
		}

		name = parts[2]
		if len(parts) == 4 {
			qualifier = parts[3]
		}
	default:
		return "", invalid("Invalid function name or ARN: %s", raw)
	}

	if !functionName.MatchString(name) {
		return "", invalid("Invalid function name: %s", raw)
	}

	if qualifier != "" && qualifier != latest {
		return "", notFound("Function " + functionARN(m.Scope, name) + ":" + qualifier)
	}

	return name, nil
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
		FunctionArn:      new(functionARN(scope, fn.Name)),
		Role:             new(fn.Role),
		CodeSize:         new(fn.CodeSize),
		Description:      new(fn.Description),
		Timeout:          new(fn.Timeout),
		MemorySize:       new(fn.MemorySize),
		LastModified:     new(fn.LastModified),
		CodeSha256:       new(fn.CodeSha256),
		Version:          new(latest),
		State:            new("Active"),
		LastUpdateStatus: new("Successful"),
		PackageType:      new(fn.PackageType),
		Architectures:    []string{"x86_64"},
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
	case len(architectures) == 1 && architectures[0] != "x86_64" && architectures[0] != "arm64":
		return invalid("Architecture %s is not supported", architectures[0])
	case len(architectures) == 1:
		fn.Architecture = architectures[0]
	}

	return nil
}

// setCode validates a Zip package or image URI for fn's package type and
// records its size and hash. It returns the package bytes of a Zip function.
func (fn *function) setCode(zipFile []byte, imageURI *string) ([]byte, error) {
	sum := sha256.New()
	if fn.PackageType == packageImage {
		if value(imageURI) == "" || len(zipFile) > 0 {
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
// deployment package.
func newFunction(in *api.CreateFunctionRequest) (*function, []byte, error) {
	fn := &function{
		Name:        value(in.FunctionName),
		PackageType: value(in.PackageType),
		Timeout:     defaultTimeout,
		MemorySize:  defaultMemory,
		RevisionID:  newUUID(),
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

	code, err := fn.setCode(in.Code.ZipFile, in.Code.ImageUri)
	if err != nil {
		return nil, nil, err
	}

	return fn, code, nil
}

func (s *Service) CreateFunction(
	ctx context.Context,
	in *api.CreateFunctionRequest,
) (*api.FunctionConfiguration, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	fn, code, err := newFunction(in)
	if err != nil {
		return nil, err
	}

	fn.AccessKey = m.AccessKey
	fn.LastModified = s.now().UTC().Format(timeFormat)
	key := scopeKey(m.Scope, kindFunction, fn.Name)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, exists := tx.Get(key); exists {
			return conflict("Function already exist: " + fn.Name)
		}

		if code != nil {
			tx.Put(scopeKey(m.Scope, "code", fn.Name), code)
		}

		return writeJSON(tx, key, fn)
	})
	if err != nil {
		return nil, err
	}

	cfg := fn.configuration(m.Scope)
	return &cfg, nil
}

// load reads a function of the caller.
func (s *Service) load(ctx context.Context, raw *string) (request.Metadata, *function, error) {
	m, err := identity(ctx)
	if err != nil {
		return m, nil, err
	}

	name, err := resolveName(m, value(raw))
	if err != nil {
		return m, nil, err
	}

	var fn *function
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var found bool
		var loadErr error
		fn, found, loadErr = loadFunction(tx, scopeKey(m.Scope, kindFunction, name))
		if loadErr == nil && !found {
			return notFound("Function " + functionARN(m.Scope, name))
		}

		return loadErr
	})
	return m, fn, err
}

func (s *Service) GetFunction(ctx context.Context, in *api.GetFunctionRequest) (*api.GetFunctionResponse, error) {
	m, fn, err := s.load(ctx, in.FunctionName)
	if err != nil {
		return nil, err
	}

	cfg := fn.configuration(m.Scope)
	out := &api.GetFunctionResponse{
		Configuration: &cfg,
		Code:          &api.FunctionCodeLocation{RepositoryType: new("S3")},
		Tags:          fn.Tags,
	}
	if fn.PackageType == packageImage {
		out.Code = &api.FunctionCodeLocation{RepositoryType: new("ECR"), ImageUri: new(fn.ImageURI)}
	}

	return out, nil
}

func (s *Service) DeleteFunction(
	ctx context.Context,
	in *api.DeleteFunctionRequest,
) (*api.DeleteFunctionResponse, error) {
	m, fn, err := s.load(ctx, in.FunctionName)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		key := scopeKey(m.Scope, kindFunction, fn.Name)
		if _, ok := tx.Get(key); !ok {
			return notFound("Function " + functionARN(m.Scope, fn.Name))
		}

		tx.Delete(key)
		tx.Delete(scopeKey(m.Scope, "code", fn.Name))
		return nil
	})
	if err != nil {
		return nil, err
	}

	s.runtime.Release(functionARN(m.Scope, fn.Name))
	return &api.DeleteFunctionResponse{StatusCode: new(int64(http.StatusNoContent))}, nil
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
	m, err := identity(ctx)
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
	prefix := scopeKey(m.Scope, kindFunction, "")
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
