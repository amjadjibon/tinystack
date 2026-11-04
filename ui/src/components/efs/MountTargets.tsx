import {
	Alert,
	Button,
	SpaceBetween,
	StatusIndicator,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { Action, Buttons, Rows } from "#/components/containers/Controls";
import { Pick, PickMany, Text } from "#/components/memorydb/Fields";
import { type EFSScope, efsCall, listMountTargets } from "#/lib/efs";
import {
	availableSubnets,
	createMountTargetInput,
	emptyMountTargetForm,
	groupLabel,
	lifeCycleStatusType,
	type MountTargetRow,
	modifyGroupsInput,
	subnetLabel,
} from "#/lib/efsHelpers";
import { listSecurityGroups, listSubnets } from "#/lib/elbv2";
import { usePoll } from "#/lib/usePoll";

type Dialog =
	| { kind: "" }
	| { kind: "create" }
	| { kind: "groups" | "delete"; target: MountTargetRow };

function CreateMountTargetDialog({
	scope,
	fileSystem,
	existing,
	onClose,
	onSaved,
}: {
	scope: EFSScope;
	fileSystem: string;
	existing: MountTargetRow[];
	onClose: () => void;
	onSaved: () => void;
}) {
	const options = usePoll(async () => {
		const [subnets, groups] = await Promise.all([
			listSubnets(scope),
			listSecurityGroups(scope),
		]);
		return { subnets, groups };
	}, 0);
	const [form, setForm] = useState(emptyMountTargetForm);
	const subnets = availableSubnets(options.data?.subnets ?? [], existing);
	// Security groups must belong to the VPC of the chosen subnet.
	const vpc = subnets.find((s) => s.SubnetId === form.subnet)?.VpcId;
	const groups = (options.data?.groups ?? []).filter((g) => g.VpcId === vpc);
	return (
		<ActionDialog
			title="Create mount target"
			submit="Create mount target"
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const input = createMountTargetInput(fileSystem, form);
				return [
					{
						label: "CreateMountTarget",
						run: () => efsCall(scope, "CreateMountTarget", input),
					},
				];
			}}
		>
			<SpaceBetween size="m">
				{!!options.error && <Alert type="error">{options.error}</Alert>}
				{options.data && subnets.length === 0 && (
					<Alert type="warning">
						No subnet is available. A file system has at most one mount target
						in each Availability Zone, all in one VPC. Create an EC2 subnet in
						another zone first.
					</Alert>
				)}
				<Pick
					label="Subnet"
					value={form.subnet}
					placeholder="Choose a subnet"
					description="Subnets in a zone or VPC that this file system cannot use are not listed."
					options={subnets.map((s) => ({
						value: s.SubnetId ?? "",
						label: subnetLabel(s),
					}))}
					onChange={(subnet) => setForm((f) => ({ ...f, subnet, groups: [] }))}
				/>
				<Text
					label="IP address"
					value={form.address}
					onChange={(address) => setForm((f) => ({ ...f, address }))}
					constraint="Optional. An unused IPv4 address in the subnet; TinyStack picks one when empty."
				/>
				{!!form.subnet && (
					<PickMany
						label="Security groups"
						values={form.groups}
						placeholder="Choose security groups"
						description="Optional, at most five, from the subnet's VPC. The VPC's default group applies when none is chosen."
						options={groups.map((g) => ({
							value: g.GroupId ?? "",
							label: groupLabel(g),
						}))}
						onChange={(selected) =>
							setForm((f) => ({ ...f, groups: selected }))
						}
					/>
				)}
			</SpaceBetween>
		</ActionDialog>
	);
}

function SecurityGroupsDialog({
	scope,
	target,
	onClose,
	onSaved,
}: {
	scope: EFSScope;
	target: MountTargetRow;
	onClose: () => void;
	onSaved: () => void;
}) {
	const groups = usePoll(() => listSecurityGroups(scope), 0);
	const [selected, setSelected] = useState(target.SecurityGroups);
	const known = (groups.data ?? []).filter((g) => g.VpcId === target.VpcId);
	return (
		<ActionDialog
			title={`Security groups of ${target.MountTargetId}`}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const input = modifyGroupsInput(target.MountTargetId, selected);
				return [
					{
						label: "ModifyMountTargetSecurityGroups",
						run: () => efsCall(scope, "ModifyMountTargetSecurityGroups", input),
					},
				];
			}}
		>
			<SpaceBetween size="m">
				{!!groups.error && <Alert type="error">{groups.error}</Alert>}
				<PickMany
					label="Security groups"
					values={selected}
					placeholder="Choose security groups"
					description={`At most five, from the VPC ${target.VpcId ?? ""}. Saving replaces the current groups; an empty list removes them all.`}
					options={[
						...known.map((g) => ({
							value: g.GroupId ?? "",
							label: groupLabel(g),
						})),
						// A group EC2 no longer lists stays a choice, so it can be kept or removed.
						...selected
							.filter((id) => !known.some((g) => g.GroupId === id))
							.map((id) => ({ value: id })),
					]}
					onChange={setSelected}
				/>
			</SpaceBetween>
		</ActionDialog>
	);
}

export function MountTargets({
	scope,
	fileSystem,
	enabled,
	onChanged,
}: {
	scope: EFSScope;
	fileSystem: string;
	enabled: boolean;
	onChanged: () => void;
}) {
	const [dialog, setDialog] = useState<Dialog>({ kind: "" });
	const poll = usePoll(() =>
		enabled ? listMountTargets(scope, fileSystem) : Promise.resolve([]),
	);
	const targets = poll.data ?? [];
	const close = () => setDialog({ kind: "" });
	const saved = () => {
		close();
		poll.reload();
		onChanged();
	};
	return (
		<SpaceBetween size="m">
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			<Rows<MountTargetRow>
				title="Mount targets"
				emptyText="Choose Create mount target to add one."
				items={targets}
				loading={poll.loading}
				actions={
					<Buttons>
						<Action onClick={poll.reload}>Refresh</Action>
						<Button
							variant="primary"
							disabled={!enabled}
							onClick={() => setDialog({ kind: "create" })}
						>
							Create mount target
						</Button>
					</Buttons>
				}
				columns={[
					{ id: "id", header: "Mount target ID", cell: (t) => t.MountTargetId },
					{
						id: "state",
						header: "State",
						cell: (t) => (
							<StatusIndicator type={lifeCycleStatusType(t.LifeCycleState)}>
								{t.LifeCycleState ?? "-"}
							</StatusIndicator>
						),
					},
					{
						id: "zone",
						header: "Availability Zone",
						cell: (t) => t.AvailabilityZoneName || "-",
					},
					{ id: "subnet", header: "Subnet", cell: (t) => t.SubnetId || "-" },
					{ id: "ip", header: "IP address", cell: (t) => t.IpAddress || "-" },
					{
						id: "groups",
						header: "Security groups",
						// One group to a line keeps the column narrow enough for the actions beside it.
						cell: (t) =>
							t.SecurityGroups.length
								? t.SecurityGroups.map((g) => <div key={g}>{g}</div>)
								: "-",
					},
					{
						id: "actions",
						header: "Actions",
						minWidth: 260,
						cell: (t) => (
							<Buttons>
								<Action
									disabled={!enabled}
									onClick={() => setDialog({ kind: "groups", target: t })}
								>
									Security groups
								</Action>
								<Action
									disabled={!enabled}
									onClick={() => setDialog({ kind: "delete", target: t })}
								>
									Delete
								</Action>
							</Buttons>
						),
					},
				]}
			/>
			{dialog.kind === "create" && (
				<CreateMountTargetDialog
					scope={scope}
					fileSystem={fileSystem}
					existing={targets}
					onClose={close}
					onSaved={saved}
				/>
			)}
			{dialog.kind === "groups" && (
				<SecurityGroupsDialog
					scope={scope}
					target={dialog.target}
					onClose={close}
					onSaved={saved}
				/>
			)}
			{dialog.kind === "delete" && (
				<ActionDialog
					title="Delete mount target"
					submit="Delete mount target"
					confirmation={dialog.target.MountTargetId}
					onClose={close}
					onSaved={saved}
					plan={async () => [
						{
							label: "DeleteMountTarget",
							run: () =>
								efsCall(scope, "DeleteMountTarget", {
									MountTargetId: dialog.target.MountTargetId,
								}),
						},
					]}
				>
					<Alert type="warning">
						Delete the mount target in{" "}
						{dialog.target.AvailabilityZoneName || dialog.target.SubnetId}. This
						cannot be undone.
					</Alert>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
