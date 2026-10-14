package local

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/lambda"
)

func environment(fn *lambda.Function, root, task, endpoint, api string) []string {
	env := map[string]string{}
	for _, key := range []string{"PATH", "HOME", "LANG", "SYSTEMROOT"} {
		if value, ok := os.LookupEnv(key); ok {
			env[key] = value
		}
	}

	// Credentials are supplied by lambda.Service.prepare, shared with Docker.
	for key, value := range fn.Environment {
		env[key] = value
	}

	env["AWS_REGION"], env["AWS_DEFAULT_REGION"] = fn.Region, fn.Region
	env["AWS_LAMBDA_FUNCTION_NAME"] = fn.Name
	env["AWS_LAMBDA_FUNCTION_VERSION"] = fn.Version
	env["AWS_LAMBDA_FUNCTION_MEMORY_SIZE"] = strconv.FormatInt(fn.MemorySize, 10)
	env["AWS_LAMBDA_FUNCTION_TIMEOUT"] = strconv.FormatFloat(fn.Timeout.Seconds(), 'f', -1, 64)
	env["AWS_LAMBDA_LOG_GROUP_NAME"] = "/aws/lambda/" + fn.Name
	env["AWS_LAMBDA_LOG_STREAM_NAME"] = "local/" + common.RandomID(32)
	env["AWS_LAMBDA_RUNTIME_API"], env["LAMBDA_TASK_ROOT"] = api, task
	env["_HANDLER"], env["TZ"] = fn.Handler, ":UTC"
	env["AWS_EXECUTION_ENV"] = "AWS_Lambda_" + fn.Runtime
	if fn.Endpoint != "" {
		endpoint = fn.Endpoint
	}

	if env["AWS_ENDPOINT_URL"] == "" {
		env["AWS_ENDPOINT_URL"] = endpoint
	}

	opt := filepath.Join(root, "opt")
	python := strings.TrimPrefix(fn.Runtime, "python")
	env["PYTHONPATH"] = strings.Join([]string{
		task, filepath.Join(opt, "python"),
		filepath.Join(opt, "python", "lib", "python"+python, "site-packages"), env["PYTHONPATH"],
	}, string(os.PathListSeparator))
	node := strings.TrimSuffix(strings.TrimPrefix(fn.Runtime, "nodejs"), ".x")
	env["NODE_PATH"] = strings.Join([]string{
		filepath.Join(opt, "nodejs", "node"+node, "node_modules"),
		filepath.Join(opt, "nodejs", "node_modules"), env["NODE_PATH"],
	}, string(os.PathListSeparator))
	// Do not let host-level Python site customizations leak into functions.
	env["PYTHONNOUSERSITE"], env["PYTHONDONTWRITEBYTECODE"] = "1", "1"
	env["PYTHONUNBUFFERED"] = "1"
	env["PYTHONPYCACHEPREFIX"] = filepath.Join(root, "pycache")
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}

	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+env[key])
	}

	return result
}
