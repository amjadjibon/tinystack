import {
	Alert,
	Button,
	Container,
	ContentLayout,
	Header,
	KeyValuePairs,
	SpaceBetween,
	Tabs,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { CodeBlock } from "#/components/CodeBlock";
import { ConsoleTags } from "#/components/ConsoleTags";
import type { Resource } from "#/lib/api";
import {
	checkEntries,
	type EventRule,
	type EventScope,
	type EventTarget,
	eventCall,
	eventRuleArn,
	inventoryRule,
	inventoryTargets,
} from "#/lib/eventbridge";
import { usePoll } from "#/lib/usePoll";
import { EventDeliveries } from "./EventDeliveries";
import { EventRuleForm } from "./EventRuleForm";
import { EventTargets } from "./EventTargets";
export function EventRuleDetails({
	scope,
	initialTab,
	resource,
	resources,
	enabled,
	onChanged,
	onDeleted,
}: {
	scope: EventScope;
	/** initialTab opens a tab named in the URL (empty for the default). */
	initialTab: string;
	resource?: Resource;
	resources: Resource[];
	enabled: boolean;
	onChanged: (message: string) => void;
	onDeleted: () => void;
}) {
	const [tab, setTab] = useState(initialTab || "pattern");
	const rule = usePoll(async () =>
		enabled
			? await eventCall<EventRule>(scope, "DescribeRule", {
					Name: scope.rule,
					EventBusName: scope.bus,
				})
			: inventoryRule(resource),
	);
	const targets = usePoll(async () =>
		enabled
			? ((
					await eventCall<{ Targets?: EventTarget[] }>(
						scope,
						"ListTargetsByRule",
						{ Rule: scope.rule, EventBusName: scope.bus },
					)
				).Targets ?? [])
			: inventoryTargets(resource),
	);
	const tags = usePoll(async () =>
		enabled
			? Object.fromEntries(
					(
						(
							await eventCall<{ Tags?: { Key: string; Value: string }[] }>(
								scope,
								"ListTagsForResource",
								{ ResourceARN: eventRuleArn(scope) },
							)
						).Tags ?? []
					).map((t) => [t.Key, t.Value]),
				)
			: (resource?.tags ?? {}),
	);
	const data = rule.data ?? inventoryRule(resource);
	const [dialog, setDialog] = useState<"edit" | "delete" | "toggle" | null>(
			null,
		),
		[editing, setEditing] = useState<EventRule>();
	const refresh = (message: string) => {
		rule.reload();
		targets.reload();
		tags.reload();
		onChanged(message);
	};
	return (
		<ContentLayout
			header={
				<Header
					variant="h1"
					actions={
						<SpaceBetween direction="horizontal" size="xs">
							<Button
								iconName="refresh"
								ariaLabel="Refresh rule"
								onClick={() => refresh("")}
							/>
							<Button
								disabled={!enabled || !data.Name}
								onClick={() => {
									setEditing({ ...data });
									setDialog("edit");
								}}
							>
								Edit
							</Button>
							<Button
								disabled={!enabled || !data.Name}
								onClick={() => setDialog("toggle")}
							>
								{data.State === "ENABLED" ? "Disable" : "Enable"}
							</Button>
							<Button
								disabled={!enabled || !data.Name}
								onClick={() => setDialog("delete")}
							>
								Delete
							</Button>
						</SpaceBetween>
					}
				>
					{scope.rule}
				</Header>
			}
		>
			<SpaceBetween size="l">
				{(rule.error || targets.error || tags.error) && (
					<Alert type="error">
						{rule.error || targets.error || tags.error}
					</Alert>
				)}
				<Container header={<Header variant="h2">Rule details</Header>}>
					<KeyValuePairs
						columns={3}
						items={[
							{ label: "Event bus", value: scope.bus },
							{ label: "State", value: data.State || "—" },
							{ label: "Schedule", value: data.ScheduleExpression || "—" },
							{ label: "Description", value: data.Description || "—" },
							{ label: "Execution role", value: data.RoleArn || "—" },
							{ label: "ARN", value: data.Arn || eventRuleArn(scope) },
						]}
					/>
				</Container>
				<Tabs
					activeTabId={tab}
					onChange={({ detail }) => setTab(detail.activeTabId)}
					tabs={[
						{
							id: "pattern",
							label: "Event pattern",
							content: (
								<CodeBlock
									text={
										data.EventPattern ||
										"This rule does not match published events."
									}
								/>
							),
						},
						{
							id: "targets",
							label: "Targets",
							content: (
								<EventTargets
									scope={scope}
									targets={targets.data ?? inventoryTargets(resource)}
									resources={resources}
									enabled={enabled && !!data.Name}
									onChanged={refresh}
								/>
							),
						},
						{
							id: "tags",
							label: "Tags",
							content: (
								<ConsoleTags
									tags={tags.data ?? resource?.tags ?? {}}
									enabled={enabled && !!tags.data}
									onChanged={() => refresh("Rule tags updated.")}
									plan={(next, removed) => [
										...(removed.length
											? [
													{
														label: "Remove tags",
														run: () =>
															eventCall(scope, "UntagResource", {
																ResourceARN: eventRuleArn(scope),
																TagKeys: removed,
															}),
													},
												]
											: []),
										...(Object.keys(next).length
											? [
													{
														label: "Save tags",
														run: () =>
															eventCall(scope, "TagResource", {
																ResourceARN: eventRuleArn(scope),
																Tags: Object.entries(next).map(
																	([Key, Value]) => ({ Key, Value }),
																),
															}),
													},
												]
											: []),
									]}
								/>
							),
						},
						{
							id: "deliveries",
							label: "Deliveries",
							content: <EventDeliveries scope={scope} />,
						},
					]}
				/>
				{dialog === "edit" && (
					<EventRuleForm
						scope={scope}
						rule={editing}
						onClose={() => setDialog(null)}
						onSaved={() => {
							setDialog(null);
							refresh("Rule updated.");
						}}
					/>
				)}
				{(dialog === "delete" || dialog === "toggle") && (
					<ActionDialog
						title={
							dialog === "delete"
								? "Delete rule"
								: data.State === "ENABLED"
									? "Disable rule"
									: "Enable rule"
						}
						submit={dialog === "delete" ? "Delete rule" : "Save changes"}
						confirmation={dialog === "delete" ? scope.rule : undefined}
						onClose={() => setDialog(null)}
						onSaved={() => {
							setDialog(null);
							if (dialog === "delete") onDeleted();
							else refresh("Rule state updated.");
						}}
						plan={async () => {
							if (dialog === "toggle")
								return [
									{
										label: "Update state",
										run: () =>
											eventCall(
												scope,
												data.State === "ENABLED" ? "DisableRule" : "EnableRule",
												{ Name: scope.rule, EventBusName: scope.bus },
											),
									},
								];
							const current =
								(
									await eventCall<{ Targets?: EventTarget[] }>(
										scope,
										"ListTargetsByRule",
										{ Rule: scope.rule, EventBusName: scope.bus },
									)
								).Targets ?? [];
							return [
								...(current.length
									? [
											{
												label: "Remove targets",
												run: async () =>
													checkEntries(
														await eventCall(scope, "RemoveTargets", {
															Rule: scope.rule,
															EventBusName: scope.bus,
															Ids: current.map((t) => t.Id),
														}),
													),
											},
										]
									: []),
								{
									label: "Delete rule",
									run: () =>
										eventCall(scope, "DeleteRule", {
											Name: scope.rule,
											EventBusName: scope.bus,
										}),
								},
							];
						}}
					>
						<Alert type={dialog === "delete" ? "warning" : "info"}>
							{dialog === "delete"
								? "Delete this rule and remove all its targets. Destination resources remain."
								: data.State === "ENABLED"
									? "Stop matching new events and running this schedule. Queued delivery retries can continue."
									: "Enable event matching and scheduled execution for this rule."}
						</Alert>
					</ActionDialog>
				)}
			</SpaceBetween>
		</ContentLayout>
	);
}
