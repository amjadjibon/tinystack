import { Alert, Box, SpaceBetween } from "@cloudscape-design/components";
import { useRef, useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import {
	type CloudFrontScope,
	cloudfrontCall,
	deleteDistribution,
	type OacSummary,
	updateDistribution,
} from "#/lib/cloudfront";
import {
	callerReference,
	createDistributionInput,
	type DistributionConfig,
	type DistributionForm,
	distributionConfig,
	distributionForm,
	emptyDistributionForm,
} from "#/lib/cloudfrontHelpers";
import { DistributionFields } from "./DistributionFields";

export function CreateDistributionDialog({
	scope,
	oacs,
	onClose,
	onSaved,
}: {
	scope: CloudFrontScope;
	oacs: OacSummary[];
	onClose: () => void;
	onSaved: (id: string) => void;
}) {
	const [form, setForm] = useState(emptyDistributionForm);
	const created = useRef("");
	return (
		<ActionDialog
			title="Create distribution"
			submit="Create distribution"
			onClose={onClose}
			onSaved={() => onSaved(created.current)}
			plan={async () => {
				const input = createDistributionInput(form, callerReference("console"));
				return [
					{
						label: "CreateDistributionWithTags",
						run: async () => {
							const out = await cloudfrontCall<{
								Distribution: { Id: string };
							}>(scope, "CreateDistributionWithTags", input);
							created.current = out.Distribution.Id;
						},
					},
				];
			}}
		>
			<SpaceBetween size="m">
				<Alert type="info">
					TinyStack keeps distributions as records: it does not cache or proxy
					requests, and a new distribution is deployed at once.
				</Alert>
				<DistributionFields
					form={form}
					oacs={oacs}
					creating
					onChange={(patch) => setForm((f) => ({ ...f, ...patch }))}
				/>
			</SpaceBetween>
		</ActionDialog>
	);
}

export function EditDistributionDialog({
	scope,
	id,
	config,
	oacs,
	onClose,
	onSaved,
}: {
	scope: CloudFrontScope;
	id: string;
	config: DistributionConfig;
	oacs: OacSummary[];
	onClose: () => void;
	onSaved: () => void;
}) {
	const [form, setForm] = useState<DistributionForm>(() =>
		distributionForm(config),
	);
	return (
		<ActionDialog
			title={`Edit distribution ${id}`}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				// Check the form against the settings it was loaded from before changing anything.
				distributionConfig(config, form);
				return [
					{
						label: "UpdateDistribution",
						run: () =>
							updateDistribution(scope, id, (latest) =>
								distributionConfig(latest, form),
							),
					},
				];
			}}
		>
			<SpaceBetween size="m">
				<Alert type="info">
					Settings this form does not show, such as extra cache behaviors,
					certificates and error pages, are kept as they are.
				</Alert>
				<DistributionFields
					form={form}
					oacs={oacs}
					creating={false}
					onChange={(patch) => setForm((f) => ({ ...f, ...patch }))}
				/>
			</SpaceBetween>
		</ActionDialog>
	);
}

export function ToggleDistributionDialog({
	scope,
	id,
	enable,
	onClose,
	onSaved,
}: {
	scope: CloudFrontScope;
	id: string;
	enable: boolean;
	onClose: () => void;
	onSaved: () => void;
}) {
	const verb = enable ? "Enable" : "Disable";
	return (
		<ActionDialog
			title={`${verb} distribution`}
			submit={`${verb} distribution`}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => [
				{
					label: `${verb} distribution`,
					run: () =>
						updateDistribution(scope, id, (config) => ({
							...config,
							Enabled: enable,
						})),
				},
			]}
		>
			<Alert type="info">
				{enable
					? `Distribution ${id} serves viewers again once it is enabled.`
					: `Distribution ${id} stops serving viewers once it is disabled. A distribution must be disabled before it can be deleted.`}
			</Alert>
		</ActionDialog>
	);
}

export function DeleteDistributionDialog({
	scope,
	id,
	enabled,
	onClose,
	onSaved,
}: {
	scope: CloudFrontScope;
	id: string;
	enabled: boolean;
	onClose: () => void;
	onSaved: () => void;
}) {
	return (
		<ActionDialog
			title="Delete distribution"
			submit="Delete distribution"
			confirmation={id}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				if (enabled) throw new Error(`Distribution ${id} is still enabled.`);
				return [
					{
						label: "DeleteDistribution",
						run: () => deleteDistribution(scope, id),
					},
				];
			}}
		>
			<SpaceBetween size="m">
				{enabled && (
					<Alert type="warning" header="Disable this distribution first">
						Distribution {id} is enabled. CloudFront deletes only disabled
						distributions, so choose Disable distribution on the distribution's
						page and then delete it.
					</Alert>
				)}
				<Box>Delete distribution {id}. This cannot be undone.</Box>
			</SpaceBetween>
		</ActionDialog>
	);
}
