import {
	Alert,
	Box,
	Button,
	Container,
	Header,
	KeyValuePairs,
	Link,
	SpaceBetween,
	Table,
} from "@cloudscape-design/components";
import { useEffect, useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import {
	type CfnScope,
	cfnCall,
	cfnPages,
	describeChangeSet,
} from "#/lib/cloudformation";
import { type ChangeSetSummary, cfnDate } from "#/lib/cloudformationHelpers";
import { usePoll } from "#/lib/usePoll";
import { StackStatus } from "./StackStatus";
import { StackParameters } from "./StackValues";

// StackChangeSets lists, describes, executes and deletes a stack's change sets.
// The stack editor creates change sets for review here.
export function StackChangeSets({
	scope,
	deleted,
	focus,
	onChanged,
}: {
	scope: CfnScope;
	deleted: boolean;
	focus?: string;
	onChanged: () => void;
}) {
	const [selected, setSelected] = useState<ChangeSetSummary[]>([]),
		[viewing, setViewing] = useState(focus ?? ""),
		[focused, setFocused] = useState(""),
		[action, setAction] = useState<"Execute" | "Delete">();
	const poll = usePoll(
		() =>
			cfnPages<ChangeSetSummary>(scope, "ListChangeSets", "Summaries", {
				StackName: scope.stack,
			}),
		0,
	);
	const sets = poll.data ?? [];
	useEffect(() => {
		if (!focus || focused === focus) return;
		const found = sets.find((c) => c.ChangeSetName === focus);
		if (!found) return;
		setFocused(focus);
		setViewing(focus);
		setSelected([found]);
	}, [focus, focused, sets]);
	const current = sets.find((c) => c.ChangeSetId === selected[0]?.ChangeSetId);
	const executable =
		!deleted &&
		current?.ExecutionStatus === "AVAILABLE" &&
		current.Status === "CREATE_COMPLETE";
	const input = {
		StackName: scope.stack,
		ChangeSetName: current?.ChangeSetName,
	};
	return (
		<SpaceBetween size="m">
			{!!poll.error && (
				<Alert type="error" header="Cannot load change sets">
					{poll.error}
				</Alert>
			)}
			<Table
				items={sets}
				trackBy="ChangeSetId"
				selectionType="single"
				selectedItems={current ? [current] : []}
				onSelectionChange={({ detail }) => setSelected(detail.selectedItems)}
				ariaLabels={{
					selectionGroupLabel: "Select change set",
					itemSelectionLabel: (_, c) => `Select change set ${c.ChangeSetName}`,
				}}
				loading={poll.loading}
				loadingText="Loading change sets"
				variant="embedded"
				header={
					<Header
						counter={`(${sets.length})`}
						actions={
							<SpaceBetween direction="horizontal" size="xs">
								<Button
									iconName="refresh"
									ariaLabel="Refresh change sets"
									onClick={poll.reload}
								/>
								<Button
									disabled={!current}
									onClick={() => current && setViewing(current.ChangeSetName)}
								>
									View
								</Button>
								<Button
									disabled={!executable}
									onClick={() => setAction("Execute")}
								>
									Execute
								</Button>
								<Button disabled={!current} onClick={() => setAction("Delete")}>
									Delete
								</Button>
							</SpaceBetween>
						}
					>
						Change sets
					</Header>
				}
				empty={
					<Box textAlign="center" padding="l">
						This stack has no change sets.
					</Box>
				}
				columnDefinitions={[
					{
						id: "name",
						header: "Name",
						isRowHeader: true,
						cell: (c) => (
							<Link onFollow={() => setViewing(c.ChangeSetName)}>
								{c.ChangeSetName}
							</Link>
						),
					},
					{
						id: "status",
						header: "Status",
						cell: (c) => <StackStatus status={c.Status} />,
					},
					{
						id: "execution",
						header: "Execution status",
						cell: (c) => c.ExecutionStatus ?? "—",
					},
					{
						id: "reason",
						header: "Status reason",
						cell: (c) => c.StatusReason ?? "—",
					},
					{
						id: "created",
						header: "Created",
						cell: (c) => cfnDate(c.CreationTime),
					},
					{
						id: "description",
						header: "Description",
						cell: (c) => c.Description ?? "—",
					},
				]}
			/>
			{!!viewing && (
				<ChangeSetView
					key={viewing}
					scope={scope}
					name={viewing}
					onClose={() => setViewing("")}
				/>
			)}
			{action && current && (
				<ActionDialog
					title={`${action} change set ${current.ChangeSetName}`}
					submit={`${action} change set`}
					confirmation={action === "Delete" ? current.ChangeSetName : undefined}
					onClose={() => setAction(undefined)}
					onSaved={() => {
						if (viewing === current.ChangeSetName && action === "Delete")
							setViewing("");
						setAction(undefined);
						setSelected([]);
						poll.reload();
						onChanged();
					}}
					plan={async () => [
						{
							label: `${action} change set`,
							run: () => cfnCall(scope, `${action}ChangeSet`, input),
						},
					]}
				>
					<Alert type={action === "Delete" ? "warning" : "info"}>
						{action === "Delete"
							? `Delete change set ${current.ChangeSetName}? The stack is not changed.`
							: `Execute change set ${current.ChangeSetName}? The stack is updated with its changes.`}
					</Alert>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}

function ChangeSetView({
	scope,
	name,
	onClose,
}: {
	scope: CfnScope;
	name: string;
	onClose: () => void;
}) {
	const poll = usePoll(() => describeChangeSet(scope, name), 0);
	const detail = poll.data;
	const changes = (detail?.Changes ?? []).flatMap((c) =>
		c.ResourceChange ? [c.ResourceChange] : [],
	);
	return (
		<Container
			header={
				<Header variant="h2" actions={<Button onClick={onClose}>Close</Button>}>
					{name}
				</Header>
			}
		>
			<SpaceBetween size="m">
				{!!poll.error && (
					<Alert type="error" header="Cannot describe the change set">
						{poll.error}
					</Alert>
				)}
				{detail && (
					<KeyValuePairs
						columns={3}
						items={[
							{
								label: "Status",
								value: <StackStatus status={detail.Status} />,
							},
							{
								label: "Execution status",
								value: detail.ExecutionStatus ?? "—",
							},
							{ label: "Created", value: cfnDate(detail.CreationTime) },
							{ label: "Status reason", value: detail.StatusReason ?? "—" },
							{ label: "Description", value: detail.Description ?? "—" },
						]}
					/>
				)}
				<Table
					items={changes}
					loading={poll.loading}
					loadingText="Loading changes"
					variant="embedded"
					header={<Header counter={`(${changes.length})`}>Changes</Header>}
					empty={
						<Box textAlign="center" padding="l">
							This change set has no resource changes.
						</Box>
					}
					columnDefinitions={[
						{ id: "action", header: "Action", cell: (c) => c.Action ?? "—" },
						{
							id: "logical",
							header: "Logical ID",
							isRowHeader: true,
							cell: (c) => c.LogicalResourceId ?? "—",
						},
						{
							id: "physical",
							header: "Physical ID",
							cell: (c) => c.PhysicalResourceId ?? "—",
						},
						{ id: "type", header: "Type", cell: (c) => c.ResourceType ?? "—" },
						{
							id: "replacement",
							header: "Replacement",
							cell: (c) => c.Replacement ?? "—",
						},
						{
							id: "scope",
							header: "Scope",
							cell: (c) => c.Scope?.join(", ") || "—",
						},
					]}
				/>
				{(detail?.Parameters?.length ?? 0) > 0 && (
					<StackParameters parameters={detail?.Parameters ?? []} />
				)}
			</SpaceBetween>
		</Container>
	);
}
