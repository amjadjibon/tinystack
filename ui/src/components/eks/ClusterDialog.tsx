import { Alert, Checkbox, SpaceBetween } from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { Choice, Field } from "#/components/containers/Controls";
import { PickMany } from "#/components/memorydb/Fields";
import { eksCall, eksList } from "#/lib/eks";
import {
	type Cluster,
	clusterConfigInputs,
	clusterInput,
	type EKSScope,
	logTypes,
} from "#/lib/eksHelpers";
import { listSecurityGroups, listSubnets } from "#/lib/elbv2";
import { usePoll } from "#/lib/usePoll";
export function ClusterDialog({
	scope,
	cluster,
	versionOnly = false,
	onClose,
	onSaved,
}: {
	scope: EKSScope;
	cluster?: Cluster;
	versionOnly?: boolean;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [f, setF] = useState({
		name: cluster?.Name ?? "",
		version: cluster?.Version ?? "1.34",
		role: "",
		subnets: cluster?.ResourcesVpcConfig?.SubnetIds ?? ([] as string[]),
		groups: cluster?.ResourcesVpcConfig?.SecurityGroupIds ?? ([] as string[]),
		public: cluster?.ResourcesVpcConfig?.EndpointPublicAccess ?? true,
		private: cluster?.ResourcesVpcConfig?.EndpointPrivateAccess ?? false,
		logs:
			cluster?.Logging?.ClusterLogging?.filter((l) => l.Enabled).flatMap(
				(l) => l.Types ?? [],
			) ?? [],
		tags: "{}",
		auth: cluster?.AccessConfig?.AuthenticationMode ?? "CONFIG_MAP",
	});
	const set = (p: Partial<typeof f>) => setF((v) => ({ ...v, ...p }));
	const options = usePoll(async () => {
		const [subnets, groups, versions] = await Promise.all([
			listSubnets(scope),
			listSecurityGroups(scope),
			eksList<{ ClusterVersion: string }>(
				scope,
				"DescribeClusterVersions",
				"ClusterVersions",
				{ IncludeAll: true },
			),
		]);
		return { subnets, groups, versions };
	}, 0);
	const vpc = options.data?.subnets.find((s) =>
		f.subnets.includes(s.SubnetId ?? ""),
	)?.VpcId;
	return (
		<ActionDialog
			title={
				cluster
					? versionOnly
						? "Update Kubernetes version"
						: "Update cluster configuration"
					: "Create cluster"
			}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const op = cluster
					? versionOnly
						? "UpdateClusterVersion"
						: "UpdateClusterConfig"
					: "CreateCluster";
				if (!f.public && !f.private)
					throw new Error("Enable public or private endpoint access.");
				const inputs = cluster
					? versionOnly
						? [{ Name: cluster.Name, Version: f.version }]
						: clusterConfigInputs(cluster, f)
					: [clusterInput(f)];
				return inputs.map((input) => ({
					label: op,
					run: () => eksCall(scope, op, input),
				}));
			}}
		>
			<SpaceBetween size="m">
				{!!options.error && <Alert type="error">{options.error}</Alert>}
				{!cluster && (
					<Field
						label="Cluster name"
						value={f.name}
						onChange={(name) => set({ name })}
					/>
				)}
				{(!cluster || versionOnly) && (
					<Choice
						label="Kubernetes version"
						value={f.version}
						options={
							options.data?.versions.map((v) => v.ClusterVersion) ?? [f.version]
						}
						onChange={(version) => set({ version })}
					/>
				)}
				{!cluster && (
					<SpaceBetween size="m">
						<Field
							label="Cluster role ARN"
							value={f.role}
							onChange={(role) => set({ role })}
						/>
						<PickMany
							label="Subnets (at least two Availability Zones)"
							placeholder="Choose subnets"
							values={f.subnets}
							options={(options.data?.subnets ?? [])
								.filter((s) => !vpc || s.VpcId === vpc)
								.map((s) => ({
									value: s.SubnetId ?? "",
									label: `${s.SubnetId} (${s.AvailabilityZone})`,
								}))}
							onChange={(subnets) => set({ subnets, groups: [] })}
						/>
						<PickMany
							label="Security groups"
							placeholder="Choose security groups"
							values={f.groups}
							options={(options.data?.groups ?? [])
								.filter((g) => g.VpcId === vpc)
								.map((g) => ({ value: g.GroupId ?? "", label: g.GroupName }))}
							onChange={(groups) => set({ groups })}
						/>
						<Field
							label="Tags (JSON string map)"
							value={f.tags}
							onChange={(tags) => set({ tags })}
						/>
					</SpaceBetween>
				)}
				{!versionOnly && (
					<SpaceBetween size="m">
						<Checkbox
							checked={f.public}
							onChange={({ detail }) => set({ public: detail.checked })}
						>
							Public endpoint access
						</Checkbox>
						<Checkbox
							checked={f.private}
							onChange={({ detail }) => set({ private: detail.checked })}
						>
							Private endpoint access
						</Checkbox>
						<PickMany
							label="Control plane logging"
							placeholder="Choose logs"
							values={f.logs}
							options={logTypes.map((value) => ({ value }))}
							onChange={(logs) => set({ logs })}
						/>
						<Choice
							label="Authentication mode"
							value={f.auth}
							options={["CONFIG_MAP", "API_AND_CONFIG_MAP", "API"]}
							onChange={(auth) => set({ auth })}
						/>
					</SpaceBetween>
				)}
			</SpaceBetween>
		</ActionDialog>
	);
}
