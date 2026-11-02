import { Alert, SpaceBetween } from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { type AppConfigScope, appconfigCall } from "#/lib/appconfig";
import {
	createEnvironmentInput,
	type Environment,
	formFromNamed,
	type NamedForm,
	updateEnvironmentInput,
} from "#/lib/appconfigHelpers";
import { NameFields } from "./Shared";

export function CreateEnvironmentDialog({
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
	const [form, setForm] = useState<NamedForm>({ name: "", description: "" });
	return (
		<ActionDialog
			title="Create environment"
			submit="Create environment"
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const input = createEnvironmentInput(applicationId, form);
				return [
					{
						label: "CreateEnvironment",
						run: () => appconfigCall(scope, "CreateEnvironment", input),
					},
				];
			}}
		>
			<SpaceBetween size="m">
				<Alert type="info">
					TinyStack does not watch deployments, so an environment takes no
					CloudWatch alarms.
				</Alert>
				<NameFields
					value={form}
					onChange={(patch) => setForm((f) => ({ ...f, ...patch }))}
				/>
			</SpaceBetween>
		</ActionDialog>
	);
}

export function EditEnvironmentDialog({
	scope,
	applicationId,
	environment,
	onClose,
	onSaved,
}: {
	scope: AppConfigScope;
	applicationId: string;
	environment: Environment;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [form, setForm] = useState(() => formFromNamed(environment));
	return (
		<ActionDialog
			title={`Edit ${environment.Name ?? environment.Id}`}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const input = updateEnvironmentInput(applicationId, environment, form);
				return [
					{
						label: "UpdateEnvironment",
						run: () => appconfigCall(scope, "UpdateEnvironment", input),
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
