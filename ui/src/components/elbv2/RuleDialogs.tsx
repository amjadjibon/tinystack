import { Alert, SpaceBetween } from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { Text } from "#/components/memorydb/Fields";
import {
	type ELBv2Scope,
	elbv2Call,
	type Listener,
	type Rule,
	type TargetGroup,
} from "#/lib/elbv2";
import {
	type ActionForm,
	actionFormFrom,
	type ConditionForm,
	conditionForms,
	emptyActionForm,
	nextPriority,
	ruleCreateInput,
	ruleModifyInput,
	rulePrioritiesInput,
} from "#/lib/elbv2RuleHelpers";
import { ActionFields, ConditionFields } from "./ActionFields";

export function RuleDialog({
	scope,
	listener,
	rule,
	rules,
	groups,
	onClose,
	onSaved,
}: {
	scope: ELBv2Scope;
	listener: Listener;
	rule?: Rule;
	rules: Rule[];
	groups: TargetGroup[];
	onClose: () => void;
	onSaved: () => void;
}) {
	const [priority, setPriority] = useState(String(nextPriority(rules)));
	const [conditions, setConditions] = useState<ConditionForm[]>(
		conditionForms(rule?.Conditions),
	);
	const [action, setAction] = useState<ActionForm>(
		rule ? actionFormFrom(rule.Actions) : emptyActionForm,
	);
	return (
		<ActionDialog
			title={rule ? `Edit rule at priority ${rule.Priority}` : "Create rule"}
			submit={rule ? "Save rule" : "Create rule"}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				if (rule) {
					const input = ruleModifyInput(
						rule.RuleArn,
						conditions,
						action,
						rule.Actions ?? [],
					);
					return [
						{
							label: "ModifyRule",
							run: () => elbv2Call(scope, "ModifyRule", input),
						},
					];
				}
				const input = ruleCreateInput(
					listener.ListenerArn,
					priority,
					conditions,
					action,
				);
				return [
					{
						label: "CreateRule",
						run: () => elbv2Call(scope, "CreateRule", input),
					},
				];
			}}
		>
			<SpaceBetween size="l">
				{!rule && (
					<Text
						label="Priority"
						value={priority}
						constraint="1 to 50000; a lower number is evaluated first."
						onChange={setPriority}
					/>
				)}
				<ConditionFields forms={conditions} onChange={setConditions} />
				<ActionFields
					form={action}
					groups={groups}
					onChange={(patch) => setAction((a) => ({ ...a, ...patch }))}
				/>
			</SpaceBetween>
		</ActionDialog>
	);
}

export function DeleteRuleDialog({
	scope,
	rule,
	onClose,
	onSaved,
}: {
	scope: ELBv2Scope;
	rule: Rule;
	onClose: () => void;
	onSaved: () => void;
}) {
	return (
		<ActionDialog
			title="Delete rule"
			submit="Delete rule"
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => [
				{
					label: "DeleteRule",
					run: () => elbv2Call(scope, "DeleteRule", { RuleArn: rule.RuleArn }),
				},
			]}
		>
			<Alert type="warning">Delete the rule at priority {rule.Priority}.</Alert>
		</ActionDialog>
	);
}

export function PrioritiesDialog({
	scope,
	rules,
	onClose,
	onSaved,
}: {
	scope: ELBv2Scope;
	rules: Rule[];
	onClose: () => void;
	onSaved: () => void;
}) {
	const [edited, setEdited] = useState<Record<string, string>>({});
	return (
		<ActionDialog
			title="Reorder rules"
			submit="Save priorities"
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const input = rulePrioritiesInput(rules, edited);
				return [
					{
						label: "SetRulePriorities",
						run: () => elbv2Call(scope, "SetRulePriorities", input),
					},
				];
			}}
		>
			<SpaceBetween size="s">
				<Alert type="info">
					Each priority must be unique. The default rule always runs last and
					has no priority.
				</Alert>
				{rules
					.filter((r) => !r.IsDefault)
					.map((r) => (
						<Text
							key={r.RuleArn}
							label={`Priority of ${r.RuleArn.split("/").at(-1)}`}
							value={edited[r.RuleArn] ?? r.Priority ?? ""}
							onChange={(value) =>
								setEdited((e) => ({ ...e, [r.RuleArn]: value }))
							}
						/>
					))}
			</SpaceBetween>
		</ActionDialog>
	);
}
