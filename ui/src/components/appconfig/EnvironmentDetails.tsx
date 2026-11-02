import {
	Container,
	Header,
	SpaceBetween,
	StatusIndicator,
} from "@cloudscape-design/components";
import { useState } from "react";
import { Action, Buttons } from "#/components/containers/Controls";
import { Copy } from "#/components/efs/Shared";
import { Details } from "#/components/elbv2/Shared";
import type { AppConfigScope } from "#/lib/appconfig";
import {
	type Application,
	type Environment,
	stateStatusType,
} from "#/lib/appconfigHelpers";
import { Deployments } from "./Deployments";
import { EditEnvironmentDialog } from "./EnvironmentDialogs";
import { DeleteDialog } from "./Shared";

type Dialog = "" | "edit" | "delete";

export function EnvironmentDetails({
	scope,
	application,
	environment,
	enabled,
	onChanged,
	onBack,
	onDeleted,
}: {
	scope: AppConfigScope;
	application: Application;
	environment: Environment;
	enabled: boolean;
	// onChanged reloads the environment and the resource inventory.
	onChanged: () => void;
	onBack: () => void;
	// onDeleted runs once the resource is gone, with nothing left to reload.
	onDeleted: () => void;
}) {
	const [dialog, setDialog] = useState<Dialog>("");
	const name = environment.Name ?? environment.Id;
	return (
		<SpaceBetween size="l">
			<Header
				variant="h1"
				description={environment.Description || undefined}
				actions={
					<Buttons>
						<Action onClick={onChanged}>Refresh</Action>
						<Action onClick={onBack}>Back to application</Action>
						<Action disabled={!enabled} onClick={() => setDialog("edit")}>
							Edit
						</Action>
						<Action disabled={!enabled} onClick={() => setDialog("delete")}>
							Delete
						</Action>
					</Buttons>
				}
			>
				{name}
			</Header>
			<Container header={<Header variant="h2">General</Header>}>
				<Details
					items={[
						[
							"Environment ID",
							<Copy key="id" text={environment.Id} label="ID" />,
						],
						["Application", application.Name ?? application.Id],
						["Name", environment.Name],
						[
							"State",
							<StatusIndicator
								key="state"
								type={stateStatusType(environment.State)}
							>
								{environment.State ?? "-"}
							</StatusIndicator>,
						],
						["Description", environment.Description],
					]}
				/>
			</Container>
			<Deployments
				scope={scope}
				application={application}
				environment={environment}
				enabled={enabled}
				onChanged={onChanged}
			/>
			{dialog === "edit" && (
				<EditEnvironmentDialog
					scope={scope}
					applicationId={application.Id}
					environment={environment}
					onClose={() => setDialog("")}
					onSaved={() => {
						setDialog("");
						onChanged();
					}}
				/>
			)}
			{dialog === "delete" && (
				<DeleteDialog
					scope={scope}
					title="Delete environment"
					noun="environment"
					confirmation={name}
					operation="DeleteEnvironment"
					input={{
						ApplicationId: application.Id,
						EnvironmentId: environment.Id,
					}}
					warning={`Delete ${name} permanently, together with its deployment history. Clients that fetch configuration from it stop receiving it.`}
					onClose={() => setDialog("")}
					onSaved={() => {
						setDialog("");
						onDeleted();
					}}
				/>
			)}
		</SpaceBetween>
	);
}
