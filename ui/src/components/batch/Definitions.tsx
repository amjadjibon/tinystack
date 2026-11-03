import {
	Alert,
	Container,
	Header,
	Link,
	Select,
	SpaceBetween,
} from "@cloudscape-design/components";
import { useNavigate } from "@tanstack/react-router";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { CodeBlock } from "#/components/CodeBlock";
import { Action, Buttons, Rows } from "#/components/containers/Controls";
import { Copy } from "#/components/efs/Shared";
import { Details } from "#/components/elbv2/Shared";
import { batchCall, definitions } from "#/lib/batch";
import {
	allStatuses,
	batchHref,
	countText,
	definitionKey,
	definitionStatuses,
	type JobDefinition,
	resources,
} from "#/lib/batchHelpers";
import { useFollow } from "#/lib/useFollow";
import { usePoll } from "#/lib/usePoll";
import { DefinitionDialog } from "./DefinitionDialog";
import { DetailTabs, ResourceTags, type SectionProps, Status } from "./Shared";
import { SubmitDialog } from "./SubmitDialog";

type Dialog = "" | "register" | "submit" | "deregister";

export function Definitions({
	scope,
	enabled,
	selected,
	status,
	tab,
	onChanged,
}: SectionProps & { status: string }) {
	const navigate = useNavigate(),
		follow = useFollow();
	const [dialog, setDialog] = useState<Dialog>("");
	// A definition's page finds a revision in any status, such as one a job ran.
	const poll = usePoll(
		() =>
			enabled
				? definitions(scope, selected ? allStatuses : status)
				: Promise.resolve([]),
		0,
	);
	const d = poll.data?.find((d) => definitionKey(d) === selected);
	const saved = () => {
		setDialog("");
		poll.reload();
		onChanged();
	};
	const list = batchHref(scope, "definitions", { status });
	return (
		<SpaceBetween size="m">
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			{!selected ? (
				<Rows
					title="Job definitions"
					items={poll.data ?? []}
					loading={poll.loading}
					emptyText="Choose Register job definition to add one."
					actions={
						<Buttons>
							<Select
								ariaLabel="Status"
								selectedOption={{ label: status, value: status }}
								options={definitionStatuses.map((value) => ({
									label: value,
									value,
								}))}
								onChange={({ detail }) =>
									navigate({
										href: batchHref(scope, "definitions", {
											status: detail.selectedOption.value,
										}),
									})
								}
							/>
							<Action onClick={poll.reload}>Refresh</Action>
							<Action disabled={!enabled} onClick={() => setDialog("register")}>
								Register job definition
							</Action>
						</Buttons>
					}
					columns={[
						{
							id: "name",
							header: "Name",
							cell: (d) => (
								<Link
									href={batchHref(scope, "definitions", {
										definition: definitionKey(d),
										status,
									})}
									onFollow={follow}
								>
									{d.JobDefinitionName}
								</Link>
							),
						},
						{
							id: "revision",
							header: "Revision",
							cell: (d) => countText(d.Revision),
						},
						{
							id: "status",
							header: "Status",
							cell: (d) => <Status value={d.Status} />,
						},
						{
							id: "image",
							header: "Image",
							cell: (d) => d.ContainerProperties?.Image ?? "-",
						},
						{
							id: "vcpus",
							header: "vCPUs",
							cell: (d) => countText(resources(d.ContainerProperties).vcpus),
						},
						{
							id: "memory",
							header: "Memory (MiB)",
							cell: (d) => countText(resources(d.ContainerProperties).memory),
						},
					]}
				/>
			) : d ? (
				<>
					<Buttons>
						<Action onClick={() => navigate({ href: list })}>
							Job definitions
						</Action>
						<Action onClick={poll.reload}>Refresh</Action>
						<Action disabled={!enabled} onClick={() => setDialog("register")}>
							Register new revision
						</Action>
						<Action
							disabled={!enabled || d.Status !== "ACTIVE"}
							onClick={() => setDialog("submit")}
						>
							Submit job
						</Action>
						<Action
							disabled={!enabled || d.Status !== "ACTIVE"}
							onClick={() => setDialog("deregister")}
						>
							Deregister
						</Action>
					</Buttons>
					<DetailTabs
						tab={tab}
						href={(next) =>
							batchHref(scope, "definitions", {
								definition: selected,
								status,
								tab: next,
							})
						}
						overview={<Overview definition={d} />}
						tags={
							<ResourceTags
								scope={scope}
								arn={d.JobDefinitionArn}
								tags={d.Tags}
								enabled={enabled}
								onChanged={saved}
							/>
						}
					/>
				</>
			) : (
				!poll.loading && (
					<Alert>Job definition not found in this account and Region.</Alert>
				)
			)}
			{dialog === "register" && (
				<DefinitionDialog
					scope={scope}
					from={d}
					onClose={() => setDialog("")}
					onSaved={(revision) => {
						saved();
						navigate({
							href: batchHref(scope, "definitions", {
								definition: revision,
								status,
							}),
						});
					}}
				/>
			)}
			{dialog === "submit" && !!d && (
				<SubmitDialog
					scope={scope}
					definition={definitionKey(d)}
					onClose={() => setDialog("")}
					onSaved={(job, queue) => {
						saved();
						navigate({ href: batchHref(scope, "jobs", { queue, job }) });
					}}
				/>
			)}
			{dialog === "deregister" && !!d && (
				<ActionDialog
					title="Deregister job definition"
					submit="Deregister job definition"
					confirmation={definitionKey(d)}
					onClose={() => setDialog("")}
					onSaved={saved}
					plan={async () => [
						{
							label: "DeregisterJobDefinition",
							run: () =>
								batchCall(scope, "DeregisterJobDefinition", {
									JobDefinition: d.JobDefinitionArn,
								}),
						},
					]}
				>
					<Alert type="warning">
						Deregister {definitionKey(d)}. It becomes INACTIVE, stays listed and
						cannot be submitted again. Registering the name adds a new revision.
					</Alert>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}

function Overview({ definition: d }: { definition: JobDefinition }) {
	const c = d.ContainerProperties;
	const need = resources(c);
	return (
		<SpaceBetween size="l">
			<Details
				items={[
					["Name", d.JobDefinitionName],
					["Revision", countText(d.Revision)],
					["ARN", <Copy key="arn" text={d.JobDefinitionArn} label="ARN" />],
					["Status", <Status key="status" value={d.Status} />],
					["Type", d.Type],
					["Platform", d.PlatformCapabilities?.join(", ")],
					["Image", c?.Image],
					["vCPUs", countText(need.vcpus)],
					["Memory (MiB)", countText(need.memory)],
					["Job role", c?.JobRoleArn],
					["Execution role", c?.ExecutionRoleArn],
					["Retry attempts", countText(d.RetryStrategy?.Attempts)],
					["Timeout (seconds)", countText(d.Timeout?.AttemptDurationSeconds)],
				]}
			/>
			<Container header={<Header variant="h3">Container properties</Header>}>
				<CodeBlock text={JSON.stringify(c ?? {}, null, 2)} />
			</Container>
			{!!Object.keys(d.Parameters ?? {}).length && (
				<Container header={<Header variant="h3">Parameters</Header>}>
					<CodeBlock text={JSON.stringify(d.Parameters, null, 2)} />
				</Container>
			)}
			{!!d.RetryStrategy?.EvaluateOnExit?.length && (
				<Container header={<Header variant="h3">Retry rules</Header>}>
					<CodeBlock
						text={JSON.stringify(d.RetryStrategy.EvaluateOnExit, null, 2)}
					/>
				</Container>
			)}
		</SpaceBetween>
	);
}
