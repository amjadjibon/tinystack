import {
	Alert,
	Checkbox,
	FormField,
	Input,
	Select,
	SpaceBetween,
	Textarea,
	Toggle,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { type HostedZone, type Route53Scope, route53Call } from "#/lib/route53";
import {
	bareZoneId,
	buildRecordSet,
	changeBatch,
	emptyRecordForm,
	type RecordForm,
	type RecordSet,
	recordFullName,
	recordToForm,
	recordTypes,
} from "#/lib/route53Helpers";

export function RecordDialog({
	scope,
	zone,
	record,
	onClose,
	onSaved,
}: {
	scope: Route53Scope;
	zone: HostedZone;
	record?: RecordSet;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [form, setForm] = useState<RecordForm>(
		record
			? recordToForm(record, zone.Name)
			: // Alias targets most often live in the same zone.
				{ ...emptyRecordForm, aliasZoneId: bareZoneId(zone.Id) },
	);
	const patch = (next: Partial<RecordForm>) =>
		setForm((previous) => ({ ...previous, ...next }));
	const label = record ? "Update record" : "Create record";
	return (
		<ActionDialog
			title={record ? "Edit record" : "Create record"}
			submit={label}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const next = buildRecordSet(form, zone.Name, record);
				return [
					{
						label,
						run: () =>
							route53Call(scope, "ChangeResourceRecordSets", {
								HostedZoneId: bareZoneId(zone.Id),
								ChangeBatch: changeBatch(record ? "UPSERT" : "CREATE", next),
							}),
					},
				];
			}}
		>
			<SpaceBetween size="m">
				<FormField
					label="Record name"
					description={`Relative to ${zone.Name} Leave empty or use @ for the zone apex.`}
					secondaryControl={
						<span>{`→ ${recordFullName(form.name, zone.Name)}`}</span>
					}
				>
					<Input
						ariaLabel="Record name"
						disabled={!!record}
						value={form.name}
						onChange={({ detail }) => patch({ name: detail.value })}
					/>
				</FormField>
				<FormField label="Record type">
					<Select
						ariaLabel="Record type"
						disabled={!!record}
						selectedOption={{ label: form.type, value: form.type }}
						options={recordTypes.map((value) => ({ label: value, value }))}
						onChange={({ detail }) =>
							patch({
								type: detail.selectedOption.value ?? "A",
								alias:
									detail.selectedOption.value === "NS" ? false : form.alias,
							})
						}
					/>
				</FormField>
				<Toggle
					checked={form.alias}
					disabled={form.type === "NS"}
					onChange={({ detail }) => patch({ alias: detail.checked })}
				>
					Alias
				</Toggle>
				{form.alias ? (
					<SpaceBetween size="m">
						<FormField
							label="Alias target DNS name"
							description="A name that exists in the target hosted zone."
						>
							<Input
								ariaLabel="Alias target DNS name"
								value={form.aliasDNSName}
								onChange={({ detail }) => patch({ aliasDNSName: detail.value })}
							/>
						</FormField>
						<FormField
							label="Alias target hosted zone ID"
							description="The hosted zone that holds the target name."
						>
							<Input
								ariaLabel="Alias target hosted zone ID"
								value={form.aliasZoneId}
								onChange={({ detail }) => patch({ aliasZoneId: detail.value })}
							/>
						</FormField>
						<Checkbox
							checked={form.evaluateTargetHealth}
							onChange={({ detail }) =>
								patch({ evaluateTargetHealth: detail.checked })
							}
						>
							Evaluate target health
						</Checkbox>
					</SpaceBetween>
				) : (
					<SpaceBetween size="m">
						<FormField
							label="Values"
							description="One value per line. Text values are quoted for you."
						>
							<Textarea
								ariaLabel="Record values"
								rows={5}
								value={form.values}
								onChange={({ detail }) => patch({ values: detail.value })}
							/>
						</FormField>
						<FormField label="TTL (seconds)">
							<Input
								ariaLabel="TTL"
								inputMode="numeric"
								value={form.ttl}
								onChange={({ detail }) => patch({ ttl: detail.value })}
							/>
						</FormField>
					</SpaceBetween>
				)}
				{!!record?.SetIdentifier && (
					<Alert type="info">
						This record uses the routing policy identified by{" "}
						<strong>{record.SetIdentifier}</strong>. Editing keeps the policy.
					</Alert>
				)}
			</SpaceBetween>
		</ActionDialog>
	);
}

export function DeleteRecordDialog({
	scope,
	zone,
	record,
	onClose,
	onSaved,
}: {
	scope: Route53Scope;
	zone: HostedZone;
	record: RecordSet;
	onClose: () => void;
	onSaved: () => void;
}) {
	return (
		<ActionDialog
			title="Delete record"
			submit="Delete record"
			confirmation={record.Name}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => [
				{
					label: "Delete record",
					run: () =>
						route53Call(scope, "ChangeResourceRecordSets", {
							HostedZoneId: bareZoneId(zone.Id),
							ChangeBatch: changeBatch("DELETE", record),
						}),
				},
			]}
		>
			<Alert type="warning" header="This cannot be undone">
				Delete the {record.Type} record set <strong>{record.Name}</strong>
			</Alert>
		</ActionDialog>
	);
}
