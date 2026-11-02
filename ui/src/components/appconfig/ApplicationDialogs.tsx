import { SpaceBetween } from "@cloudscape-design/components";
import { useRef, useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { TagRowsField } from "#/components/elbv2/Shared";
import { type AppConfigScope, appconfigCall } from "#/lib/appconfig";
import {
	type Application,
	type ApplicationForm,
	createApplicationInput,
	emptyApplicationForm,
	formFromNamed,
	updateApplicationInput,
} from "#/lib/appconfigHelpers";
import { NameFields } from "./Shared";

export function CreateApplicationDialog({
	scope,
	onClose,
	onSaved,
}: {
	scope: AppConfigScope;
	onClose: () => void;
	onSaved: (id: string) => void;
}) {
	const [form, setForm] = useState(emptyApplicationForm);
	const created = useRef("");
	const set = (patch: Partial<ApplicationForm>) =>
		setForm((f) => ({ ...f, ...patch }));
	return (
		<ActionDialog
			title="Create application"
			submit="Create application"
			onClose={onClose}
			onSaved={() => onSaved(created.current)}
			plan={async () => {
				const input = createApplicationInput(form);
				return [
					{
						label: "CreateApplication",
						run: async () => {
							const out = await appconfigCall<Application>(
								scope,
								"CreateApplication",
								input,
							);
							created.current = out.Id;
						},
					},
				];
			}}
		>
			<SpaceBetween size="m">
				<NameFields value={form} onChange={set} />
				<TagRowsField rows={form.tags} onChange={(tags) => set({ tags })} />
			</SpaceBetween>
		</ActionDialog>
	);
}

export function EditApplicationDialog({
	scope,
	application,
	onClose,
	onSaved,
}: {
	scope: AppConfigScope;
	application: Application;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [form, setForm] = useState(() => formFromNamed(application));
	return (
		<ActionDialog
			title={`Edit ${application.Name ?? application.Id}`}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const input = updateApplicationInput(application, form);
				return [
					{
						label: "UpdateApplication",
						run: () => appconfigCall(scope, "UpdateApplication", input),
					},
				];
			}}
		>
			<NameFields
				value={form}
				onChange={(patch) => setForm((f) => ({ ...f, ...patch }))}
			/>
		</ActionDialog>
	);
}
