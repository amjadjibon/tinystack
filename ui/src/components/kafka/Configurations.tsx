import { Alert, SpaceBetween } from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { Action, Buttons, Field, Rows } from "#/components/containers/Controls";
import { isoTime } from "#/lib/format";
import { kafkaCall, kafkaList } from "#/lib/kafka";
import {
	type Configuration,
	type ConfigurationRevision,
	configurationInput,
	type KafkaScope,
} from "#/lib/kafkaHelpers";
import { usePoll } from "#/lib/usePoll";
export function Configurations({
	scope,
	enabled,
}: {
	scope: KafkaScope;
	enabled: boolean;
}) {
	const [selected, setSelected] = useState<Configuration>(),
		[dialog, setDialog] = useState("");
	const poll = usePoll(
		() =>
			enabled
				? kafkaList<Configuration>(
						scope,
						"ListConfigurations",
						"Configurations",
					)
				: Promise.resolve([]),
		0,
	);
	const saved = () => {
		setDialog("");
		setSelected(undefined);
		poll.reload();
	};
	return (
		<SpaceBetween size="m">
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			<Rows
				title="Configurations"
				items={poll.data ?? []}
				loading={poll.loading}
				emptyText="Create a configuration with server.properties text."
				actions={
					<Buttons>
						<Action onClick={poll.reload}>Refresh</Action>
						<Action
							disabled={!enabled}
							onClick={() => {
								setSelected(undefined);
								setDialog("create");
							}}
						>
							Create configuration
						</Action>
					</Buttons>
				}
				columns={[
					{ id: "name", header: "Name", cell: (c) => c.Name },
					{ id: "arn", header: "ARN", cell: (c) => c.Arn },
					{
						id: "revision",
						header: "Latest revision",
						cell: (c) => c.LatestRevision?.Revision ?? "-",
					},
					{
						id: "created",
						header: "Created",
						cell: (c) => isoTime(c.CreationTime),
					},
					{
						id: "actions",
						header: "Actions",
						cell: (c) => (
							<Buttons>
								<Action
									onClick={() => {
										setSelected(c);
										setDialog("revisions");
									}}
								>
									Revisions
								</Action>
								<Action
									disabled={!enabled}
									onClick={() => {
										setSelected(c);
										setDialog("update");
									}}
								>
									Update
								</Action>
								<Action
									disabled={!enabled}
									onClick={() => {
										setSelected(c);
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
			{dialog === "revisions" && !!selected && (
				<Revisions
					key={selected.Arn}
					scope={scope}
					configuration={selected}
					onClose={() => setDialog("")}
				/>
			)}
			{(dialog === "create" || dialog === "update") && (
				<ConfigurationDialog
					key={selected?.Arn ?? "create"}
					scope={scope}
					configuration={selected}
					onClose={() => setDialog("")}
					onSaved={saved}
				/>
			)}
			{dialog === "delete" && !!selected && (
				<ActionDialog
					title="Delete configuration"
					submit="Delete configuration"
					confirmation={selected.Name}
					onClose={() => setDialog("")}
					onSaved={saved}
					plan={async () => [
						{
							label: "DeleteConfiguration",
							run: () =>
								kafkaCall(scope, "DeleteConfiguration", { Arn: selected.Arn }),
						},
					]}
				>
					<Alert type="warning">
						Delete all revisions of {selected.Name}. Configurations attached to
						clusters cannot be deleted.
					</Alert>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
function Revisions({
	scope,
	configuration,
	onClose,
}: {
	scope: KafkaScope;
	configuration: Configuration;
	onClose: () => void;
}) {
	const poll = usePoll(async () => {
		const c = await kafkaCall<Configuration>(scope, "DescribeConfiguration", {
			Arn: configuration.Arn,
		});
		const revisions = await kafkaList<ConfigurationRevision>(
			scope,
			"ListConfigurationRevisions",
			"Revisions",
			{ Arn: c.Arn },
		);
		return { c, revisions };
	}, 0);
	const [revision, setRevision] = useState<number>();

	return (
		<SpaceBetween size="m">
			<Action onClick={onClose}>Close revisions</Action>
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			<Rows
				title={`${configuration.Name} revisions`}
				emptyText="No revisions found."
				items={poll.data?.revisions ?? []}
				columns={[
					{
						id: "revision",
						header: "Revision",
						cell: (r) => (
							<Action onClick={() => setRevision(r.Revision)}>
								{r.Revision}
							</Action>
						),
					},
					{
						id: "description",
						header: "Description",
						cell: (r) => r.Description ?? "-",
					},
					{
						id: "created",
						header: "Created",
						cell: (r) => isoTime(r.CreationTime),
					},
				]}
			/>
			{!!revision && (
				<RevisionText
					key={revision}
					scope={scope}
					arn={configuration.Arn}
					revision={revision}
				/>
			)}
		</SpaceBetween>
	);
}
function ConfigurationDialog({
	scope,
	configuration,
	onClose,
	onSaved,
}: {
	scope: KafkaScope;
	configuration?: Configuration;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [name, setName] = useState(configuration?.Name ?? ""),
		[description, setDescription] = useState(""),
		[properties, setProperties] = useState("");
	const op = configuration ? "UpdateConfiguration" : "CreateConfiguration";
	return (
		<ActionDialog
			title={configuration ? `Update ${name}` : "Create configuration"}
			submit="Save configuration"
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const input = configurationInput(name, description, properties);
				return [
					{
						label: op,
						run: () =>
							kafkaCall(
								scope,
								op,
								configuration
									? {
											Arn: configuration.Arn,
											Description: input.Description,
											ServerProperties: input.ServerProperties,
										}
									: input,
							),
					},
				];
			}}
		>
			<SpaceBetween size="m">
				{!configuration && (
					<Field label="Name" value={name} onChange={setName} />
				)}
				<Field
					label="Revision description"
					value={description}
					onChange={setDescription}
				/>
				<Field
					label="server.properties"
					value={properties}
					onChange={setProperties}
					multiline
				/>
			</SpaceBetween>
		</ActionDialog>
	);
}

function RevisionText({
	scope,
	arn,
	revision,
}: {
	scope: KafkaScope;
	arn: string;
	revision: number;
}) {
	const text = usePoll(
		() =>
			kafkaCall<{ ServerProperties: string }>(
				scope,
				"DescribeConfigurationRevision",
				{ Arn: arn, Revision: revision },
			),
		0,
	);
	return (
		<SpaceBetween size="m">
			{!!text.error && <Alert type="error">{text.error}</Alert>}
			{!!text.data && (
				<pre>
					{new TextDecoder().decode(
						Uint8Array.from(atob(text.data.ServerProperties), (c) =>
							c.charCodeAt(0),
						),
					)}
				</pre>
			)}
		</SpaceBetween>
	);
}
