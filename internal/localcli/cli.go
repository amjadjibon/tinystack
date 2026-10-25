package localcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

// DefaultEndpoint is the default TinyStack local endpoint.
const DefaultEndpoint = "http://127.0.0.1:4566"

// The credentials and Region TinyStack's tools use by default.
const (
	defaultCredential = "test"
	defaultRegion     = "us-east-1"
)

// Endpoint returns the configured TinyStack endpoint from environment variables,
// falling back to http://127.0.0.1:4566.
func Endpoint() string {
	if ep := os.Getenv("TINYSTACK_ENDPOINT"); ep != "" {
		return ep
	}
	if ep := os.Getenv("TINYSTACK_URL"); ep != "" {
		return ep
	}
	if ep := os.Getenv("AWS_ENDPOINT_URL"); ep != "" {
		return ep
	}
	if host := os.Getenv("LOCALSTACK_HOST"); host != "" {
		if !strings.HasPrefix(host, "http://") && !strings.HasPrefix(host, "https://") {
			return "http://" + host
		}
		return host
	}
	return DefaultEndpoint
}

// BaseEnv returns the current process environment with default AWS credentials
// and configuration for TinyStack injected if not already set.
func BaseEnv(endpoint string, extra map[string]string) []string {
	defaults := map[string]string{
		"AWS_ACCESS_KEY_ID":         defaultCredential,
		"AWS_SECRET_ACCESS_KEY":     defaultCredential,
		"AWS_DEFAULT_REGION":        defaultRegion,
		"AWS_REGION":                defaultRegion,
		"AWS_ENDPOINT_URL":          endpoint,
		"AWS_EC2_METADATA_DISABLED": "true",
	}
	for k, v := range extra {
		defaults[k] = v
	}

	envMap := make(map[string]string)
	for _, env := range os.Environ() {
		if idx := strings.IndexByte(env, '='); idx > 0 {
			envMap[env[:idx]] = env[idx+1:]
		}
	}

	for k, v := range defaults {
		if _, exists := envMap[k]; !exists {
			envMap[k] = v
		}
	}

	result := make([]string, 0, len(envMap))
	for k, v := range envMap {
		result = append(result, k+"="+v)
	}
	return result
}

// Execute runs the target executable with the provided arguments and environment,
// forwarding standard I/O, signals, and the exit status.
func Execute(binaryName string, args, env []string) int {
	binPath, err := exec.LookPath(binaryName)
	if err != nil {
		fmt.Fprintf(
			os.Stderr,
			"tinystack: %q not found in PATH. Please ensure %s is installed.\n",
			binaryName,
			binaryName,
		)
		return 127
	}

	// #nosec G204 -- running the tool the user named, with their arguments, is this wrapper's purpose.
	cmd := exec.CommandContext(context.Background(), binPath, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = env

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "tinystack: failed to execute %s: %v\n", binaryName, err)
		return 1
	}

	sigChan := make(chan os.Signal, 2)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer func() {
		signal.Stop(sigChan)
		close(sigChan)
	}()

	go func() {
		for sig := range sigChan {
			if cmd.Process != nil {
				_ = cmd.Process.Signal(sig)
			}
		}
	}()

	if err := cmd.Wait(); err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "tinystack: %s exited with error: %v\n", binaryName, err)
		return 1
	}

	return 0
}

// AWS CLI Helper: Ensures --endpoint-url is present in the arguments.
func EnsureEndpointArg(args []string, endpoint string) []string {
	for _, arg := range args {
		if arg == "--endpoint-url" || strings.HasPrefix(arg, "--endpoint-url=") {
			return args
		}
	}
	return append([]string{"--endpoint-url", endpoint}, args...)
}

// Terraform Services supported by TinyStack.
var TerraformServices = []string{
	"bedrockagent",
	"mediaconvert",
	"mediaconnect",
	"transcribe",
	"datasync",
	"servicecatalog",
	"accessanalyzer",
	"shield",
	"swf",
	"globalaccelerator",
	"dsql",
	"fis",
	"inspector2",
	"resourceexplorer2",
	"acm",
	"amp",
	"codedeploy",
	"applicationautoscaling",
	"apigateway",
	"apigatewayv2",
	"appsync",
	"athena",
	"autoscaling",
	"cloudformation",
	"cloudwatch",
	"cognitoidentity",
	"cognitoidp",
	"configservice",
	"backup",
	"efs",
	"batch",
	"codebuild",
	"codepipeline",
	"codecommit",
	"codeartifact",
	"timestreamwrite",
	"codeconnections",
	"mq",
	"kafka",
	"mwaa",
	"transfer",
	"location",
	"s3tables",
	"wafv2",
	"docdb",
	"neptune",
	"rds",
	"dynamodb",
	"ec2",
	"ecr",
	"ecs",
	"eks",
	"elasticache",
	"elb",
	"elbv2",
	"eventbridge",
	"firehose",
	"glue",
	"iam",
	"identitystore",
	"iot",
	"kinesis",
	"kms",
	"lambda",
	"logs",
	"opensearch",
	"route53resolver",
	"route53",
	"s3",
	"scheduler",
	"secretsmanager",
	"sesv2",
	"sfn",
	"sns",
	"sqs",
	"ssm",
	"ssoadmin",
	"sts",
	"ram",
	"memorydb",
	"redshift",
	"xray",
	"glacier",
	"acmpca",
	"verifiedpermissions",
}

// TerraformOverrideConfig generates the JSON structure for a Terraform provider override.
func TerraformOverrideConfig(endpoint string) ([]byte, error) {
	endpoints := make(map[string]string, len(TerraformServices))
	for _, svc := range TerraformServices {
		endpoints[svc] = endpoint
	}

	override := map[string]any{
		"provider": map[string]any{
			"aws": map[string]any{
				"access_key":                  defaultCredential,
				"secret_key":                  defaultCredential,
				"region":                      defaultRegion,
				"skip_credentials_validation": true,
				"skip_metadata_api_check":     true,
				"skip_requesting_account_id":  false,
				"s3_use_path_style":           true,
				"endpoints":                   endpoints,
			},
		},
	}

	return json.MarshalIndent(override, "", "  ")
}

// FindTerraformWorkDir inspects arguments for -chdir=<dir> or -chdir <dir>.
func FindTerraformWorkDir(args []string) string {
	for i := range args {
		arg := args[i]
		if strings.HasPrefix(arg, "-chdir=") {
			return strings.TrimPrefix(arg, "-chdir=")
		}
		if arg == "-chdir" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return "."
}

// SetupTerraformOverride writes tinystack_providers_override.tf.json into workDir,
// returning a cleanup function that deletes the override file.
func SetupTerraformOverride(workDir, endpoint string) (func(), error) {
	if os.Getenv("TINYSTACK_TF_NO_OVERRIDE") != "" || os.Getenv("TF_NO_OVERRIDE") != "" {
		return func() {}, nil
	}

	data, err := TerraformOverrideConfig(endpoint)
	if err != nil {
		return func() {}, err
	}

	overridePath := filepath.Join(workDir, "tinystack_providers_override.tf.json")
	if err := os.WriteFile(overridePath, data, 0o600); err != nil {
		return func() {}, err
	}

	cleanup := func() {
		_ = os.Remove(overridePath)
	}
	return cleanup, nil
}

// defaultAccount is TinyStack's default account, which CDK deploys into.
const defaultAccount = "000000000000"

// CDKEnv returns the variables the CDK CLI needs besides BaseEnv's: the
// account and Region its stacks target, and no version check that would
// leave the machine. A 12-digit AWS_ACCESS_KEY_ID names the account, as it
// does for TinyStack itself.
func CDKEnv() map[string]string {
	account := defaultAccount
	if key := os.Getenv("AWS_ACCESS_KEY_ID"); len(key) == 12 && strings.Trim(key, "0123456789") == "" {
		account = key
	}

	region := defaultRegion
	for _, name := range []string{"AWS_REGION", "AWS_DEFAULT_REGION"} {
		if value := os.Getenv(name); value != "" {
			region = value
			break
		}
	}

	return map[string]string{
		"CDK_DEFAULT_ACCOUNT":       account,
		"CDK_DEFAULT_REGION":        region,
		"CDK_DISABLE_VERSION_CHECK": "1",
		"AWS_PAGER":                 "",
	}
}
