import { Alert, SpaceBetween } from "@cloudscape-design/components";
import { useState } from "react";
import { Action, Buttons, Rows } from "#/components/containers/Controls";
import {
	type ELBv2Scope,
	type Listener,
	listRules,
	type Rule,
	type TargetGroup,
} from "#/lib/elbv2";
import { describeAction, describeCondition } from "#/lib/elbv2RuleHelpers";
import { usePoll } from "#/lib/usePoll";
import { DeleteRuleDialog, PrioritiesDialog, RuleDialog } from "./RuleDialogs";

export function Rules({
	scope,
	listener,
	groups,
	enabled,
}: {
	scope: ELBv2Scope;
	listener: Listener;
	groups: TargetGroup[];
	enabled: boolean;
}) {
	const poll = usePoll(() => listRules(scope, listener.ListenerArn), 0);
	const [dialog, setDialog] = useState<
		"" | "create" | "edit" | "delete" | "order"
	>("");
	const [target, setTarget] = useState<Rule>();
	const rules = poll.data ?? [];
	const names = (arn: string) =>
		groups.find((g) => g.TargetGroupArn === arn)?.TargetGroupName ?? arn;
	const open = (kind: "create" | "edit" | "delete" | "order", rule?: Rule) => {
		setTarget(rule);
		setDialog(kind);
	};
	const saved = () => {
		setDialog("");
		poll.reload();
	};
	const editable = rules.filter((r) => !r.IsDefault);
	return (
		<SpaceBetween size="m">
			{poll.error && <Alert type="error">{poll.error}</Alert>}
			<Rows
				title="Rules"
				emptyText="Choose Create rule to route requests by path, host, header, query string or source IP."
				items={[...rules].sort(
					(a, b) =>
						(a.IsDefault ? Number.MAX_SAFE_INTEGER : Number(a.Priority)) -
						(b.IsDefault ? Number.MAX_SAFE_INTEGER : Number(b.Priority)),
				)}
				loading={poll.loading}
				actions={
					<Buttons>
						<Action onClick={poll.reload}>Refresh</Action>
						<Action
							disabled={!enabled || editable.length < 2}
							onClick={() => open("order")}
						>
							Reorder rules
						</Action>
						<Action disabled={!enabled} onClick={() => open("create")}>
							Create rule
						</Action>
					</Buttons>
				}
				columns={[
					{
						id: "priority",
						header: "Priority",
						cell: (r) => (r.IsDefault ? "Default" : r.Priority),
					},
					{
						id: "conditions",
						header: "Conditions",
						cell: (r) =>
							r.IsDefault
								? "All other requests"
								: (r.Conditions ?? []).map(describeCondition).join("; "),
					},
					{
						id: "actions",
						header: "Action",
						cell: (r) =>
							(r.Actions ?? []).map((a) => describeAction(a, names)).join(", "),
					},
					{
						id: "manage",
						header: "Actions",
						cell: (r) => (
							<Buttons>
								<Action
									disabled={!enabled || r.IsDefault}
									onClick={() => open("edit", r)}
								>
									Edit
								</Action>
								<Action
									disabled={!enabled || r.IsDefault}
									onClick={() => open("delete", r)}
								>
									Delete
								</Action>
							</Buttons>
						),
					},
				]}
			/>
			<Alert type="info">
				The default rule applies to every other request. Change it by editing
				the listener's default action.
			</Alert>
			{(dialog === "create" || (dialog === "edit" && target)) && (
				<RuleDialog
					key={target?.RuleArn ?? "create"}
					scope={scope}
					listener={listener}
					rule={dialog === "edit" ? target : undefined}
					rules={rules}
					groups={groups}
					onClose={() => setDialog("")}
					onSaved={saved}
				/>
			)}
			{dialog === "delete" && target && (
				<DeleteRuleDialog
					scope={scope}
					rule={target}
					onClose={() => setDialog("")}
					onSaved={saved}
				/>
			)}
			{dialog === "order" && (
				<PrioritiesDialog
					scope={scope}
					rules={rules}
					onClose={() => setDialog("")}
					onSaved={saved}
				/>
			)}
		</SpaceBetween>
	);
}
