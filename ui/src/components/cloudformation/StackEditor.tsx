import {
	Alert,
	Button,
	Checkbox,
	FormField,
	Input,
	SpaceBetween,
	Textarea,
} from "@cloudscape-design/components";
import { useRef, useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { ConsoleTags } from "#/components/ConsoleTags";
import {
	type CfnScope,
	inspectTemplate,
	saveStack,
} from "#/lib/cloudformation";
import {
	defaultChangeSetName,
	initialParameters,
	parameterInput,
	requiredCapabilities,
	type Stack,
	type TemplateSummary,
	templateInput,
} from "#/lib/cloudformationHelpers";
import { tagList } from "#/lib/tags";

export function StackEditor({
	scope,
	stack,
	disabled,
	onSaved,
}: {
	scope: CfnScope;
	stack?: Stack;
	disabled?: boolean;
	onSaved: (id?: string, changeSet?: string) => void;
}) {
	const [open, setOpen] = useState(false);
	return (
		<>
			<Button disabled={disabled} onClick={() => setOpen(true)}>
				{stack ? "Update stack" : "Create stack"}
			</Button>
			{open && (
				<Editor
					scope={scope}
					stack={stack}
					onClose={() => setOpen(false)}
					onSaved={(id, changeSet) => {
						setOpen(false);
						onSaved(id, changeSet);
					}}
				/>
			)}
		</>
	);
}
function Editor({
	scope,
	stack,
	onClose,
	onSaved,
}: {
	scope: CfnScope;
	stack?: Stack;
	onClose: () => void;
	onSaved: (id?: string, changeSet?: string) => void;
}) {
	const [name, setName] = useState(stack?.StackName ?? ""),
		[body, setBody] = useState(""),
		[previous, setPrevious] = useState(!!stack),
		[summary, setSummary] = useState<TemplateSummary>(),
		[values, setValues] = useState<ReturnType<typeof initialParameters>>({}),
		[caps, setCaps] = useState<string[]>([]),
		[tags, setTags] = useState<Record<string, string>>(
			Object.fromEntries((stack?.Tags ?? []).map((t) => [t.Key, t.Value])),
		),
		[protectedStack, setProtected] = useState(false),
		[changeSet, setChangeSet] = useState(false),
		[changeName, setChangeName] = useState<string>(),
		[stamp] = useState(() => new Date()),
		[error, setError] = useState(""),
		[loading, setLoading] = useState(false);
	const savedId = useRef<string | undefined>(undefined);
	const savedChangeSet = useRef<string | undefined>(undefined);
	const effectiveChangeName =
		changeName ?? defaultChangeSetName(name.trim(), stamp);
	const invalidate = () => {
		setSummary(undefined);
		setValues({});
		setCaps([]);
		setError("");
	};
	const inspect = async () => {
		setLoading(true);
		setError("");
		try {
			templateInput(body, previous);
			const out = await inspectTemplate(scope, body, previous);
			setSummary(out);
			setValues(initialParameters(out.Parameters ?? [], stack));
			setCaps([]);
		} catch {
			setError(
				"Template validation failed. Check the template syntax and supported resource types.",
			);
		} finally {
			setLoading(false);
		}
	};
	return (
		<ActionDialog
			title={stack ? `Update ${stack.StackName}` : "Create stack"}
			submit={
				changeSet
					? "Create change set"
					: stack
						? "Update stack"
						: "Create stack"
			}
			onClose={onClose}
			onSaved={() => onSaved(savedId.current, savedChangeSet.current)}
			plan={async () => {
				if (!name.trim()) throw new Error("Enter a stack name.");
				if (!summary)
					throw new Error("Validate and inspect the template first.");
				if (requiredCapabilities(summary).some((c) => !caps.includes(c)))
					throw new Error("Acknowledge required capabilities.");
				if (changeSet && !effectiveChangeName.trim())
					throw new Error("Enter a change set name.");
				const input = {
					StackName: name,
					...templateInput(body, previous),
					Parameters: parameterInput(summary.Parameters ?? [], values),
					Capabilities: caps,
					Tags: tagList(tags),
					...(changeSet
						? {
								ChangeSetName: effectiveChangeName.trim(),
								ChangeSetType: stack ? "UPDATE" : "CREATE",
							}
						: stack
							? {}
							: { EnableTerminationProtection: protectedStack }),
				};
				return [
					{
						label: changeSet ? "Create change set" : "Save stack",
						run: async () => {
							try {
								savedId.current = (
									await saveStack(
										scope,
										changeSet
											? "CreateChangeSet"
											: stack
												? "UpdateStack"
												: "CreateStack",
										input,
									)
								).StackId;
								savedChangeSet.current = changeSet
									? effectiveChangeName.trim()
									: undefined;
							} catch {
								throw new Error(
									"Stack action failed. Check template, parameters, capabilities and stack status.",
								);
							}
						},
					},
				];
			}}
		>
			<SpaceBetween size="m">
				<FormField label="Stack name">
					<Input
						ariaLabel="Stack name"
						value={name}
						disabled={!!stack}
						onChange={({ detail }) => setName(detail.value)}
					/>
				</FormField>
				{stack && (
					<Checkbox
						disabled={loading}
						checked={previous}
						onChange={({ detail }) => {
							setPrevious(detail.checked);
							invalidate();
						}}
					>
						Use current template
					</Checkbox>
				)}
				{!previous && (
					<>
						<FormField label="Template body (JSON or YAML)">
							<Textarea
								disabled={loading}
								ariaLabel="Template body"
								rows={10}
								value={body}
								onChange={({ detail }) => {
									setBody(detail.value);
									invalidate();
								}}
							/>
						</FormField>
						<FormField label="Upload template (.json, .yaml or .yml)">
							<input
								type="file"
								accept=".json,.yaml,.yml"
								aria-label="Upload template"
								disabled={loading}
								onChange={async (e) => {
									const file = e.target.files?.[0];
									if (!file) return;
									invalidate();
									setLoading(true);
									try {
										setBody(await file.text());
									} catch {
										setError("Cannot read the selected file.");
									} finally {
										setLoading(false);
									}
								}}
							/>
						</FormField>
					</>
				)}
				<Button loading={loading} onClick={inspect}>
					Validate and inspect template
				</Button>
				{error && <Alert type="error">{error}</Alert>}
				{summary && (
					<>
						{(summary.Parameters ?? []).map((p) => (
							<FormField
								key={p.ParameterKey}
								label={p.ParameterKey}
								description={[
									p.Description,
									p.ParameterType,
									!p.NoEcho &&
										p.ParameterConstraints?.AllowedValues?.join(", "),
								]
									.filter(Boolean)
									.join(" — ")}
							>
								<SpaceBetween size="xs">
									{stack?.Parameters?.some(
										(v) => v.ParameterKey === p.ParameterKey,
									) && (
										<Checkbox
											checked={values[p.ParameterKey]?.previous ?? false}
											onChange={({ detail }) =>
												setValues((v) => ({
													...v,
													[p.ParameterKey]: {
														...v[p.ParameterKey],
														previous: detail.checked,
													},
												}))
											}
										>
											Use previous value
										</Checkbox>
									)}
									<Input
										ariaLabel={p.ParameterKey}
										type={p.NoEcho ? "password" : "text"}
										autoComplete={false}
										disabled={values[p.ParameterKey]?.previous}
										value={values[p.ParameterKey]?.value ?? ""}
										placeholder={
											p.DefaultValue !== undefined
												? "Leave empty to use template default"
												: "Required"
										}
										onChange={({ detail }) =>
											setValues((v) => ({
												...v,
												[p.ParameterKey]: {
													...v[p.ParameterKey],
													value: detail.value,
												},
											}))
										}
									/>
								</SpaceBetween>
							</FormField>
						))}
						{summary.CapabilitiesReason && (
							<Alert>{summary.CapabilitiesReason}</Alert>
						)}
						{requiredCapabilities(summary).map((c) => (
							<Checkbox
								key={c}
								checked={caps.includes(c)}
								onChange={({ detail }) =>
									setCaps((v) =>
										detail.checked ? [...v, c] : v.filter((x) => x !== c),
									)
								}
							>
								I acknowledge {c}
							</Checkbox>
						))}
					</>
				)}
				<ConsoleTags
					tags={tags}
					enabled
					plan={(next) => [
						{
							label: "Set stack tags",
							run: async () => {
								setTags(next);
							},
						},
					]}
					onChanged={() => {}}
				/>
				<Checkbox
					checked={changeSet}
					onChange={({ detail }) => setChangeSet(detail.checked)}
				>
					Create a change set for review
				</Checkbox>
				{changeSet && (
					<FormField label="Change set name">
						<Input
							ariaLabel="Change set name"
							value={effectiveChangeName}
							onChange={({ detail }) => setChangeName(detail.value)}
						/>
					</FormField>
				)}
				{changeSet && (
					<Alert>
						Review the change set in the stack's Change sets tab, then choose
						Execute.
					</Alert>
				)}
				{!stack && !changeSet && (
					<Checkbox
						checked={protectedStack}
						onChange={({ detail }) => setProtected(detail.checked)}
					>
						Enable termination protection
					</Checkbox>
				)}
			</SpaceBetween>
		</ActionDialog>
	);
}
