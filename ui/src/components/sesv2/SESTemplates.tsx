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
import { CodeBlock } from "#/components/CodeBlock";
import { Action, Buttons, Rows } from "#/components/containers/Controls";
import { Text } from "#/components/memorydb/Fields";
import { Facts, ResourceTags } from "#/components/sesv2/Facts";
import { errorText } from "#/lib/consoleActions";
import { unixTime } from "#/lib/format";
import {
	getTemplate,
	resourceArn,
	type SESScope,
	sesv2Call,
	type TemplateMetadata,
} from "#/lib/sesv2";
import {
	createTemplateInput,
	emptyTemplate,
	parseTemplateData,
	type TemplateForm,
	templateForm,
	updateTemplateInput,
} from "#/lib/sesv2Helpers";
import { usePoll } from "#/lib/usePoll";

export function SESTemplates({
	scope,
	enabled,
	templates,
	loading,
	onChanged,
}: {
	scope: SESScope;
	enabled: boolean;
	templates: TemplateMetadata[];
	loading: boolean;
	onChanged: () => void;
}) {
	const [dialog, setDialog] = useState<"" | "create" | "update" | "delete">(""),
		[form, setForm] = useState<TemplateForm>(emptyTemplate),
		[selected, setSelected] = useState(""),
		[version, setVersion] = useState(0),
		[rendering, setRendering] = useState(""),
		[error, setError] = useState(""),
		[busy, setBusy] = useState(false);
	const patch = (change: Partial<TemplateForm>) =>
		setForm({ ...form, ...change });
	const edit = async (name: string) => {
		setBusy(true);
		setError("");
		try {
			const t = await getTemplate(scope, name);
			setForm(templateForm(name, t.TemplateContent));
			setDialog("update");
		} catch (err) {
			setError(errorText(err));
		} finally {
			setBusy(false);
		}
	};
	return (
		<SpaceBetween size="m">
			{!!error && <Alert type="error">{error}</Alert>}
			<Rows
				title="Templates"
				emptyText="Choose Create template to store a reusable subject and body."
				items={templates}
				loading={loading || busy}
				actions={
					<Buttons>
						<Action onClick={onChanged}>Refresh</Action>
						<Action
							disabled={!enabled}
							onClick={() => {
								setForm(emptyTemplate);
								setDialog("create");
							}}
						>
							Create template
						</Action>
					</Buttons>
				}
				columns={[
					{ id: "name", header: "Name", cell: (t) => t.TemplateName },
					{
						id: "created",
						header: "Created",
						cell: (t) => unixTime(t.CreatedTimestamp),
					},
					{
						id: "actions",
						header: "Actions",
						cell: (t) => (
							<Buttons>
								<Action onClick={() => setSelected(t.TemplateName ?? "")}>
									View
								</Action>
								<Action
									disabled={!enabled}
									onClick={() => edit(t.TemplateName ?? "")}
								>
									Edit
								</Action>
								<Action
									disabled={!enabled}
									onClick={() => setRendering(t.TemplateName ?? "")}
								>
									Test render
								</Action>
								<Action
									disabled={!enabled}
									onClick={() => {
										setForm({ ...emptyTemplate, name: t.TemplateName ?? "" });
										setDialog("delete");
									}}
								>
									Delete
								</Action>
							</Buttons>
						),
					},
				]}
			/>
			{!!selected && (
				<TemplateDetails
					key={`${selected}/${version}`}
					scope={scope}
					name={selected}
					enabled={enabled}
				/>
			)}
			{(dialog === "create" || dialog === "update") && (
				<ActionDialog
					title={dialog === "create" ? "Create template" : `Edit ${form.name}`}
					submit={dialog === "create" ? "Create template" : "Save template"}
					onClose={() => setDialog("")}
					onSaved={() => {
						setDialog("");
						setSelected(form.name);
						setVersion(version + 1);
						onChanged();
					}}
					plan={async () => {
						const create = dialog === "create";
						const operation = create
							? "CreateEmailTemplate"
							: "UpdateEmailTemplate";
						const input = create
							? createTemplateInput(form)
							: updateTemplateInput(form);
						return [
							{
								label: operation,
								run: () => sesv2Call(scope, operation, input),
							},
						];
					}}
				>
					<TemplateFields
						form={form}
						nameLocked={dialog === "update"}
						onChange={patch}
					/>
				</ActionDialog>
			)}
			{dialog === "delete" && (
				<ActionDialog
					title="Delete template"
					submit="Delete template"
					confirmation={form.name}
					onClose={() => setDialog("")}
					onSaved={() => {
						setDialog("");
						if (selected === form.name) setSelected("");
						onChanged();
					}}
					plan={async () => [
						{
							label: "DeleteEmailTemplate",
							run: () =>
								sesv2Call(scope, "DeleteEmailTemplate", {
									TemplateName: form.name,
								}),
						},
					]}
				>
					<Alert type="warning">
						Messages that name {form.name} can no longer be sent.
					</Alert>
				</ActionDialog>
			)}
			{!!rendering && (
				<TestRender
					scope={scope}
					name={rendering}
					onClose={() => setRendering("")}
				/>
			)}
		</SpaceBetween>
	);
}

function TemplateFields({
	form,
	nameLocked,
	onChange,
}: {
	form: TemplateForm;
	nameLocked: boolean;
	onChange: (change: Partial<TemplateForm>) => void;
}) {
	return (
		<SpaceBetween size="m">
			<Text
				label="Template name"
				value={form.name}
				disabled={nameLocked}
				onChange={(name) => onChange({ name })}
				constraint="1 to 64 letters, digits, hyphens or underscores."
			/>
			<Text
				label="Subject"
				value={form.subject}
				onChange={(subject) => onChange({ subject })}
				description="Use {{name}} placeholders for values supplied when sending."
			/>
			<FormField label="Text body">
				<Textarea
					ariaLabel="Text body"
					rows={5}
					value={form.text}
					onChange={({ detail }) => onChange({ text: detail.value })}
				/>
			</FormField>
			<FormField
				label="HTML body"
				description="At least one of the text and HTML bodies is required."
			>
				<Textarea
					ariaLabel="HTML body"
					rows={5}
					value={form.html}
					onChange={({ detail }) => onChange({ html: detail.value })}
				/>
			</FormField>
		</SpaceBetween>
	);
}

function TemplateDetails({
	scope,
	name,
	enabled,
}: {
	scope: SESScope;
	name: string;
	enabled: boolean;
}) {
	const poll = usePoll(() => getTemplate(scope, name), 0),
		c = poll.data?.TemplateContent;
	return (
		<SpaceBetween size="m">
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			<Facts
				title={name}
				actions={<Action onClick={poll.reload}>Refresh</Action>}
				facts={[["Subject", c?.Subject]]}
			/>
			{!!c?.Text && (
				<SpaceBetween size="xxs">
					<Box variant="awsui-key-label">Text body</Box>
					<CodeBlock text={c.Text} />
				</SpaceBetween>
			)}
			{!!c?.Html && (
				<SpaceBetween size="xxs">
					<Box variant="awsui-key-label">HTML body</Box>
					<CodeBlock text={c.Html} />
				</SpaceBetween>
			)}
			<ResourceTags
				scope={scope}
				arn={resourceArn(scope, "template", name)}
				enabled={enabled}
			/>
		</SpaceBetween>
	);
}

function TestRender({
	scope,
	name,
	onClose,
}: {
	scope: SESScope;
	name: string;
	onClose: () => void;
}) {
	const [data, setData] = useState("{}"),
		[output, setOutput] = useState(""),
		[error, setError] = useState(""),
		[busy, setBusy] = useState(false);
	const render = async () => {
		setBusy(true);
		setError("");
		setOutput("");
		try {
			const out = await sesv2Call<{ RenderedTemplate?: string }>(
				scope,
				"TestRenderEmailTemplate",
				{ TemplateName: name, TemplateData: parseTemplateData(data) },
			);
			setOutput(out.RenderedTemplate ?? "");
		} catch (err) {
			setError(errorText(err));
		} finally {
			setBusy(false);
		}
	};
	return (
		<Modal
			visible
			size="large"
			header={`Test render ${name}`}
			closeAriaLabel="Close dialog"
			onDismiss={onClose}
			footer={
				<Box float="right">
					<SpaceBetween direction="horizontal" size="xs">
						<Button onClick={onClose}>Close</Button>
						<Button variant="primary" loading={busy} onClick={render}>
							Render template
						</Button>
					</SpaceBetween>
				</Box>
			}
		>
			<SpaceBetween size="m">
				{!!error && <Alert type="error">{error}</Alert>}
				<FormField
					label="Template data (JSON)"
					description="Values for the placeholders, as a JSON object."
				>
					<Textarea
						ariaLabel="Template data"
						rows={6}
						value={data}
						onChange={({ detail }) => setData(detail.value)}
					/>
				</FormField>
				{!!output && <CodeBlock text={output} />}
			</SpaceBetween>
		</Modal>
	);
}
