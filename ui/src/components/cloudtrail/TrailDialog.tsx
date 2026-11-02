import { SpaceBetween } from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { Field } from "#/components/containers/Controls";
import { type CloudTrailScope, cloudtrailCall } from "#/lib/cloudtrail";
import { type TrailForm, trailInput } from "#/lib/cloudtrailHelpers";
export function TrailDialog({
	scope,
	initial,
	creating,
	onClose,
	onSaved,
}: {
	scope: CloudTrailScope;
	initial: TrailForm;
	creating: boolean;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [form, setForm] = useState(initial);
	const operation = creating ? "CreateTrail" : "UpdateTrail";
	return (
		<ActionDialog
			title={creating ? "Create trail" : `Edit ${initial.name}`}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const input = trailInput(form, creating);
				return [
					{
						label: operation,
						run: () => cloudtrailCall(scope, operation, input),
					},
				];
			}}
		>
			<SpaceBetween size="m">
				{creating ? (
					<Field
						label="Name"
						value={form.name}
						onChange={(name) => setForm((f) => ({ ...f, name }))}
					/>
				) : (
					<div>{form.name}</div>
				)}
				<Field
					label="S3 bucket (must already exist)"
					value={form.bucket}
					onChange={(bucket) => setForm((f) => ({ ...f, bucket }))}
				/>
				<Field
					label="S3 key prefix"
					value={form.prefix}
					onChange={(prefix) => setForm((f) => ({ ...f, prefix }))}
				/>
				{creating && (
					<Field
						label="Tags (JSON object of string values)"
						multiline
						value={form.tags}
						onChange={(tags) => setForm((f) => ({ ...f, tags }))}
					/>
				)}
			</SpaceBetween>
		</ActionDialog>
	);
}
