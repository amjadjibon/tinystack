import {
	Button,
	Header,
	Link,
	SpaceBetween,
	Table,
} from "@cloudscape-design/components";
import { useState } from "react";
import { EmptyState } from "#/components/EmptyState";
import type { Resource } from "#/lib/api";
import {
	emptyPolicy,
	type IAMKind,
	iamCall,
	iamEntity,
	iamGroupsForUser,
	iamHref,
	iamInput,
	iamSequence,
	policyText,
} from "#/lib/iam";
import { useFollow } from "#/lib/useFollow";
import type { IAMActions } from "./IAMForm";

type PolicyRow = {
	id: string;
	name: string;
	arn: string;
	type: string;
	owner: Resource;
	inherited: boolean;
};
export function IAMPermissions({
	resource,
	resources,
	enabled,
	run,
	open,
	onError,
}: IAMActions & { resources: Resource[]; onError: (error: string) => void }) {
	const entity = iamEntity[resource.type as IAMKind];
	const follow = useFollow();
	const [selected, setSelected] = useState<PolicyRow[]>([]);
	const attached = (resource.details?.AttachedPolicies as string[]) ?? [];
	const available = resources.filter(
		(r) => r.type === "policy" && !attached.includes(r.arn),
	);
	const owners = [
		resource,
		...(resource.type === "user"
			? iamGroupsForUser(resources, resource.name)
			: []),
	];
	const rows: PolicyRow[] = owners.flatMap((owner) => [
		...((owner.details?.AttachedPolicies as string[]) ?? []).map((arn) => ({
			id: `${owner.arn}:${arn}`,
			name:
				resources.find((r) => r.arn === arn)?.name ??
				arn.split("/").pop() ??
				arn,
			arn,
			type: "Customer managed",
			owner,
			inherited: owner !== resource,
		})),
		...((owner.details?.InlinePolicies as string[]) ?? []).map((name) => ({
			id: `${owner.arn}:${name}`,
			name,
			arn: "",
			type: "Inline",
			owner,
			inherited: owner !== resource,
		})),
	]);
	const editInline = async (name?: string, owner = resource) => {
		try {
			const result = name
				? await iamCall(
						resource.account,
						`Get${iamEntity[owner.type as IAMKind]}Policy`,
						{ ...iamInput(owner), PolicyName: name },
					)
				: undefined;
			const inherited = owner !== resource;
			open({
				title: inherited
					? "View group inline policy"
					: name
						? "Edit inline policy"
						: "Create inline policy",
				confirm: inherited ? "Close" : name ? "Save changes" : "Create policy",
				description: inherited
					? `Inherited from user group ${owner.name}. Edit this policy from the group permissions page.`
					: undefined,
				fields: [
					...(!name
						? [{ id: "Name", label: "Policy name", required: true }]
						: []),
					{
						id: "Document",
						label: "Permissions policy",
						type: "json",
						required: true,
						readOnly: inherited,
					},
				],
				values: {
					Name: name ?? "",
					Document: name ? policyText(result?.PolicyDocument) : emptyPolicy,
				},
				submit: async (values) => {
					if (!inherited)
						await run(`Put${entity}Policy`, {
							PolicyName: name ?? values.Name,
							PolicyDocument: values.Document,
						});
				},
			});
		} catch (err) {
			onError(err instanceof Error ? err.message : String(err));
		}
	};
	const remove = (targets: PolicyRow[]) => {
		const completed = new Set<string>();
		open({
			title: "Remove permissions",
			confirm: "Remove",
			description: `Detach managed policies and delete inline policies: ${targets.map((r) => r.name).join(", ")}.`,
			completed,
			submit: async () => {
				await iamSequence(
					targets.map((r) => ({
						id: r.id,
						label: `Remove ${r.name}`,
						run: () =>
							run(
								`${r.arn ? "Detach" : "Delete"}${entity}Policy`,
								r.arn ? { PolicyArn: r.arn } : { PolicyName: r.name },
							),
					})),
					completed,
				);
				setSelected([]);
			},
		});
	};
	return (
		<Table
			items={rows}
			trackBy="id"
			selectionType="multi"
			isItemDisabled={(r) => r.inherited}
			selectedItems={rows.filter((r) => selected.some((s) => s.id === r.id))}
			onSelectionChange={({ detail }) => setSelected(detail.selectedItems)}
			ariaLabels={{
				selectionGroupLabel: "Permissions policies",
				allItemsSelectionLabel: () => "Select all direct policies",
				itemSelectionLabel: (_, item) => `Select ${item.name}`,
			}}
			header={
				<Header
					variant="h2"
					counter={`(${rows.length})`}
					description={
						resource.type === "user"
							? "Direct policies and policies inherited from user groups. Manage inherited policies on the group page."
							: "Policies attached directly to this identity."
					}
					actions={
						<SpaceBetween direction="horizontal" size="xs">
							<Button
								disabled={!enabled || !selected.length}
								onClick={() => remove(selected)}
							>
								Remove
							</Button>
							<Button disabled={!enabled} onClick={() => editInline()}>
								Create inline policy
							</Button>
							<Button
								disabled={!enabled || !available.length}
								onClick={() => {
									const completed = new Set<string>();
									open({
										title: "Attach permissions policies",
										confirm: "Attach policies",
										completed,
										fields: [
											{
												id: "Policies",
												label: "Permissions policies",
												type: "multi",
												required: true,
												options: available.map((r) => ({
													label: r.name,
													value: r.arn,
													description: r.arn,
												})),
											},
										],
										submit: async (values) => {
											await iamSequence(
												(JSON.parse(values.Policies) as string[]).map(
													(arn) => ({
														id: arn,
														label: `Attach ${arn}`,
														run: () =>
															run(`Attach${entity}Policy`, { PolicyArn: arn }),
													}),
												),
												completed,
											);
										},
									});
								}}
							>
								Add permissions
							</Button>
						</SpaceBetween>
					}
				>
					Permissions policies
				</Header>
			}
			columnDefinitions={[
				{
					id: "name",
					header: "Policy name",
					cell: (r) =>
						r.arn ? (
							<Link
								href={iamHref("policy", resource.account, r.name)}
								onFollow={follow}
							>
								{r.name}
							</Link>
						) : (
							<Button
								variant="inline-link"
								disabled={!enabled}
								onClick={() => editInline(r.name, r.owner)}
							>
								{r.name}
							</Button>
						),
				},
				{ id: "type", header: "Type", cell: (r) => r.type },
				{
					id: "source",
					header: "Attached via",
					cell: (r) =>
						r.inherited ? (
							<Link
								href={iamHref("group", resource.account, r.owner.name)}
								onFollow={follow}
							>
								Group: {r.owner.name}
							</Link>
						) : (
							"Directly"
						),
				},
				{
					id: "actions",
					header: "Actions",
					cell: (r) =>
						r.inherited ? (
							<Link
								href={iamHref("group", resource.account, r.owner.name)}
								onFollow={follow}
							>
								Manage group
							</Link>
						) : (
							<Button disabled={!enabled} onClick={() => remove([r])}>
								{r.arn ? "Detach" : "Delete"}
							</Button>
						),
				},
			]}
			empty={
				<EmptyState
					title="No permissions policies"
					subtitle="Attach a customer managed policy or create an inline policy."
				/>
			}
		/>
	);
}
