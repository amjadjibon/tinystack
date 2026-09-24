package lambda

import (
	"context"
	"regexp"
	"strings"
	"time"
)

// Function is what a Runtime needs to run one function.
type Function struct {
	Environment map[string]string
	ARN         string
	Name        string
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
	// Code is the deployment package of Zip functions.
	Code       []byte
	Timeout    time.Duration
	MemorySize int64
}

// Result is a completed invocation. Error reports that the function failed
// (it raised, crashed, or timed out) and Payload then holds the error document.
type Result struct {
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
