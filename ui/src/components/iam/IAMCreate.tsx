import {
	Alert,
	Box,
	Button,
	Checkbox,
	Container,
	ContentLayout,
	Form,
	FormField,
	Header,
	RadioGroup,
	Select,
	SpaceBetween,
	Wizard,
} from "@cloudscape-design/components";
import { useRef, useState } from "react";
import type { Resource } from "#/lib/api";
import {
	emptyPolicy,
	type IAMKind,
	type IAMStep,
	iamCall,
	iamEntity,
	iamGroupsForUser,
	iamLabels,
	iamSequence,
	policyText,
} from "#/lib/iam";
import { type IAMField, IAMFields, validateIAMFields } from "./IAMForm";
import { IAMSelection } from "./IAMSelection";

export function IAMCreate({
	kind,
	account,
	resources,
	onCancel,
	onCreated,
}: {
	kind: IAMKind;
	account: string;
	resources: Resource[];
	onCancel: () => void;
	onCreated: (name: string) => void;
}) {
	const entity = iamEntity[kind];
	const label = iamLabels[kind].singular.toLowerCase();
	const [values, setValues] = useState<Record<string, string>>({
		Path: "/",
		MaxSessionDuration: "3600",
		Document: emptyPolicy,
		TrustedAccount: account,
	});
	const [errors, setErrors] = useState<Record<string, string>>({});
	const [error, setError] = useState("");
	const [busy, setBusy] = useState(false);
	const [partial, setPartial] = useState(false);
	const [step, setStep] = useState(0);
	const [permissionMode, setPermissionMode] = useState("groups");
	const [groups, setGroups] = useState<string[]>([]);
	const [policies, setPolicies] = useState<string[]>([]);
	const [users, setUsers] = useState<string[]>([]);
	const [copyUser, setCopyUser] = useState<string[]>([]);
	const [trustType, setTrustType] = useState("service");
	const [service, setService] = useState("lambda.amazonaws.com");
	const [profile, setProfile] = useState(true);
	const completed = useRef(new Set<string>());
	const pending = useRef<IAMStep[] | null>(null);
	const options = (type: string) =>
		resources
			.filter((r) => r.type === type)
			.map((r) => ({
				label: r.name,
				value: type === "policy" ? r.arn : r.name,
				description:
					type === "group"
						? `${((r.details?.Users as string[]) ?? []).length} users · ${((r.details?.AttachedPolicies as string[]) ?? []).length} policies`
						: r.arn,
			}));
	const fields: IAMField[] = [
		{
			id: "Name",
			label: `${iamLabels[kind].singular} name`,
			required: true,
			description: "Letters, numbers, and +=,.@_- characters.",
		},
		{
			id: "Path",
			label: "Path",
			required: true,
			description: "Use / or a path that starts and ends with /.",
		},
		...(kind === "role" || kind === "policy"
			? [{ id: "Description", label: "Description — optional" }]
			: []),
		...(kind === "role"
			? [
					{
						id: "MaxSessionDuration",
						label: "Maximum session duration (seconds)",
						type: "number" as const,
						required: true,
						description: "From 3,600 to 43,200 seconds.",
					},
				]
			: []),
		...(kind === "policy"
			? [
					{
						id: "Document",
						label: "Permissions policy",
						type: "json" as const,
						required: true,
					},
				]
			: []),
	];
	const trust =
		trustType === "custom"
			? values.Document
			: JSON.stringify(
					{
						Version: "2012-10-17",
						Statement: [
							{
								Effect: "Allow",
								Principal:
									trustType === "service"
										? { Service: service }
										: { AWS: `arn:aws:iam::${values.TrustedAccount}:root` },
								Action: "sts:AssumeRole",
							},
						],
					},
					null,
					2,
				);
	const validateTrust = () => {
		const invalid =
			trustType === "custom"
				? validateIAMFields(
						[
							{
								id: "Document",
								label: "Trust policy",
								type: "json",
								required: true,
							},
						],
						values,
					)
				: trustType === "account" && !/^\d{12}$/.test(values.TrustedAccount)
					? { TrustedAccount: "Enter a 12-digit AWS account ID." }
					: {};
		setErrors(invalid);
		return !Object.keys(invalid).length;
	};
	const validateDetails = () => {
		const invalid = validateIAMFields(fields, values);
		if (
			values.Name &&
			(!/^[\w+=,.@-]+$/.test(values.Name) ||
				values.Name.length > (kind === "user" || kind === "role" ? 64 : 128))
		)
			invalid.Name = "Enter a valid name within the IAM length limit.";
		if (values.Path !== "/" && !/^\/[^\s]*\/$/.test(values.Path))
			invalid.Path = "The path must start and end with /.";
		if (
			kind === "role" &&
			(Number(values.MaxSessionDuration) < 3600 ||
				Number(values.MaxSessionDuration) > 43200)
		)
			invalid.MaxSessionDuration =
				"Enter a duration between 3,600 and 43,200 seconds.";
		setErrors(invalid);
		return !Object.keys(invalid).length;
	};
	const validatePermissions = () => {
		if (
			kind === "user" &&
			permissionMode === "copy" &&
			(!copyUser.length ||
				!resources.some((r) => r.type === "user" && r.name === copyUser[0]))
		) {
			setError("Select an existing user to copy permissions from.");
			return false;
		}
		setError("");
		return true;
	};
	const submit = async () => {
		if (
			!partial &&
			(!validateDetails() ||
				(kind === "role" && !validateTrust()) ||
				!validatePermissions())
		)
			return;
		setBusy(true);
		setError("");
		try {
			if (!pending.current) {
				const steps: IAMStep[] = [
					{
						id: "create",
						label: `Create ${label}`,
						run: () =>
							iamCall(account, `Create${entity}`, {
								[`${entity}Name`]: values.Name,
								Path: values.Path,
								...((kind === "role" || kind === "policy") && {
									Description: values.Description ?? "",
								}),
								...(kind === "role" && {
									AssumeRolePolicyDocument: trust,
									MaxSessionDuration: Number(values.MaxSessionDuration),
								}),
								...(kind === "policy" && { PolicyDocument: values.Document }),
							}),
					},
				];
				let selectedGroups =
					kind === "user" && permissionMode === "groups" ? groups : [];
				let selectedPolicies =
					kind !== "user" || permissionMode === "direct" ? policies : [];
				const source = resources.find(
					(r) => r.type === "user" && r.name === copyUser[0],
				);
				if (kind === "user" && permissionMode === "copy" && source) {
					selectedGroups = iamGroupsForUser(resources, source.name).map(
						(r) => r.name,
					);
					selectedPolicies =
						(source.details?.AttachedPolicies as string[]) ?? [];
					// Read source documents before creating anything; retries use the same snapshot.
					for (const name of (source.details?.InlinePolicies as string[]) ??
						[]) {
						const output = await iamCall(account, "GetUserPolicy", {
							UserName: source.name,
							PolicyName: name,
						});
						const document = policyText(output.PolicyDocument);
						steps.push({
							id: `inline:${name}`,
							label: `Copy inline policy ${name}`,
							run: () =>
								iamCall(account, "PutUserPolicy", {
									UserName: values.Name,
									PolicyName: name,
									PolicyDocument: document,
								}),
						});
					}
				}
				for (const name of kind === "group" ? users : selectedGroups)
					steps.push({
						id: `membership:${name}`,
						label: `Add ${name}`,
						run: () =>
							iamCall(account, "AddUserToGroup", {
								UserName: kind === "group" ? name : values.Name,
								GroupName: kind === "group" ? values.Name : name,
							}),
					});
				for (const arn of selectedPolicies)
					steps.push({
						id: arn,
						label: `Attach ${arn}`,
						run: () =>
							iamCall(account, `Attach${entity}Policy`, {
								[`${entity}Name`]: values.Name,
								PolicyArn: arn,
							}),
					});
				if (
					kind === "role" &&
					trustType === "service" &&
					service === "ec2.amazonaws.com" &&
					profile
				) {
					steps.push({
						id: "profile",
						label: "Create EC2 instance profile",
						run: () =>
							iamCall(account, "CreateInstanceProfile", {
								InstanceProfileName: values.Name,
								Path: values.Path,
							}),
					});
					steps.push({
						id: "profile-role",
						label: "Add role to instance profile",
						run: () =>
							iamCall(account, "AddRoleToInstanceProfile", {
								InstanceProfileName: values.Name,
								RoleName: values.Name,
							}),
					});
				}
				pending.current = steps;
			}
			await iamSequence(pending.current, completed.current);
			onCreated(values.Name);
		} catch (err) {
			setPartial(completed.current.size > 0);
			if (!completed.current.size) pending.current = null;
			setError(err instanceof Error ? err.message : String(err));
		} finally {
			setBusy(false);
		}
	};
	const details = (
		<Container
			header={<Header variant="h2">{iamLabels[kind].singular} details</Header>}
		>
			<IAMFields
				disabled={busy}
				fields={fields}
				values={values}
				onChange={setValues}
				errors={errors}
			/>
		</Container>
	);
	const policySelection = (
		<IAMSelection
			disabled={busy}
			title="Permissions policies"
			options={options("policy")}
			selected={policies}
			onChange={setPolicies}
		/>
	);
	const permissions = (
		<SpaceBetween size="l">
			{kind === "user" && (
				<Container header={<Header variant="h2">Permissions options</Header>}>
					<RadioGroup
						value={permissionMode}
						onChange={({ detail }) => setPermissionMode(detail.value)}
						items={[
							{
								value: "groups",
								label: "Add user to group",
								description: "Manage permissions through user groups.",
							},
							{
								value: "copy",
								label: "Copy permissions",
								description:
									"Copy an existing user's group memberships, managed policies, and inline policies.",
							},
							{
								value: "direct",
								label: "Attach policies directly",
								description: "Attach customer managed policies to this user.",
							},
						]}
					/>
				</Container>
			)}
			{kind === "user" && permissionMode === "groups" ? (
				<IAMSelection
					disabled={busy}
					title="User groups"
					options={options("group")}
					selected={groups}
					onChange={setGroups}
				/>
			) : kind === "user" && permissionMode === "copy" ? (
				<IAMSelection
					disabled={busy}
					title="Users"
					options={options("user")}
					selected={copyUser}
					onChange={setCopyUser}
					single
				/>
			) : (
				policySelection
			)}
			<Alert type="info">
				Policies define allowed actions and resources. TinyStack stores policies
				but does not evaluate general IAM permission policies.
			</Alert>
		</SpaceBetween>
	);
	const trustedEntity = (
		<SpaceBetween size="l">
			<Container header={<Header variant="h2">Trusted entity type</Header>}>
				<SpaceBetween size="l">
					<RadioGroup
						value={trustType}
						onChange={({ detail }) => {
							setTrustType(detail.value);
							setErrors({});
						}}
						items={[
							{
								value: "service",
								label: "AWS service",
								description: "Allow a service to assume this role.",
							},
							{
								value: "account",
								label: "AWS account",
								description:
									"Allow identities in an AWS account to assume this role.",
							},
							{
								value: "custom",
								label: "Custom trust policy",
								description:
									"Define principals, actions, and conditions in JSON.",
							},
						]}
					/>
					{trustType === "service" && (
						<FormField label="Service or use case">
							<Select
								selectedOption={{
									label: (
										{
											"lambda.amazonaws.com": "Lambda",
											"ec2.amazonaws.com": "EC2",
											"ecs-tasks.amazonaws.com": "ECS tasks",
										} as Record<string, string>
									)[service],
									value: service,
								}}
								options={[
									{ label: "Lambda", value: "lambda.amazonaws.com" },
									{ label: "EC2", value: "ec2.amazonaws.com" },
									{ label: "ECS tasks", value: "ecs-tasks.amazonaws.com" },
								]}
								onChange={({ detail }) =>
									setService(
										detail.selectedOption.value ?? "lambda.amazonaws.com",
									)
								}
							/>
						</FormField>
					)}
					{trustType === "account" && (
						<IAMFields
							disabled={busy}
							fields={[
								{ id: "TrustedAccount", label: "Account ID", required: true },
							]}
							values={values}
							onChange={setValues}
							errors={errors}
						/>
					)}
					{trustType === "custom" && (
						<IAMFields
							disabled={busy}
							fields={[
								{
									id: "Document",
									label: "Trust policy",
									type: "json",
									required: true,
								},
							]}
							values={values}
							onChange={setValues}
							errors={errors}
						/>
					)}
					{trustType === "service" && service === "ec2.amazonaws.com" && (
						<Checkbox
							checked={profile}
							onChange={({ detail }) => setProfile(detail.checked)}
						>
							Create an instance profile with the same name for use with EC2
						</Checkbox>
					)}
				</SpaceBetween>
			</Container>
		</SpaceBetween>
	);
	const source = resources.find(
		(r) => r.type === "user" && r.name === copyUser[0],
	);
	const review = (
		<SpaceBetween size="l">
			{partial && (
				<Alert type="warning">
					{values.Name} has been created. Retry to finish the remaining steps,
					or open the resource to manage the saved changes.
				</Alert>
			)}
			{kind === "role" && !partial ? (
				details
			) : (
				<Container
					header={
						<Header variant="h2">{iamLabels[kind].singular} details</Header>
					}
				>
					<Box>Name: {values.Name}</Box>
					<Box>Path: {values.Path}</Box>
				</Container>
			)}
			<Container header={<Header variant="h2">Permissions summary</Header>}>
				<SpaceBetween size="s">
					{kind === "user" && permissionMode === "groups" ? (
						<Box>User groups: {groups.join(", ") || "None"}</Box>
					) : kind === "user" && permissionMode === "copy" ? (
						<SpaceBetween size="s">
							<Box>Copy from: {copyUser[0] ?? "None"}</Box>
							<Box>
								Group memberships:{" "}
								{source
									? iamGroupsForUser(resources, source.name)
											.map((r) => r.name)
											.join(", ") || "None"
									: "None"}
							</Box>
							<Box>
								Managed policies:{" "}
								{((source?.details?.AttachedPolicies as string[]) ?? []).join(
									", ",
								) || "None"}
							</Box>
							<Box>
								Inline policies:{" "}
								{((source?.details?.InlinePolicies as string[]) ?? []).join(
									", ",
								) || "None"}
							</Box>
						</SpaceBetween>
					) : (
						<Box>
							Policies:{" "}
							{policies
								.map((arn) => resources.find((r) => r.arn === arn)?.name ?? arn)
								.join(", ") || "None"}
						</Box>
					)}
					{kind === "role" && (
						<SpaceBetween size="s">
							<Box>Trust policy</Box>
							<pre style={{ whiteSpace: "pre-wrap", overflowWrap: "anywhere" }}>
								{trust}
							</pre>
							{trustType === "service" && service === "ec2.amazonaws.com" && (
								<Box>
									EC2 instance profile:{" "}
									{profile ? values.Name || "Same name as role" : "Not created"}
								</Box>
							)}
						</SpaceBetween>
					)}
				</SpaceBetween>
			</Container>
		</SpaceBetween>
	);
	return (
		<ContentLayout
			header={
				<Header variant="h1" description={`Account ${account} · Global`}>
					Create {label}
				</Header>
			}
		>
			{kind === "user" || kind === "role" ? (
				<Wizard
					activeStepIndex={step}
					isLoadingNextStep={busy}
					submitButtonText={
						partial ? "Retry remaining steps" : `Create ${label}`
					}
					i18nStrings={{
						stepNumberLabel: (n) => `Step ${n}`,
						collapsedStepsLabel: (n, count) => `Step ${n} of ${count}`,
						navigationAriaLabel: "Creation steps",
						cancelButton: partial ? "Open resource" : "Cancel",
						previousButton: "Previous",
						nextButton: "Next",
					}}
					onCancel={() => {
						if (!busy) {
							if (partial) onCreated(values.Name);
							else onCancel();
						}
					}}
					onSubmit={submit}
					onNavigate={({ detail }) => {
						if (busy || partial) return;
						if (
							detail.requestedStepIndex > step &&
							!(step === 0
								? kind === "role"
									? validateTrust()
									: validateDetails()
								: validatePermissions())
						)
							return;
						setError("");
						setStep(detail.requestedStepIndex);
					}}
					steps={[
						{
							title:
								kind === "role"
									? "Select trusted entity"
									: "Specify user details",
							content: (
								<div inert={busy}>
									{kind === "role" ? trustedEntity : details}
								</div>
							),
							errorText: step === 0 ? error : undefined,
						},
						{
							title: "Set permissions",
							content: <div inert={busy}>{permissions}</div>,
							errorText: step === 1 ? error : undefined,
						},
						{
							title:
								kind === "role"
									? "Name, review, and create"
									: "Review and create",
							content: <div inert={busy}>{review}</div>,
							errorText: error,
						},
					]}
				/>
			) : (
				<Form
					errorText={error}
					actions={
						<SpaceBetween direction="horizontal" size="xs">
							<Button
								variant="link"
								disabled={busy}
								onClick={() => (partial ? onCreated(values.Name) : onCancel())}
							>
								{partial ? "Open resource" : "Cancel"}
							</Button>
							<Button variant="primary" loading={busy} onClick={submit}>
								{partial ? "Retry remaining steps" : `Create ${label}`}
							</Button>
						</SpaceBetween>
					}
				>
					<SpaceBetween size="l">
						{partial ? (
							<Alert type="warning">
								The resource was created. Retry to finish the remaining steps.
							</Alert>
						) : (
							<SpaceBetween size="l">
								{details}
								{kind === "group" && (
									<SpaceBetween size="l">
										<IAMSelection
											disabled={busy}
											title="Users"
											options={options("user")}
											selected={users}
											onChange={setUsers}
										/>
										{permissions}
									</SpaceBetween>
								)}
							</SpaceBetween>
						)}
					</SpaceBetween>
				</Form>
			)}
		</ContentLayout>
	);
}
