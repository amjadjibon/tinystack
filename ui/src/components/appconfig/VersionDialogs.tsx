import {
	Alert,
	Box,
	Button,
	FormField,
	Modal,
	SpaceBetween,
	Textarea,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { Choice } from "#/components/acm/Fields";
import { CodeBlock } from "#/components/CodeBlock";
import { Details } from "#/components/elbv2/Shared";
import { Text } from "#/components/memorydb/Fields";
import {
	type AppConfigScope,
	appconfigCall,
	getVersion,
} from "#/lib/appconfig";
import {
	type Application,
	bytesText,
	type ConfigurationProfile,
	contentTypeChoices,
	createVersionInput,
	decodeContent,
	formFromVersion,
	type VersionForm,
	versionText,
} from "#/lib/appconfigHelpers";
import { usePoll } from "#/lib/usePoll";

export function CreateVersionDialog({
	scope,
	application,
	profile,
	initial,
	onClose,
	onSaved,
}: {
	scope: AppConfigScope;
	application: Application;
	profile: ConfigurationProfile;
	initial: VersionForm;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [form, setForm] = useState(initial);
	const set = (patch: Partial<VersionForm>) =>
		setForm((f) => ({ ...f, ...patch }));
	return (
		<ActionDialog
			title="Create hosted configuration version"
			submit="Create version"
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const input = createVersionInput(application.Id, profile.Id, form);
				return [
					{
						label: "CreateHostedConfigurationVersion",
						run: () =>
							appconfigCall(scope, "CreateHostedConfigurationVersion", input),
					},
				];
			}}
		>
			<SpaceBetween size="m">
				<Alert type="info">
					A version cannot be changed once it is created. Save a change as a new
					version, then deploy it. JSON content is checked before it is saved;
					YAML and text are kept as typed.
				</Alert>
				<Choice
					label="Content type"
					value={form.contentType}
					options={contentTypeChoices(initial.contentType)}
					onChange={(contentType) => set({ contentType })}
				/>
				<FormField
					label="Content"
					description={`The configuration of profile ${profile.Name ?? profile.Id}.`}
					constraintText="Up to 1 MiB."
				>
					<Textarea
						ariaLabel="Configuration content"
						rows={14}
						value={form.content}
						onChange={({ detail }) => set({ content: detail.value })}
					/>
				</FormField>
				<Text
					label="Version label"
					value={form.label}
					onChange={(label) => set({ label })}
					constraint="Optional, up to 64 characters with at least one that is not a digit. Unique within the profile."
				/>
				<Text
					label="Description"
					value={form.description}
					onChange={(description) => set({ description })}
					constraint="Optional, up to 1024 characters."
				/>
			</SpaceBetween>
		</ActionDialog>
	);
}

export function ViewVersionDialog({
	scope,
	application,
	profile,
	number,
	onClose,
	onCopy,
}: {
	scope: AppConfigScope;
	application: string;
	profile: string;
	number: number;
	onClose: () => void;
	// onCopy opens the editor with this version's content, for a new version.
	onCopy: (form: VersionForm) => void;
}) {
	const poll = usePoll(
		() => getVersion(scope, application, profile, number),
		0,
	);
	const v = poll.data;
	const content = v ? decodeContent(v.Content) : undefined;
	return (
		<Modal
			visible
			size="large"
			header={`Version ${versionText(number, v?.VersionLabel)}`}
			closeAriaLabel="Close version"
			onDismiss={onClose}
			footer={
				<Box float="right">
					<SpaceBetween direction="horizontal" size="xs">
						<Button onClick={onClose}>Close</Button>
						{v && content?.text !== undefined && (
							<Button
								variant="primary"
								onClick={() => onCopy(formFromVersion(v, content.text ?? ""))}
							>
								Create new version from this
							</Button>
						)}
					</SpaceBetween>
				</Box>
			}
		>
			<SpaceBetween size="m">
				{!!poll.error && <Alert type="error">{poll.error}</Alert>}
				{!v && !poll.error && <Box>Loading version…</Box>}
				{v && content && (
					<SpaceBetween size="m">
						<Details
							items={[
								["Version", String(v.VersionNumber)],
								["Label", v.VersionLabel],
								["Content type", v.ContentType],
								["Size", bytesText(content.size)],
								["Description", v.Description],
							]}
						/>
						{content.text === undefined && (
							<Alert type="info">
								This content is not text, so it is shown as base64.
							</Alert>
						)}
						<CodeBlock text={content.text ?? v.Content ?? ""} />
					</SpaceBetween>
				)}
			</SpaceBetween>
		</Modal>
	);
}
