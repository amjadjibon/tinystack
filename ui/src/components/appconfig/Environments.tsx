import {
	Alert,
	Button,
	Link,
	SpaceBetween,
	StatusIndicator,
} from "@cloudscape-design/components";
import { useState } from "react";
import { Action, Buttons, Rows } from "#/components/containers/Controls";
import { Copy } from "#/components/efs/Shared";
import {
	type AppConfigScope,
	appconfigHref,
	listEnvironments,
} from "#/lib/appconfig";
import {
	type Application,
	type Environment,
	stateStatusType,
} from "#/lib/appconfigHelpers";
import { useFollow } from "#/lib/useFollow";
import { usePoll } from "#/lib/usePoll";
import { CreateEnvironmentDialog } from "./EnvironmentDialogs";

export function Environments({
	scope,
	application,
	enabled,
	onChanged,
}: {
	scope: AppConfigScope;
	application: Application;
	enabled: boolean;
	onChanged: () => void;
}) {
	const follow = useFollow();
	const [creating, setCreating] = useState(false);
	const poll = usePoll(
		() =>
			enabled ? listEnvironments(scope, application.Id) : Promise.resolve([]),
		0,
	);
	return (
		<SpaceBetween size="m">
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			<Rows<Environment>
				title="Environments"
				emptyText="Choose Create environment to add one."
				items={poll.data ?? []}
				loading={poll.loading}
				actions={
					<Buttons>
						<Action onClick={poll.reload}>Refresh</Action>
						<Button
							variant="primary"
							disabled={!enabled}
							onClick={() => setCreating(true)}
						>
							Create environment
						</Button>
					</Buttons>
				}
				columns={[
					{
						id: "name",
						header: "Name",
						cell: (e) => (
							<Link
								href={appconfigHref({
									...scope,
									application: application.Id,
									environment: e.Id,
								})}
								onFollow={follow}
							>
								{e.Name ?? e.Id}
							</Link>
						),
					},
					{
						id: "id",
						header: "Environment ID",
						cell: (e) => <Copy text={e.Id} label="ID" />,
					},
					{
						id: "state",
						header: "State",
						cell: (e) => (
							<StatusIndicator type={stateStatusType(e.State)}>
								{e.State ?? "-"}
							</StatusIndicator>
						),
					},
					{
						id: "description",
						header: "Description",
						cell: (e) => e.Description || "-",
					},
				]}
			/>
			{creating && (
				<CreateEnvironmentDialog
					scope={scope}
					applicationId={application.Id}
					onClose={() => setCreating(false)}
					onSaved={() => {
						setCreating(false);
						poll.reload();
						onChanged();
					}}
				/>
			)}
		</SpaceBetween>
	);
}
