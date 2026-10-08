import {
	Button,
	Header,
	SpaceBetween,
	Table,
} from "@cloudscape-design/components";
import { EmptyState } from "#/components/EmptyState";
import { type IAMKind, iamEntity } from "#/lib/iam";
import type { IAMActions } from "./IAMForm";
export function IAMTags({ resource, enabled, run, open }: IAMActions) {
	const entity = iamEntity[resource.type as IAMKind];
	const edit = (key?: string) =>
		open({
			title: key ? "Edit tag" : "Add tag",
			confirm: "Save tag",
			fields: [
				...(!key ? [{ id: "Key", label: "Key", required: true }] : []),
				{ id: "Value", label: "Value" },
			],
			values: {
				Key: key ?? "",
				Value: key ? (resource.tags?.[key] ?? "") : "",
			},
			submit: async (values) => {
				await run(`Tag${entity}`, {
					Tags: [{ Key: key ?? values.Key, Value: values.Value ?? "" }],
				});
			},
		});
	return (
		<Table
			items={Object.entries(resource.tags ?? {}).map(([key, value]) => ({
				key,
				value,
			}))}
			trackBy="key"
			header={
				<Header
					variant="h2"
					actions={
						<Button disabled={!enabled} onClick={() => edit()}>
							Add tag
						</Button>
					}
				>
					Tags
				</Header>
			}
			columnDefinitions={[
				{ id: "key", header: "Key", cell: (r) => r.key },
				{ id: "value", header: "Value", cell: (r) => r.value || "—" },
				{
					id: "actions",
					header: "Actions",
					cell: (r) => (
						<SpaceBetween direction="horizontal" size="xs">
							<Button disabled={!enabled} onClick={() => edit(r.key)}>
								Edit
							</Button>
							<Button
								disabled={!enabled}
								onClick={() =>
									open({
										title: "Remove tag",
										confirm: "Remove",
										description: `Remove the tag ${r.key}?`,
										submit: async () => {
											await run(`Untag${entity}`, { TagKeys: [r.key] });
										},
									})
								}
							>
								Remove
							</Button>
						</SpaceBetween>
					),
				},
			]}
			empty={
				<EmptyState
					title="No tags"
					subtitle="Add tags to organize this IAM resource."
				/>
			}
		/>
	);
}
