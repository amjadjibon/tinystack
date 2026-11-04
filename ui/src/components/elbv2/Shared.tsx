import {
	Alert,
	Box,
	Button,
	ColumnLayout,
	FormField,
	Input,
	SpaceBetween,
	Spinner,
} from "@cloudscape-design/components";
import type { ReactNode } from "react";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { ConsoleTags } from "#/components/ConsoleTags";
import {
	type Attribute,
	describeAttributes,
	describeTags,
	type ELBv2Scope,
	elbv2Call,
} from "#/lib/elbv2";
import { attributeChanges } from "#/lib/elbv2Helpers";
import { tagList } from "#/lib/tags";
import { usePoll } from "#/lib/usePoll";

export function Details({ items }: { items: [string, ReactNode][] }) {
	return (
		<ColumnLayout columns={3} variant="text-grid">
			{items.map(([label, value]) => (
				<div key={label}>
					<Box variant="awsui-key-label">{label}</Box>
					<div>{value || "-"}</div>
				</div>
			))}
		</ColumnLayout>
	);
}

export function shortArn(arn: string) {
	return arn.split("/").slice(1, 2).join("") || arn;
}

// TagRowsField edits a list of key and value pairs in a create dialog.
export function TagRowsField({
	rows,
	onChange,
}: {
	rows: { key: string; value: string }[];
	onChange: (rows: { key: string; value: string }[]) => void;
}) {
	return (
		<FormField label="Tags" description="Optional keys and values.">
			<SpaceBetween size="xs">
				{rows.map((row, i) => (
					// biome-ignore lint/suspicious/noArrayIndexKey: editable rows have no identity.
					<SpaceBetween key={`${i}`} direction="horizontal" size="xs">
						<Input
							ariaLabel={`Tag key ${i + 1}`}
							placeholder="Key"
							value={row.key}
							onChange={({ detail }) =>
								onChange(
									rows.map((r, j) =>
										j === i ? { ...r, key: detail.value } : r,
									),
								)
							}
						/>
						<Input
							ariaLabel={`Tag value ${i + 1}`}
							placeholder="Value"
							value={row.value}
							onChange={({ detail }) =>
								onChange(
									rows.map((r, j) =>
										j === i ? { ...r, value: detail.value } : r,
									),
								)
							}
						/>
						<Button onClick={() => onChange(rows.filter((_, j) => j !== i))}>
							Remove tag
						</Button>
					</SpaceBetween>
				))}
				<Button onClick={() => onChange([...rows, { key: "", value: "" }])}>
					Add tag
				</Button>
			</SpaceBetween>
		</FormField>
	);
}

// AttributesDialog edits the attributes the service reports for a resource.
export function AttributesDialog({
	scope,
	title,
	arn,
	idKey,
	describe,
	modify,
	onClose,
	onSaved,
}: {
	scope: ELBv2Scope;
	title: string;
	arn: string;
	idKey: string;
	describe: string;
	modify: string;
	onClose: () => void;
	onSaved: () => void;
}) {
	const attributes = usePoll<Attribute[]>(
		() => describeAttributes(scope, describe, idKey, arn),
		0,
	);
	const [edited, setEdited] = useState<Record<string, string>>({});
	const original = attributes.data ?? [];
	return (
		<ActionDialog
			title={title}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const changes = attributeChanges(original, edited);
				if (!changes.length) throw new Error("No attribute was changed.");
				return [
					{
						label: modify,
						run: () =>
							elbv2Call(scope, modify, { [idKey]: arn, Attributes: changes }),
					},
				];
			}}
		>
			<SpaceBetween size="s">
				{!!attributes.error && <Alert type="error">{attributes.error}</Alert>}
				{attributes.loading && <Spinner />}
				{original.map((a) => (
					<FormField key={a.Key} label={a.Key}>
						<Input
							ariaLabel={a.Key}
							value={edited[a.Key] ?? a.Value ?? ""}
							onChange={({ detail }) =>
								setEdited((e) => ({ ...e, [a.Key]: detail.value }))
							}
						/>
					</FormField>
				))}
			</SpaceBetween>
		</ActionDialog>
	);
}

export function ResourceTags({
	scope,
	arn,
	enabled,
}: {
	scope: ELBv2Scope;
	arn: string;
	enabled: boolean;
}) {
	const tags = usePoll(() => describeTags(scope, arn), 0);
	return (
		<>
			{!!tags.error && <Alert type="error">{tags.error}</Alert>}
			<ConsoleTags
				tags={tags.data ?? {}}
				enabled={enabled}
				onChanged={tags.reload}
				plan={(next, removed) => [
					...(Object.keys(next).length
						? [
								{
									label: "AddTags",
									run: () =>
										elbv2Call(scope, "AddTags", {
											ResourceArns: [arn],
											Tags: tagList(next),
										}),
								},
							]
						: []),
					...(removed.length
						? [
								{
									label: "RemoveTags",
									run: () =>
										elbv2Call(scope, "RemoveTags", {
											ResourceArns: [arn],
											TagKeys: removed,
										}),
								},
							]
						: []),
				]}
			/>
		</>
	);
}
