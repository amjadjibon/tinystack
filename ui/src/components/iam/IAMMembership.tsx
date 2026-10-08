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
	type IAMKind,
	iamGroupsForUser,
	iamHref,
	iamSequence,
} from "#/lib/iam";
import { useFollow } from "#/lib/useFollow";
import type { IAMActions } from "./IAMForm";
export function IAMMembership({
	resource,
	resources,
	enabled,
	run,
	open,
}: IAMActions & { resources: Resource[] }) {
	const follow = useFollow();
	const [selected, setSelected] = useState<Resource[]>([]);
	const isUser = resource.type === "user";
	const members = isUser
		? iamGroupsForUser(resources, resource.name)
		: resources.filter(
				(r) =>
					r.type === "user" &&
					((resource.details?.Users as string[]) ?? []).some(
						(u) => u.toLowerCase() === r.name.toLowerCase(),
					),
			);
	const available = resources.filter(
		(r) => r.type === (isUser ? "group" : "user") && !members.includes(r),
	);
	const input = (name: string) =>
		isUser
			? { UserName: resource.name, GroupName: name }
			: { GroupName: resource.name, UserName: name };
	const remove = (targets: Resource[]) => {
		const completed = new Set<string>();
		open({
			title: isUser ? "Remove from groups" : "Remove users",
			confirm: "Remove",
			description: `Remove ${targets.map((r) => r.name).join(", ")}${isUser ? ` memberships for ${resource.name}` : ` from ${resource.name}`}?`,
			completed,
			submit: async () => {
				await iamSequence(
					targets.map((r) => ({
						id: r.arn,
						label: `Remove ${r.name}`,
						run: () => run("RemoveUserFromGroup", input(r.name)),
					})),
					completed,
				);
				setSelected([]);
			},
		});
	};
	return (
		<Table
			items={members}
			trackBy="arn"
			selectionType="multi"
			selectedItems={members.filter((r) =>
				selected.some((s) => s.arn === r.arn),
			)}
			onSelectionChange={({ detail }) => setSelected(detail.selectedItems)}
			ariaLabels={{
				selectionGroupLabel: isUser ? "User groups" : "Users",
				allItemsSelectionLabel: () => "Select all",
				itemSelectionLabel: (_, item) => `Select ${item.name}`,
			}}
			header={
				<Header
					variant="h2"
					counter={`(${members.length})`}
					actions={
						<SpaceBetween direction="horizontal" size="xs">
							<Button
								disabled={!enabled || !selected.length}
								onClick={() => remove(selected)}
							>
								Remove
							</Button>
							<Button
								disabled={!enabled || !available.length}
								onClick={() => {
									const completed = new Set<string>();
									open({
										title: isUser ? "Add user to groups" : "Add users",
										confirm: "Add",
										completed,
										fields: [
											{
												id: "Names",
												label: isUser ? "User groups" : "Users",
												type: "multi",
												required: true,
												options: available.map((r) => ({
													label: r.name,
													value: r.name,
													description: r.arn,
												})),
											},
										],
										submit: async (values) => {
											const names = JSON.parse(values.Names) as string[];
											await iamSequence(
												names.map((name) => ({
													id: name,
													label: `Add ${name}`,
													run: () => run("AddUserToGroup", input(name)),
												})),
												completed,
											);
										},
									});
								}}
							>
								{isUser ? "Add user to groups" : "Add users"}
							</Button>
						</SpaceBetween>
					}
				>
					{isUser ? "User groups" : "Users"}
				</Header>
			}
			columnDefinitions={[
				{
					id: "name",
					header: isUser ? "User group name" : "User name",
					cell: (r) => (
						<Link
							href={iamHref(r.type as IAMKind, resource.account, r.name)}
							onFollow={follow}
						>
							{r.name}
						</Link>
					),
				},
				{ id: "arn", header: "ARN", cell: (r) => r.arn },
				{
					id: "actions",
					header: "Actions",
					cell: (r) => (
						<Button disabled={!enabled} onClick={() => remove([r])}>
							Remove
						</Button>
					),
				},
			]}
			empty={
				<EmptyState
					title={isUser ? "No group memberships" : "No users"}
					subtitle={
						isUser
							? "Add this user to a user group."
							: "Add existing IAM users to this group."
					}
				/>
			}
		/>
	);
}
