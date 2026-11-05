import { HOTSPOT } from "./hotspots.ts";
import type { Tutorial } from "./types.ts";
export const iamLeastPrivilege: Tutorial = {
	id: "iam-least-privilege",
	title: "Grant least privilege with IAM",
	description:
		"Create a role with one permission on one object. Compare an allowed read with an implicitly denied delete.",
	services: ["iam"],
	duration: "10 min",
	prerequisites:
		"Requires IAM CreateRole, PutRolePolicy, SimulatePrincipalPolicy and console reads; cleanup requires DeleteRolePolicy and DeleteRole. IAM roles are account-wide. No S3 bucket is created.",
	learnMore: "https://amjadjibon.github.io/tinystack/guides/console-tutorials/",
	cleanup: async ({ names: n }, call) => [
		{
			label: `Inline policy ${n.policy} on role ${n.role}`,
			run: () =>
				call("iam", "DeleteRolePolicy", {
					RoleName: n.role,
					PolicyName: n.policy,
				}),
		},
		{
			label: `Role ${n.role}`,
			run: () => call("iam", "DeleteRole", { RoleName: n.role }),
		},
	],
	tasks: [
		{
			title: "Grant least privilege with IAM",
			steps: [
				{
					title: "Create a role",
					content:
						"Choose Create role, keep the Lambda service trust, skip permissions, and name it {role}. Trust controls who can assume a role; permissions control what it can do.",
					command:
						'aws-local iam create-role --role-name {role} --assume-role-policy-document \'{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"lambda.amazonaws.com"},"Action":"sts:AssumeRole"}]}\'',
					hotspotId: HOTSPOT.iamCreate,
					route: "/services/iam?section=role",
					check: "iamRole",
				},
				{
					title: "Grant one action on one resource",
					content:
						"Open {role}, choose Create inline policy, and name it {policy}. Paste the PolicyDocument from the command: allow only s3:GetObject on arn:aws:s3:::{bucket}/{key}. The object need not exist to simulate access.",
					command:
						'aws-local iam put-role-policy --role-name {role} --policy-name {policy} --policy-document \'{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::{bucket}/{key}"}]}\'',
					hotspotId: HOTSPOT.iamInline,
					route: "/services/iam?section=role&name={role}",
					check: "iamPolicy",
				},
				{
					title: "Compare allowed and denied actions",
					content:
						"In Policy simulator use Principal mode and select {role}. Enter s3:GetObject,s3:DeleteObject and resource arn:aws:s3:::{bucket}/{key}, then Run simulation. GetObject is allowed; DeleteObject has no grant and is implicitly denied.",
					command:
						"aws-local iam simulate-principal-policy --policy-source-arn arn:aws:iam::{account}:role/{role} --action-names s3:GetObject s3:DeleteObject --resource-arns arn:aws:s3:::{bucket}/{key}",
					hotspotId: HOTSPOT.iamSimulate,
					route: "/services/iam?section=simulator",
					check: "iamSimulation",
				},
			],
		},
	],
};
