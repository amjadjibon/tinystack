const http = require('node:http');
const path = require('node:path');
const fs = require('node:fs');
const { pathToFileURL } = require('node:url');
const base = 'http://' + process.env.AWS_LAMBDA_RUNTIME_API + '/2018-06-01/runtime/';
function request(route, value) {
  return new Promise((resolve, reject) => {
    const body = value === undefined ? undefined : JSON.stringify(value);
    const req = http.request(base + route, { method: body === undefined ? 'GET' : 'POST',
      headers: body === undefined ? {} : { 'Content-Type': 'application/json' } }, res => {
      let data = '';
      res.on('data', chunk => data += chunk);
      res.on('end', () => res.statusCode >= 400 ? reject(new Error('Runtime API: ' + res.statusCode)) :
        resolve({ headers: res.headers, value: data ? JSON.parse(data) : null }));
    });
    req.on('error', reject);
    req.end(body);
  });
}
const error = exc => ({ errorMessage: String(exc?.message ?? exc), errorType: exc?.name ?? 'Error',
  stackTrace: String(exc?.stack ?? '').split('\n') });
async function flush() {
  await Promise.all([process.stdout, process.stderr].map(stream => new Promise(resolve => stream.write('', resolve))));
}
async function main() {
  let handler;
  try {
    const spec = process.env._HANDLER;
    const dot = spec.lastIndexOf('.');
    if (dot < 1) throw new Error('Handler must be module.function');
    const module = path.resolve(process.env.LAMBDA_TASK_ROOT, spec.slice(0, dot));
    const file = ['.js', '.mjs', '.cjs'].map(ext => module + ext).find(file => fs.existsSync(file));
    if (!file) throw new Error('Handler module does not exist: ' + module);
    const exports = await import(pathToFileURL(file));
    handler = exports[spec.slice(dot + 1)] ?? exports.default?.[spec.slice(dot + 1)];
    if (typeof handler !== 'function') throw new Error('Handler is not callable');
  } catch (exc) {
    await flush();
    await request('init/error', error(exc));
    return;
  }
  for (;;) {
    const { headers, value } = await request('invocation/next');
    const id = headers['lambda-runtime-aws-request-id'];
    process.env._X_AMZN_TRACE_ID = headers['lambda-runtime-trace-id'] ?? '';
    const context = {
      awsRequestId: id, functionName: process.env.AWS_LAMBDA_FUNCTION_NAME,
      functionVersion: process.env.AWS_LAMBDA_FUNCTION_VERSION,
      memoryLimitInMB: process.env.AWS_LAMBDA_FUNCTION_MEMORY_SIZE,
      invokedFunctionArn: headers['lambda-runtime-invoked-function-arn'],
      logGroupName: process.env.AWS_LAMBDA_LOG_GROUP_NAME, logStreamName: process.env.AWS_LAMBDA_LOG_STREAM_NAME,
      clientContext: JSON.parse(headers['lambda-runtime-client-context'] ?? 'null'),
      identity: JSON.parse(headers['lambda-runtime-cognito-identity'] ?? 'null'),
      callbackWaitsForEmptyEventLoop: true,
      getRemainingTimeInMillis: () => Math.max(0, Number(headers['lambda-runtime-deadline-ms']) - Date.now())
    };
    const prefix = 'invocation/' + id;
    try {
      const result = await new Promise((resolve, reject) => {
        const callback = (err, result) => err ? reject(err) : resolve(result);
        context.done = callback;
        context.succeed = resolve;
        context.fail = reject;
        const returned = handler(value, context, callback);
        if (returned && typeof returned.then === 'function') returned.then(resolve, reject);
        else if (handler.length < 3 && returned !== undefined) resolve(returned);
      });
      await flush();
      await request(prefix + '/response', result === undefined ? null : result);
    } catch (exc) {
      await flush();
      await request(prefix + '/error', error(exc));
    }
  }
}
main().catch(exc => { console.error(exc); process.exitCode = 1; });
