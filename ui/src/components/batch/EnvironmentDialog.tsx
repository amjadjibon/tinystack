import { Alert, SpaceBetween } from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { Choice, Field } from "#/components/containers/Controls";
import { PickMany } from "#/components/memorydb/Fields";
import { batchCall } from "#/lib/batch";
import {
	type BatchScope,
	type ComputeEnvironment,
	type EnvironmentForm,
	emptyEnvironmentForm,
	environmentInput,
	environmentTypes,
	environmentUpdate,
	states,
	strategies,
} from "#/lib/batchHelpers";
import { listSecurityGroups, listSubnets } from "#/lib/elbv2";
import { usePoll } from "#/lib/usePoll";

type Capacity = Pick<EnvironmentForm, "min" | "desired" | "max">;

function CapacityFields({
	value,
	onChange,
}: {
	value: Capacity;
	onChange: (patch: Partial<Capacity>) => void;
}) {
	return (
		<>
			<Field
				label="Minimum vCPUs"
				value={value.min}
				onChange={(min) => onChange({ min })}
			/>
			<Field
				label="Desired vCPUs"
				value={value.desired}
				onChange={(desired) => onChange({ desired })}
			/>
			<Field
				label="Maximum vCPUs"
				value={value.max}
				onChange={(max) => onChange({ max })}
			/>
		</>
	);
}

export function CreateEnvironmentDialog({
	scope,
	onClose,
	onSaved,
}: {
	scope: BatchScope;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [f, setF] = useState(emptyEnvironmentForm);
	const set = (p: Partial<EnvironmentForm>) => setF((v) => ({ ...v, ...p }));
	const options = usePoll(async () => {
		const [subnets, groups] = await Promise.all([
			listSubnets(scope),
			listSecurityGroups(scope),
		]);
		return { subnets, groups };
	}, 0);
	const vpc = options.data?.subnets.find((s) =>
		f.subnets.includes(s.SubnetId ?? ""),
	)?.VpcId;
	const managed = f.type === "MANAGED";
	return (
		<ActionDialog
			title="Create compute environment"
			submit="Create compute environment"
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const input = environmentInput(f);
				return [
					{
						label: "CreateComputeEnvironment",
						run: () => batchCall(scope, "CreateComputeEnvironment", input),
					},
				];
			}}
		>
			<SpaceBetween size="m">
				{!!options.error && <Alert type="error">{options.error}</Alert>}
				<Field label="Name" value={f.name} onChange={(name) => set({ name })} />
				<Choice
					label="Type"
					value={f.type}
					options={environmentTypes}
					onChange={(type) => set({ type })}
				/>
				<Field
					label={managed ? "Service role (optional)" : "Service role"}
					value={f.serviceRole}
					onChange={(serviceRole) => set({ serviceRole })}
				/>
				{managed ? (
					<>
						<Alert>
							Managed environments describe EC2 capacity. No instances start;
							Docker mode runs jobs within the maximum vCPUs.
						</Alert>
						<Choice
							label="Allocation strategy"
							value={f.strategy}
							options={strategies}
							onChange={(strategy) => set({ strategy })}
						/>
						<CapacityFields value={f} onChange={set} />
						<Field
							label="Instance types (comma separated)"
							value={f.instances}
							onChange={(instances) => set({ instances })}
						/>
						<Field
							label="Instance profile"
							value={f.instanceRole}
							onChange={(instanceRole) => set({ instanceRole })}
						/>
						<PickMany
							label="Subnets"
							placeholder="Choose subnets"
							values={f.subnets}
							options={(options.data?.subnets ?? [])
								.filter((s) => !vpc || s.VpcId === vpc)
								.map((s) => ({
									value: s.SubnetId ?? "",
									label: `${s.SubnetId} (${s.AvailabilityZone})`,
								}))}
							onChange={(subnets) => set({ subnets, groups: [] })}
						/>
						<PickMany
							label="Security groups"
							placeholder="Choose security groups"
							values={f.groups}
							options={(options.data?.groups ?? [])
								.filter((g) => !!vpc && g.VpcId === vpc)
								.map((g) => ({ value: g.GroupId ?? "", label: g.GroupName }))}
							onChange={(groups) => set({ groups })}
						/>
					</>
				) : (
					<Alert>
						An unmanaged environment is capacity you bring. It has no
						computeResources, so Docker mode never starts a job in it.
					</Alert>
				)}
				<Field
					label="Tags (JSON string map)"
					value={f.tags}
					onChange={(tags) => set({ tags })}
				/>
			</SpaceBetween>
		</ActionDialog>
	);
}

export function EditEnvironmentDialog({
	scope,
	environment: c,
	onClose,
	onSaved,
}: {
	scope: BatchScope;
	environment: ComputeEnvironment;
	onClose: () => void;
	onSaved: () => void;
}) {
	const r = c.ComputeResources;
	const [f, setF] = useState({
		state: c.State ?? "ENABLED",
		min: String(r?.MinvCpus ?? 0),
		desired: String(r?.DesiredvCpus ?? 0),
		max: String(r?.MaxvCpus ?? 1),
	});
	return (
		<ActionDialog
			title={`Edit ${c.ComputeEnvironmentName}`}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const input = environmentUpdate(c, f);
				return [
					{
						label: "UpdateComputeEnvironment",
						run: () => batchCall(scope, "UpdateComputeEnvironment", input),
					},
				];
			}}
		>
			<SpaceBetween size="m">
				<Choice
					label="State"
					value={f.state}
					options={states}
					onChange={(state) => setF((v) => ({ ...v, state }))}
				/>
				{r ? (
					<>
						<CapacityFields
							value={f}
							onChange={(patch) => setF((v) => ({ ...v, ...patch }))}
						/>
						<Alert>
							Desired vCPUs can grow but not shrink, and must stay between the
							minimum and maximum.
						</Alert>
					</>
				) : (
					<Alert>An unmanaged environment has no capacity to change.</Alert>
				)}
			</SpaceBetween>
		</ActionDialog>
	);
}
