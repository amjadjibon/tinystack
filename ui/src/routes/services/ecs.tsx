import {
	Alert,
	Checkbox,
	Header,
	SpaceBetween,
	Tabs,
} from "@cloudscape-design/components";
import { createFileRoute } from "@tanstack/react-router";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import {
	Action,
	Buttons,
	Choice,
	ContainerLayout,
	Field,
	Rows,
} from "#/components/containers/Controls";
import {
	arnTail,
	containerPages,
	desiredCount,
	taskDefinitionInput,
} from "#/lib/containerHelpers";
import type { ContainerScope } from "#/lib/ecr";
import {
	type Cluster,
	type ECSService,
	type ECSTask,
	ecsCall,
	ecsDescribe,
} from "#/lib/ecs";
import { validRegion } from "#/lib/s3";
import { useScope } from "#/lib/scope";
import { usePoll } from "#/lib/usePoll";
export const Route = createFileRoute("/services/ecs")({
	validateSearch: (search: Record<string, unknown>) => ({
		account: String(search.account ?? ""),
		region: String(search.region ?? ""),
	}),
	component: Page,
});
function Page() {
	const s = useScope();
	const enabled =
		!!s.capabilities.data?.pages.ecs &&
		s.canWrite &&
		/^\d{12}$/.test(s.account) &&
		validRegion(s.region);
	return (
		<ContainerLayout service="ecs" title="Elastic Container Service">
			<Alert
				type="info"
				header={
					s.capabilities.data?.ecs_runtime === "docker"
						? "Docker runtime configured"
						: "Records-only runtime"
				}
			>
				{s.capabilities.data?.ecs_runtime === "docker"
					? "Tasks use the configured Docker runtime. Check task status and stopped reasons for execution results."
					: "Clusters, services and tasks are stored as records. Running a task does not start containers. Docker is optional."}
			</Alert>
			<Workspace
				key={`${s.account}/${s.region}/${enabled}`}
				scope={s}
				enabled={enabled}
			/>
		</ContainerLayout>
	);
}
function Workspace({
	scope,
	enabled,
}: {
	scope: ContainerScope;
	enabled: boolean;
}) {
	const call = (op: string, input: Record<string, unknown> = {}) =>
		ecsCall(scope, op, input);
	const clusters = usePoll(async () =>
		enabled
			? ecsDescribe<Cluster>(
					scope,
					"DescribeClusters",
					"Clusters",
					"clusters",
					await containerPages<string>(call, "ListClusters", "ClusterArns"),
				)
			: [],
	);
	const [cluster, setCluster] = useState(""),
		[dialog, setDialog] = useState(""),
		[name, setName] = useState("");
	return (
		<SpaceBetween size="l">
			{clusters.error && <Alert type="error">{clusters.error}</Alert>}
			<Rows
				title="Clusters"
				emptyText="Choose Create cluster to get started."
				items={clusters.data ?? []}
				loading={clusters.loading}
				actions={
					<Buttons>
						<Action onClick={clusters.reload}>Refresh</Action>
						<Action
							disabled={!enabled}
							onClick={() => {
								setName("");
								setDialog("Create cluster");
							}}
						>
							Create cluster
						</Action>
					</Buttons>
				}
				columns={[
					{
						id: "name",
						header: "Cluster",
						cell: (c) => (
							<Action onClick={() => setCluster(c.ClusterArn)}>
								{c.ClusterName}
							</Action>
						),
					},
					{ id: "status", header: "Status", cell: (c) => c.Status },
					{ id: "arn", header: "ARN", cell: (c) => c.ClusterArn },
					{
						id: "actions",
						header: "Actions",
						cell: (c) => (
							<Action
								onClick={() => {
									setName(c.ClusterArn);
									setDialog("Delete cluster");
								}}
							>
								Delete cluster
							</Action>
						),
					},
				]}
			/>
			{cluster && (
				<ClusterDetails
					key={cluster}
					scope={scope}
					cluster={cluster}
					enabled={enabled}
				/>
			)}
			<Definitions
				key={`${scope.account}/${scope.region}`}
				scope={scope}
				enabled={enabled}
			/>
			{dialog && (
				<ActionDialog
					title={dialog}
					submit={dialog}
					confirmation={dialog === "Delete cluster" ? arnTail(name) : undefined}
					onClose={() => setDialog("")}
					onSaved={() => {
						setDialog("");
						if (dialog === "Delete cluster" && cluster === name) setCluster("");
						clusters.reload();
					}}
					plan={async () => {
						if (!name.trim()) throw new Error("Enter a cluster name.");
						return [
							{
								label: dialog,
								run: () =>
									call(
										dialog === "Create cluster"
											? "CreateCluster"
											: "DeleteCluster",
										dialog === "Create cluster"
											? { clusterName: name }
											: { cluster: name },
									),
							},
						];
					}}
				>
					{dialog === "Create cluster" ? (
						<Field label="Cluster name" value={name} onChange={setName} />
					) : (
						"Delete this empty cluster. Stop tasks and remove services first."
					)}
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
function Definitions({
	scope,
	enabled,
}: {
	scope: ContainerScope;
	enabled: boolean;
}) {
	const [status, setStatus] = useState("ACTIVE");
	return (
		<SpaceBetween size="m">
			<Choice
				label="Task definition status"
				value={status}
				options={["ACTIVE", "INACTIVE"]}
				onChange={setStatus}
			/>
			<DefinitionList
				key={status}
				scope={scope}
				enabled={enabled}
				status={status}
			/>
		</SpaceBetween>
	);
}
function DefinitionList({
	scope,
	enabled,
	status,
}: {
	scope: ContainerScope;
	enabled: boolean;
	status: string;
}) {
	const call = (op: string, input: Record<string, unknown> = {}) =>
		ecsCall(scope, op, input);
	const poll = usePoll(() =>
		enabled
			? containerPages<string>(
					call,
					"ListTaskDefinitions",
					"TaskDefinitionArns",
					{ status, sort: "DESC" },
				)
			: Promise.resolve([]),
	);
	const [dialog, setDialog] = useState(""),
		[arn, setArn] = useState(""),
		[text, setText] = useState(""),
		[revealed, setRevealed] = useState(false),
		[error, setError] = useState("");
	const view = async (a: string) => {
		setError("");
		try {
			const out = await call("DescribeTaskDefinition", { taskDefinition: a });
			setText(JSON.stringify(out.TaskDefinition, null, 2));
			setArn(a);
			setRevealed(false);
			setDialog("View task definition");
		} catch (e) {
			setError(String(e));
		}
	};
	return (
		<SpaceBetween size="m">
			{(poll.error || error) && (
				<Alert type="error">{poll.error || error}</Alert>
			)}
			<Rows
				title="Task definition revisions"
				emptyText="Choose Register task definition to add a revision."
				items={poll.data ?? []}
				actions={
					<Buttons>
						<Action onClick={poll.reload}>Refresh</Action>
						<Action
							disabled={!enabled}
							onClick={() => {
								setText(
									JSON.stringify(
										{
											family: "example",
											containerDefinitions: [
												{
													name: "app",
													image: "example:latest",
													memory: 128,
													essential: true,
												},
											],
										},
										null,
										2,
									),
								);
								setDialog("Register task definition");
							}}
						>
							Register task definition
						</Action>
					</Buttons>
				}
				columns={[
					{
						id: "arn",
						header: "Revision ARN",
						cell: (a) => (
							<Action
								onClick={() => {
									void view(a);
								}}
							>
								{a}
							</Action>
						),
					},
					{
						id: "actions",
						header: "Actions",
						cell: (a) => (
							<Action
								disabled={status !== "ACTIVE"}
								onClick={() => {
									setArn(a);
									setDialog("Deregister task definition");
								}}
							>
								Deregister
							</Action>
						),
					},
				]}
			/>
			{dialog && (
				<ActionDialog
					title={dialog}
					submit={dialog === "View task definition" ? "Close" : dialog}
					confirmation={
						dialog === "Deregister task definition" ? arnTail(arn) : undefined
					}
					onClose={() => setDialog("")}
					onSaved={() => {
						setDialog("");
						poll.reload();
					}}
					plan={async () =>
						dialog === "View task definition"
							? []
							: [
									{
										label: dialog,
										run: () =>
											call(
												dialog === "Register task definition"
													? "RegisterTaskDefinition"
													: "DeregisterTaskDefinition",
												dialog === "Register task definition"
													? taskDefinitionInput(text)
													: { taskDefinition: arn },
											),
									},
								]
					}
				>
					{dialog === "Register task definition" ? (
						<Field
							multiline
							label="Task definition JSON (registration request)"
							value={text}
							onChange={setText}
						/>
					) : dialog === "View task definition" ? (
						<SpaceBetween size="m">
							<Checkbox
								checked={revealed}
								onChange={({ detail }) => setRevealed(detail.checked)}
							>
								Reveal definition (may contain environment secrets)
							</Checkbox>
							{revealed && (
								<pre
									style={{ whiteSpace: "pre-wrap", overflowWrap: "anywhere" }}
								>
									{text}
								</pre>
							)}
						</SpaceBetween>
					) : (
						"Existing tasks continue running; this revision becomes inactive."
					)}
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
function ClusterDetails({
	scope,
	cluster,
	enabled,
}: {
	scope: ContainerScope;
	cluster: string;
	enabled: boolean;
}) {
	const call = (op: string, input: Record<string, unknown> = {}) =>
		ecsCall(scope, op, { cluster, ...input });
	const definitions = usePoll(() =>
		enabled
			? containerPages<string>(
					(op, input) => ecsCall(scope, op, input),
					"ListTaskDefinitions",
					"TaskDefinitionArns",
					{ status: "ACTIVE", sort: "DESC" },
				)
			: Promise.resolve([]),
	);
	const services = usePoll(async () =>
		enabled
			? ecsDescribe<ECSService>(
					scope,
					"DescribeServices",
					"Services",
					"services",
					await containerPages<string>(call, "ListServices", "ServiceArns"),
					{ cluster },
					10,
				)
			: [],
	);
	const [taskStatus, setTaskStatus] = useState("RUNNING"),
		[dialog, setDialog] = useState(""),
		[name, setName] = useState(""),
		[definition, setDefinition] = useState(""),
		[count, setCount] = useState("1"),
		[force, setForce] = useState(false);
	const changed = () => {
		setDialog("");
		services.reload();
	};
	const openService = (service?: ECSService) => {
		setName(service?.ServiceName ?? "");
		setDefinition(service?.TaskDefinition ?? definitions.data?.[0] ?? "");
		setCount(String(service?.DesiredCount ?? 1));
		setDialog(service ? "Update service" : "Create service");
	};
	return (
		<SpaceBetween size="m">
			<Header>{cluster.split("/").at(-1)}</Header>
			{definitions.error && <Alert type="error">{definitions.error}</Alert>}
			<Tabs
				tabs={[
					{
						id: "services",
						label: "Services",
						content: (
							<SpaceBetween size="m">
								{services.error && <Alert type="error">{services.error}</Alert>}
								<Rows
									title="Services"
									emptyText="Choose Create service to run a task definition as a service."
									items={services.data ?? []}
									actions={
										<Buttons>
											<Action onClick={services.reload}>Refresh</Action>
											<Action
												disabled={!definitions.data?.length}
												onClick={() => openService()}
											>
												Create service
											</Action>
										</Buttons>
									}
									columns={[
										{
											id: "name",
											header: "Service",
											cell: (s) => s.ServiceName,
										},
										{ id: "status", header: "Status", cell: (s) => s.Status },
										{
											id: "desired",
											header: "Desired count",
											cell: (s) => s.DesiredCount,
										},
										{
											id: "running",
											header: "Running",
											cell: (s) => s.RunningCount,
										},
										{
											id: "definition",
											header: "Task definition",
											cell: (s) => s.TaskDefinition,
										},
										{
											id: "actions",
											header: "Actions",
											cell: (s) => (
												<Buttons>
													<Action onClick={() => openService(s)}>Update</Action>
													<Action
														onClick={() => {
															setName(s.ServiceName);
															setForce(false);
															setDialog("Delete service");
														}}
													>
														Delete
													</Action>
												</Buttons>
											),
										},
									]}
								/>
							</SpaceBetween>
						),
					},
					{
						id: "tasks",
						label: "Tasks",
						content: (
							<SpaceBetween size="m">
								<Choice
									label="Task desired status"
									value={taskStatus}
									options={["RUNNING", "STOPPED", "PENDING"]}
									onChange={setTaskStatus}
								/>
								<Tasks
									key={`${taskStatus}/${dialog}`}
									scope={scope}
									cluster={cluster}
									status={taskStatus}
								/>
								<Action
									disabled={!definitions.data?.length}
									onClick={() => {
										setDefinition(definitions.data?.[0] ?? "");
										setCount("1");
										setDialog("Run task");
									}}
								>
									Run task
								</Action>
							</SpaceBetween>
						),
					},
				]}
			/>
			{dialog && (
				<ActionDialog
					title={dialog}
					submit={dialog}
					confirmation={dialog === "Delete service" ? name : undefined}
					onClose={() => setDialog("")}
					onSaved={changed}
					plan={async () => {
						if (dialog === "Delete service")
							return [
								{
									label: dialog,
									run: () => call("DeleteService", { service: name, force }),
								},
							];
						if (!definition)
							throw new Error("Select a task definition revision.");
						const n = desiredCount(count, dialog === "Run task" ? 1 : 0);
						if (dialog === "Run task" && n > 10)
							throw new Error("Run between 1 and 10 tasks.");
						if (dialog === "Create service" && !name.trim())
							throw new Error("Enter a service name.");
						return [
							{
								label: dialog,
								run: async () => {
									const out = await call(
										dialog === "Run task"
											? "RunTask"
											: dialog === "Create service"
												? "CreateService"
												: "UpdateService",
										{
											taskDefinition: definition,
											...(dialog === "Run task"
												? { count: n }
												: {
														desiredCount: n,
														...(dialog === "Create service"
															? { serviceName: name }
															: { service: name }),
													}),
										},
									);
									if ((out.Failures as unknown[])?.length)
										throw new Error(JSON.stringify(out.Failures));
								},
							},
						];
					}}
				>
					<SpaceBetween size="m">
						{dialog === "Delete service" ? (
							<Checkbox
								checked={force}
								onChange={({ detail }) => setForce(detail.checked)}
							>
								Force service deletion
							</Checkbox>
						) : (
							<>
								{dialog === "Create service" && (
									<Field label="Service name" value={name} onChange={setName} />
								)}
								<Choice
									label="Task definition revision"
									value={definition}
									options={Array.from(
										new Set([
											...(definitions.data ?? []),
											...(definition ? [definition] : []),
										]),
									)}
									onChange={setDefinition}
								/>
								<Field
									label={
										dialog === "Run task"
											? "Task count (1–10)"
											: "Desired count"
									}
									value={count}
									onChange={setCount}
								/>
							</>
						)}
					</SpaceBetween>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
function Tasks({
	scope,
	cluster,
	status,
}: {
	scope: ContainerScope;
	cluster: string;
	status: string;
}) {
	const call = (op: string, input: Record<string, unknown> = {}) =>
		ecsCall(scope, op, { cluster, ...input });
	const poll = usePoll(async () =>
		ecsDescribe<ECSTask>(
			scope,
			"DescribeTasks",
			"Tasks",
			"tasks",
			await containerPages<string>(call, "ListTasks", "TaskArns", {
				desiredStatus: status,
			}),
			{ cluster },
		),
	);
	const [task, setTask] = useState(""),
		[reason, setReason] = useState("Stopped from console");
	return (
		<SpaceBetween size="m">
			{poll.error && <Alert type="error">{poll.error}</Alert>}
			<Rows
				title="Tasks"
				emptyText="Choose Run task to create a task, or select another desired status."
				items={poll.data ?? []}
				actions={<Action onClick={poll.reload}>Refresh</Action>}
				columns={[
					{ id: "arn", header: "Task", cell: (t) => t.TaskArn },
					{
						id: "definition",
						header: "Definition",
						cell: (t) => t.TaskDefinitionArn,
					},
					{ id: "status", header: "Last status", cell: (t) => t.LastStatus },
					{
						id: "desired",
						header: "Desired status",
						cell: (t) => t.DesiredStatus,
					},
					{
						id: "reason",
						header: "Stopped reason",
						cell: (t) => t.StoppedReason ?? "—",
					},
					{
						id: "actions",
						header: "Actions",
						cell: (t) => (
							<Action
								disabled={t.LastStatus === "STOPPED"}
								onClick={() => {
									setTask(t.TaskArn);
									setReason("Stopped from console");
								}}
							>
								Stop task
							</Action>
						),
					},
				]}
			/>
			{task && (
				<ActionDialog
					title="Stop task"
					submit="Stop task"
					confirmation={arnTail(task)}
					onClose={() => setTask("")}
					onSaved={() => {
						setTask("");
						poll.reload();
					}}
					plan={async () => [
						{
							label: "Stop task",
							run: () => call("StopTask", { task, reason }),
						},
					]}
				>
					<Field label="Reason" value={reason} onChange={setReason} />
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
