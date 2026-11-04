import {
	Alert,
	Box,
	Modal,
	SpaceBetween,
	StatusIndicator,
} from "@cloudscape-design/components";
import { useEffect, useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { Copy } from "#/components/efs/Shared";
import { Details } from "#/components/elbv2/Shared";
import { Pick, Text } from "#/components/memorydb/Fields";
import {
	type AppConfigScope,
	appconfigCall,
	getDeployment,
	listProfiles,
	listStrategies,
	listVersions,
} from "#/lib/appconfig";
import {
	type Application,
	type DeploymentForm,
	type DeploymentSummary,
	deploymentInProgress,
	type Environment,
	emptyDeploymentForm,
	percentText,
	rolloutText,
	startDeploymentInput,
	stateStatusType,
	stopDeploymentInput,
	strategyLabel,
	versionText,
} from "#/lib/appconfigHelpers";
import { isoTime } from "#/lib/format";
import { usePoll } from "#/lib/usePoll";

export function StartDeploymentDialog({
	scope,
	application,
	environment,
	onClose,
	onSaved,
}: {
	scope: AppConfigScope;
	application: Application;
	environment: Environment;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [form, setForm] = useState(emptyDeploymentForm);
	const set = (patch: Partial<DeploymentForm>) =>
		setForm((f) => ({ ...f, ...patch }));
	const profiles = usePoll(() => listProfiles(scope, application.Id), 0);
	const strategies = usePoll(() => listStrategies(scope), 0);
	// The versions come with the profile they were listed for, so the ones of a
	// profile chosen before are never offered for the next.
	const versions = usePoll(async () => {
		const profile = form.profile;
		return {
			profile,
			items: profile ? await listVersions(scope, application.Id, profile) : [],
		};
	}, 0);
	useEffect(() => {
		if (form.profile) versions.reload();
	}, [form.profile, versions.reload]);
	const versionItems =
		versions.data?.profile === form.profile ? versions.data.items : undefined;
	const error = profiles.error || strategies.error || versions.error;
	return (
		<ActionDialog
			title={`Start deployment to ${environment.Name ?? environment.Id}`}
			submit="Start deployment"
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const input = startDeploymentInput(
					application.Id,
					environment.Id,
					form,
				);
				return [
					{
						label: "StartDeployment",
						run: () => appconfigCall(scope, "StartDeployment", input),
					},
				];
			}}
		>
			<SpaceBetween size="m">
				<Alert type="info">
					TinyStack completes a deployment at once, whatever its strategy. The
					strategy's settings are recorded with the deployment.
				</Alert>
				{!!error && <Alert type="error">{error}</Alert>}
				<Pick
					label="Configuration profile"
					value={form.profile}
					options={(profiles.data ?? []).map((p) => ({
						value: p.Id,
						label: p.Name ?? p.Id,
					}))}
					placeholder="Choose a configuration profile"
					onChange={(profile) => set({ profile, version: "" })}
				/>
				<Pick
					label="Hosted configuration version"
					value={form.version}
					disabled={!form.profile}
					options={(versionItems ?? []).map((v) => ({
						value: String(v.VersionNumber),
						label: versionText(v.VersionNumber, v.VersionLabel),
					}))}
					placeholder="Choose a version"
					description={
						versionItems && form.profile && !versionItems.length
							? "This profile has no hosted configuration versions yet."
							: undefined
					}
					onChange={(version) => set({ version })}
				/>
				<Pick
					label="Deployment strategy"
					value={form.strategy}
					options={(strategies.data ?? []).map((s) => ({
						value: s.Id,
						label: strategyLabel(s),
					}))}
					placeholder="Choose a deployment strategy"
					onChange={(strategy) => set({ strategy })}
				/>
				<Text
					label="Description"
					value={form.description}
					onChange={(description) => set({ description })}
					constraint="Optional, up to 1024 characters."
				/>
			</SpaceBetween>
		</ActionDialog>
	);
}

export function StopDeploymentDialog({
	scope,
	application,
	environment,
	deployment,
	onClose,
	onSaved,
}: {
	scope: AppConfigScope;
	application: Application;
	environment: Environment;
	deployment: DeploymentSummary;
	onClose: () => void;
	onSaved: () => void;
}) {
	return (
		<ActionDialog
			title={`Stop deployment ${deployment.DeploymentNumber}`}
			submit="Stop deployment"
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const input = stopDeploymentInput(
					application.Id,
					environment.Id,
					deployment,
				);
				return [
					{
						label: "StopDeployment",
						run: () => appconfigCall(scope, "StopDeployment", input),
					},
				];
			}}
		>
			<Alert type="warning">
				Stopping a completed deployment reverts it: the environment returns to
				the configuration version it ran before, or to none if there was no
				earlier deployment. Only the latest completed deployment can be stopped.
			</Alert>
		</ActionDialog>
	);
}

export function DeploymentDetails({
	scope,
	application,
	environment,
	number,
	onClose,
}: {
	scope: AppConfigScope;
	application: string;
	environment: string;
	number: number;
	onClose: () => void;
}) {
	const [active, setActive] = useState(false);
	const poll = usePoll(
		async () => {
			const out = await getDeployment(scope, application, environment, number);
			setActive(deploymentInProgress(out.State));
			return out;
		},
		active ? 3000 : 0,
	);
	const d = poll.data;
	return (
		<Modal
			visible
			size="large"
			header={`Deployment ${number}`}
			closeAriaLabel="Close deployment details"
			onDismiss={onClose}
		>
			<SpaceBetween size="m">
				{!!poll.error && <Alert type="error">{poll.error}</Alert>}
				{!d && !poll.error && <Box>Loading deployment…</Box>}
				{d && (
					<Details
						items={[
							["Deployment", String(d.DeploymentNumber ?? number)],
							[
								"State",
								<StatusIndicator key="state" type={stateStatusType(d.State)}>
									{d.State ?? "-"}
								</StatusIndicator>,
							],
							["Complete", percentText(d.PercentageComplete)],
							["Configuration profile", d.ConfigurationName],
							[
								"Profile ID",
								<Copy
									key="profile"
									text={d.ConfigurationProfileId}
									label="ID"
								/>,
							],
							["Version", versionText(d.ConfigurationVersion, d.VersionLabel)],
							["Source", d.ConfigurationLocationUri],
							[
								"Strategy ID",
								<Copy
									key="strategy"
									text={d.DeploymentStrategyId}
									label="ID"
								/>,
							],
							["Rollout", rolloutText(d)],
							["Description", d.Description],
							["Started", isoTime(d.StartedAt)],
							["Completed", isoTime(d.CompletedAt)],
						]}
					/>
				)}
				{d && (
					<Alert type="info">
						TinyStack completes a deployment at once and keeps no event log for
						it.
					</Alert>
				)}
			</SpaceBetween>
		</Modal>
	);
}
