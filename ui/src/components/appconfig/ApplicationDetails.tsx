import {
	Container,
	Header,
	SpaceBetween,
	Tabs,
} from "@cloudscape-design/components";
import { useNavigate } from "@tanstack/react-router";
import { useState } from "react";
import { Action, Buttons } from "#/components/containers/Controls";
import { Copy } from "#/components/efs/Shared";
import { Details } from "#/components/elbv2/Shared";
import {
	type AppConfigScope,
	type AppConfigTab,
	appconfigHref,
} from "#/lib/appconfig";
import { type Application, applicationArn } from "#/lib/appconfigHelpers";
import { EditApplicationDialog } from "./ApplicationDialogs";
import { Environments } from "./Environments";
import { FetchConfig } from "./FetchConfig";
import { Profiles } from "./Profiles";
import { DeleteDialog, ResourceTags } from "./Shared";

type Dialog = "" | "edit" | "delete";

export type ApplicationTab = Extract<
	AppConfigTab,
	"environments" | "profiles" | "fetch"
>;

export function ApplicationDetails({
	scope,
	application,
	enabled,
	tab,
	onChanged,
	onBack,
	onDeleted,
}: {
	scope: AppConfigScope;
	application: Application;
	enabled: boolean;
	tab: ApplicationTab;
	// onChanged reloads the application and the resource inventory.
	onChanged: () => void;
	onBack: () => void;
	// onDeleted runs once the resource is gone, with nothing left to reload.
	onDeleted: () => void;
}) {
	const navigate = useNavigate();
	const [dialog, setDialog] = useState<Dialog>("");
	const name = application.Name ?? application.Id;
	const arn = applicationArn(scope, application.Id);
	const open = (next: ApplicationTab) =>
		navigate({
			href: appconfigHref({ ...scope, application: application.Id, tab: next }),
		});
	return (
		<SpaceBetween size="l">
			<Header
				variant="h1"
				description={application.Description || undefined}
				actions={
					<Buttons>
						<Action onClick={onChanged}>Refresh</Action>
						<Action onClick={onBack}>Back to applications</Action>
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
							"Application ID",
							<Copy key="id" text={application.Id} label="ID" />,
						],
						["ARN", <Copy key="arn" text={arn} label="ARN" />],
						["Name", application.Name],
						["Description", application.Description],
					]}
				/>
			</Container>
			<ResourceTags scope={scope} arn={arn} enabled={enabled} />
			<Tabs
				activeTabId={tab}
				onChange={({ detail }) => open(detail.activeTabId as ApplicationTab)}
				tabs={[
					{
						id: "environments",
						label: "Environments",
						content:
							tab === "environments" ? (
								<Environments
									scope={scope}
									application={application}
									enabled={enabled}
									onChanged={onChanged}
								/>
							) : null,
					},
					{
						id: "profiles",
						label: "Configuration profiles",
						content:
							tab === "profiles" ? (
								<Profiles
									scope={scope}
									application={application}
									enabled={enabled}
									onChanged={onChanged}
								/>
							) : null,
					},
					{
						id: "fetch",
						label: "Fetch configuration",
						content:
							tab === "fetch" ? (
								<FetchConfig
									scope={scope}
									application={application}
									enabled={enabled}
								/>
							) : null,
					},
				]}
			/>
			{dialog === "edit" && (
				<EditApplicationDialog
					scope={scope}
					application={application}
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
					title="Delete application"
					noun="application"
					confirmation={name}
					operation="DeleteApplication"
					input={{ ApplicationId: application.Id }}
					warning={`Delete ${name} permanently. An application that still has environments or configuration profiles cannot be deleted, so delete them first.`}
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
