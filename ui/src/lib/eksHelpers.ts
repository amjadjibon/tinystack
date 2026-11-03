import { integer, stringMap } from "./consoleActions.ts";
export type EKSScope = { account: string; region: string };
export type Cluster = {
	Name: string;
	Arn?: string;
	Version?: string;
	Status?: string;
	Endpoint?: string;
	CreatedAt?: number;
	CertificateAuthority?: { Data?: string };
	Identity?: { Oidc?: { Issuer?: string } };
	Tags?: Record<string, string>;
	ResourcesVpcConfig?: {
		VpcId?: string;
		SubnetIds?: string[];
		SecurityGroupIds?: string[];
		EndpointPublicAccess?: boolean;
		EndpointPrivateAccess?: boolean;
	};
	Logging?: { ClusterLogging?: { Types?: string[]; Enabled?: boolean }[] };
	AccessConfig?: { AuthenticationMode?: string };
};
export const words = (text: string) => text.split(/[\s,]+/).filter(Boolean);
export function scaling(min: string, max: string, desired: string) {
	const MinSize = integer(min, "Minimum size", 0, 1000),
		MaxSize = integer(max, "Maximum size", 1, 1000),
		DesiredSize = integer(desired, "Desired size", 0, 1000);
	if (MinSize > MaxSize || DesiredSize < MinSize || DesiredSize > MaxSize)
		throw new Error("Desired size must be between minimum and maximum size.");
	return { MinSize, MaxSize, DesiredSize };
}
export function resourceName(value: string) {
	if (!/^[A-Za-z0-9][A-Za-z0-9_-]{0,99}$/.test(value))
		throw new Error(
			"Name must start with a letter or digit and contain at most 100 letters, digits, underscores or hyphens.",
		);
	return value;
}
export function role(value: string) {
	if (!/^arn:aws[a-z-]*:iam::\d{12}:role\/.+$/.test(value))
		throw new Error("Enter an IAM role ARN.");
	return value;
}
export const pending = (status?: string) =>
	["CREATING", "UPDATING", "DELETING", "PENDING", "InProgress"].includes(
		status ?? "",
	);
export const logTypes = [
	"api",
	"audit",
	"authenticator",
	"controllerManager",
	"scheduler",
];
export function logging(types: string[]) {
	return {
		ClusterLogging: [
			{ Types: types, Enabled: true },
			{ Types: logTypes.filter((t) => !types.includes(t)), Enabled: false },
		].filter((s) => s.Types.length),
	};
}
export function clusterInput(f: {
	name: string;
	version: string;
	role: string;
	subnets: string[];
	groups: string[];
	public: boolean;
	private: boolean;
	logs: string[];
	tags: string;
	auth: string;
}) {
	if (f.subnets.length < 2)
		throw new Error("Choose subnets in at least two Availability Zones.");
	if (!f.public && !f.private)
		throw new Error("Enable public or private endpoint access.");
	return {
		Name: resourceName(f.name.trim()),
		Version: f.version,
		RoleArn: role(f.role.trim()),
		ResourcesVpcConfig: {
			SubnetIds: f.subnets,
			SecurityGroupIds: f.groups,
			EndpointPublicAccess: f.public,
			EndpointPrivateAccess: f.private,
		},
		Logging: logging(f.logs),
		Tags: stringMap(f.tags, "Tags"),
		AccessConfig: { AuthenticationMode: f.auth },
	};
}
export function kubeconfig(c: Cluster, region: string) {
	// JSON is valid YAML; quoting every server-supplied value prevents YAML injection.
	return JSON.stringify(
		{
			apiVersion: "v1",
			kind: "Config",
			clusters: [
				{
					name: c.Name,
					cluster: {
						server: c.Endpoint,
						"certificate-authority-data": c.CertificateAuthority?.Data,
					},
				},
			],
			contexts: [{ name: c.Name, context: { cluster: c.Name, user: c.Name } }],
			"current-context": c.Name,
			users: [
				{
					name: c.Name,
					user: {
						exec: {
							apiVersion: "client.authentication.k8s.io/v1beta1",
							command: "aws",
							args: [
								"eks",
								"get-token",
								"--cluster-name",
								c.Name,
								"--region",
								region,
							],
						},
					},
				},
			],
		},
		null,
		2,
	);
}

// EKS accepts one configuration category per request. ActionDialog retries only
// unfinished steps if a later category fails.
export function clusterConfigInputs(
	c: Cluster,
	f: { public: boolean; private: boolean; logs: string[]; auth: string },
) {
	if (!f.public && !f.private)
		throw new Error("Enable public or private endpoint access.");
	const changes: Record<string, unknown>[] = [];
	if (
		f.public !== c.ResourcesVpcConfig?.EndpointPublicAccess ||
		f.private !== c.ResourcesVpcConfig?.EndpointPrivateAccess
	)
		changes.push({
			ResourcesVpcConfig: {
				EndpointPublicAccess: f.public,
				EndpointPrivateAccess: f.private,
			},
		});
	const oldLogs =
		c.Logging?.ClusterLogging?.filter((l) => l.Enabled).flatMap(
			(l) => l.Types ?? [],
		) ?? [];
	if ([...oldLogs].sort().join() !== [...f.logs].sort().join())
		changes.push({ Logging: logging(f.logs) });
	if (f.auth !== c.AccessConfig?.AuthenticationMode) {
		const modes = ["CONFIG_MAP", "API_AND_CONFIG_MAP", "API"];
		if (
			modes.indexOf(f.auth) <
			modes.indexOf(c.AccessConfig?.AuthenticationMode ?? "CONFIG_MAP")
		)
			throw new Error(
				"Authentication mode can only move toward API authentication.",
			);
		changes.push({ AccessConfig: { AuthenticationMode: f.auth } });
	}
	if (!changes.length)
		throw new Error("Change at least one configuration setting.");
	return changes.map((change) => ({ Name: c.Name, ...change }));
}
