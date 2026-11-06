import { HOTSPOT } from "./hotspots.ts";
import type { Tutorial } from "./types.ts";
export const lambdaFirstFunction: Tutorial = {
	id: "lambda-first-function",
	title: "Create and test a Lambda function",
	description:
		"Deploy a small Python handler, invoke it, and inspect its CloudWatch log stream.",
	services: ["lambda", "logs"],
	duration: "5 min",
	prerequisites:
		"Start TinyStack with lambda.executor: local and python3 on its PATH (Python 3.11 for runtime python3.11). Requires Lambda create/invoke/delete and console reads, plus Logs read/delete-group permissions. The console has no inline-code editor: use a ZIP containing index.py. The CLI command embeds that same package. Use an existing execution role ARN when required by your authorization setup.",
	learnMore: "https://amjadjibon.github.io/tinystack/guides/console-tutorials/",
	tasks: [
		{
			title: "Create and test a Lambda function",
			steps: [
				{
					title: "Create a function",
					content:
						'Choose Create function, use {function}, ZIP archive, runtime python3.11, handler index.handler and role arn:aws:iam::{account}:role/lambda. Upload a ZIP containing index.py with: def handler(event, context): print("Hello TinyStack"); return {"message": "Hello TinyStack"}. The console accepts uploaded packages rather than inline code; the command embeds this handler as a base64 ZIP. With the local executor, the host Python runs the handler despite the console Docker notice.',
					command:
						'aws-local lambda create-function --cli-input-json \'{"FunctionName":"{function}","Runtime":"python3.11","Handler":"index.handler","Role":"arn:aws:iam::{account}:role/lambda","Code":{"ZipFile":"UEsDBBQAAAAIAPSuZV0Fm6++UgAAAGQAAAAIAAAAaW5kZXgucHlLSU1TyEjMS8lJLdJILUvNK9FRSM7PK0mtKNG04lIAgoKizLwSDSWP1JycfIWQzLzK4JLE5GwlTbBkUWpJaVGeQrVSbmpxcWJ6qpKVAobKWi4AUEsBAhQDFAAAAAgA9K5lXQWbr75SAAAAZAAAAAgAAAAAAAAAAAAAAIABAAAAAGluZGV4LnB5UEsFBgAAAAABAAEANgAAAHgAAAAAAA=="}}\' --cli-binary-format base64',
					hotspotId: HOTSPOT.lambdaCreate,
					route: "/services/lambda",
					check: "function",
				},
				{
					title: "Test the function",
					content:
						"Open {function}, select Test, keep Synchronous and an empty qualifier, and choose Test with {}. Inspect the response and confirm Execution succeeded. This check records a successful Invoke API request; inspect the result for handler errors.",
					command:
						"aws-local lambda invoke --function-name {function} --invocation-type RequestResponse --log-type Tail /tmp/tutorial-lambda-response.json",
					hotspotId: HOTSPOT.lambdaTest,
					route: "/services/lambda?function={function}&tab=test",
					check: "invoke",
				},
				{
					title: "Inspect the log stream",
					content:
						"Open the generated log group /aws/lambda/{function}, refresh, then open its stream to see START, Hello TinyStack and END. Lambda writes a stream for the invocation; the live check waits for a stream in this exact group.",
					command:
						"aws-local logs describe-log-streams --log-group-name /aws/lambda/{function}",
					hotspotId: HOTSPOT.lambdaLogs,
					route: "/services/logs?group=/aws/lambda/{function}",
					check: "logStream",
				},
			],
		},
	],
	cleanup: async ({ names: n }, call) => [
		{
			label: `Function ${n.function}`,
			run: () => call("lambda", "DeleteFunction", { FunctionName: n.function }),
		},
		{
			label: `Log group /aws/lambda/${n.function} and its streams`,
			run: () =>
				call("logs", "DeleteLogGroup", {
					LogGroupName: `/aws/lambda/${n.function}`,
				}),
		},
	],
};
