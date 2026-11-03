import {
	Alert,
	CopyToClipboard,
	SpaceBetween,
} from "@cloudscape-design/components";
import { Rows } from "#/components/containers/Controls";
import { isoTime } from "#/lib/format";
import { kafkaCall, kafkaList } from "#/lib/kafka";
import type { Cluster, KafkaScope, Node } from "#/lib/kafkaHelpers";
import { usePoll } from "#/lib/usePoll";
export function Details({
	scope,
	cluster: c,
	docker,
}: {
	scope: KafkaScope;
	cluster: Cluster;
	docker: boolean;
}) {
	const ready = !docker || c.State === "ACTIVE";
	const poll = usePoll(async () => {
		if (!ready) return undefined;
		const [brokers, nodes] = await Promise.all([
			kafkaCall<Record<string, string>>(scope, "GetBootstrapBrokers", {
				ClusterArn: c.ClusterArn,
			}),
			kafkaList<Node>(scope, "ListNodes", "NodeInfoList", {
				ClusterArn: c.ClusterArn,
			}),
		]);
		return { brokers, nodes };
	}, 0);
	const copy = (value: string) => (
		<CopyToClipboard
			variant="inline"
			textToCopy={value}
			copySuccessText="Copied"
			copyErrorText="Copy failed"
		/>
	);
	return (
		<SpaceBetween size="m">
			<div>ARN: {copy(c.ClusterArn)}</div>
			<div>
				State: {c.State ?? "-"} · Created: {isoTime(c.CreationTime)}
			</div>
			{!!c.StateInfo?.Message && (
				<Alert type="error" header={c.StateInfo.Code}>
					{c.StateInfo.Message}
				</Alert>
			)}
			<div>
				Type: Provisioned · Kafka{" "}
				{c.CurrentBrokerSoftwareInfo?.KafkaVersion ?? "-"} · Metadata: ZooKeeper
				(no ZooKeeper runtime)
			</div>
			<div>Current version: {c.CurrentVersion ?? "-"}</div>
			<div>
				Requested brokers: {c.NumberOfBrokerNodes ?? "-"} · Instance:{" "}
				{c.BrokerNodeGroupInfo?.InstanceType ?? "-"} · Storage per broker:{" "}
				{c.BrokerNodeGroupInfo?.StorageInfo?.EbsStorageInfo?.VolumeSize ?? "-"}{" "}
				GiB
			</div>
			<div>
				Subnets: {c.BrokerNodeGroupInfo?.ClientSubnets?.join(", ") || "-"}
			</div>
			<div>
				Security groups:{" "}
				{c.BrokerNodeGroupInfo?.SecurityGroups?.join(", ") || "-"}
			</div>
			<div>Enhanced monitoring: {c.EnhancedMonitoring ?? "-"}</div>
			<div>
				Client authentication:{" "}
				<pre>{JSON.stringify(c.ClientAuthentication ?? {}, null, 2)}</pre>
			</div>
			<div>
				Encryption in transit:{" "}
				<pre>{JSON.stringify(c.EncryptionInfo ?? {}, null, 2)}</pre>
			</div>
			{!ready && (
				<Alert>
					Connection details are available once the cluster is ACTIVE.
				</Alert>
			)}
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			{ready &&
				Object.entries(poll.data?.brokers ?? {}).map(([key, value]) => (
					<div key={key}>
						{(
							{
								BootstrapBrokerString: "Bootstrap brokers (plaintext)",
								BootstrapBrokerStringTls: "Bootstrap brokers (TLS)",
								BootstrapBrokerStringSaslScram:
									"Bootstrap brokers (SASL/SCRAM)",
								BootstrapBrokerStringSaslIam: "Bootstrap brokers (SASL/IAM)",
							} as Record<string, string>
						)[key] ?? key}
						: {copy(value)}
					</div>
				))}
			<Rows
				title="Broker nodes"
				items={ready ? (poll.data?.nodes ?? []) : []}
				loading={poll.loading}
				emptyText="Connection details appear when the cluster is ready."
				columns={[
					{
						id: "id",
						header: "Broker ID",
						cell: (n) => n.BrokerNodeInfo?.BrokerId ?? "-",
					},
					{
						id: "instance",
						header: "Instance type",
						cell: (n) => n.InstanceType ?? "-",
					},
					{
						id: "subnet",
						header: "Subnet",
						cell: (n) => n.BrokerNodeInfo?.ClientSubnet ?? "-",
					},
					{
						id: "endpoints",
						header: "Endpoints",
						cell: (n) => n.BrokerNodeInfo?.Endpoints?.join(", ") || "-",
					},
					{
						id: "added",
						header: "Added",
						cell: (n) => isoTime(n.AddedToClusterTime),
					},
				]}
			/>
		</SpaceBetween>
	);
}
