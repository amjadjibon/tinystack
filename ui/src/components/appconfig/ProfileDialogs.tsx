import {
	Alert,
	Button,
	FormField,
	SpaceBetween,
	Textarea,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { Choice } from "#/components/acm/Fields";
import { Text } from "#/components/memorydb/Fields";
import { type AppConfigScope, appconfigCall } from "#/lib/appconfig";
import {
	type ConfigurationProfile,
	createProfileInput,
	emptyProfileForm,
	formFromProfile,
	maxValidators,
	type ProfileForm,
	profileTypes,
	updateProfileInput,
	type ValidatorRow,
	validatorTypes,
} from "#/lib/appconfigHelpers";
import { NameFields } from "./Shared";

// ValidatorRows edits the validators of a profile, which the service keeps without running.
function ValidatorRows({
	rows,
	onChange,
}: {
	rows: ValidatorRow[];
	onChange: (rows: ValidatorRow[]) => void;
}) {
	const change = (i: number, patch: Partial<ValidatorRow>) =>
		onChange(rows.map((r, j) => (j === i ? { ...r, ...patch } : r)));
	return (
		<FormField
			label="Validators"
			description="Optional. TinyStack stores validators and does not run them."
		>
			<SpaceBetween size="m">
				{rows.map((row, i) => (
					// biome-ignore lint/suspicious/noArrayIndexKey: editable rows have no identity.
					<SpaceBetween key={`${i}`} size="xs">
						<Choice
							label={`Validator ${i + 1} type`}
							value={row.type}
							options={validatorTypes}
							onChange={(type) => change(i, { type })}
						/>
						<FormField
							label={`Validator ${i + 1} ${row.type === "LAMBDA" ? "Lambda function ARN" : "JSON schema"}`}
						>
							<Textarea
								rows={row.type === "LAMBDA" ? 1 : 6}
								value={row.content}
								onChange={({ detail }) => change(i, { content: detail.value })}
							/>
						</FormField>
						<Button
							ariaLabel={`Remove validator ${i + 1}`}
							onClick={() => onChange(rows.filter((_, j) => j !== i))}
						>
							Remove validator
						</Button>
					</SpaceBetween>
				))}
				<Button
					disabled={rows.length >= maxValidators}
					onClick={() =>
						onChange([...rows, { type: validatorTypes[0], content: "" }])
					}
				>
					Add validator
				</Button>
			</SpaceBetween>
		</FormField>
	);
}

export function CreateProfileDialog({
	scope,
	applicationId,
	onClose,
	onSaved,
}: {
	scope: AppConfigScope;
	applicationId: string;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [form, setForm] = useState(emptyProfileForm);
	const set = (patch: Partial<ProfileForm>) =>
		setForm((f) => ({ ...f, ...patch }));
	return (
		<ActionDialog
			title="Create configuration profile"
			submit="Create profile"
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const input = createProfileInput(applicationId, form);
				return [
					{
						label: "CreateConfigurationProfile",
						run: () =>
							appconfigCall(scope, "CreateConfigurationProfile", input),
					},
				];
			}}
		>
			<SpaceBetween size="m">
				<Alert type="info">
					Configuration is kept in the hosted configuration store. TinyStack
					does not read S3, Systems Manager or Secrets Manager sources, and a
					profile takes no retrieval role.
				</Alert>
				<NameFields value={form} onChange={set} />
				<Choice
					label="Configuration type"
					value={form.type}
					options={profileTypes}
					onChange={(type) => set({ type })}
					description="Fixed once the profile is created."
				/>
				<ValidatorRows
					rows={form.validators}
					onChange={(validators) => set({ validators })}
				/>
			</SpaceBetween>
		</ActionDialog>
	);
}

export function EditProfileDialog({
	scope,
	applicationId,
	profile,
	onClose,
	onSaved,
}: {
	scope: AppConfigScope;
	applicationId: string;
	profile: ConfigurationProfile;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [form, setForm] = useState(() => formFromProfile(profile));
	const set = (patch: Partial<ProfileForm>) =>
		setForm((f) => ({ ...f, ...patch }));
	return (
		<ActionDialog
			title={`Edit ${profile.Name ?? profile.Id}`}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const input = updateProfileInput(applicationId, profile, form);
				return [
					{
						label: "UpdateConfigurationProfile",
						run: () =>
							appconfigCall(scope, "UpdateConfigurationProfile", input),
					},
				];
			}}
		>
			<SpaceBetween size="m">
				<Alert type="info">
					The configuration type and the hosted source cannot be changed once a
					profile is created.
				</Alert>
				<NameFields value={form} onChange={set} />
				<Text
					label="Configuration type"
					value={form.type}
					disabled
					onChange={() => undefined}
				/>
				<ValidatorRows
					rows={form.validators}
					onChange={(validators) => set({ validators })}
				/>
			</SpaceBetween>
		</ActionDialog>
	);
}
