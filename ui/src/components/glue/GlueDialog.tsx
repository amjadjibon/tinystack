import { Alert, SpaceBetween } from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { Field } from "#/components/containers/Controls";
import { Text } from "#/components/memorydb/Fields";
import { type GlueScope, glueCall } from "#/lib/glue";
import {
	type GlueContext,
	type GlueKind,
	type GlueRecord,
	glueInput,
} from "#/lib/glueHelpers";
export function GlueDialog({
	scope,
	kind,
	context,
	initial,
	creating,
	onClose,
	onSaved,
}: {
	scope: GlueScope;
	kind: GlueKind;
	context: GlueContext;
	initial: GlueRecord;
	creating: boolean;
	onClose: () => void;
	onSaved: () => void;
}) {
	const commonFields = [
		"Name",
		"Description",
		"Role",
		"DatabaseName",
		"LocationUri",
	];
	const [fields, setFields] = useState(
		() =>
			Object.fromEntries(
				Object.entries(initial).filter(
					([k, v]) => commonFields.includes(k) && typeof v === "string",
				),
			) as Record<string, string>,
	);
	const [advanced, setAdvanced] = useState(() =>
		JSON.stringify(
			Object.fromEntries(
				Object.entries(initial).filter(([k]) => !(k in fields)),
			),
			null,
			2,
		),
	);
	const operation = `${creating ? "Create" : "Update"}${kind}`;
	return (
		<ActionDialog
			title={`${creating ? "Create" : "Edit"} ${kind.toLowerCase()}`}
			submit={creating ? "Create" : "Save changes"}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const parsed: unknown = JSON.parse(advanced);
				if (!parsed || typeof parsed !== "object" || Array.isArray(parsed))
					throw new Error("Configuration must be a JSON object.");
				const input = glueInput(
					kind,
					JSON.stringify({ ...parsed, ...fields }),
					creating,
					context,
					initial.Name,
				);
				return [
					{ label: operation, run: () => glueCall(scope, operation, input) },
				];
			}}
		>
			<SpaceBetween size="m">
				{Object.entries(fields).map(([key, value]) => (
					<Text
						key={key}
						label={key}
						disabled={key === "Name" && !creating}
						value={value}
						onChange={(v) => {
							if (key !== "Name" || creating)
								setFields((f) => ({ ...f, [key]: v }));
						}}
					/>
				))}
				<Alert type="info">
					{kind === "Table"
						? "Configure StorageDescriptor.Columns, Location, InputFormat, OutputFormat and SerdeInfo; PartitionKeys, TableType and Parameters below."
						: kind === "Job"
							? "Role must exist. Only Python shell jobs can execute; scripts use the local Python installation."
							: kind === "Crawler"
								? "Role, database and S3 target buckets must exist. Only S3 targets are supported."
								: "Names are fixed after creation. Configure additional fields below."}
				</Alert>
				<Field
					label="Additional configuration (JSON)"
					multiline
					value={advanced}
					onChange={setAdvanced}
				/>
			</SpaceBetween>
		</ActionDialog>
	);
}
