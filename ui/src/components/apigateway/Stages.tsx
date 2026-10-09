import {
	Alert,
	Box,
	Button,
	Checkbox,
	Container,
	Header,
	SpaceBetween,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { CodeBlock } from "#/components/CodeBlock";
import { ConsoleTags } from "#/components/ConsoleTags";
import {
	apiCall,
	type GatewayScope,
	type GatewaySnapshot,
	type GatewayStage,
	gatewayARN,
	gatewayTagPlan,
	isHTTP,
	stageInput,
} from "#/lib/apigateway";
import { stringMap } from "#/lib/consoleActions";
import { GatewayInput, GatewaySelect, GatewayText } from "./Fields";
import { GatewayTable } from "./GatewayTable";
export function StageForm({
	scope,
	snapshot,
	initial,
	onClose,
	onSaved,
}: {
	scope: GatewayScope;
	snapshot: GatewaySnapshot;
	initial?: GatewayStage;
	onClose: () => void;
	onSaved: () => void;
}) {
	const http = isHTTP(scope),
		[name, setName] = useState(
			initial?.StageName ?? (http ? "$default" : "dev"),
		),
		[description, setDescription] = useState(initial?.Description ?? ""),
		[auto, setAuto] = useState(initial?.AutoDeploy ?? http);
	const [deployment, setDeployment] = useState(initial?.DeploymentId ?? ""),
		[variables, setVariables] = useState(
			JSON.stringify(
				initial?.StageVariables ?? initial?.Variables ?? {},
				null,
				2,
			),
		),
		[logs, setLogs] = useState(
			JSON.stringify(initial?.AccessLogSettings ?? {}, null, 2),
		),
		[tags, setTags] = useState("{}");
	return (
		<ActionDialog
			title={initial ? "Edit stage" : "Create stage"}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				if (
					!(http && name === "$default") &&
					!/^[A-Za-z0-9_-]{1,128}$/.test(name)
				)
					throw new Error(
						"Stage names use 1–128 letters, numbers, hyphens or underscores.",
					);
				if ((!http || !auto) && !deployment && !initial)
					throw new Error("Choose a deployment or enable auto deploy.");
				const next = stageInput(scope, initial ?? { StageName: name }, {
					description,
					auto,
					deployment,
					variables,
					logs,
				});
				return [
					{
						label: "Save stage",
						run: () =>
							apiCall(
								scope,
								initial ? "UpdateStage" : "CreateStage",
								initial
									? next
									: http
										? { ...next, Tags: stringMap(tags, "Tags") }
										: {
												StageName: name,
												DeploymentId: deployment,
												Description: description,
												Variables: stringMap(variables, "Variables"),
												Tags: stringMap(tags, "Tags"),
											},
							),
					},
					...(!initial &&
					!http &&
					Object.keys(stringMap(logs, "Access logs")).length
						? [
								{
									label: "Set access logs",
									run: () =>
										apiCall(
											scope,
											"UpdateStage",
											stageInput(
												scope,
												{
													StageName: name,
													Variables: stringMap(variables, "Variables"),
												},
												{ description, auto, deployment, variables, logs },
											),
										),
								},
							]
						: []),
				];
			}}
		>
			<SpaceBetween size="l">
				<GatewayInput
					label="Stage name"
					value={name}
					onChange={setName}
					disabled={!!initial}
				/>
				<GatewayInput
					label="Description"
					value={description}
					onChange={setDescription}
				/>
				{http && (
					<Checkbox
						checked={auto}
						onChange={({ detail }) => setAuto(detail.checked)}
					>
						Auto deploy changes to this stage
					</Checkbox>
				)}
				{(!http || !auto) && (
					<GatewaySelect
						label="Deployment"
						value={deployment}
						onChange={setDeployment}
						options={snapshot.deployments.map((d) => ({
							value: d.DeploymentId ?? d.Id ?? "",
							label: `${d.DeploymentId ?? d.Id} · ${d.Description ?? ""}`,
						}))}
					/>
				)}
				<GatewayText
					label="Stage variables (JSON)"
					value={variables}
					onChange={setVariables}
				/>
				<GatewayText
					label="Access log settings (JSON)"
					value={logs}
					onChange={setLogs}
					description="DestinationArn and Format. Use {} to disable logs."
				/>
				{!initial && (
					<GatewayText
						label="Stage tags (JSON)"
						value={tags}
						onChange={setTags}
					/>
				)}
				<Alert type="info">
					Publishing or switching a deployment changes the requests served by
					this stage.
				</Alert>
			</SpaceBetween>
		</ActionDialog>
	);
}
export function Stages({
	scope,
	snapshot,
	enabled,
	onChanged,
}: {
	scope: GatewayScope;
	snapshot: GatewaySnapshot;
	enabled: boolean;
	onChanged: (message: string) => void;
}) {
	const [dialog, setDialog] = useState(""),
		[selected, setSelected] = useState<GatewayStage>(),
		[stage, setStage] = useState(""),
		[description, setDescription] = useState(""),
		[deployment, setDeployment] = useState("");
	const http = isHTTP(scope),
		done = (message: string) => {
			setDialog("");
			onChanged(message);
		};
	return (
		<SpaceBetween size="l">
			<GatewayTable
				items={snapshot.stages}
				trackBy="StageName"
				wrapLines
				header={
					<Header
						actions={
							<SpaceBetween direction="horizontal" size="xs">
								<Button disabled={!enabled} onClick={() => setDialog("deploy")}>
									Deploy API
								</Button>
								<Button
									variant="primary"
									disabled={!enabled}
									onClick={() => {
										setSelected(undefined);
										setDialog("stage");
									}}
								>
									Create stage
								</Button>
							</SpaceBetween>
						}
					>
						Stages
					</Header>
				}
				columnDefinitions={[
					{ id: "name", header: "Stage", cell: (s) => s.StageName },
					{
						id: "deployment",
						header: "Deployment",
						cell: (s) => s.DeploymentId ?? "None",
					},
					...(http
						? [
								{
									id: "auto",
									header: "Auto deploy",
									cell: (s: GatewayStage) =>
										s.AutoDeploy ? "Enabled" : "Disabled",
								},
							]
						: []),
					{
						id: "actions",
						header: "Actions",
						cell: (s) => (
							<SpaceBetween direction="horizontal" size="xs">
								<Button
									variant="inline-link"
									disabled={!enabled}
									onClick={() => {
										setSelected(s);
										setDialog("stage");
									}}
								>
									Edit
								</Button>
								<Button
									variant="inline-link"
									disabled={!enabled}
									onClick={() => {
										setSelected(s);
										setDialog("delete-stage");
									}}
								>
									Delete
								</Button>
								<Button
									variant="inline-link"
									onClick={() => {
										setSelected(s);
										setDialog("details");
									}}
								>
									Details
								</Button>
							</SpaceBetween>
						),
					},
				]}
				empty={
					<Box>
						No stages. Deploy a REST API to a stage, or create an HTTP stage.
					</Box>
				}
			/>
			<GatewayTable
				items={snapshot.deployments}
				trackBy={(d) => d.DeploymentId ?? d.Id ?? ""}
				header={<Header>Deployments</Header>}
				columnDefinitions={[
					{
						id: "id",
						header: "ID",
						cell: (d) => d.DeploymentId ?? d.Id ?? "—",
					},
					{
						id: "description",
						header: "Description",
						cell: (d) => d.Description || "—",
					},
					{
						id: "created",
						header: "Created",
						cell: (d) =>
							typeof d.CreatedDate === "number"
								? new Date(d.CreatedDate * 1000).toISOString()
								: (d.CreatedDate ?? "—"),
					},
					{
						id: "delete",
						header: "Actions",
						cell: (d) => (
							<Button
								variant="inline-link"
								disabled={!enabled}
								onClick={() => {
									setDeployment(d.DeploymentId ?? d.Id ?? "");
									setDialog("delete-deployment");
								}}
							>
								Delete
							</Button>
						),
					},
				]}
				empty={<Box>No deployment snapshots.</Box>}
			/>
			{dialog === "details" && selected && (
				<Container
					header={
						<Header
							actions={<Button onClick={() => setDialog("")}>Close</Button>}
						>
							Stage configuration
						</Header>
					}
				>
					<SpaceBetween size="l">
						<CodeBlock text={JSON.stringify(selected, null, 2)} />
						<ConsoleTags
							tags={selected.Tags ?? {}}
							enabled={enabled}
							plan={(tags, removed) =>
								gatewayTagPlan(
									scope,
									gatewayARN(scope, selected.StageName),
									tags,
									removed,
								)
							}
							onChanged={() => done("Stage tags saved.")}
						/>
					</SpaceBetween>
				</Container>
			)}
			{dialog === "stage" && (
				<StageForm
					scope={scope}
					snapshot={snapshot}
					initial={selected}
					onClose={() => setDialog("")}
					onSaved={() => done("Stage saved.")}
				/>
			)}
			{dialog === "deploy" && (
				<ActionDialog
					title="Deploy API"
					submit="Deploy API"
					onClose={() => setDialog("")}
					onSaved={() => done("API deployed.")}
					plan={async () => {
						if (
							stage &&
							!(http && stage === "$default") &&
							!/^[A-Za-z0-9_-]{1,128}$/.test(stage)
						)
							throw new Error("Enter a valid stage name.");
						return [
							{
								label: "Deploy API",
								run: () =>
									apiCall(scope, "CreateDeployment", {
										Description: description,
										...(stage ? { StageName: stage } : {}),
									}),
							},
						];
					}}
				>
					<SpaceBetween size="l">
						{http ? (
							<GatewaySelect
								label="Stage"
								value={stage}
								onChange={setStage}
								options={[
									{ value: "", label: "Create snapshot only" },
									...snapshot.stages
										.filter((s) => !s.AutoDeploy)
										.map((s) => ({ value: s.StageName, label: s.StageName })),
								]}
							/>
						) : (
							<GatewayInput
								label="Stage name — optional"
								value={stage}
								onChange={setStage}
								description="Deploy to an existing stage or create a new stage. Leave empty to create a snapshot only."
							/>
						)}
						<GatewayInput
							label="Deployment description"
							value={description}
							onChange={setDescription}
						/>
						<Alert type="warning">
							This captures the current routes, integrations and authorizers.
							Deploying to a stage changes live behavior.
						</Alert>
					</SpaceBetween>
				</ActionDialog>
			)}
			{(dialog === "delete-stage" || dialog === "delete-deployment") && (
				<ActionDialog
					title={
						dialog === "delete-stage" ? "Delete stage" : "Delete deployment"
					}
					submit="Delete"
					confirmation={
						dialog === "delete-stage" ? selected?.StageName : deployment
					}
					onClose={() => setDialog("")}
					onSaved={() => done("Deleted.")}
					plan={async () => [
						{
							label: "Delete",
							run: () =>
								apiCall(
									scope,
									dialog === "delete-stage"
										? "DeleteStage"
										: "DeleteDeployment",
									dialog === "delete-stage"
										? { StageName: selected?.StageName }
										: { DeploymentId: deployment },
								),
						},
					]}
				>
					<Box>
						{dialog === "delete-stage"
							? "Requests to this stage will stop working."
							: "Deployments referenced by a stage cannot be deleted."}
					</Box>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
