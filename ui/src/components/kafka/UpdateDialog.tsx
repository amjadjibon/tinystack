import { SpaceBetween } from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { Field } from "#/components/containers/Controls";
import { kafkaCall } from "#/lib/kafka";
import {
	type Cluster,
	clusterUpdateInput,
	type KafkaScope,
} from "#/lib/kafkaHelpers";
export function UpdateDialog({
	scope,
	cluster,
	action,
	onClose,
	onSaved,
}: {
	scope: KafkaScope;
	cluster: Cluster;
	action: string;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [value, setValue] = useState(""),
		[revision, setRevision] = useState("1");
	const op =
		action === "count"
			? "UpdateBrokerCount"
			: action === "storage"
				? "UpdateBrokerStorage"
				: "UpdateClusterConfiguration";
	return (
		<ActionDialog
			title={
				action === "count"
					? "Increase broker count"
					: action === "storage"
						? "Increase storage"
						: "Change configuration"
			}
			submit="Save changes"
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const input = clusterUpdateInput(cluster, action, value, revision);
				return [{ label: op, run: () => kafkaCall(scope, op, input) }];
			}}
		>
			<SpaceBetween size="m">
				<Field
					label={
						action === "count"
							? "Target broker count"
							: action === "storage"
								? "Target EBS size per broker (GiB)"
								: "Configuration ARN"
					}
					value={value}
					onChange={setValue}
				/>
				{action === "configuration" && (
					<Field
						label="Configuration revision"
						value={revision}
						onChange={setRevision}
					/>
				)}
			</SpaceBetween>
		</ActionDialog>
	);
}
