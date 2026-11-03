import { Alert, Button, SpaceBetween } from "@cloudscape-design/components";
import { useState } from "react";
import { Choice, Field, Rows } from "#/components/containers/Controls";
import { errorText } from "#/lib/consoleActions";
import { type CloudMapScope, cloudMapCall } from "#/lib/servicediscovery";
import { discoveryInput, type Pair } from "#/lib/servicediscoveryHelpers";
import { Pairs } from "./Fields";

type Result = {
	InstanceId: string;
	NamespaceName: string;
	ServiceName: string;
	HealthStatus: string;
	Attributes: Record<string, string>;
};
export function Discover({
	scope,
	enabled,
	namespaceName,
}: {
	scope: CloudMapScope;
	enabled: boolean;
	namespaceName: string;
}) {
	const [namespace, setNamespace] = useState(namespaceName),
		[service, setService] = useState("");
	const [query, setQuery] = useState<Pair[]>([]),
		[optional, setOptional] = useState<Pair[]>([]);
	const [health, setHealth] = useState("HEALTHY"),
		[error, setError] = useState("");
	const [busy, setBusy] = useState(false),
		[result, setResult] = useState<{
			Instances: Result[];
			InstancesRevision?: number;
		}>();
	const discover = async () => {
		setBusy(true);
		setError("");
		setResult(undefined);
		try {
			setResult(
				await cloudMapCall(
					scope,
					"DiscoverInstances",
					discoveryInput(namespace, service, query, optional, health),
				),
			);
		} catch (err) {
			setError(errorText(err));
		} finally {
			setBusy(false);
		}
	};
	return (
		<SpaceBetween size="m">
			<h2>Discover instances</h2>
			<Field label="Namespace name" value={namespace} onChange={setNamespace} />
			<Field label="Service name" value={service} onChange={setService} />
			<Pairs label="Query parameters" rows={query} onChange={setQuery} />
			<Pairs
				label="Optional parameters"
				rows={optional}
				onChange={setOptional}
			/>
			<Choice
				label="Health status"
				value={health}
				options={["HEALTHY", "UNHEALTHY", "ALL", "HEALTHY_OR_ELSE_ALL"]}
				onChange={setHealth}
			/>
			<Button
				variant="primary"
				disabled={!enabled}
				loading={busy}
				onClick={discover}
			>
				Discover instances
			</Button>
			{error && <Alert type="error">{error}</Alert>}
			{result && (
				<SpaceBetween size="m">
					<p>Instances revision: {result.InstancesRevision ?? "-"}</p>
					<Rows
						title="Discovery results"
						items={result.Instances ?? []}
						emptyText="No matching instances."
						columns={[
							{ id: "id", header: "Instance ID", cell: (i) => i.InstanceId },
							{
								id: "namespace",
								header: "Namespace",
								cell: (i) => i.NamespaceName,
							},
							{ id: "service", header: "Service", cell: (i) => i.ServiceName },
							{ id: "health", header: "Health", cell: (i) => i.HealthStatus },
							{
								id: "attributes",
								header: "Attributes",
								cell: (i) => JSON.stringify(i.Attributes),
							},
						]}
					/>
				</SpaceBetween>
			)}
		</SpaceBetween>
	);
}
