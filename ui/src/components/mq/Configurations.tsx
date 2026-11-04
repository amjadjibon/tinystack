import { Alert, SpaceBetween } from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import {
	Action,
	Buttons,
	Choice,
	Field,
	Rows,
} from "#/components/containers/Controls";
import { isoTime } from "#/lib/format";
import { mqCall, mqList } from "#/lib/mq";
import {
	configurationData,
	decodeConfiguration,
	type MQConfiguration,
	type MQScope,
} from "#/lib/mqHelpers";
import { usePoll } from "#/lib/usePoll";

type Revision = { Revision: number; Created?: string; Description?: string };
export function Configurations({
	scope,
	docker,
	enabled,
}: {
	scope: MQScope;
	docker: boolean;
	enabled: boolean;
}) {
	const poll = usePoll(
		() =>
			enabled
				? mqList<MQConfiguration>(scope, "ListConfigurations", "Configurations")
				: Promise.resolve([]),
		0,
	);
	const [selected, setSelected] = useState<MQConfiguration>(),
		[dialog, setDialog] = useState("");
	const [name, setName] = useState(""),
		[engine, setEngine] = useState("ACTIVEMQ"),
		[version, setVersion] = useState("5.18"),
		[data, setData] = useState(""),
		[description, setDescription] = useState(""),
		[error, setError] = useState("");
	const choose = (c: MQConfiguration) => {
		setSelected(c);
	};
	const view = async (c: MQConfiguration, revision: number, edit: boolean) => {
		try {
			const d = await mqCall<{ Data: string; Description?: string }>(
				scope,
				"DescribeConfigurationRevision",
				{ ConfigurationId: c.Id, ConfigurationRevision: String(revision) },
			);
			setSelected(c);
			setData(decodeConfiguration(d.Data));
			setDescription(d.Description ?? "");
			setDialog(edit ? "edit" : "view");
		} catch (e) {
			setError(String(e));
		}
	};
	return (
		<SpaceBetween size="m">
			{docker && (
				<Alert>Custom configurations are unsupported in Docker mode.</Alert>
			)}
			{!!(poll.error || error) && (
				<Alert type="error">{poll.error || error}</Alert>
			)}
			<Rows
				title="Configurations"
				items={poll.data ?? []}
				loading={poll.loading}
				emptyText="No configurations"
				actions={
					<Buttons>
						<Action onClick={poll.reload}>Refresh</Action>
						<Action
							disabled={docker || !enabled}
							onClick={() => {
								setName("");
								setDialog("create");
							}}
						>
							Create configuration
						</Action>
					</Buttons>
				}
				columns={[
					{ id: "name", header: "Name", cell: (c) => c.Name },
					{
						id: "engine",
						header: "Engine",
						cell: (c) => `${c.EngineType} ${c.EngineVersion}`,
					},
					{
						id: "revision",
						header: "Latest revision",
						cell: (c) => c.LatestRevision.Revision,
					},
					{ id: "created", header: "Created", cell: (c) => isoTime(c.Created) },
					{
						id: "actions",
						header: "Actions",
						cell: (c) => (
							<Buttons>
								<Action onClick={() => choose(c)}>Revisions</Action>
								<Action
									disabled={docker || !enabled}
									onClick={() => void view(c, c.LatestRevision.Revision, true)}
								>
									Edit data
								</Action>
								<Action
									disabled={docker || !enabled}
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
			{!!selected && (
				<RevisionList
					key={selected.Id}
					scope={scope}
					configuration={selected}
					onView={(n) => void view(selected, n, false)}
				/>
			)}
			{!!dialog && (
				<ActionDialog
					title={`${dialog} configuration`}
					submit={dialog === "view" ? "Close" : `${dialog} configuration`}
					confirmation={dialog === "delete" ? selected?.Name : undefined}
					onClose={() => setDialog("")}
					onSaved={() => {
						setDialog("");
						poll.reload();
					}}
					plan={async () => {
						if (dialog === "view") return [];
						const op =
							dialog === "create"
								? "CreateConfiguration"
								: dialog === "delete"
									? "DeleteConfiguration"
									: "UpdateConfiguration";
						const input =
							dialog === "create"
								? { Name: name, EngineType: engine, EngineVersion: version }
								: dialog === "delete"
									? { ConfigurationId: selected?.Id }
									: {
											ConfigurationId: selected?.Id,
											Data: configurationData(data),
											Description: description,
										};
						return [{ label: op, run: () => mqCall(scope, op, input) }];
					}}
				>
					{dialog === "create" ? (
						<SpaceBetween size="m">
							<Field label="Name" value={name} onChange={setName} />
							<Choice
								label="Engine"
								value={engine}
								options={["ACTIVEMQ", "RABBITMQ"]}
								onChange={(v) => {
									setEngine(v);
									setVersion(v === "ACTIVEMQ" ? "5.18" : "3.13");
								}}
							/>
							<Field
								label="Engine version"
								value={version}
								onChange={setVersion}
							/>
						</SpaceBetween>
					) : dialog === "delete" ? (
						<Alert>
							Delete {selected?.Name} and all revisions. Associated
							configurations cannot be deleted.
						</Alert>
					) : (
						<SpaceBetween size="m">
							<Field
								label="Description"
								value={description}
								onChange={(v) => {
									if (dialog !== "view") setDescription(v);
								}}
							/>
							<Field
								label="XML / Cuttlefish configuration"
								value={data}
								onChange={(v) => {
									if (dialog !== "view") setData(v);
								}}
								multiline
							/>
							<Alert>
								Saving creates a new revision. Select it on the broker and
								reboot to apply it.
							</Alert>
						</SpaceBetween>
					)}
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
function RevisionList({
	scope,
	configuration,
	onView,
}: {
	scope: MQScope;
	configuration: MQConfiguration;
	onView: (n: number) => void;
}) {
	const poll = usePoll(
		() =>
			mqList<Revision>(scope, "ListConfigurationRevisions", "Revisions", {
				ConfigurationId: configuration.Id,
			}),
		2000,
	);
	return (
		<SpaceBetween size="m">
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			<Rows
				title={`Revisions: ${configuration.Name}`}
				items={poll.data ?? []}
				loading={poll.loading}
				emptyText="No revisions"
				columns={[
					{ id: "revision", header: "Revision", cell: (r) => r.Revision },
					{
						id: "description",
						header: "Description",
						cell: (r) => r.Description ?? "-",
					},
					{ id: "created", header: "Created", cell: (r) => isoTime(r.Created) },
					{
						id: "view",
						header: "Data",
						cell: (r) => (
							<Action onClick={() => onView(r.Revision)}>View</Action>
						),
					},
				]}
			/>
		</SpaceBetween>
	);
}
