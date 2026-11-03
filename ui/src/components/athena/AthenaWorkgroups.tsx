import { Alert, Checkbox, SpaceBetween } from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { ConsoleTags } from "#/components/ConsoleTags";
import {
	Action,
	Buttons,
	Choice,
	Field,
	Rows,
} from "#/components/containers/Controls";
import { Text } from "#/components/memorydb/Fields";
import { type AthenaScope, athenaCall, athenaPages } from "#/lib/athena";
import {
	type WorkGroup,
	workgroupInput,
	workgroupSettings,
} from "#/lib/athenaHelpers";
import { unixTime } from "#/lib/format";
import { tagList, tagMap } from "#/lib/tags";
import { usePoll } from "#/lib/usePoll";
export function AthenaWorkgroups({
	scope,
	enabled,
	groups,
	onChanged,
}: {
	scope: AthenaScope;
	enabled: boolean;
	groups: WorkGroup[];
	onChanged: () => void;
}) {
	const [dialog, setDialog] = useState(""),
		[target, setTarget] = useState<WorkGroup>(),
		[view, setView] = useState(""),
		[name, setName] = useState(""),
		[description, setDescription] = useState(""),
		[output, setOutput] = useState(""),
		[enforce, setEnforce] = useState(true),
		[state, setState] = useState("ENABLED");
	const [error, setError] = useState("");
	const [settings, setSettings] = useState(workgroupSettings());
	const setting = <K extends keyof typeof settings>(
		key: K,
		value: (typeof settings)[K],
	) => setSettings((s) => ({ ...s, [key]: value }));
	async function edit(g: WorkGroup) {
		try {
			const out = await athenaCall<{ WorkGroup: WorkGroup }>(
				scope,
				"GetWorkGroup",
				{ WorkGroup: g.Name },
			);
			const w = out.WorkGroup;
			setTarget(w);
			setSettings(workgroupSettings(w));
			setName(w.Name);
			setDescription(w.Description ?? "");
			setOutput(w.Configuration?.ResultConfiguration?.OutputLocation ?? "");
			setEnforce(w.Configuration?.EnforceWorkGroupConfiguration ?? false);
			setState(w.State ?? "ENABLED");
			setDialog("edit");
			setError("");
		} catch (e) {
			setError(String(e));
		}
	}
	return (
		<SpaceBetween size="m">
			{error && <Alert type="error">{error}</Alert>}
			<Rows
				title="Workgroups"
				items={groups}
				emptyText="Create a workgroup to configure query output."
				actions={
					<Buttons>
						<Action onClick={onChanged}>Refresh</Action>
						<Action
							disabled={!enabled}
							onClick={() => {
								setSettings(workgroupSettings());
								setName("");
								setDescription("");
								setOutput("");
								setEnforce(true);
								setDialog("create");
							}}
						>
							Create workgroup
						</Action>
					</Buttons>
				}
				columns={[
					{ id: "name", header: "Name", cell: (g) => g.Name },
					{ id: "state", header: "State", cell: (g) => g.State },
					{
						id: "created",
						header: "Created",
						cell: (g) => unixTime(g.CreationTime),
					},
					{
						id: "description",
						header: "Description",
						cell: (g) => g.Description,
					},
					{
						id: "actions",
						header: "Actions",
						cell: (g) => (
							<Buttons>
								<Action disabled={!enabled} onClick={() => setView(g.Name)}>
									Details and tags
								</Action>
								<Action disabled={!enabled} onClick={() => edit(g)}>
									Edit
								</Action>
								<Action
									disabled={!enabled || g.Name === "primary"}
									onClick={() => {
										setTarget(g);
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
			{view && (
				<WorkgroupDetail
					key={view}
					scope={scope}
					name={view}
					enabled={enabled}
				/>
			)}
			{dialog && (
				<ActionDialog
					title={`${dialog === "create" ? "Create" : dialog === "edit" ? "Edit" : "Delete"} workgroup`}
					submit={dialog === "delete" ? "Delete" : "Save"}
					confirmation={dialog === "delete" ? target?.Name : undefined}
					onClose={() => setDialog("")}
					onSaved={() => {
						if (dialog === "delete" && target?.Name === view) setView("");
						setDialog("");
						onChanged();
					}}
					plan={async () => {
						const operation =
							dialog === "delete"
								? "DeleteWorkGroup"
								: dialog === "create"
									? "CreateWorkGroup"
									: "UpdateWorkGroup";
						const input =
							dialog === "delete"
								? { WorkGroup: target?.Name }
								: {
										...workgroupInput(
											name,
											description,
											output.trim(),
											enforce,
											dialog === "edit",
											settings,
										),
										...(dialog === "edit" ? { State: state } : {}),
									};
						return [
							{
								label: operation,
								run: () => athenaCall(scope, operation, input),
							},
						];
					}}
				>
					{dialog === "delete" ? (
						<Alert type="warning">
							Delete {target?.Name}. Query executions and saved queries remain
							stored.
						</Alert>
					) : (
						<SpaceBetween size="m">
							<Text
								disabled={dialog === "edit"}
								label="Name"
								value={name}
								onChange={dialog === "edit" ? () => {} : setName}
							/>
							<Field
								label="Description"
								value={description}
								onChange={setDescription}
							/>
							<Field
								label="Output location (S3 URI)"
								value={output}
								onChange={setOutput}
							/>
							<Alert type="info">
								Queries use the local SQLite engine. Encryption, expected owner,
								scan cutoff, metrics and requester-pays settings are retained as
								configuration metadata.
							</Alert>
							<Choice
								label="Result encryption"
								value={settings.encryption}
								options={["None", "SSE_S3", "SSE_KMS", "CSE_KMS"]}
								onChange={(v) => setting("encryption", v)}
							/>
							{settings.encryption.endsWith("KMS") && (
								<Field
									label="KMS key"
									value={settings.kmsKey}
									onChange={(v) => setting("kmsKey", v)}
								/>
							)}
							<Field
								label="Expected bucket owner"
								value={settings.owner}
								onChange={(v) => setting("owner", v)}
							/>
							<Field
								label="Bytes scanned cutoff per query (blank removes cutoff)"
								value={settings.cutoff}
								onChange={(v) => setting("cutoff", v)}
							/>
							<Choice
								label="Selected engine version"
								value={settings.engine}
								options={["AUTO", "Athena engine version 3"]}
								onChange={(v) => setting("engine", v)}
							/>
							<Checkbox
								checked={settings.metrics}
								onChange={({ detail }) => setting("metrics", detail.checked)}
							>
								Publish CloudWatch metrics
							</Checkbox>
							<Checkbox
								checked={settings.requesterPays}
								onChange={({ detail }) =>
									setting("requesterPays", detail.checked)
								}
							>
								Requester pays
							</Checkbox>
							<Checkbox
								checked={enforce}
								onChange={({ detail }) => setEnforce(detail.checked)}
							>
								Enforce workgroup configuration
							</Checkbox>
							{dialog === "edit" && (
								<Choice
									label="State"
									value={state}
									options={["ENABLED", "DISABLED"]}
									onChange={setState}
								/>
							)}
						</SpaceBetween>
					)}
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
function WorkgroupDetail({
	scope,
	name,
	enabled,
}: {
	scope: AthenaScope;
	name: string;
	enabled: boolean;
}) {
	const arn = `arn:aws:athena:${scope.region}:${scope.account}:workgroup/${name}`;
	const detail = usePoll(async () =>
		enabled
			? (
					await athenaCall<{ WorkGroup: WorkGroup }>(scope, "GetWorkGroup", {
						WorkGroup: name,
					})
				).WorkGroup
			: undefined,
	);
	const tags = usePoll(
		async () =>
			enabled
				? tagMap(
						await athenaPages<{ Key?: string; Value?: string }>(
							scope,
							"ListTagsForResource",
							"Tags",
							{ ResourceARN: arn },
						),
					)
				: {},
		0,
	);
	return (
		<SpaceBetween size="m">
			{detail.error && <Alert type="error">{detail.error}</Alert>}
			{tags.error && <Alert type="error">{tags.error}</Alert>}
			<Rows
				title={`Workgroup ${name}`}
				emptyText="Load workgroup details."
				items={
					detail.data
						? [
								{ label: "ARN", value: arn },
								{ label: "State", value: detail.data.State ?? "-" },
								{ label: "Created", value: unixTime(detail.data.CreationTime) },
								{
									label: "Output location",
									value:
										detail.data.Configuration?.ResultConfiguration
											?.OutputLocation ?? "-",
								},
								{
									label: "Enforce configuration",
									value: String(
										detail.data.Configuration?.EnforceWorkGroupConfiguration ??
											false,
									),
								},
								{
									label: "Encryption",
									value:
										detail.data.Configuration?.ResultConfiguration
											?.EncryptionConfiguration?.EncryptionOption ?? "-",
								},
								{
									label: "KMS key",
									value:
										detail.data.Configuration?.ResultConfiguration
											?.EncryptionConfiguration?.KmsKey ?? "-",
								},
								{
									label: "Expected bucket owner",
									value:
										detail.data.Configuration?.ResultConfiguration
											?.ExpectedBucketOwner ?? "-",
								},
								{
									label: "Engine version",
									value:
										detail.data.Configuration?.EngineVersion
											?.EffectiveEngineVersion ?? "-",
								},
							]
						: []
				}
				columns={[
					{ id: "setting", header: "Setting", cell: (r) => r.label },
					{ id: "value", header: "Value", cell: (r) => r.value },
				]}
			/>

			<ConsoleTags
				tags={tags.data ?? {}}
				enabled={enabled}
				onChanged={tags.reload}
				plan={(next, removed) => [
					...(Object.keys(next).length
						? [
								{
									label: "TagResource",
									run: () =>
										athenaCall(scope, "TagResource", {
											ResourceARN: arn,
											Tags: tagList(next),
										}),
								},
							]
						: []),
					...(removed.length
						? [
								{
									label: "UntagResource",
									run: () =>
										athenaCall(scope, "UntagResource", {
											ResourceARN: arn,
											TagKeys: removed,
										}),
								},
							]
						: []),
				]}
			/>
		</SpaceBetween>
	);
}
