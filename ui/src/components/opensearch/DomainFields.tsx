import {
	FormField,
	Header,
	SpaceBetween,
	Textarea,
	Toggle,
} from "@cloudscape-design/components";
import type { ReactNode } from "react";
import { TagRowsField } from "#/components/elbv2/Shared";
import { Pick, Text } from "#/components/memorydb/Fields";
import {
	carriesIops,
	carriesThroughput,
	type DomainForm,
	type DomainStatus,
	engineVersions,
	instanceTypes,
	tlsPolicies,
	volumeTypeChoices,
	volumeTypes,
} from "#/lib/opensearchHelpers";

type Patch = (patch: Partial<DomainForm>) => void;

function Section({ title, children }: { title: string; children: ReactNode }) {
	return (
		<SpaceBetween size="m">
			<Header variant="h3">{title}</Header>
			{children}
		</SpaceBetween>
	);
}

const options = (values: string[]) => values.map((value) => ({ value }));

function ClusterFields({ form, set }: { form: DomainForm; set: Patch }) {
	return (
		<Section title="Cluster">
			<Pick
				label="Instance type"
				value={form.instanceType}
				placeholder="Choose an instance type"
				options={options(instanceTypes)}
				onChange={(instanceType) => set({ instanceType })}
			/>
			<Text
				label="Instance count"
				value={form.instanceCount}
				onChange={(instanceCount) => set({ instanceCount })}
				constraint="A whole number from 1 to 1000."
			/>
			<Toggle
				checked={form.zoneAwareness}
				onChange={({ detail }) => set({ zoneAwareness: detail.checked })}
				description="Spread the instances over two Availability Zones. Needs an even instance count."
			>
				Zone awareness
			</Toggle>
			<Toggle
				checked={form.dedicatedMaster}
				onChange={({ detail }) => set({ dedicatedMaster: detail.checked })}
				description="Run cluster management on master nodes of their own."
			>
				Dedicated master nodes
			</Toggle>
			{form.dedicatedMaster && (
				<Pick
					label="Dedicated master instance type"
					value={form.masterType}
					placeholder="Choose an instance type"
					options={options(instanceTypes)}
					onChange={(masterType) => set({ masterType })}
				/>
			)}
			{form.dedicatedMaster && (
				<Text
					label="Dedicated master count"
					value={form.masterCount}
					onChange={(masterCount) => set({ masterCount })}
					constraint="A whole number from 1 to 9."
				/>
			)}
		</Section>
	);
}

function StorageFields({
	form,
	set,
	domain,
}: {
	form: DomainForm;
	set: Patch;
	domain?: DomainStatus;
}) {
	// A domain keeps its IOPS and throughput when the volume type changes, so it can move only to a type that takes them.
	const types = domain ? volumeTypeChoices(domain.EBSOptions) : volumeTypes;
	const optional = domain
		? "Optional. A stored value can be changed but not removed."
		: "Optional.";
	return (
		<Section title="Storage">
			<Toggle
				checked={form.ebsEnabled}
				onChange={({ detail }) => set({ ebsEnabled: detail.checked })}
				description="Turn off to use the storage of the instances."
			>
				EBS storage
			</Toggle>
			{form.ebsEnabled && (
				<Pick
					label="Volume type"
					value={form.volumeType}
					placeholder="Choose a volume type"
					options={options(types)}
					onChange={(volumeType) => set({ volumeType })}
				/>
			)}
			{form.ebsEnabled && (
				<Text
					label="Volume size (GiB)"
					value={form.volumeSize}
					onChange={(volumeSize) => set({ volumeSize })}
					constraint="A whole number from 10 to 16384."
				/>
			)}
			{form.ebsEnabled && carriesIops(form.volumeType) && (
				<Text
					label="Provisioned IOPS"
					value={form.iops}
					onChange={(iops) => set({ iops })}
					constraint={optional}
				/>
			)}
			{form.ebsEnabled && carriesThroughput(form.volumeType) && (
				<Text
					label="Throughput (MiB/s)"
					value={form.throughput}
					onChange={(throughput) => set({ throughput })}
					constraint={optional}
				/>
			)}
		</Section>
	);
}

function SecurityFields({
	form,
	set,
	domain,
}: {
	form: DomainForm;
	set: Patch;
	domain?: DomainStatus;
}) {
	const atRestLocked = !!domain?.EncryptionAtRestOptions?.Enabled;
	const nodeToNodeLocked = !!domain?.NodeToNodeEncryptionOptions?.Enabled;
	return (
		<Section title="Security">
			<Toggle
				checked={form.atRest}
				disabled={atRestLocked}
				onChange={({ detail }) => set({ atRest: detail.checked })}
				description={
					atRestLocked
						? "Encryption at rest cannot be turned off once it is on."
						: "Encrypt the data of the domain."
				}
			>
				Encryption at rest
			</Toggle>
			{form.atRest && (
				<Text
					label="KMS key"
					value={form.kmsKey}
					onChange={(kmsKey) => set({ kmsKey })}
					description="A key ID or ARN. TinyStack stores it without looking the key up."
					constraint="Optional, up to 500 characters."
				/>
			)}
			<Toggle
				checked={form.nodeToNode}
				disabled={nodeToNodeLocked}
				onChange={({ detail }) => set({ nodeToNode: detail.checked })}
				description={
					nodeToNodeLocked
						? "Node-to-node encryption cannot be turned off once it is on."
						: "Encrypt the traffic between the nodes of the domain."
				}
			>
				Node-to-node encryption
			</Toggle>
			<Toggle
				checked={form.enforceHTTPS}
				onChange={({ detail }) => set({ enforceHTTPS: detail.checked })}
				description="Refuse requests to the endpoint that do not use HTTPS."
			>
				Require HTTPS
			</Toggle>
			<Pick
				label="TLS security policy"
				value={form.tlsPolicy}
				placeholder="Choose a TLS security policy"
				options={options(tlsPolicies)}
				onChange={(tlsPolicy) => set({ tlsPolicy })}
			/>
		</Section>
	);
}

// DomainFormFields asks for the settings of a domain. Creating one also names it, picks its engine version and
// tags it; a domain being edited has those fixed or edited elsewhere.
export function DomainFormFields({
	form,
	set,
	domain,
}: {
	form: DomainForm;
	set: Patch;
	domain?: DomainStatus;
}) {
	return (
		<SpaceBetween size="l">
			{!domain && (
				<Section title="Domain">
					<Text
						label="Domain name"
						value={form.name}
						onChange={(name) => set({ name })}
						constraint="3 to 28 characters: lowercase letters, digits and hyphens, beginning with a letter."
					/>
					<Pick
						label="Engine version"
						value={form.engineVersion}
						placeholder="Choose an engine version"
						description="The versions TinyStack can run in Docker. A domain keeps its version: TinyStack does not implement upgrades."
						options={options(engineVersions)}
						onChange={(engineVersion) => set({ engineVersion })}
					/>
				</Section>
			)}
			<ClusterFields form={form} set={set} />
			<StorageFields form={form} set={set} domain={domain} />
			<Section title="Access policy">
				<FormField
					label="Access policy (JSON)"
					description="An IAM-style policy document. TinyStack stores it without evaluating it."
					constraintText="Optional. Leave empty for no policy."
				>
					<Textarea
						ariaLabel="Access policy"
						rows={8}
						value={form.policy}
						onChange={({ detail }) => set({ policy: detail.value })}
					/>
				</FormField>
			</Section>
			<SecurityFields form={form} set={set} domain={domain} />
			{!domain && (
				<TagRowsField rows={form.tags} onChange={(tags) => set({ tags })} />
			)}
		</SpaceBetween>
	);
}
