import {
	Alert,
	Container,
	Header,
	SpaceBetween,
	StatusIndicator,
} from "@cloudscape-design/components";
import { useState } from "react";
import { Action, Buttons, Rows } from "#/components/containers/Controls";
import {
	type ELBv2Scope,
	elbv2Call,
	type Instance,
	listInstances,
	type TargetGroup,
	type TargetHealth,
	type Vpc,
} from "#/lib/elbv2";
import { usePoll } from "#/lib/usePoll";
import { AttributesDialog, Details, ResourceTags } from "./Shared";
import {
	CreateTargetGroupDialog,
	DeleteTargetGroupDialog,
	DeregisterTargetsDialog,
	ModifyHealthDialog,
	RegisterTargetsDialog,
} from "./TargetGroupDialogs";

type Dialog =
	| ""
	| "create"
	| "health"
	| "attributes"
	| "delete"
	| "register"
	| "deregister";

function healthType(state?: string) {
	if (state === "healthy") return "success";
	if (state === "unhealthy") return "error";
	return state === "initial" ? "in-progress" : "info";
}

function Targets({
	scope,
	group,
	enabled,
	instances,
}: {
	scope: ELBv2Scope;
	group: TargetGroup;
	enabled: boolean;
	instances: Instance[];
}) {
	const health = usePoll(async () => {
		const out = await elbv2Call<{ TargetHealthDescriptions?: TargetHealth[] }>(
			scope,
			"DescribeTargetHealth",
			{ TargetGroupArn: group.TargetGroupArn },
		);
		return out.TargetHealthDescriptions ?? [];
	}, 5000);
	const [dialog, setDialog] = useState<"" | "register" | "deregister">("");
	const saved = () => {
		setDialog("");
		health.reload();
	};
	return (
		<SpaceBetween size="m">
			{health.error && <Alert type="error">{health.error}</Alert>}
			<Rows
				title="Targets"
				emptyText="Choose Register targets to add instances, IP addresses or a Lambda function."
				items={health.data ?? []}
				loading={health.loading}
				actions={
					<Buttons>
						<Action onClick={health.reload}>Refresh</Action>
						<Action
							disabled={!enabled || !health.data?.length}
							onClick={() => setDialog("deregister")}
						>
							Deregister targets
						</Action>
						<Action disabled={!enabled} onClick={() => setDialog("register")}>
							Register targets
						</Action>
					</Buttons>
				}
				columns={[
					{
						id: "id",
						header: "Target",
						cell: (h: TargetHealth) => h.Target?.Id,
					},
					{ id: "port", header: "Port", cell: (h) => h.Target?.Port ?? "-" },
					{
						id: "zone",
						header: "Availability Zone",
						cell: (h) => h.Target?.AvailabilityZone ?? "-",
					},
					{
						id: "state",
						header: "Health status",
						cell: (h) => (
							<StatusIndicator type={healthType(h.TargetHealth?.State)}>
								{h.TargetHealth?.State}
							</StatusIndicator>
						),
					},
					{
						id: "reason",
						header: "Details",
						cell: (h) =>
							h.TargetHealth?.Description ?? h.TargetHealth?.Reason ?? "-",
					},
				]}
			/>
			{dialog === "register" && (
				<RegisterTargetsDialog
					scope={scope}
					group={group}
					instances={instances}
					onClose={() => setDialog("")}
					onSaved={saved}
				/>
			)}
			{dialog === "deregister" && (
				<DeregisterTargetsDialog
					scope={scope}
					group={group}
					health={health.data ?? []}
					onClose={() => setDialog("")}
					onSaved={saved}
				/>
			)}
		</SpaceBetween>
	);
}

export function TargetGroups({
	scope,
	enabled,
	vpcs,
	groups,
	loading,
	error,
	onChanged,
}: {
	scope: ELBv2Scope;
	enabled: boolean;
	vpcs: Vpc[];
	groups: TargetGroup[];
	loading: boolean;
	error?: string;
	onChanged: () => void;
}) {
	const instances = usePoll(
		() => (enabled ? listInstances(scope) : Promise.resolve([])),
		0,
	);
	const [selected, setSelected] = useState("");
	const [dialog, setDialog] = useState<Dialog>("");
	const [target, setTarget] = useState<TargetGroup>();
	const detail = groups.find((g) => g.TargetGroupName === selected);
	const open = (kind: Dialog, g?: TargetGroup) => {
		setTarget(g);
		setDialog(kind);
	};
	const changed = () => {
		setDialog("");
		onChanged();
	};
	const subject = target ?? detail;
	return (
		<SpaceBetween size="l">
			{error && <Alert type="error">{error}</Alert>}
			{instances.error && (
				<Alert type="warning">
					Instances are not listed: {instances.error}
				</Alert>
			)}
			<Rows
				title="Target groups"
				emptyText="Choose Create target group to get started."
				items={groups}
				loading={loading}
				actions={
					<Buttons>
						<Action onClick={onChanged}>Refresh</Action>
						<Action disabled={!enabled} onClick={() => open("create")}>
							Create target group
						</Action>
					</Buttons>
				}
				columns={[
					{
						id: "name",
						header: "Name",
						cell: (g: TargetGroup) => (
							<Action onClick={() => setSelected(g.TargetGroupName)}>
								{g.TargetGroupName}
							</Action>
						),
					},
					{ id: "type", header: "Target type", cell: (g) => g.TargetType },
					{
						id: "protocol",
						header: "Protocol : port",
						cell: (g) => (g.Protocol ? `${g.Protocol}:${g.Port}` : "-"),
					},
					{ id: "vpc", header: "VPC", cell: (g) => g.VpcId ?? "-" },
					{
						id: "lbs",
						header: "Load balancers",
						cell: (g) => g.LoadBalancerArns?.length ?? 0,
					},
					{
						id: "manage",
						header: "Actions",
						cell: (g) => (
							<Action disabled={!enabled} onClick={() => open("delete", g)}>
								Delete
							</Action>
						),
					},
				]}
			/>
			{detail && (
				<SpaceBetween size="l" key={detail.TargetGroupArn}>
					<Container
						header={
							<Header
								variant="h2"
								description={detail.TargetGroupArn}
								actions={
									<Buttons>
										<Action
											disabled={!enabled}
											onClick={() => open("health", detail)}
										>
											Edit health check
										</Action>
										<Action
											disabled={!enabled}
											onClick={() => open("attributes", detail)}
										>
											Edit attributes
										</Action>
										<Action
											disabled={!enabled}
											onClick={() => open("delete", detail)}
										>
											Delete
										</Action>
									</Buttons>
								}
							>
								{detail.TargetGroupName}
							</Header>
						}
					>
						<Details
							items={[
								["Target type", detail.TargetType],
								["Protocol", detail.Protocol],
								["Port", detail.Port],
								["VPC", detail.VpcId],
								[
									"Health check",
									detail.HealthCheckEnabled === false
										? "Disabled"
										: `${detail.HealthCheckProtocol ?? ""} ${detail.HealthCheckPath ?? ""}`.trim(),
								],
								["Health check port", detail.HealthCheckPort],
								["Interval (seconds)", detail.HealthCheckIntervalSeconds],
								["Timeout (seconds)", detail.HealthCheckTimeoutSeconds],
								[
									"Thresholds",
									`${detail.HealthyThresholdCount ?? "-"} healthy, ${detail.UnhealthyThresholdCount ?? "-"} unhealthy`,
								],
								[
									"Success codes",
									detail.Matcher?.HttpCode ?? detail.Matcher?.GrpcCode,
								],
							]}
						/>
					</Container>
					<Targets
						scope={scope}
						group={detail}
						enabled={enabled}
						instances={instances.data ?? []}
					/>
					<ResourceTags
						scope={scope}
						arn={detail.TargetGroupArn}
						enabled={enabled}
					/>
				</SpaceBetween>
			)}
			{dialog === "create" && (
				<CreateTargetGroupDialog
					scope={scope}
					vpcs={vpcs}
					onClose={() => setDialog("")}
					onSaved={(name) => {
						setSelected(name);
						changed();
					}}
				/>
			)}
			{dialog === "health" && subject && (
				<ModifyHealthDialog
					scope={scope}
					group={subject}
					onClose={() => setDialog("")}
					onSaved={changed}
				/>
			)}
			{dialog === "attributes" && subject && (
				<AttributesDialog
					scope={scope}
					title={`Attributes of ${subject.TargetGroupName}`}
					arn={subject.TargetGroupArn}
					idKey="TargetGroupArn"
					describe="DescribeTargetGroupAttributes"
					modify="ModifyTargetGroupAttributes"
					onClose={() => setDialog("")}
					onSaved={changed}
				/>
			)}
			{dialog === "delete" && subject && (
				<DeleteTargetGroupDialog
					scope={scope}
					group={subject}
					onClose={() => setDialog("")}
					onSaved={() => {
						if (selected === subject.TargetGroupName) setSelected("");
						changed();
					}}
				/>
			)}
		</SpaceBetween>
	);
}
