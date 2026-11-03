import { Alert, Checkbox, SpaceBetween } from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { Choice, Field } from "#/components/containers/Controls";
import { PickMany } from "#/components/memorydb/Fields";
import { listSecurityGroups, listSubnets } from "#/lib/elbv2";
import { kafkaCall } from "#/lib/kafka";
import {
	type Cluster,
	type ClusterForm,
	clusterInput,
	instances,
	type KafkaScope,
	monitoring,
	versions,
} from "#/lib/kafkaHelpers";
import { usePoll } from "#/lib/usePoll";
export function ClusterDialog({
	scope,
	docker,
	cluster,
	onClose,
	onSaved,
}: {
	scope: KafkaScope;
	docker: boolean;
	cluster?: Cluster;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [f, setF] = useState<ClusterForm>({
		name: "",
		version: "3.9.x",
		count: "2",
		instance: instances[0],
		subnets: [],
		groups: [],
		storage: "100",
		monitoring: cluster?.EnhancedMonitoring ?? "DEFAULT",
		tags: "{}",
		auth: "Unauthenticated",
		encryption: "TLS",
		inCluster: true,
		authorities: "",
	});
	const set = (p: Partial<ClusterForm>) => setF((v) => ({ ...v, ...p }));
	const options = usePoll(async () => {
		if (cluster) return { subnets: [], groups: [] };
		const [subnets, groups] = await Promise.all([
			listSubnets(scope),
			listSecurityGroups(scope),
		]);
		return { subnets, groups };
	}, 0);
	const vpc = options.data?.subnets.find((s) =>
		f.subnets.includes(s.SubnetId ?? ""),
	)?.VpcId;
	return (
		<ActionDialog
			title={cluster ? "Edit monitoring" : "Create cluster"}
			submit={cluster ? "Save changes" : "Create cluster"}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => [
				{
					label: cluster ? "UpdateMonitoring" : "CreateCluster",
					run: () =>
						kafkaCall(
							scope,
							cluster ? "UpdateMonitoring" : "CreateCluster",
							cluster
								? {
										ClusterArn: cluster.ClusterArn,
										CurrentVersion: cluster.CurrentVersion,
										EnhancedMonitoring: f.monitoring,
									}
								: clusterInput(f, scope.region, docker),
						),
				},
			]}
		>
			<SpaceBetween size="m">
				{!!options.error && <Alert type="error">{options.error}</Alert>}
				{!cluster && (
					<>
						<Alert>
							Provisioned clusters use ZooKeeper version metadata. Choose
							subnets in distinct Availability Zones within one VPC. Docker runs
							one Kafka-compatible broker regardless of the requested count.
						</Alert>
						<Field
							label="Cluster name"
							value={f.name}
							onChange={(name) => set({ name })}
						/>
						<Choice
							label="Kafka version"
							value={f.version}
							options={versions}
							onChange={(version) => set({ version })}
						/>
						<Field
							label="Broker count (1–15, multiple of subnet count)"
							value={f.count}
							onChange={(count) => set({ count })}
						/>
						<Choice
							label="Instance type"
							value={f.instance}
							options={instances}
							onChange={(instance) => set({ instance })}
						/>
						<PickMany
							label="Subnets"
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
							placeholder="Choose groups"
							values={f.groups}
							options={(options.data?.groups ?? [])
								.filter((g) => g.VpcId === vpc)
								.map((g) => ({ value: g.GroupId ?? "", label: g.GroupName }))}
							onChange={(groups) => set({ groups })}
						/>
						<Field
							label="EBS storage per broker (GiB)"
							value={f.storage}
							onChange={(storage) => set({ storage })}
						/>
						<Choice
							label="Client authentication"
							value={f.auth}
							options={
								docker
									? ["Unauthenticated"]
									: ["Unauthenticated", "IAM", "SCRAM", "mTLS"]
							}
							onChange={(auth) => set({ auth })}
						/>
						{f.auth === "mTLS" && (
							<Field
								label="Certificate authority ARNs (comma separated)"
								value={f.authorities}
								onChange={(authorities) => set({ authorities })}
							/>
						)}
						<Choice
							label="Client encryption"
							value={f.encryption}
							options={["TLS", "PLAINTEXT", "TLS_PLAINTEXT"]}
							onChange={(encryption) => set({ encryption })}
						/>
						<Checkbox
							checked={f.inCluster}
							onChange={({ detail }) => set({ inCluster: detail.checked })}
						>
							In-cluster encryption
						</Checkbox>
						<Field
							label="Tags (JSON string map)"
							value={f.tags}
							onChange={(tags) => set({ tags })}
						/>
					</>
				)}
				<Choice
					label="Enhanced monitoring"
					value={f.monitoring}
					options={monitoring}
					onChange={(monitoring) => set({ monitoring })}
				/>
			</SpaceBetween>
		</ActionDialog>
	);
}
