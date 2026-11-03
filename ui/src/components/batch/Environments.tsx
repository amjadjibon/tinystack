import { Alert, Link, SpaceBetween } from "@cloudscape-design/components";
import { useNavigate } from "@tanstack/react-router";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { Action, Buttons, Rows } from "#/components/containers/Controls";
import { Copy } from "#/components/efs/Shared";
import { Details } from "#/components/elbv2/Shared";
import { batchCall, environments } from "#/lib/batch";
import {
	batchHref,
	type ComputeEnvironment,
	capacityText,
	pending,
} from "#/lib/batchHelpers";
import { useFollow } from "#/lib/useFollow";
import { usePoll } from "#/lib/usePoll";
import {
	CreateEnvironmentDialog,
	EditEnvironmentDialog,
} from "./EnvironmentDialog";
import { DetailTabs, ResourceTags, type SectionProps, Status } from "./Shared";

type Dialog = "" | "create" | "edit" | "delete";

export function Environments({
	scope,
	enabled,
	selected,
	tab,
	onChanged,
}: SectionProps) {
	const navigate = useNavigate(),
		follow = useFollow();
	const [changing, setChanging] = useState(true),
		[dialog, setDialog] = useState<Dialog>("");
	const poll = usePoll(
		async () => {
			const rows = enabled ? await environments(scope) : [];
			setChanging(rows.some((c) => pending(c.Status)));
			return rows;
		},
		changing ? 2000 : 0,
	);
	const c = poll.data?.find((c) => c.ComputeEnvironmentName === selected);
	const saved = () => {
		setDialog("");
		setChanging(true);
		poll.reload();
		onChanged();
	};
	const list = batchHref(scope, "environments");
	return (
		<SpaceBetween size="m">
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			{!selected ? (
				<Rows
					title="Compute environments"
					items={poll.data ?? []}
					loading={poll.loading}
					emptyText="Choose Create compute environment to add one."
					actions={
						<Buttons>
							<Action onClick={poll.reload}>Refresh</Action>
							<Action disabled={!enabled} onClick={() => setDialog("create")}>
								Create compute environment
							</Action>
						</Buttons>
					}
					columns={[
						{
							id: "name",
							header: "Name",
							cell: (c) => (
								<Link
									href={batchHref(scope, "environments", {
										environment: c.ComputeEnvironmentName,
									})}
									onFollow={follow}
								>
									{c.ComputeEnvironmentName}
								</Link>
							),
						},
						{ id: "type", header: "Type", cell: (c) => c.Type ?? "-" },
						{
							id: "state",
							header: "State",
							cell: (c) => <Status value={c.State} />,
						},
						{
							id: "status",
							header: "Status",
							cell: (c) => <Status value={c.Status} />,
						},
						{
							id: "capacity",
							header: "vCPUs (min / desired / max)",
							cell: (c) => capacityText(c.ComputeResources),
						},
						{
							id: "instances",
							header: "Instance types",
							cell: (c) => c.ComputeResources?.InstanceTypes?.join(", ") || "-",
						},
					]}
				/>
			) : c ? (
				<SpaceBetween size="m">
					<Buttons>
						<Action onClick={() => navigate({ href: list })}>
							Compute environments
						</Action>
						<Action onClick={poll.reload}>Refresh</Action>
						<Action
							disabled={!enabled || pending(c.Status)}
							onClick={() => setDialog("edit")}
						>
							Edit
						</Action>
						<Action
							disabled={!enabled || pending(c.Status)}
							onClick={() => setDialog("delete")}
						>
							Delete
						</Action>
					</Buttons>
					<DetailTabs
						tab={tab}
						href={(next) =>
							batchHref(scope, "environments", {
								environment: selected,
								tab: next,
							})
						}
						overview={<Overview environment={c} />}
						tags={
							<ResourceTags
								scope={scope}
								arn={c.ComputeEnvironmentArn}
								tags={c.Tags}
								enabled={enabled && !pending(c.Status)}
								onChanged={saved}
							/>
						}
					/>
				</SpaceBetween>
			) : (
				!poll.loading && (
					<Alert>
						Compute environment not found in this account and Region.
					</Alert>
				)
			)}
			{dialog === "create" && (
				<CreateEnvironmentDialog
					scope={scope}
					onClose={() => setDialog("")}
					onSaved={saved}
				/>
			)}
			{dialog === "edit" && !!c && (
				<EditEnvironmentDialog
					scope={scope}
					environment={c}
					onClose={() => setDialog("")}
					onSaved={saved}
				/>
			)}
			{dialog === "delete" && !!c && (
				<ActionDialog
					title="Delete compute environment"
					submit="Delete compute environment"
					confirmation={c.ComputeEnvironmentName}
					onClose={() => setDialog("")}
					onSaved={() => {
						saved();
						navigate({ href: list });
					}}
					plan={async () => [
						{
							label: "DeleteComputeEnvironment",
							run: () =>
								batchCall(scope, "DeleteComputeEnvironment", {
									ComputeEnvironment: c.ComputeEnvironmentName,
								}),
						},
					]}
				>
					<Alert type="warning">
						Delete {c.ComputeEnvironmentName} permanently. Disable it and remove
						it from every job queue first.
					</Alert>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}

function Overview({ environment: c }: { environment: ComputeEnvironment }) {
	const r = c.ComputeResources;
	return (
		<Details
			items={[
				["Name", c.ComputeEnvironmentName],
				["ARN", <Copy key="arn" text={c.ComputeEnvironmentArn} label="ARN" />],
				["Type", c.Type],
				["State", <Status key="state" value={c.State} />],
				["Status", <Status key="status" value={c.Status} />],
				["Status reason", c.StatusReason],
				["Service role", c.ServiceRole],
				["Allocation strategy", r?.AllocationStrategy],
				["vCPUs (min / desired / max)", capacityText(r)],
				["Instance types", r?.InstanceTypes?.join(", ")],
				["Instance profile", r?.InstanceRole],
				["Subnets", r?.Subnets?.join(", ")],
				["Security groups", r?.SecurityGroupIds?.join(", ")],
			]}
		/>
	);
}
