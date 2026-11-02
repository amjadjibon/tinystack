import {
	Alert,
	Box,
	ColumnLayout,
	Container,
	FormField,
	Header,
	SpaceBetween,
	Textarea,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { ConsoleTags } from "#/components/ConsoleTags";
import { Action, Buttons, Rows } from "#/components/containers/Controls";
import { Text } from "#/components/memorydb/Fields";
import { stringMap } from "#/lib/consoleActions";
import { tagList, tagMap } from "#/lib/memorydbHelpers";
import {
	type ScheduleGroup,
	type SchedulerScope,
	schedulerCall,
} from "#/lib/scheduler";
import { defaultGroup, formatDate, validName } from "#/lib/schedulerHelpers";
import { usePoll } from "#/lib/usePoll";

type Dialog = "" | "create" | "delete";

export function SchedulerGroups({
	scope,
	enabled,
	groups,
	loading,
	onChanged,
}: {
	scope: SchedulerScope;
	enabled: boolean;
	groups: ScheduleGroup[];
	loading: boolean;
	onChanged: () => void;
}) {
	const [dialog, setDialog] = useState<Dialog>(""),
		[target, setTarget] = useState<ScheduleGroup>(),
		[viewing, setViewing] = useState<string>(),
		[name, setName] = useState(""),
		[tags, setTags] = useState("{}");
	const viewed = groups.find((g) => g.Name === viewing);
	return (
		<SpaceBetween size="m">
			<Rows
				title="Schedule groups"
				emptyText="Choose Create schedule group to add one. Schedules without a group use the default group."
				items={groups}
				loading={loading}
				actions={
					<Buttons>
						<Action onClick={onChanged}>Refresh</Action>
						<Action
							disabled={!enabled}
							onClick={() => {
								setName("");
								setTags("{}");
								setDialog("create");
							}}
						>
							Create schedule group
						</Action>
					</Buttons>
				}
				columns={[
					{ id: "name", header: "Name", cell: (g) => g.Name },
					{ id: "state", header: "State", cell: (g) => g.State },
					{
						id: "created",
						header: "Created",
						cell: (g) => formatDate(g.CreationDate),
					},
					{
						id: "actions",
						header: "Actions",
						cell: (g) => (
							<Buttons>
								<Action onClick={() => setViewing(g.Name)}>View</Action>
								<Action
									disabled={!enabled || g.Name === defaultGroup}
									onClick={() => {
										setTarget(g);
										setDialog("delete");
									}}
								>
									Delete
								</Action>
							</Buttons>
						),
					},
				]}
			/>
			{viewed && (
				<GroupDetail
					key={viewed.Name}
					scope={scope}
					enabled={enabled}
					group={viewed}
				/>
			)}
			{dialog === "create" && (
				<ActionDialog
					title="Create schedule group"
					submit="Create schedule group"
					onClose={() => setDialog("")}
					onSaved={() => {
						setDialog("");
						onChanged();
					}}
					plan={async () => {
						const groupName = validName(name.trim(), "group");
						if (groupName === defaultGroup)
							throw new Error("The default schedule group already exists.");
						const parsed = stringMap(tags, "Tags");
						return [
							{
								label: "CreateScheduleGroup",
								run: () =>
									schedulerCall(scope, "CreateScheduleGroup", {
										Name: groupName,
										...(Object.keys(parsed).length
											? { Tags: tagList(parsed) }
											: {}),
									}),
							},
						];
					}}
				>
					<SpaceBetween size="m">
						<Text
							label="Group name"
							value={name}
							onChange={setName}
							constraint="1 to 64 letters, digits, hyphens, underscores and periods."
						/>
						<FormField
							label="Tags (JSON)"
							description='An object of string values, such as {"env": "dev"}.'
						>
							<Textarea
								ariaLabel="Tags (JSON)"
								rows={4}
								value={tags}
								onChange={({ detail }) => setTags(detail.value)}
							/>
						</FormField>
					</SpaceBetween>
				</ActionDialog>
			)}
			{dialog === "delete" && target && (
				<ActionDialog
					title="Delete schedule group"
					submit="Delete schedule group"
					confirmation={target.Name}
					onClose={() => setDialog("")}
					onSaved={() => {
						setDialog("");
						if (viewing === target.Name) setViewing(undefined);
						onChanged();
					}}
					plan={async () => [
						{
							label: "DeleteScheduleGroup",
							run: () =>
								schedulerCall(scope, "DeleteScheduleGroup", {
									Name: target.Name,
								}),
						},
					]}
				>
					<Alert type="warning" header="Every schedule in the group is deleted">
						Deleting {target.Name} also deletes all of its schedules at once.
						This cannot be undone.
					</Alert>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}

function GroupDetail({
	scope,
	enabled,
	group,
}: {
	scope: SchedulerScope;
	enabled: boolean;
	group: ScheduleGroup;
}) {
	const isDefault = group.Name === defaultGroup;
	const poll = usePoll(async () => {
		if (!enabled || isDefault) return {};
		const out = await schedulerCall<{
			Tags?: { Key?: string; Value?: string }[];
		}>(scope, "ListTagsForResource", { ResourceArn: group.Arn });
		return tagMap(out.Tags);
	}, 0);
	return (
		<SpaceBetween size="m">
			<Container header={<Header variant="h2">{group.Name}</Header>}>
				<ColumnLayout columns={2} variant="text-grid">
					<div>
						<Box variant="awsui-key-label">ARN</Box>
						<Box>{group.Arn}</Box>
					</div>
					<div>
						<Box variant="awsui-key-label">State</Box>
						<Box>{group.State}</Box>
					</div>
					<div>
						<Box variant="awsui-key-label">Created</Box>
						<Box>{formatDate(group.CreationDate)}</Box>
					</div>
					<div>
						<Box variant="awsui-key-label">Last modified</Box>
						<Box>{formatDate(group.LastModificationDate)}</Box>
					</div>
				</ColumnLayout>
			</Container>
			{poll.error && <Alert type="error">{poll.error}</Alert>}
			{isDefault ? (
				<Alert type="info">
					The default schedule group cannot be tagged or deleted.
				</Alert>
			) : (
				<ConsoleTags
					tags={poll.data ?? {}}
					enabled={enabled}
					onChanged={poll.reload}
					plan={(next, removed) => [
						...(Object.keys(next).length
							? [
									{
										label: "TagResource",
										run: () =>
											schedulerCall(scope, "TagResource", {
												ResourceArn: group.Arn,
												Tags: tagList(next),
											}),
									},
								]
							: []),
						...(removed.length
							? [
									{
										label: "UntagResource",
										run: () =>
											schedulerCall(scope, "UntagResource", {
												ResourceArn: group.Arn,
												TagKeys: removed,
											}),
									},
								]
							: []),
					]}
				/>
			)}
		</SpaceBetween>
	);
}
