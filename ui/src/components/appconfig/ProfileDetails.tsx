import {
	Box,
	Container,
	Header,
	SpaceBetween,
} from "@cloudscape-design/components";
import { useState } from "react";
import { CodeBlock } from "#/components/CodeBlock";
import { Action, Buttons } from "#/components/containers/Controls";
import { Copy } from "#/components/efs/Shared";
import { Details } from "#/components/elbv2/Shared";
import type { AppConfigScope } from "#/lib/appconfig";
import {
	type Application,
	type ConfigurationProfile,
	prettyContent,
} from "#/lib/appconfigHelpers";
import { EditProfileDialog } from "./ProfileDialogs";
import { DeleteDialog } from "./Shared";
import { Versions } from "./Versions";

type Dialog = "" | "edit" | "delete";

export function ProfileDetails({
	scope,
	application,
	profile,
	enabled,
	onChanged,
	onBack,
	onDeleted,
}: {
	scope: AppConfigScope;
	application: Application;
	profile: ConfigurationProfile;
	enabled: boolean;
	// onChanged reloads the profile and the resource inventory.
	onChanged: () => void;
	onBack: () => void;
	// onDeleted runs once the resource is gone, with nothing left to reload.
	onDeleted: () => void;
}) {
	const [dialog, setDialog] = useState<Dialog>("");
	const name = profile.Name ?? profile.Id;
	const validators = profile.Validators ?? [];
	return (
		<SpaceBetween size="l">
			<Header
				variant="h1"
				description={profile.Description || undefined}
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
						["Profile ID", <Copy key="id" text={profile.Id} label="ID" />],
						["Application", application.Name ?? application.Id],
						["Name", profile.Name],
						["Type", profile.Type],
						["Source", profile.LocationUri],
						["Description", profile.Description],
					]}
				/>
			</Container>
			<Container
				header={
					<Header variant="h2" counter={`(${validators.length})`}>
						Validators
					</Header>
				}
			>
				{validators.length ? (
					<SpaceBetween size="m">
						{validators.map((v) => (
							<div key={`${v.Type}:${v.Content}`}>
								<Box variant="awsui-key-label">{v.Type}</Box>
								<CodeBlock
									text={
										v.Type === "JSON_SCHEMA"
											? prettyContent(v.Content ?? "")
											: (v.Content ?? "")
									}
								/>
							</div>
						))}
					</SpaceBetween>
				) : (
					<Box>No validators</Box>
				)}
			</Container>
			<Versions
				scope={scope}
				application={application}
				profile={profile}
				enabled={enabled}
				onChanged={onChanged}
			/>
			{dialog === "edit" && (
				<EditProfileDialog
					scope={scope}
					applicationId={application.Id}
					profile={profile}
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
					title="Delete configuration profile"
					noun="profile"
					confirmation={name}
					operation="DeleteConfigurationProfile"
					input={{
						ApplicationId: application.Id,
						ConfigurationProfileId: profile.Id,
					}}
					warning={`Delete ${name} permanently. A profile that still has hosted configuration versions cannot be deleted, so delete them first.`}
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
