import { Alert, SpaceBetween } from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { Pick, PickMany, Text } from "#/components/memorydb/Fields";
import {
	type ELBv2Scope,
	elbv2Call,
	type LoadBalancer,
	type SecurityGroup,
	type Subnet,
} from "#/lib/elbv2";
import {
	createLoadBalancerInput,
	deleteHint,
	type LoadBalancerForm,
	setSubnetsInput,
} from "#/lib/elbv2Helpers";
import { TagRowsField } from "./Shared";

const subnetLabel = (s: Subnet) =>
	`${s.SubnetId} (${s.AvailabilityZone ?? "?"}, ${s.CidrBlock ?? "?"}, ${s.VpcId ?? "?"})`;

function vpcOf(subnets: Subnet[], ids: string[]) {
	const vpcs = new Set(
		ids.map((id) => subnets.find((s) => s.SubnetId === id)?.VpcId),
	);
	if (vpcs.size > 1)
		throw new Error("All subnets must belong to the same VPC.");
	return [...vpcs][0];
}

const emptyForm: LoadBalancerForm = {
	name: "",
	type: "application",
	scheme: "internet-facing",
	ipAddressType: "ipv4",
	subnets: [],
	securityGroups: [],
	tags: [],
};

export function CreateLoadBalancerDialog({
	scope,
	subnets,
	groups,
	onClose,
	onSaved,
}: {
	scope: ELBv2Scope;
	subnets: Subnet[];
	groups: SecurityGroup[];
	onClose: () => void;
	onSaved: (name: string) => void;
}) {
	const [form, setForm] = useState(emptyForm);
	const set = (patch: Partial<LoadBalancerForm>) =>
		setForm((f) => ({ ...f, ...patch }));
	const vpc = form.subnets.length
		? subnets.find((s) => s.SubnetId === form.subnets[0])?.VpcId
		: undefined;
	return (
		<ActionDialog
			title="Create load balancer"
			submit="Create load balancer"
			onClose={onClose}
			onSaved={() => onSaved(form.name.trim())}
			plan={async () => {
				vpcOf(subnets, form.subnets);
				const input = createLoadBalancerInput(form);
				return [
					{
						label: "CreateLoadBalancer",
						run: () => elbv2Call(scope, "CreateLoadBalancer", input),
					},
				];
			}}
		>
			<SpaceBetween size="m">
				<Text
					label="Load balancer name"
					value={form.name}
					onChange={(name) => set({ name })}
					constraint="1 to 32 letters, digits or hyphens; cannot begin or end with a hyphen."
				/>
				<Pick
					label="Load balancer type"
					value={form.type}
					placeholder="Choose a type"
					options={[
						{ value: "application", label: "Application Load Balancer" },
						{ value: "network", label: "Network Load Balancer" },
					]}
					onChange={(type) => set({ type, securityGroups: [] })}
				/>
				<Pick
					label="Scheme"
					value={form.scheme}
					placeholder="Choose a scheme"
					options={[{ value: "internet-facing" }, { value: "internal" }]}
					onChange={(scheme) => set({ scheme })}
				/>
				<Pick
					label="IP address type"
					value={form.ipAddressType}
					placeholder="Choose an IP address type"
					options={[{ value: "ipv4" }, { value: "dualstack" }]}
					onChange={(ipAddressType) => set({ ipAddressType })}
				/>
				<PickMany
					label="Subnets"
					values={form.subnets}
					placeholder="Choose subnets"
					description="Choose subnets in different Availability Zones of one VPC; an application load balancer needs two."
					options={subnets.map((s) => ({
						value: s.SubnetId ?? "",
						label: subnetLabel(s),
					}))}
					onChange={(values) => set({ subnets: values, securityGroups: [] })}
				/>
				{form.type === "application" && (
					<PickMany
						label="Security groups"
						values={form.securityGroups}
						placeholder="Choose security groups"
						description={
							vpc
								? `Security groups in ${vpc}. Leave empty to use the VPC default group.`
								: "Choose subnets first. Leave empty to use the VPC default group."
						}
						options={groups
							.filter((g) => vpc && g.VpcId === vpc)
							.map((g) => ({
								value: g.GroupId ?? "",
								label: `${g.GroupId} (${g.GroupName ?? ""})`,
							}))}
						onChange={(securityGroups) => set({ securityGroups })}
					/>
				)}
				<TagRowsField rows={form.tags} onChange={(tags) => set({ tags })} />
			</SpaceBetween>
		</ActionDialog>
	);
}

export function EditNetworkDialog({
	scope,
	balancer,
	subnets,
	groups,
	onClose,
	onSaved,
}: {
	scope: ELBv2Scope;
	balancer: LoadBalancer;
	subnets: Subnet[];
	groups: SecurityGroup[];
	onClose: () => void;
	onSaved: () => void;
}) {
	const application = balancer.Type !== "network";
	const currentSubnets = (balancer.AvailabilityZones ?? []).map(
		(z) => z.SubnetId ?? "",
	);
	const currentGroups = balancer.SecurityGroups ?? [];
	const [chosen, setChosen] = useState(currentSubnets);
	const [chosenGroups, setChosenGroups] = useState(currentGroups);
	const same = (a: string[], b: string[]) =>
		a.length === b.length && a.every((v) => b.includes(v));
	return (
		<ActionDialog
			title={`Edit network for ${balancer.LoadBalancerName}`}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const steps = [];
				if (!same(chosen, currentSubnets)) {
					const input = setSubnetsInput(
						balancer.LoadBalancerArn,
						balancer.Type,
						chosen,
					);
					steps.push({
						label: "SetSubnets",
						run: () => elbv2Call(scope, "SetSubnets", input),
					});
				}
				if (application && !same(chosenGroups, currentGroups)) {
					if (!chosenGroups.length)
						throw new Error("Choose at least one security group.");
					steps.push({
						label: "SetSecurityGroups",
						run: () =>
							elbv2Call(scope, "SetSecurityGroups", {
								LoadBalancerArn: balancer.LoadBalancerArn,
								SecurityGroups: chosenGroups,
							}),
					});
				}
				if (!steps.length) throw new Error("Nothing was changed.");
				return steps;
			}}
		>
			<SpaceBetween size="m">
				<PickMany
					label="Subnets"
					values={chosen}
					placeholder="Choose subnets"
					description={`Subnets must stay in ${balancer.VpcId ?? "the load balancer's VPC"}.`}
					options={subnets
						.filter((s) => !balancer.VpcId || s.VpcId === balancer.VpcId)
						.map((s) => ({ value: s.SubnetId ?? "", label: subnetLabel(s) }))}
					onChange={setChosen}
				/>
				{application ? (
					<PickMany
						label="Security groups"
						values={chosenGroups}
						placeholder="Choose security groups"
						options={groups
							.filter((g) => g.VpcId === balancer.VpcId)
							.map((g) => ({
								value: g.GroupId ?? "",
								label: `${g.GroupId} (${g.GroupName ?? ""})`,
							}))}
						onChange={setChosenGroups}
					/>
				) : (
					<Alert type="info">
						Network load balancers have no security groups here.
					</Alert>
				)}
			</SpaceBetween>
		</ActionDialog>
	);
}

export function DeleteLoadBalancerDialog({
	scope,
	balancer,
	onClose,
	onSaved,
}: {
	scope: ELBv2Scope;
	balancer: LoadBalancer;
	onClose: () => void;
	onSaved: () => void;
}) {
	return (
		<ActionDialog
			title="Delete load balancer"
			submit="Delete load balancer"
			confirmation={balancer.LoadBalancerName}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => [
				{
					label: "DeleteLoadBalancer",
					run: async () => {
						try {
							await elbv2Call(scope, "DeleteLoadBalancer", {
								LoadBalancerArn: balancer.LoadBalancerArn,
							});
						} catch (err) {
							throw new Error(
								deleteHint(err instanceof Error ? err.message : String(err)),
							);
						}
					},
				},
			]}
		>
			<Alert type="warning">
				Deleting {balancer.LoadBalancerName} also removes its listeners and
				rules. Target groups are kept. A load balancer with deletion protection
				enabled cannot be deleted until you turn the protection off in its
				attributes.
			</Alert>
		</ActionDialog>
	);
}
