import {
	Alert,
	Button,
	Container,
	Header,
	KeyValuePairs,
	Link,
	SpaceBetween,
	Tabs,
} from "@cloudscape-design/components";
import { useState } from "react";
import type { Resource } from "#/lib/api";
import {
	type CfnScope,
	cfnCall,
	cfnHref,
	cfnPages,
} from "#/lib/cloudformation";
import {
	cfnDate,
	inProgress,
	type Stack,
	type StackResource,
} from "#/lib/cloudformationHelpers";
import { useFollow } from "#/lib/useFollow";
import { usePoll } from "#/lib/usePoll";
import { DeleteStack } from "./DeleteStack";
import { StackChangeSets } from "./StackChangeSets";
import { StackEditor } from "./StackEditor";
import { StackEvents } from "./StackEvents";
import { StackProtection } from "./StackProtection";
import { NestedStacks, StackResources } from "./StackResources";
import { StackStatus } from "./StackStatus";
import { StackTemplate } from "./StackTemplate";
import { StackOutputs, StackParameters } from "./StackValues";

// StackDetails follows one stack: the summary and resources reload every few
// seconds, faster while an operation runs.
export function StackDetails({
	scope,
	inventory,
	initialChangeSet,
	onChanged,
}: {
	scope: CfnScope;
	inventory: Resource[];
	initialChangeSet?: string;
	onChanged: () => void;
}) {
	const [tab, setTab] = useState(initialChangeSet ? "changesets" : "events"),
		[focus, setFocus] = useState(initialChangeSet);
	const follow = useFollow();
	const stackPoll = usePoll(
		async () =>
			(
				await cfnCall<{ Stacks?: Stack[] }>(scope, "DescribeStacks", {
					StackName: scope.stack,
				})
			).Stacks?.[0],
	);
	const stack = stackPoll.data;
	const status = stack?.StackStatus ?? "";
	const running = inProgress(status);
	const resources = usePoll(
		() =>
			cfnPages<StackResource>(
				scope,
				"ListStackResources",
				"StackResourceSummaries",
				{
					StackName: scope.stack,
				},
			),
		running ? 3000 : 0,
	);
	const refresh = () => {
		stackPoll.reload();
		resources.reload();
		onChanged();
	};
	if (!stack)
		return stackPoll.error ? (
			<Alert type="error" header="Cannot load the stack">
				{stackPoll.error}
			</Alert>
		) : (
			<Header variant="h1">Loading stack</Header>
		);
	const deleted = stack.StackStatus === "DELETE_COMPLETE";
	return (
		<SpaceBetween size="l">
			{stackPoll.error && (
				<Alert type="error" header="Cannot refresh the stack">
					{stackPoll.error}
				</Alert>
			)}
			<Header
				variant="h1"
				actions={
					<SpaceBetween direction="horizontal" size="xs">
						<Button
							iconName="refresh"
							ariaLabel="Refresh stack"
							onClick={refresh}
						/>
						<StackEditor
							scope={scope}
							stack={stack}
							disabled={deleted || running}
							onSaved={(_, changeSet) => {
								refresh();
								if (changeSet) {
									setFocus(changeSet);
									setTab("changesets");
								}
							}}
						/>
						<StackProtection scope={scope} stack={stack} onChanged={refresh} />
						<DeleteStack
							scope={scope}
							stack={stack}
							resources={resources.data ?? []}
							onDeleted={refresh}
						/>
					</SpaceBetween>
				}
			>
				{stack.StackName}
			</Header>
			<Container header={<Header variant="h2">Stack info</Header>}>
				<KeyValuePairs
					columns={3}
					items={[
						{ label: "Status", value: <StackStatus status={status} /> },
						{ label: "Status reason", value: stack.StackStatusReason ?? "—" },
						{ label: "Stack ID", value: stack.StackId },
						{ label: "Created", value: cfnDate(stack.CreationTime) },
						{ label: "Last updated", value: cfnDate(stack.LastUpdatedTime) },
						{ label: "Deleted", value: cfnDate(stack.DeletionTime) },
						{
							label: "Drift status",
							value: stack.DriftInformation?.StackDriftStatus ?? "—",
						},
						{
							label: "Termination protection",
							value: stack.EnableTerminationProtection ? "Enabled" : "Disabled",
						},
						{
							label: "Rollback on failure",
							value: stack.DisableRollback ? "Disabled" : "Enabled",
						},
						{ label: "Description", value: stack.Description ?? "—" },
						{
							label: "Capabilities",
							value: stack.Capabilities?.join(", ") || "—",
						},
						{ label: "Role ARN", value: stack.RoleARN ?? "—" },
						{
							label: "Parent stack",
							value: stack.ParentId ? (
								<Link
									href={cfnHref({ ...scope, stack: stack.ParentId })}
									onFollow={follow}
								>
									{stack.ParentId}
								</Link>
							) : (
								"—"
							),
						},
						{
							label: "Root stack",
							value:
								stack.RootId && stack.RootId !== stack.ParentId ? (
									<Link
										href={cfnHref({ ...scope, stack: stack.RootId })}
										onFollow={follow}
									>
										{stack.RootId}
									</Link>
								) : (
									"—"
								),
						},
						{
							label: "Tags",
							value:
								stack.Tags?.map((t) => `${t.Key}=${t.Value}`).join(", ") || "—",
						},
					]}
				/>
			</Container>
			<Tabs
				activeTabId={tab}
				onChange={({ detail }) => setTab(detail.activeTabId)}
				tabs={[
					{
						id: "events",
						label: "Events",
						content: <StackEvents scope={scope} status={status} />,
					},
					{
						id: "resources",
						label: "Resources",
						content: (
							<StackResources
								scope={scope}
								poll={resources}
								inventory={inventory}
							/>
						),
					},
					{
						id: "outputs",
						label: "Outputs",
						content: <StackOutputs outputs={stack.Outputs ?? []} />,
					},
					{
						id: "parameters",
						label: "Parameters",
						content: <StackParameters parameters={stack.Parameters ?? []} />,
					},
					{
						id: "template",
						label: "Template",
						content: <StackTemplate scope={scope} />,
					},
					{
						id: "changesets",
						label: "Change sets",
						content: (
							<StackChangeSets
								scope={scope}
								deleted={deleted}
								focus={focus}
								onChanged={refresh}
							/>
						),
					},
					{
						id: "nested",
						label: "Nested stacks",
						content: <NestedStacks scope={scope} poll={resources} />,
					},
				]}
			/>
		</SpaceBetween>
	);
}
