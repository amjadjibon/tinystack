import { Alert, SpaceBetween } from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { Action, Buttons, Rows } from "#/components/containers/Controls";
import { Pick, Text } from "#/components/memorydb/Fields";
import {
	type CloudFrontScope,
	cloudfrontCall,
	deleteOac,
	type OacSummary,
	oacForm,
	updateOac,
} from "#/lib/cloudfront";
import {
	emptyOacForm,
	type OacForm,
	oacConfig,
	oacOriginTypes,
	oacSigningBehaviors,
	oacSigningProtocols,
} from "#/lib/cloudfrontHelpers";
import { Copy } from "./Shared";

type Dialog =
	| { kind: "" }
	| { kind: "create" }
	| { kind: "edit" | "delete"; oac: OacSummary };

function OacFields({
	form,
	onChange,
}: {
	form: OacForm;
	onChange: (patch: Partial<OacForm>) => void;
}) {
	return (
		<SpaceBetween size="m">
			<Text
				label="Name"
				value={form.name}
				onChange={(name) => onChange({ name })}
				constraint="1 to 64 characters."
			/>
			<Text
				label="Description"
				value={form.description}
				onChange={(description) => onChange({ description })}
				constraint="Optional. Up to 256 characters."
			/>
			<Pick
				label="Origin type"
				value={form.originType}
				placeholder="Choose an origin type"
				options={oacOriginTypes.map((value) => ({ value }))}
				onChange={(originType) => onChange({ originType })}
			/>
			<Pick
				label="Signing behavior"
				value={form.signingBehavior}
				placeholder="Choose a signing behavior"
				description="always signs every request; never sends requests unsigned; no-override signs only requests that have no Authorization header."
				options={oacSigningBehaviors.map((value) => ({ value }))}
				onChange={(signingBehavior) => onChange({ signingBehavior })}
			/>
			<Pick
				label="Signing protocol"
				value={form.signingProtocol}
				placeholder="Choose a signing protocol"
				options={oacSigningProtocols.map((value) => ({ value }))}
				onChange={(signingProtocol) => onChange({ signingProtocol })}
			/>
		</SpaceBetween>
	);
}

export function OriginAccessControls({
	scope,
	enabled,
	items,
	loading,
	error,
	onChanged,
}: {
	scope: CloudFrontScope;
	enabled: boolean;
	items: OacSummary[];
	loading: boolean;
	error?: string;
	onChanged: () => void;
}) {
	const [dialog, setDialog] = useState<Dialog>({ kind: "" });
	const [form, setForm] = useState<OacForm>(emptyOacForm);
	const patch = (change: Partial<OacForm>) =>
		setForm((f) => ({ ...f, ...change }));
	const close = () => setDialog({ kind: "" });
	const saved = () => {
		close();
		onChanged();
	};
	const open = (next: Dialog) => {
		setForm(next.kind === "edit" ? oacForm(next.oac) : emptyOacForm);
		setDialog(next);
	};
	return (
		<SpaceBetween size="m">
			{!!error && <Alert type="error">{error}</Alert>}
			<Rows
				title="Origin access controls"
				emptyText="Choose Create origin access control to add one."
				items={items}
				loading={loading}
				actions={
					<Buttons>
						<Action onClick={onChanged}>Refresh</Action>
						<Action
							disabled={!enabled}
							onClick={() => open({ kind: "create" })}
						>
							Create origin access control
						</Action>
					</Buttons>
				}
				columns={[
					{ id: "name", header: "Name", cell: (o) => o.Name ?? "-" },
					{
						id: "id",
						header: "ID",
						cell: (o) => <Copy text={o.Id} label="ID" />,
					},
					{
						id: "type",
						header: "Origin type",
						cell: (o) => o.OriginAccessControlOriginType ?? "-",
					},
					{
						id: "behavior",
						header: "Signing behavior",
						cell: (o) => o.SigningBehavior ?? "-",
					},
					{
						id: "description",
						header: "Description",
						cell: (o) => o.Description || "-",
					},
					{
						id: "actions",
						header: "Actions",
						minWidth: 230,
						cell: (o) => (
							<Buttons>
								<Action
									disabled={!enabled}
									onClick={() => open({ kind: "edit", oac: o })}
								>
									Edit
								</Action>
								<Action
									disabled={!enabled}
									onClick={() => open({ kind: "delete", oac: o })}
								>
									Delete
								</Action>
							</Buttons>
						),
					},
				]}
			/>
			{dialog.kind === "create" && (
				<ActionDialog
					title="Create origin access control"
					submit="Create origin access control"
					onClose={close}
					onSaved={saved}
					plan={async () => {
						const config = oacConfig(form);
						return [
							{
								label: "CreateOriginAccessControl",
								run: () =>
									cloudfrontCall(scope, "CreateOriginAccessControl", {
										OriginAccessControlConfig: config,
									}),
							},
						];
					}}
				>
					<OacFields form={form} onChange={patch} />
				</ActionDialog>
			)}
			{dialog.kind === "edit" && (
				<ActionDialog
					title={`Edit ${dialog.oac.Name ?? dialog.oac.Id}`}
					onClose={close}
					onSaved={saved}
					plan={async () => {
						const config = oacConfig(form);
						const id = dialog.oac.Id;
						return [
							{
								label: "UpdateOriginAccessControl",
								run: () => updateOac(scope, id, config),
							},
						];
					}}
				>
					<OacFields form={form} onChange={patch} />
				</ActionDialog>
			)}
			{dialog.kind === "delete" && (
				<ActionDialog
					title="Delete origin access control"
					submit="Delete origin access control"
					confirmation={dialog.oac.Name ?? dialog.oac.Id}
					onClose={close}
					onSaved={saved}
					plan={async () => {
						const id = dialog.oac.Id;
						return [
							{
								label: "DeleteOriginAccessControl",
								run: () => deleteOac(scope, id),
							},
						];
					}}
				>
					<Alert type="warning">
						Delete {dialog.oac.Name ?? dialog.oac.Id}. An origin access control
						that a distribution's origin uses cannot be deleted; remove it from
						that origin first.
					</Alert>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
