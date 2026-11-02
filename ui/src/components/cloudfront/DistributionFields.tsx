import {
	Box,
	Button,
	Checkbox,
	Container,
	FormField,
	Header,
	SpaceBetween,
	Textarea,
} from "@cloudscape-design/components";
import { TagRowsField } from "#/components/elbv2/Shared";
import { Pick, Text } from "#/components/memorydb/Fields";
import type { OacSummary } from "#/lib/cloudfront";
import {
	type DistributionForm,
	defaultTarget,
	emptyOrigin,
	managedCachePolicies,
	methodChoices,
	type OriginForm,
	type OriginKind,
	originId,
	originProtocolPolicies,
	viewerProtocolPolicies,
} from "#/lib/cloudfrontHelpers";

const none = "(none)";

function OriginFields({
	origin,
	index,
	oacs,
	canRemove,
	onChange,
	onRemove,
}: {
	origin: OriginForm;
	index: number;
	oacs: OacSummary[];
	canRemove: boolean;
	onChange: (origin: OriginForm) => void;
	onRemove: () => void;
}) {
	const set = (patch: Partial<OriginForm>) => onChange({ ...origin, ...patch });
	const accessControls = oacs.filter(
		(o) => o.OriginAccessControlOriginType === "s3",
	);
	return (
		<Container
			header={
				<Header
					variant="h3"
					actions={
						<Button disabled={!canRemove} onClick={onRemove}>
							Remove origin
						</Button>
					}
				>
					Origin {index + 1}
				</Header>
			}
		>
			{origin.kind === "other" ? (
				<Box>
					Origin {origin.id} is a type of origin that this form cannot edit. It
					stays as it is unless you remove it.
				</Box>
			) : (
				<SpaceBetween size="m">
					<Pick
						label="Origin type"
						value={origin.kind}
						placeholder="Choose an origin type"
						options={[
							{ value: "s3", label: "S3 bucket" },
							{ value: "custom", label: "Custom origin" },
						]}
						onChange={(kind) => set({ kind: kind as OriginKind })}
					/>
					<Text
						label="Domain name"
						value={origin.domain}
						onChange={(domain) => set({ domain })}
						description={
							origin.kind === "s3"
								? "The bucket's domain name, for example my-bucket.s3.us-east-1.amazonaws.com."
								: "For example api.example.com."
						}
					/>
					<Text
						label="Origin ID"
						value={origin.id}
						onChange={(id) => set({ id })}
						description="Optional. Defaults to the domain name."
					/>
					<Text
						label="Origin path"
						value={origin.path}
						onChange={(path) => set({ path })}
						constraint="Optional. Start with a slash and do not end with one, for example /assets."
					/>
					{origin.kind === "s3" ? (
						<Pick
							label="Origin access control"
							value={origin.accessControl || none}
							placeholder="Choose an origin access control"
							description="Signs requests to the bucket. Create one on the Origin access controls tab."
							options={[
								{ value: none },
								...accessControls.map((o) => ({
									value: o.Id,
									label: `${o.Name ?? o.Id} (${o.Id})`,
								})),
							]}
							onChange={(value) =>
								set({ accessControl: value === none ? "" : value })
							}
						/>
					) : (
						<SpaceBetween size="m">
							<Pick
								label="Origin protocol policy"
								value={origin.protocol}
								placeholder="Choose a protocol policy"
								options={originProtocolPolicies.map((value) => ({ value }))}
								onChange={(protocol) => set({ protocol })}
							/>
							<Text
								label="HTTP port"
								value={origin.httpPort}
								onChange={(httpPort) => set({ httpPort })}
							/>
							<Text
								label="HTTPS port"
								value={origin.httpsPort}
								onChange={(httpsPort) => set({ httpsPort })}
							/>
						</SpaceBetween>
					)}
				</SpaceBetween>
			)}
		</Container>
	);
}

// DistributionFields is the form for creating a distribution and for editing the
// settings of one; the tags are edited on the distribution's page.
export function DistributionFields({
	form,
	oacs,
	creating,
	onChange,
}: {
	form: DistributionForm;
	oacs: OacSummary[];
	creating: boolean;
	onChange: (patch: Partial<DistributionForm>) => void;
}) {
	const ids = [...new Set(form.origins.map(originId).filter(Boolean))];
	const target = defaultTarget(form);
	return (
		<SpaceBetween size="l">
			<SpaceBetween size="m">
				<Text
					label="Comment"
					value={form.comment}
					onChange={(comment) => onChange({ comment })}
					constraint="Optional. Up to 128 characters."
				/>
				<Checkbox
					checked={form.enabled}
					onChange={({ detail }) => onChange({ enabled: detail.checked })}
					description="A disabled distribution does not serve viewers, and only a disabled one can be deleted."
				>
					Enabled
				</Checkbox>
				<FormField
					label="Alternate domain names (CNAMEs)"
					description="Optional. One per line; a name can belong to one distribution only."
				>
					<Textarea
						ariaLabel="Alternate domain names"
						rows={3}
						value={form.aliases}
						onChange={({ detail }) => onChange({ aliases: detail.value })}
					/>
				</FormField>
				<Text
					label="Default root object"
					value={form.defaultRootObject}
					onChange={(defaultRootObject) => onChange({ defaultRootObject })}
					constraint="Optional, for example index.html, without a leading slash."
				/>
			</SpaceBetween>
			<SpaceBetween size="m">
				<Box variant="h3">Origins</Box>
				{form.origins.map((origin, i) => (
					<OriginFields
						// biome-ignore lint/suspicious/noArrayIndexKey: editable rows have no identity.
						key={i}
						origin={origin}
						index={i}
						oacs={oacs}
						canRemove={form.origins.length > 1}
						onChange={(next) =>
							onChange({
								origins: form.origins.map((o, j) => (j === i ? next : o)),
							})
						}
						onRemove={() =>
							onChange({ origins: form.origins.filter((_, j) => j !== i) })
						}
					/>
				))}
				<Button
					onClick={() =>
						onChange({ origins: [...form.origins, emptyOrigin()] })
					}
				>
					Add origin
				</Button>
			</SpaceBetween>
			<Container header={<Header variant="h3">Default cache behavior</Header>}>
				<SpaceBetween size="m">
					<Pick
						label="Target origin"
						value={ids.includes(target) ? target : ""}
						placeholder="Choose an origin"
						options={ids.map((value) => ({ value }))}
						onChange={(value) => onChange({ target: value })}
					/>
					<Pick
						label="Viewer protocol policy"
						value={form.viewerProtocol}
						placeholder="Choose a viewer protocol policy"
						options={viewerProtocolPolicies.map((value) => ({ value }))}
						onChange={(viewerProtocol) => onChange({ viewerProtocol })}
					/>
					<Pick
						label="Allowed HTTP methods"
						value={form.methods}
						placeholder="Choose the allowed methods"
						options={methodChoices.map((c) => ({
							value: c.value,
							label: c.label,
						}))}
						onChange={(methods) => onChange({ methods })}
					/>
					<Checkbox
						checked={form.compress}
						onChange={({ detail }) => onChange({ compress: detail.checked })}
					>
						Compress objects automatically
					</Checkbox>
					<Text
						label="Cache policy ID"
						value={form.cachePolicyId}
						onChange={(cachePolicyId) => onChange({ cachePolicyId })}
						description={`Optional. Managed policies: ${managedCachePolicies
							.map((p) => `${p.name} (${p.id})`)
							.join(", ")}. TinyStack does not evaluate cache policies.`}
					/>
				</SpaceBetween>
			</Container>
			{creating && (
				<TagRowsField
					rows={form.tags}
					onChange={(tags) => onChange({ tags })}
				/>
			)}
		</SpaceBetween>
	);
}
