import { Alert, Checkbox, SpaceBetween } from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { Action, Buttons, Rows } from "#/components/containers/Controls";
import { Pick, PickMany, Text } from "#/components/memorydb/Fields";
import { Facts, ResourceTags } from "#/components/sesv2/Facts";
import {
	getConfigSet,
	getDestinations,
	resourceArn,
	type SESScope,
	sesv2Call,
} from "#/lib/sesv2";
import {
	type ConfigSetForm,
	createConfigSetInput,
	createDestinationInput,
	type DestinationForm,
	type DestinationKind,
	destinationForm,
	destinationKind,
	destinationKinds,
	destinationTarget,
	type EventDestination,
	emptyConfigSet,
	emptyDestination,
	eventTypes,
	tlsPolicies,
	updateDestinationInput,
} from "#/lib/sesv2Helpers";
import { usePoll } from "#/lib/usePoll";

export function SESConfigSets({
	scope,
	enabled,
	configSets,
	loading,
	onChanged,
}: {
	scope: SESScope;
	enabled: boolean;
	configSets: string[];
	loading: boolean;
	onChanged: () => void;
}) {
	const [dialog, setDialog] = useState<"" | "create" | "delete">(""),
		[form, setForm] = useState<ConfigSetForm>(emptyConfigSet),
		[target, setTarget] = useState(""),
		[selected, setSelected] = useState("");
	return (
		<SpaceBetween size="m">
			<Rows
				title="Configuration sets"
				emptyText="Choose Create configuration set to group sending options and event destinations."
				items={configSets}
				loading={loading}
				actions={
					<Buttons>
						<Action onClick={onChanged}>Refresh</Action>
						<Action
							disabled={!enabled}
							onClick={() => {
								setForm(emptyConfigSet);
								setDialog("create");
							}}
						>
							Create configuration set
						</Action>
					</Buttons>
				}
				columns={[
					{ id: "name", header: "Name", cell: (n) => n },
					{
						id: "actions",
						header: "Actions",
						cell: (n) => (
							<Buttons>
								<Action onClick={() => setSelected(n)}>View</Action>
								<Action
									disabled={!enabled}
									onClick={() => {
										setTarget(n);
										setDialog("delete");
									}}
								>
									Delete
								</Action>
							</Buttons>
						),
					},
				]}
			/>
			{selected && (
				<ConfigSetDetails
					key={selected}
					scope={scope}
					name={selected}
					enabled={enabled}
				/>
			)}
			{dialog === "create" && (
				<ActionDialog
					title="Create configuration set"
					submit="Create configuration set"
					onClose={() => setDialog("")}
					onSaved={() => {
						setDialog("");
						setSelected(form.name);
						onChanged();
					}}
					plan={async () => {
						const input = createConfigSetInput(form);
						return [
							{
								label: "CreateConfigurationSet",
								run: () => sesv2Call(scope, "CreateConfigurationSet", input),
							},
						];
					}}
				>
					<SpaceBetween size="m">
						<Text
							label="Configuration set name"
							value={form.name}
							onChange={(name) => setForm({ ...form, name })}
							constraint="1 to 64 letters, digits, hyphens or underscores."
						/>
						<Pick
							label="TLS policy"
							value={form.tlsPolicy}
							placeholder="Choose a TLS policy"
							options={tlsPolicies.map((value) => ({ value }))}
							onChange={(tlsPolicy) => setForm({ ...form, tlsPolicy })}
						/>
						<Checkbox
							checked={form.sendingEnabled}
							onChange={({ detail }) =>
								setForm({ ...form, sendingEnabled: detail.checked })
							}
						>
							Sending enabled
						</Checkbox>
						<Checkbox
							checked={form.reputationMetrics}
							onChange={({ detail }) =>
								setForm({ ...form, reputationMetrics: detail.checked })
							}
						>
							Reputation metrics enabled
						</Checkbox>
					</SpaceBetween>
				</ActionDialog>
			)}
			{dialog === "delete" && (
				<ActionDialog
					title="Delete configuration set"
					submit="Delete configuration set"
					confirmation={target}
					onClose={() => setDialog("")}
					onSaved={() => {
						setDialog("");
						if (selected === target) setSelected("");
						onChanged();
					}}
					plan={async () => [
						{
							label: "DeleteConfigurationSet",
							run: () =>
								sesv2Call(scope, "DeleteConfigurationSet", {
									ConfigurationSetName: target,
								}),
						},
					]}
				>
					<Alert type="warning">
						The event destinations of {target} are deleted with it. An identity
						that uses it as its default blocks the delete.
					</Alert>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}

function ConfigSetDetails({
	scope,
	name,
	enabled,
}: {
	scope: SESScope;
	name: string;
	enabled: boolean;
}) {
	const set = usePoll(() => getConfigSet(scope, name), 0),
		destinations = usePoll(() => getDestinations(scope, name), 0),
		d = set.data;
	const reload = () => {
		set.reload();
		destinations.reload();
	};
	return (
		<SpaceBetween size="m">
			{set.error && <Alert type="error">{set.error}</Alert>}
			<Facts
				title={name}
				actions={<Action onClick={reload}>Refresh</Action>}
				facts={[
					[
						"Sending",
						d
							? d.SendingOptions?.SendingEnabled
								? "Enabled"
								: "Disabled"
							: "",
					],
					["TLS policy", d?.DeliveryOptions?.TlsPolicy],
					[
						"Reputation metrics",
						d
							? d.ReputationOptions?.ReputationMetricsEnabled
								? "Enabled"
								: "Disabled"
							: "",
					],
					[
						"Suppressed reasons",
						d?.SuppressionOptions?.SuppressedReasons?.join(", "),
					],
					["Custom redirect domain", d?.TrackingOptions?.CustomRedirectDomain],
					["Sending pool", d?.DeliveryOptions?.SendingPoolName],
				]}
			/>
			<EventDestinations
				scope={scope}
				set={name}
				enabled={enabled}
				destinations={destinations.data ?? []}
				loading={destinations.loading}
				error={destinations.error}
				onChanged={reload}
			/>
			<ResourceTags
				scope={scope}
				arn={resourceArn(scope, "configuration-set", name)}
				enabled={enabled}
			/>
		</SpaceBetween>
	);
}

function EventDestinations({
	scope,
	set,
	enabled,
	destinations,
	loading,
	error,
	onChanged,
}: {
	scope: SESScope;
	set: string;
	enabled: boolean;
	destinations: EventDestination[];
	loading: boolean;
	error?: string;
	onChanged: () => void;
}) {
	const [dialog, setDialog] = useState<"" | "create" | "update" | "delete">(""),
		[form, setForm] = useState<DestinationForm>(emptyDestination);
	const open = (kind: "create" | "update" | "delete", d?: EventDestination) => {
		setForm(d ? destinationForm(d) : emptyDestination);
		setDialog(kind);
	};
	const patch = (change: Partial<DestinationForm>) =>
		setForm({ ...form, ...change });
	return (
		<>
			{error && <Alert type="error">{error}</Alert>}
			<Rows
				title="Event destinations"
				emptyText="Choose Add event destination to send events such as bounces to SNS, EventBridge or Firehose."
				items={destinations}
				loading={loading}
				actions={
					<Buttons>
						<Action disabled={!enabled} onClick={() => open("create")}>
							Add event destination
						</Action>
					</Buttons>
				}
				columns={[
					{ id: "name", header: "Name", cell: (d) => d.Name },
					{
						id: "type",
						header: "Type",
						cell: (d) =>
							destinationKinds.find((k) => k.value === destinationKind(d))
								?.label,
					},
					{ id: "target", header: "Target", cell: destinationTarget },
					{
						id: "events",
						header: "Events",
						cell: (d) => (d.MatchingEventTypes ?? []).join(", "),
					},
					{
						id: "enabled",
						header: "Enabled",
						cell: (d) => (d.Enabled ? "Yes" : "No"),
					},
					{
						id: "actions",
						header: "Actions",
						cell: (d) => (
							<Buttons>
								<Action disabled={!enabled} onClick={() => open("update", d)}>
									Update
								</Action>
								<Action disabled={!enabled} onClick={() => open("delete", d)}>
									Delete
								</Action>
							</Buttons>
						),
					},
				]}
			/>
			{(dialog === "create" || dialog === "update") && (
				<ActionDialog
					title={
						dialog === "create"
							? "Add event destination"
							: `Update ${form.name}`
					}
					submit={dialog === "create" ? "Add event destination" : "Update"}
					onClose={() => setDialog("")}
					onSaved={() => {
						setDialog("");
						onChanged();
					}}
					plan={async () => {
						const operation =
							dialog === "create"
								? "CreateConfigurationSetEventDestination"
								: "UpdateConfigurationSetEventDestination";
						const input =
							dialog === "create"
								? createDestinationInput(set, form)
								: updateDestinationInput(set, form);
						return [
							{
								label: operation,
								run: () => sesv2Call(scope, operation, input),
							},
						];
					}}
				>
					<SpaceBetween size="m">
						<Text
							label="Destination name"
							value={form.name}
							disabled={dialog === "update"}
							onChange={(name) => patch({ name })}
							constraint="1 to 64 letters, digits, hyphens or underscores."
						/>
						<Pick
							label="Destination type"
							value={form.kind}
							placeholder="Choose a destination type"
							options={destinationKinds}
							onChange={(kind) => patch({ kind: kind as DestinationKind })}
						/>
						{form.kind === "sns" && (
							<Text
								label="SNS topic ARN"
								value={form.topicArn}
								onChange={(topicArn) => patch({ topicArn })}
							/>
						)}
						{form.kind === "eventbridge" && (
							<Text
								label="Event bus ARN"
								value={form.busArn}
								onChange={(busArn) => patch({ busArn })}
							/>
						)}
						{form.kind === "firehose" && (
							<>
								<Text
									label="Delivery stream ARN"
									value={form.streamArn}
									onChange={(streamArn) => patch({ streamArn })}
								/>
								<Text
									label="IAM role ARN"
									value={form.roleArn}
									onChange={(roleArn) => patch({ roleArn })}
								/>
							</>
						)}
						<PickMany
							label="Event types"
							values={form.events}
							placeholder="Choose event types"
							options={eventTypes.map((value) => ({ value }))}
							onChange={(events) => patch({ events })}
						/>
						<Checkbox
							checked={form.enabled}
							onChange={({ detail }) => patch({ enabled: detail.checked })}
						>
							Enabled
						</Checkbox>
					</SpaceBetween>
				</ActionDialog>
			)}
			{dialog === "delete" && (
				<ActionDialog
					title="Delete event destination"
					submit="Delete event destination"
					onClose={() => setDialog("")}
					onSaved={() => {
						setDialog("");
						onChanged();
					}}
					plan={async () => [
						{
							label: "DeleteConfigurationSetEventDestination",
							run: () =>
								sesv2Call(scope, "DeleteConfigurationSetEventDestination", {
									ConfigurationSetName: set,
									EventDestinationName: form.name,
								}),
						},
					]}
				>
					<Alert type="warning">
						Events from {set} stop reaching {form.name}.
					</Alert>
				</ActionDialog>
			)}
		</>
	);
}
