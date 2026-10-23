package lambda

import (
	"context"
	"regexp"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
)

// Function is what a Runtime needs to run one function.
type Function struct {
	ImageConfig *api.ImageConfig
	Environment map[string]string
	// Invocation metadata is not part of the environment revision.
	Endpoint        string
	RequestID       string
	TraceID         string
	ClientContext   string
	CognitoIdentity string
	ARN             string
	Name            string
	// Version is the version that runs: a number or $LATEST.
	Version     string
	Region      string
	Runtime     string
	Handler     string
	PackageType string
	ImageURI    string
	// Architecture is empty unless the function names one explicitly.
	Architecture string
	// Revision identifies the code and configuration; a runtime must not reuse
	// an environment started for a different revision.
	Revision string
	// HotReloadPath is a live host directory, mutually exclusive with Code.
	HotReloadPath string
	// Code is the deployment package of Zip functions.
	Code []byte
	// Layers are the packages of the function's layers, in order; each is
	// extracted into /opt over the ones before it.
	Layers     [][]byte
	Timeout    time.Duration
	MemorySize int64
}

// Result is a completed invocation. Error reports that the function failed
// (it raised, crashed, or timed out) and Payload then holds the error document.
// Log is what the function wrote to stdout and stderr during the invocation;
// Lambda adds its own START, END, and REPORT lines around it.
type Result struct {
	Log     string
	Payload []byte
	Error   bool
}

// Runtime runs function code. Service logic never starts processes itself.
type Runtime interface {
	// Invoke runs fn with payload. err reports that the runtime could not run
	// the function at all; function failures are reported in Result.Error.
	Invoke(ctx context.Context, fn *Function, payload []byte) (Result, error)
	// Release discards the warm environments of the function with this ARN.
	Release(arn string)
	// Close discards every environment.
	Close()
}

// WarmRuntime pre-starts reusable environments without invoking the handler.
type WarmRuntime interface {
	Runtime
	Warm(context.Context, *Function, int) error
}

// DirectoryRuntime opts into running live host directories. Other runtimes
// are rejected before invocation instead of silently ignoring the code reference.
type DirectoryRuntime interface {
	Runtime
	SupportsDirectories() bool
}

// runtimes maps the supported managed runtimes to their AWS base images, which
// include the Lambda Runtime Interface Emulator.
var runtimes = regexp.MustCompile(
	`^(python3\.(9|1[0-4])|nodejs(18|20|22|24)\.x|java(11|17|21|25)|ruby3\.[2-4]|dotnet(8|10)|provided\.al2(023)?)$`,
)

// Image returns the base image of a managed runtime such as "python3.12".
func Image(runtime string) (string, bool) {
	if !runtimes.MatchString(runtime) {
		return "", false
	}

	name, version := runtime, ""
	for _, family := range []string{"python", "nodejs", "java", "ruby", "dotnet"} {
		if rest, ok := strings.CutPrefix(runtime, family); ok {
			name, version = family, strings.TrimSuffix(rest, ".x")
		}
	}

	if rest, ok := strings.CutPrefix(runtime, "provided."); ok {
		name, version = "provided", rest
	}

	return "public.ecr.aws/lambda/" + name + ":" + version, true
}
