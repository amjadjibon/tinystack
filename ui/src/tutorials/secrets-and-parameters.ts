import { HOTSPOT } from "./hotspots.ts";
import type { Tutorial } from "./types.ts";
export const secretsAndParameters: Tutorial = {
	id: "secrets-and-parameters",
	title: "Store configuration in Secrets Manager and Parameter Store",
	description:
		"Store a demo secret and a String parameter, then explicitly retrieve each value. Learn where application configuration lives.",
	services: ["secretsmanager", "ssm"],
	duration: "5 min",
	prerequisites:
		"Requires secretsmanager:CreateSecret, GetSecretValue, DeleteSecret, SSM PutParameter, GetParameter, DeleteParameter and console reads. Use demo values only. Cleanup permanently deletes the generated secret and parameter, including their versions.",
	learnMore: "https://amjadjibon.github.io/tinystack/guides/console-tutorials/",
	tasks: [
		{
			title: "Store configuration in Secrets Manager and Parameter Store",
			steps: [
				{
					title: "Store a secret",
					content:
						"Choose Store a new secret, name it {secret}, select Plaintext and enter demo-only. Keep the default encryption settings and save. Secrets Manager versions values separately from the resource metadata.",
					command:
						"aws-local secretsmanager create-secret --name {secret} --secret-string demo-only",
					hotspotId: HOTSPOT.secretCreate,
					route: "/services/secretsmanager",
					check: "secret",
				},
				{
					title: "Retrieve its value",
					content:
						"Open {secret} and choose Retrieve secret value. Confirm demo-only, then hide it when done. Retrieval is explicit so listing metadata does not reveal values.",
					command:
						"aws-local secretsmanager get-secret-value --secret-id {secret}",
					hotspotId: HOTSPOT.secretRead,
					route: "/services/secretsmanager?secret={secret}",
					check: "secretValue",
				},
				{
					title: "Create a parameter",
					content:
						"Open Parameter Store, choose Create parameter, name it {parameter}, select String, and enter demo-only. Parameters store application configuration; String avoids needing a KMS key for this first exercise.",
					command:
						"aws-local ssm put-parameter --name {parameter} --type String --value demo-only",
					hotspotId: HOTSPOT.parameterCreate,
					route: "/services/ssm",
					check: "parameter",
				},
				{
					title: "Read the parameter",
					content:
						"Open {parameter} and choose Reveal value in Details. Confirm demo-only. GetParameter can read a particular version; the console selects the current version.",
					command: "aws-local ssm get-parameter --name {parameter}",
					hotspotId: HOTSPOT.parameterRead,
					route: "/services/ssm?parameter={parameter}",
					check: "parameterValue",
				},
			],
		},
	],
	cleanup: async ({ names: n }, call) => [
		{
			label: `Secret ${n.secret} (permanent deletion)`,
			run: () =>
				call("secretsmanager", "DeleteSecret", {
					SecretId: n.secret,
					ForceDeleteWithoutRecovery: true,
				}),
		},
		{
			label: `Parameter ${n.parameter} and all versions`,
			run: () => call("ssm", "DeleteParameter", { Name: n.parameter }),
		},
	],
};
