import {
	Alert,
	Box,
	Button,
	Checkbox,
	Container,
	FormField,
	Header,
	Input,
	Select,
	SpaceBetween,
	Textarea,
} from "@cloudscape-design/components";
import { useRef, useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { CodeBlock } from "#/components/CodeBlock";
import { integer, jsonObject } from "#/lib/consoleActions";
import {
	type Secret,
	type SecretScope,
	secretCall,
	secretDate,
} from "#/lib/secretsmanager";
import { usePoll } from "#/lib/usePoll";

export function SecretRotation({
	scope,
	secret,
	enabled,
	onChanged,
}: {
	scope: SecretScope;
	secret: Secret;
	enabled: boolean;
	onChanged: () => void;
}) {
	const [mode, setMode] = useState(""),
		[arn, setArn] = useState(""),
		[schedule, setSchedule] = useState("days"),
		[expression, setExpression] = useState(""),
		[days, setDays] = useState("30"),
		[duration, setDuration] = useState("");
	const token = useRef(crypto.randomUUID());
	const edit = () => {
		token.current = crypto.randomUUID();
		setArn(secret.RotationLambdaARN ?? "");
		setSchedule(
			secret.RotationRules?.ScheduleExpression ? "expression" : "days",
		);
		setExpression(secret.RotationRules?.ScheduleExpression ?? "rate(1 day)");
		setDays(String(secret.RotationRules?.AutomaticallyAfterDays ?? 30));
		setDuration(secret.RotationRules?.Duration ?? "");
		setMode("edit");
	};
	const choices = [
		{ label: "Days", value: "days" },
		{ label: "Schedule expression", value: "expression" },
	];
	return (
		<Container
			header={
				<Header
					variant="h2"
					actions={
						<SpaceBetween direction="horizontal" size="xs">
							<Button
								disabled={!enabled || !secret.RotationEnabled}
								onClick={() => setMode("disable")}
							>
								Disable rotation
							</Button>
							<Button
								disabled={
									!enabled ||
									!secret.RotationEnabled ||
									!secret.RotationLambdaARN
								}
								onClick={() => {
									token.current = crypto.randomUUID();
									setMode("rotate");
								}}
							>
								Rotate secret now
							</Button>
							<Button disabled={!enabled} onClick={edit}>
								Edit rotation
							</Button>
						</SpaceBetween>
					}
				>
					Rotation configuration
				</Header>
			}
		>
			<SpaceBetween size="m">
				<Box>
					Automatic rotation: {secret.RotationEnabled ? "Enabled" : "Disabled"}
				</Box>
				<Box>Lambda function: {secret.RotationLambdaARN || "—"}</Box>
				<Box>
					Schedule:{" "}
					{secret.RotationRules?.ScheduleExpression ??
						(secret.RotationRules?.AutomaticallyAfterDays
							? `Every ${secret.RotationRules.AutomaticallyAfterDays} days`
							: "—")}
				</Box>
				<Box>Duration: {secret.RotationRules?.Duration ?? "—"}</Box>
				<Box>
					Last rotated: {secretDate(secret.LastRotatedDate)} · Next rotation:{" "}
					{secretDate(secret.NextRotationDate)}
				</Box>
			</SpaceBetween>
			{!!mode && (
				<ActionDialog
					title={
						mode === "edit"
							? "Edit rotation configuration"
							: mode === "rotate"
								? "Rotate secret now"
								: "Disable automatic rotation"
					}
					submit={mode === "rotate" ? "Rotate secret" : "Save changes"}
					onClose={() => setMode("")}
					onSaved={() => {
						setMode("");
						onChanged();
					}}
					plan={async () => {
						if (mode === "disable")
							return [
								{
									label: "Disable rotation",
									run: () => secretCall(scope, "CancelRotateSecret"),
								},
							];
						if (mode === "rotate")
							return [
								{
									label: "Start rotation",
									run: () =>
										secretCall(scope, "RotateSecret", {
											ClientRequestToken: token.current,
											RotateImmediately: true,
										}),
								},
							];
						if (
							!arn.startsWith(
								`arn:aws:lambda:${scope.region}:${scope.account}:function:`,
							)
						)
							throw new Error(
								"Enter a Lambda function ARN in the selected account and Region.",
							);
						if (duration && !/^(?:[1-9]|1\d|2[0-4])h$/.test(duration))
							throw new Error("Duration must be 1h through 24h.");
						if (schedule === "expression" && !expression.trim())
							throw new Error("Enter a rate or cron expression.");
						const rules = {
							...(schedule === "days"
								? { AutomaticallyAfterDays: integer(days, "Days", 1, 1000) }
								: { ScheduleExpression: expression.trim() }),
							...(duration ? { Duration: duration } : {}),
						};
						return [
							{
								label: "Save rotation configuration",
								run: () =>
									secretCall(scope, "RotateSecret", {
										RotationLambdaARN: arn.trim(),
										RotationRules: rules,
										RotateImmediately: false,
										ClientRequestToken: token.current,
									}),
							},
						];
					}}
				>
					{mode === "edit" ? (
						<SpaceBetween size="m">
							<Alert type="info">
								Saving enables scheduled rotation without invoking the Lambda
								function immediately. The function must implement Secrets
								Manager's four rotation steps.
							</Alert>
							<FormField label="Rotation Lambda ARN">
								<Input
									ariaLabel="Rotation Lambda ARN"
									value={arn}
									onChange={({ detail }) => setArn(detail.value)}
								/>
							</FormField>
							<FormField label="Schedule type">
								<Select
									ariaLabel="Rotation schedule type"
									options={choices}
									selectedOption={
										choices.find((o) => o.value === schedule) ?? null
									}
									onChange={({ detail }) =>
										setSchedule(detail.selectedOption.value ?? "days")
									}
								/>
							</FormField>
							{schedule === "days" ? (
								<FormField label="Days between rotations (1–1,000)">
									<Input
										ariaLabel="Rotation days"
										value={days}
										onChange={({ detail }) => setDays(detail.value)}
									/>
								</FormField>
							) : (
								<FormField label="Rate or cron expression">
									<Input
										ariaLabel="Rotation schedule"
										value={expression}
										onChange={({ detail }) => setExpression(detail.value)}
									/>
								</FormField>
							)}
							<FormField
								label="Rotation duration – optional"
								description="1h to 24h. TinyStack records this window but does not enforce it."
							>
								<Input
									ariaLabel="Rotation duration"
									value={duration}
									onChange={({ detail }) => setDuration(detail.value)}
								/>
							</FormField>
						</SpaceBetween>
					) : (
						<Alert type="info">
							{mode === "rotate"
								? "Start a local four-step rotation using the configured Lambda function. Completion and errors appear in the delivery log."
								: "Stop scheduled rotation. A rotation already running may complete; pending version labels remain."}
						</Alert>
					)}
				</ActionDialog>
			)}
		</Container>
	);
}
export function SecretPolicy({
	scope,
	enabled,
	readable,
}: {
	scope: SecretScope;
	enabled: boolean;
	readable: boolean;
}) {
	const policy = usePoll(
		() =>
			readable
				? secretCall<{ ResourcePolicy?: string }>(scope, "GetResourcePolicy")
				: Promise.resolve({ ResourcePolicy: undefined }),
		0,
	);
	const [mode, setMode] = useState(""),
		[text, setText] = useState(""),
		[block, setBlock] = useState(true);
	return (
		<Container
			header={
				<Header
					variant="h2"
					actions={
						<SpaceBetween direction="horizontal" size="xs">
							<Button
								iconName="refresh"
								ariaLabel="Refresh resource policy"
								onClick={policy.reload}
							/>
							<Button
								disabled={
									!enabled || !policy.data?.ResourcePolicy || !!policy.error
								}
								onClick={() => setMode("delete")}
							>
								Delete policy
							</Button>
							<Button
								disabled={!enabled || policy.loading || !!policy.error}
								onClick={() => {
									setText(
										policy.data?.ResourcePolicy ??
											'{\n  "Version": "2012-10-17",\n  "Statement": []\n}',
									);
									setBlock(true);
									setMode("edit");
								}}
							>
								Edit resource policy
							</Button>
						</SpaceBetween>
					}
				>
					Resource policy
				</Header>
			}
		>
			<SpaceBetween size="m">
				<Box>
					Resource policies are stored as metadata; TinyStack does not evaluate
					them for authorization.
				</Box>
				{!!policy.error && <Alert type="error">{policy.error}</Alert>}
				{!readable ? (
					<Box>Use signed AWS CLI or SDK requests to read this policy.</Box>
				) : policy.data?.ResourcePolicy ? (
					<CodeBlock text={policy.data.ResourcePolicy} />
				) : (
					<Box>
						{policy.loading
							? "Loading policy…"
							: "No resource policy attached."}
					</Box>
				)}
			</SpaceBetween>
			{!!mode && (
				<ActionDialog
					title={
						mode === "delete"
							? "Delete resource policy"
							: "Edit resource policy"
					}
					onClose={() => setMode("")}
					onSaved={() => {
						setMode("");
						policy.reload();
					}}
					plan={async () => {
						if (mode === "delete")
							return [
								{
									label: "Delete policy",
									run: () => secretCall(scope, "DeleteResourcePolicy"),
								},
							];
						jsonObject(text, "Resource policy");
						const result = await secretCall<{
							PolicyValidationPassed: boolean;
						}>(scope, "ValidateResourcePolicy", { ResourcePolicy: text });
						if (!result.PolicyValidationPassed)
							throw new Error("Resource policy validation failed.");
						return [
							{
								label: "Save resource policy",
								run: () =>
									secretCall(scope, "PutResourcePolicy", {
										ResourcePolicy: text,
										BlockPublicPolicy: block,
									}),
							},
						];
					}}
				>
					{mode === "delete" ? (
						<Box>Remove the resource policy attached to this secret.</Box>
					) : (
						<SpaceBetween size="m">
							<FormField label="Resource policy (JSON)">
								<Textarea
									ariaLabel="Resource policy"
									rows={12}
									value={text}
									onChange={({ detail }) => setText(detail.value)}
								/>
							</FormField>
							<Checkbox
								checked={block}
								onChange={({ detail }) => setBlock(detail.checked)}
							>
								Block public policy
							</Checkbox>
						</SpaceBetween>
					)}
				</ActionDialog>
			)}
		</Container>
	);
}
