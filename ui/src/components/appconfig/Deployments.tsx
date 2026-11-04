import {
	Alert,
	Button,
	SpaceBetween,
	StatusIndicator,
} from "@cloudscape-design/components";
import { useEffect, useRef, useState } from "react";
import { Action, Buttons, Rows } from "#/components/containers/Controls";
import { type AppConfigScope, listDeployments } from "#/lib/appconfig";
import {
	type Application,
	canStop,
	type DeploymentSummary,
	type Environment,
	hasActiveDeployments,
	percentText,
	stateStatusType,
	versionText,
} from "#/lib/appconfigHelpers";
import { isoTime } from "#/lib/format";
import { usePoll } from "#/lib/usePoll";
import {
	DeploymentDetails,
	StartDeploymentDialog,
	StopDeploymentDialog,
} from "./DeploymentDialogs";

export function Deployments({
	scope,
	application,
	environment,
	enabled,
	onChanged,
}: {
	scope: AppConfigScope;
	application: Application;
	environment: Environment;
	enabled: boolean;
	// A deployment changes the environment's state, which the page above shows.
	onChanged: () => void;
}) {
	// A deployment that is still moving is watched until it settles.
	const [active, setActive] = useState(false);
	const poll = usePoll(
		async () => {
			const items = enabled
				? await listDeployments(scope, application.Id, environment.Id)
				: [];
			setActive(hasActiveDeployments(items));
			return items;
		},
		active ? 3000 : 0,
	);
	// The environment is in a new state once the deployment it was watching has settled.
	const wasActive = useRef(false);
	useEffect(() => {
		if (wasActive.current && !active) onChanged();
		wasActive.current = active;
	}, [active, onChanged]);
	const [starting, setStarting] = useState(false);
	const [viewing, setViewing] = useState<number>();
	const [stopping, setStopping] = useState<DeploymentSummary>();
	const deployments = poll.data ?? [];
	const changed = () => {
		poll.reload();
		onChanged();
	};
	return (
		<SpaceBetween size="m">
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			<Rows<DeploymentSummary>
				title="Deployments"
				emptyText="Choose Start deployment to deploy a hosted configuration version."
				items={deployments}
				loading={poll.loading}
				actions={
					<Buttons>
						<Action onClick={poll.reload}>Refresh</Action>
						<Button
							variant="primary"
							disabled={!enabled}
							onClick={() => setStarting(true)}
						>
							Start deployment
						</Button>
					</Buttons>
				}
				columns={[
					{
						id: "number",
						header: "Deployment",
						cell: (d) => (
							<Button
								variant="link"
								onClick={() => setViewing(d.DeploymentNumber)}
							>
								{d.DeploymentNumber}
							</Button>
						),
					},
					{
						id: "state",
						header: "State",
						cell: (d) => (
							<StatusIndicator type={stateStatusType(d.State)}>
								{d.State ?? "-"}
							</StatusIndicator>
						),
					},
					{
						id: "complete",
						header: "Complete",
						cell: (d) => percentText(d.PercentageComplete),
					},
					{
						id: "configuration",
						header: "Configuration profile",
						cell: (d) => d.ConfigurationName || "-",
					},
					{
						id: "version",
						header: "Version",
						cell: (d) => versionText(d.ConfigurationVersion, d.VersionLabel),
					},
					{
						id: "started",
						header: "Started",
						cell: (d) => isoTime(d.StartedAt),
					},
					{
						id: "actions",
						header: "Actions",
						minWidth: 110,
						cell: (d) =>
							canStop(d, deployments) ? (
								<Button disabled={!enabled} onClick={() => setStopping(d)}>
									Stop
								</Button>
							) : (
								"-"
							),
					},
				]}
			/>
			{starting && (
				<StartDeploymentDialog
					scope={scope}
					application={application}
					environment={environment}
					onClose={() => setStarting(false)}
					onSaved={() => {
						setStarting(false);
						changed();
					}}
				/>
			)}
			{viewing !== undefined && (
				<DeploymentDetails
					key={viewing}
					scope={scope}
					application={application.Id}
					environment={environment.Id}
					number={viewing}
					onClose={() => setViewing(undefined)}
				/>
			)}
			{stopping && (
				<StopDeploymentDialog
					scope={scope}
					application={application}
					environment={environment}
					deployment={stopping}
					onClose={() => setStopping(undefined)}
					onSaved={() => {
						setStopping(undefined);
						changed();
					}}
				/>
			)}
		</SpaceBetween>
	);
}
