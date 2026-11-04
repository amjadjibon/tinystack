import {
	Alert,
	Button,
	Checkbox,
	FormField,
	Input,
	SpaceBetween,
	Textarea,
} from "@cloudscape-design/components";
import { useEffect, useRef, useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { errorText, jsonObject } from "#/lib/consoleActions";
import { type EventRule, type EventScope, eventCall } from "#/lib/eventbridge";

export function EventRuleForm({
	scope,
	rule,
	onClose,
	onSaved,
}: {
	scope: EventScope;
	rule?: EventRule;
	onClose: () => void;
	onSaved: (name: string) => void;
}) {
	const [name, setName] = useState(rule?.Name ?? ""),
		[description, setDescription] = useState(rule?.Description ?? "");
	const [pattern, setPattern] = useState(
			rule ? (rule.EventPattern ?? "") : '{"source":["my.app"]}',
		),
		[schedule, setSchedule] = useState(rule?.ScheduleExpression ?? "");
	const [role, setRole] = useState(rule?.RoleArn ?? ""),
		[active, setActive] = useState(rule?.State !== "DISABLED");
	const [sample, setSample] = useState(
		'{"source":"my.app","detail-type":"Example","detail":{}}',
	);
	const [testing, setTesting] = useState(false),
		[result, setResult] = useState<boolean>(),
		[error, setError] = useState("");
	const testVersion = useRef(0);
	useEffect(
		() => () => {
			testVersion.current++;
		},
		[],
	);
	const test = async () => {
		if (testing) return;
		const version = ++testVersion.current;
		setTesting(true);
		setError("");
		setResult(undefined);
		try {
			jsonObject(pattern, "Event pattern");
			jsonObject(sample, "Sample event");
			const tested = await eventCall<{ Result: boolean }>(
				scope,
				"TestEventPattern",
				{
					EventPattern: pattern,
					Event: sample,
				},
			);
			if (version === testVersion.current) setResult(tested.Result);
		} catch (e) {
			if (version === testVersion.current) setError(errorText(e));
		} finally {
			setTesting(false);
		}
	};
	return (
		<ActionDialog
			title={rule ? "Edit rule" : "Create rule"}
			submit={rule ? "Save changes" : "Create rule"}
			onClose={onClose}
			onSaved={() => onSaved(name)}
			plan={async () => {
				if (!/^[A-Za-z0-9_.-]{1,64}$/.test(name))
					throw new Error(
						"Rule names use 1–64 letters, numbers, underscores, hyphens or periods.",
					);
				if (!pattern.trim() && !schedule.trim())
					throw new Error("Enter an event pattern or schedule.");
				if (pattern.trim()) jsonObject(pattern, "Event pattern");
				if (scope.bus !== "default" && schedule.trim())
					throw new Error("Schedules require the default event bus.");
				if (!rule) {
					try {
						await eventCall(scope, "DescribeRule", {
							Name: name,
							EventBusName: scope.bus,
						});
					} catch (e) {
						if (!errorText(e).startsWith("ResourceNotFoundException:")) throw e;
						return [
							{
								label: "Create rule",
								run: () => eventCall(scope, "PutRule", input()),
							},
						];
					}
					throw new Error(
						"A rule with this name already exists. Open it to edit it.",
					);
				}
				return [
					{
						label: "Save rule",
						run: () => eventCall(scope, "PutRule", input()),
					},
				];
			}}
		>
			<SpaceBetween size="l">
				<FormField
					label="Rule name"
					constraintText="1–64 letters, numbers, underscores, hyphens or periods."
				>
					<Input
						ariaLabel="Rule name"
						disabled={!!rule}
						value={name}
						onChange={({ detail }) => setName(detail.value)}
					/>
				</FormField>
				<FormField label="Description — optional">
					<Input
						ariaLabel="Rule description"
						value={description}
						onChange={({ detail }) => setDescription(detail.value)}
					/>
				</FormField>
				<Checkbox
					checked={active}
					onChange={({ detail }) => setActive(detail.checked)}
				>
					Enable rule
				</Checkbox>
				<FormField
					label="Event pattern (JSON) — optional"
					description="Match fields from an event envelope. Clear this for a schedule-only rule."
				>
					<Textarea
						ariaLabel="Event pattern"
						rows={7}
						value={pattern}
						onChange={({ detail }) => {
							setPattern(detail.value);
							testVersion.current++;
							setResult(undefined);
						}}
					/>
				</FormField>
				<FormField
					label="Schedule expression — optional"
					description="Default bus only. For example rate(5 minutes) or cron(0 12 * * ? *), evaluated in UTC."
				>
					<Input
						ariaLabel="Schedule expression"
						disabled={scope.bus !== "default"}
						value={schedule}
						onChange={({ detail }) => setSchedule(detail.value)}
					/>
				</FormField>
				<FormField label="Execution role ARN — optional">
					<Input
						ariaLabel="Rule execution role"
						value={role}
						onChange={({ detail }) => setRole(detail.value)}
					/>
				</FormField>
				<Alert type="info">
					Enabled schedules run automatically. Editing a schedule restarts its
					timing. A rule can match events and also run on a schedule.
				</Alert>
				<FormField
					label="Sample event (JSON)"
					description="Test matching without delivering an event."
				>
					<Textarea
						ariaLabel="Sample event"
						rows={5}
						value={sample}
						onChange={({ detail }) => {
							setSample(detail.value);
							testVersion.current++;
							setResult(undefined);
						}}
					/>
				</FormField>
				<Button disabled={!pattern.trim()} loading={testing} onClick={test}>
					Test pattern
				</Button>
				{!!error && <Alert type="error">{error}</Alert>}
				{result !== undefined && (
					<Alert type={result ? "success" : "info"}>
						{result
							? "The sample event matches."
							: "The sample event does not match."}
					</Alert>
				)}
			</SpaceBetween>
		</ActionDialog>
	);
	function input() {
		return {
			Name: name,
			EventBusName: scope.bus,
			State: active ? "ENABLED" : "DISABLED",
			Description: description,
			EventPattern: pattern.trim(),
			ScheduleExpression: schedule.trim(),
			RoleArn: role,
		};
	}
}
