import {
	Alert,
	FormField,
	SpaceBetween,
	Textarea,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { Pick, PickMany, Text } from "#/components/memorydb/Fields";
import {
	type ELBv2Scope,
	elbv2Call,
	type Instance,
	type TargetGroup,
	type TargetHealth,
	type Vpc,
} from "#/lib/elbv2";
import {
	applicationTargetProtocols,
	createTargetGroupInput,
	emptyTargetGroupForm,
	isWebProtocol,
	lines,
	modifyTargetGroupInput,
	networkTargetProtocols,
	type TargetGroupForm,
	targetGroupFormFrom,
	targetsInput,
} from "#/lib/elbv2Helpers";
import { TagRowsField } from "./Shared";

function HealthFields({
	form,
	set,
}: {
	form: TargetGroupForm;
	set: (patch: Partial<TargetGroupForm>) => void;
}) {
	const lambda = form.targetType === "lambda";
	const web = lambda || isWebProtocol(form.healthProtocol);
	return (
		<SpaceBetween size="m">
			{!lambda && (
				<>
					<Pick
						label="Health check protocol"
						value={form.healthProtocol}
						placeholder="Choose a protocol"
						options={["HTTP", "HTTPS", "TCP"].map((value) => ({ value }))}
						onChange={(healthProtocol) => set({ healthProtocol })}
					/>
					<Text
						label="Health check port"
						value={form.healthPort}
						constraint="traffic-port or 1 to 65535."
						onChange={(healthPort) => set({ healthPort })}
					/>
				</>
			)}
			{web && (
				<>
					<Text
						label="Health check path"
						value={form.healthPath}
						onChange={(healthPath) => set({ healthPath })}
					/>
					<Text
						label="Success codes"
						value={form.matcher}
						constraint="For example 200 or 200-299."
						onChange={(matcher) => set({ matcher })}
					/>
				</>
			)}
			<Text
				label="Interval (seconds)"
				value={form.interval}
				constraint="5 to 300."
				onChange={(interval) => set({ interval })}
			/>
			<Text
				label="Timeout (seconds)"
				value={form.timeout}
				constraint="2 to 120 and smaller than the interval."
				onChange={(timeout) => set({ timeout })}
			/>
			<Text
				label="Healthy threshold"
				value={form.healthy}
				constraint="2 to 10."
				onChange={(healthy) => set({ healthy })}
			/>
			<Text
				label="Unhealthy threshold"
				value={form.unhealthy}
				constraint="2 to 10."
				onChange={(unhealthy) => set({ unhealthy })}
			/>
		</SpaceBetween>
	);
}

export function CreateTargetGroupDialog({
	scope,
	vpcs,
	onClose,
	onSaved,
}: {
	scope: ELBv2Scope;
	vpcs: Vpc[];
	onClose: () => void;
	onSaved: (name: string) => void;
}) {
	const [form, setForm] = useState(emptyTargetGroupForm);
	const set = (patch: Partial<TargetGroupForm>) =>
		setForm((f) => ({ ...f, ...patch }));
	const lambda = form.targetType === "lambda";
	const protocols = [...applicationTargetProtocols, ...networkTargetProtocols];
	return (
		<ActionDialog
			title="Create target group"
			submit="Create target group"
			onClose={onClose}
			onSaved={() => onSaved(form.name.trim())}
			plan={async () => {
				const input = createTargetGroupInput(form);
				return [
					{
						label: "CreateTargetGroup",
						run: () => elbv2Call(scope, "CreateTargetGroup", input),
					},
				];
			}}
		>
			<SpaceBetween size="m">
				<Text
					label="Target group name"
					value={form.name}
					onChange={(name) => set({ name })}
					constraint="1 to 32 letters, digits or hyphens; cannot begin or end with a hyphen."
				/>
				<Pick
					label="Target type"
					value={form.targetType}
					placeholder="Choose a target type"
					options={[
						{ value: "instance", label: "Instances" },
						{ value: "ip", label: "IP addresses" },
						{ value: "lambda", label: "Lambda function" },
					]}
					onChange={(targetType) => set({ targetType, healthPath: "/" })}
				/>
				{!lambda && (
					<>
						<Pick
							label="Protocol"
							value={form.protocol}
							placeholder="Choose a protocol"
							options={protocols.map((value) => ({ value }))}
							onChange={(protocol) =>
								set({
									protocol,
									healthProtocol: isWebProtocol(protocol) ? protocol : "TCP",
								})
							}
						/>
						<Text
							label="Port"
							value={form.port}
							onChange={(port) => set({ port })}
						/>
						<Pick
							label="VPC"
							value={form.vpc}
							placeholder="Choose a VPC"
							options={vpcs.map((v) => ({
								value: v.VpcId ?? "",
								label: `${v.VpcId} (${v.CidrBlock ?? ""})`,
							}))}
							onChange={(vpc) => set({ vpc })}
						/>
					</>
				)}
				<HealthFields form={form} set={set} />
				<TagRowsField rows={form.tags} onChange={(tags) => set({ tags })} />
			</SpaceBetween>
		</ActionDialog>
	);
}

export function ModifyHealthDialog({
	scope,
	group,
	onClose,
	onSaved,
}: {
	scope: ELBv2Scope;
	group: TargetGroup;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [form, setForm] = useState(targetGroupFormFrom(group));
	return (
		<ActionDialog
			title={`Edit health check of ${group.TargetGroupName}`}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const input = modifyTargetGroupInput(group.TargetGroupArn, form);
				return [
					{
						label: "ModifyTargetGroup",
						run: () => elbv2Call(scope, "ModifyTargetGroup", input),
					},
				];
			}}
		>
			<HealthFields
				form={form}
				set={(patch) => setForm((f) => ({ ...f, ...patch }))}
			/>
		</ActionDialog>
	);
}

export function DeleteTargetGroupDialog({
	scope,
	group,
	onClose,
	onSaved,
}: {
	scope: ELBv2Scope;
	group: TargetGroup;
	onClose: () => void;
	onSaved: () => void;
}) {
	return (
		<ActionDialog
			title="Delete target group"
			submit="Delete target group"
			confirmation={group.TargetGroupName}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => [
				{
					label: "DeleteTargetGroup",
					run: () =>
						elbv2Call(scope, "DeleteTargetGroup", {
							TargetGroupArn: group.TargetGroupArn,
						}),
				},
			]}
		>
			<Alert type="warning">
				Deleting {group.TargetGroupName} fails while a listener or rule forwards
				to it. Remove those actions first.
			</Alert>
		</ActionDialog>
	);
}

export function RegisterTargetsDialog({
	scope,
	group,
	instances,
	onClose,
	onSaved,
}: {
	scope: ELBv2Scope;
	group: TargetGroup;
	instances: Instance[];
	onClose: () => void;
	onSaved: () => void;
}) {
	const kind = group.TargetType ?? "instance";
	const [chosen, setChosen] = useState<string[]>([]);
	const [text, setText] = useState("");
	const [port, setPort] = useState(String(group.Port ?? ""));
	const choices = instances.filter(
		(i) =>
			i.State?.Name === "running" && (!group.VpcId || i.VpcId === group.VpcId),
	);
	return (
		<ActionDialog
			title={`Register targets in ${group.TargetGroupName}`}
			submit="Register targets"
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const input = targetsInput(
					group.TargetGroupArn,
					kind,
					kind === "instance" ? chosen : lines(text),
					port,
				);
				return [
					{
						label: "RegisterTargets",
						run: () => elbv2Call(scope, "RegisterTargets", input),
					},
				];
			}}
		>
			<SpaceBetween size="m">
				{kind === "instance" ? (
					<PickMany
						label="Instances"
						values={chosen}
						placeholder="Choose running instances"
						description={`Running instances in ${group.VpcId ?? "the target group's VPC"}.`}
						options={choices.map((i) => ({ value: i.InstanceId ?? "" }))}
						onChange={setChosen}
					/>
				) : (
					<FormField
						label={kind === "lambda" ? "Function ARN" : "IP addresses"}
						description={
							kind === "lambda"
								? "A lambda target group has exactly one target."
								: "One private address per line."
						}
					>
						<Textarea
							ariaLabel={kind === "lambda" ? "Function ARN" : "IP addresses"}
							rows={3}
							value={text}
							onChange={({ detail }) => setText(detail.value)}
						/>
					</FormField>
				)}
				{kind !== "lambda" && (
					<Text
						label="Port"
						value={port}
						description="Defaults to the target group port when empty."
						onChange={setPort}
					/>
				)}
			</SpaceBetween>
		</ActionDialog>
	);
}

export function DeregisterTargetsDialog({
	scope,
	group,
	health,
	onClose,
	onSaved,
}: {
	scope: ELBv2Scope;
	group: TargetGroup;
	health: TargetHealth[];
	onClose: () => void;
	onSaved: () => void;
}) {
	const [chosen, setChosen] = useState<string[]>([]);
	const key = (h: TargetHealth) => `${h.Target?.Id}|${h.Target?.Port ?? ""}`;
	return (
		<ActionDialog
			title={`Deregister targets from ${group.TargetGroupName}`}
			submit="Deregister targets"
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				if (!chosen.length) throw new Error("Choose at least one target.");
				const Targets = chosen.map((c) => {
					const [Id, port] = c.split("|");
					return { Id, ...(port ? { Port: Number(port) } : {}) };
				});
				return [
					{
						label: "DeregisterTargets",
						run: () =>
							elbv2Call(scope, "DeregisterTargets", {
								TargetGroupArn: group.TargetGroupArn,
								Targets,
							}),
					},
				];
			}}
		>
			<PickMany
				label="Targets"
				values={chosen}
				placeholder="Choose registered targets"
				options={health.map((h) => ({
					value: key(h),
					label: `${h.Target?.Id}${h.Target?.Port ? `:${h.Target.Port}` : ""}`,
				}))}
				onChange={setChosen}
			/>
		</ActionDialog>
	);
}
