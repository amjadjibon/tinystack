import {
	Alert,
	Button,
	Checkbox,
	FormField,
	Input,
	Select,
	SpaceBetween,
	Textarea,
} from "@cloudscape-design/components";
import { useEffect, useRef, useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import type { Resource } from "#/lib/api";
import { errorText, stringMap } from "#/lib/consoleActions";
import {
	type DefinitionValidation,
	type Machine,
	machineArn,
	type SfnScope,
	sampleDefinition,
	sfnCall,
	validateDefinition,
} from "#/lib/sfn";
import { tagList } from "#/lib/tags";

export function StateMachineForm({
	scope,
	machine,
	resources,
	onClose,
	onSaved,
}: {
	scope: SfnScope;
	machine?: Machine;
	resources: Resource[];
	onClose: () => void;
	onSaved: (name: string) => void;
}) {
	const [name, setName] = useState(machine?.Name ?? ""),
		[type, setType] = useState(machine?.Type ?? "STANDARD");
	const [definition, setDefinition] = useState(
		machine?.Definition ?? sampleDefinition,
	);
	const [role, setRole] = useState(
		machine?.RoleArn ?? `arn:aws:iam::${scope.account}:role/states`,
	);
	const [level, setLevel] = useState(
		machine?.LoggingConfiguration?.Level ?? "OFF",
	);
	const [group, setGroup] = useState(
		machine?.LoggingConfiguration?.Destinations?.[0]?.CloudWatchLogsLogGroup
			?.LogGroupArn ?? "",
	);
	const [includeData, setIncludeData] = useState(
		machine?.LoggingConfiguration?.IncludeExecutionData ?? false,
	);
	const [tags, setTags] = useState("{}"),
		[validating, setValidating] = useState(false),
		[error, setError] = useState("");
	const [validation, setValidation] = useState<{
		text: string;
		type: string;
		result: DefinitionValidation;
	}>();
	const mounted = useRef(true);
	useEffect(() => {
		mounted.current = true;
		return () => {
			mounted.current = false;
		};
	}, []);
	const roles = resources.filter(
		(r) =>
			r.service === "iam" && r.type === "role" && r.account === scope.account,
	);
	const groups = resources.filter(
		(r) =>
			r.service === "logs" &&
			r.type === "log-group" &&
			r.account === scope.account &&
			r.region === scope.region,
	);
	const validate = async () => {
		const text = definition,
			kind = type;
		setValidating(true);
		setError("");
		try {
			const result = await validateDefinition(scope, text, kind);
			if (mounted.current) setValidation({ text, type: kind, result });
		} catch (err) {
			if (mounted.current) setError(errorText(err));
		} finally {
			if (mounted.current) setValidating(false);
		}
	};
	const currentValidation =
		validation?.text === definition && validation.type === type
			? validation.result
			: undefined;
	return (
		<ActionDialog
			title={machine ? `Edit ${machine.Name}` : "Create state machine"}
			submit={machine ? "Save changes" : "Create state machine"}
			onClose={onClose}
			onSaved={() => onSaved(name)}
			plan={async () => {
				if (!name || Array.from(name).length > 80)
					throw new Error("Name must contain 1–80 characters.");
				if (!/^arn:aws[a-z-]*:iam::\d{12}:role\/.+$/.test(role))
					throw new Error("Enter a valid IAM role ARN.");
				if (level !== "OFF" && !group)
					throw new Error("Choose or enter a CloudWatch Logs group ARN.");
				const tagValues = machine
					? undefined
					: tagList(stringMap(tags, "Tags"));
				const checked = await validateDefinition(scope, definition, type);
				if (checked.Result !== "OK")
					throw new Error(
						(checked.Diagnostics ?? [])
							.map((d) => `${d.Location ?? ""} ${d.Message}`)
							.join("\n") || "Definition is invalid.",
					);
				const input = {
					Definition: definition,
					RoleArn: role,
					LoggingConfiguration: {
						Level: level,
						IncludeExecutionData: includeData,
						Destinations:
							level === "OFF"
								? []
								: [{ CloudWatchLogsLogGroup: { LogGroupArn: group } }],
					},
				};
				return [
					{
						label: machine ? "Update state machine" : "Create state machine",
						run: () =>
							sfnCall(
								scope,
								machine ? "UpdateStateMachine" : "CreateStateMachine",
								machine
									? { ...input, StateMachineArn: machineArn(scope) }
									: { ...input, Name: name, Type: type, Tags: tagValues },
							),
					},
				];
			}}
		>
			<SpaceBetween size="l">
				{machine && (
					<Alert type="info">
						Changes apply to future executions. Running executions keep their
						original definition.
					</Alert>
				)}
				<FormField
					label="State machine name"
					constraintText="1–80 characters. Names are unique in this account and Region."
				>
					<Input
						ariaLabel="State machine name"
						value={name}
						disabled={!!machine}
						onChange={({ detail }) => setName(detail.value)}
					/>
				</FormField>
				<FormField
					label="Type"
					description="Standard retains execution history. Express returns a synchronous result in this console. Type cannot be changed after creation."
				>
					<Select
						ariaLabel="State machine type"
						disabled={!!machine}
						selectedOption={{ label: type, value: type }}
						options={[
							{ label: "Standard", value: "STANDARD" },
							{ label: "Express", value: "EXPRESS" },
						]}
						onChange={({ detail }) =>
							setType(detail.selectedOption.value ?? "STANDARD")
						}
					/>
				</FormField>
				<FormField
					label="Definition (Amazon States Language)"
					constraintText="JSON definition, at most 1 MiB. Validation uses TinyStack's supported ASL subset."
				>
					<Textarea
						ariaLabel="State machine definition"
						rows={16}
						value={definition}
						onChange={({ detail }) => setDefinition(detail.value)}
					/>
				</FormField>
				<Button loading={validating} onClick={validate}>
					Validate definition
				</Button>
				{!!error && <Alert type="error">{error}</Alert>}
				{currentValidation && (
					<Alert
						type={currentValidation.Result === "OK" ? "success" : "error"}
						header={
							currentValidation.Result === "OK"
								? "Definition is valid"
								: "Definition validation failed"
						}
					>
						{(currentValidation.Diagnostics ?? []).map((d) => (
							<div key={`${d.Code}-${d.Location}-${d.Message}`}>
								{d.Severity}: {d.Location} {d.Message}
							</div>
						))}
						{currentValidation.Truncated &&
							"Additional diagnostics were truncated."}
					</Alert>
				)}
				<FormField
					label="Execution role"
					description="Choose a role from this account or enter its ARN. TinyStack does not enforce IAM role permissions for execution."
				>
					<SpaceBetween size="xs">
						<Select
							ariaLabel="Existing execution role"
							placeholder="Choose an existing role"
							selectedOption={
								roles.some((r) => r.arn === role)
									? {
											label: roles.find((r) => r.arn === role)?.name,
											value: role,
										}
									: null
							}
							options={roles.map((r) => ({ label: r.name, value: r.arn }))}
							onChange={({ detail }) =>
								setRole(detail.selectedOption.value ?? "")
							}
						/>
						<Input
							ariaLabel="Execution role ARN"
							value={role}
							onChange={({ detail }) => setRole(detail.value)}
						/>
					</SpaceBetween>
				</FormField>
				<FormField label="Log level">
					<Select
						ariaLabel="Execution log level"
						selectedOption={{ label: level, value: level }}
						options={["OFF", "ALL", "ERROR", "FATAL"].map((value) => ({
							label: value,
							value,
						}))}
						onChange={({ detail }) =>
							setLevel(detail.selectedOption.value ?? "OFF")
						}
					/>
				</FormField>
				{level !== "OFF" && (
					<SpaceBetween size="l">
						<FormField
							label="CloudWatch Logs group"
							constraintText="Log group ARN ending in :*. The log group must exist for execution logs to be written."
						>
							<SpaceBetween size="xs">
								<Select
									ariaLabel="Existing log group"
									placeholder="Choose a log group in this Region"
									selectedOption={null}
									options={groups.map((r) => ({
										label: r.name,
										value: `${r.arn.replace(/:\*$/, "")}:*`,
									}))}
									onChange={({ detail }) =>
										setGroup(detail.selectedOption.value ?? "")
									}
								/>
								<Input
									ariaLabel="Log group ARN"
									value={group}
									onChange={({ detail }) => setGroup(detail.value)}
								/>
							</SpaceBetween>
						</FormField>
						<Checkbox
							checked={includeData}
							onChange={({ detail }) => setIncludeData(detail.checked)}
						>
							Include execution input and output in logs
						</Checkbox>
					</SpaceBetween>
				)}
				{!machine && (
					<FormField label="Tags (JSON)" description="Object of string values.">
						<Textarea
							ariaLabel="State machine tags"
							rows={4}
							value={tags}
							onChange={({ detail }) => setTags(detail.value)}
						/>
					</FormField>
				)}
			</SpaceBetween>
		</ActionDialog>
	);
}
