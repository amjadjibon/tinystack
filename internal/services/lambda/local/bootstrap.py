import importlib
import json
import os
import sys
import time
import traceback
import urllib.request
from types import SimpleNamespace

api = "http://" + os.environ["AWS_LAMBDA_RUNTIME_API"] + "/2018-06-01/runtime/"
opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

def post(path, value):
    sys.stdout.flush()
    sys.stderr.flush()
    request = urllib.request.Request(api + path, json.dumps(value).encode(), {"Content-Type": "application/json"})
    with opener.open(request) as response:
        response.read()

def error(exc):
    return {"errorMessage": str(exc), "errorType": type(exc).__name__, "stackTrace": traceback.format_exception(type(exc), exc, exc.__traceback__)}

try:
    module, method = os.environ["_HANDLER"].rsplit(".", 1)
    handler = getattr(importlib.import_module(module.replace("/", ".")), method)
    if not callable(handler):
        raise TypeError("Handler is not callable")
except BaseException as exc:
    post("init/error", error(exc))
    sys.exit(1)

while True:
    with opener.open(api + "invocation/next") as response:
        headers = response.headers
        event = json.load(response)
    request_id = headers["Lambda-Runtime-Aws-Request-Id"]
    deadline = int(headers["Lambda-Runtime-Deadline-Ms"])
    os.environ["_X_AMZN_TRACE_ID"] = headers.get("Lambda-Runtime-Trace-Id", "")
    context = SimpleNamespace(
        aws_request_id=request_id,
        function_name=os.environ["AWS_LAMBDA_FUNCTION_NAME"],
        function_version=os.environ["AWS_LAMBDA_FUNCTION_VERSION"],
        memory_limit_in_mb=os.environ["AWS_LAMBDA_FUNCTION_MEMORY_SIZE"],
        invoked_function_arn=headers["Lambda-Runtime-Invoked-Function-Arn"],
        log_group_name=os.environ["AWS_LAMBDA_LOG_GROUP_NAME"],
        log_stream_name=os.environ["AWS_LAMBDA_LOG_STREAM_NAME"],
        client_context=json.loads(headers.get("Lambda-Runtime-Client-Context", "null")),
        identity=json.loads(headers.get("Lambda-Runtime-Cognito-Identity", "null")),
        get_remaining_time_in_millis=lambda deadline=deadline: max(0, deadline - int(time.time() * 1000)),
    )
    try:
        result = handler(event, context)
        post("invocation/" + request_id + "/response", result)
    except BaseException as exc:
        post("invocation/" + request_id + "/error", error(exc))
