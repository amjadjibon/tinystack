import { Alert, Button, SpaceBetween } from "@cloudscape-design/components";
import { useState } from "react";
import { Action, Buttons, Rows } from "#/components/containers/Controls";
import { type AppConfigScope, listVersions } from "#/lib/appconfig";
import {
	type Application,
	type ConfigurationProfile,
	emptyVersionForm,
	type VersionForm,
	type VersionSummary,
} from "#/lib/appconfigHelpers";
import { usePoll } from "#/lib/usePoll";
import { DeleteDialog } from "./Shared";
import { CreateVersionDialog, ViewVersionDialog } from "./VersionDialogs";

export function Versions({
	scope,
	application,
	profile,
	enabled,
	onChanged,
}: {
	scope: AppConfigScope;
	application: Application;
	profile: ConfigurationProfile;
	enabled: boolean;
	onChanged: () => void;
}) {
	const poll = usePoll(
		() =>
			enabled
				? listVersions(scope, application.Id, profile.Id)
				: Promise.resolve([]),
		0,
	);
	// A form opens the editor, empty or copied from a version.
	const [editing, setEditing] = useState<VersionForm>();
	const [viewing, setViewing] = useState<number>();
	const [deleting, setDeleting] = useState<VersionSummary>();
	const changed = () => {
		poll.reload();
		onChanged();
	};
	return (
		<SpaceBetween size="m">
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			<Rows<VersionSummary>
				title="Hosted configuration versions"
				emptyText="Choose Create version to add one."
				items={poll.data ?? []}
				loading={poll.loading}
				actions={
					<Buttons>
						<Action onClick={poll.reload}>Refresh</Action>
						<Button
							variant="primary"
							disabled={!enabled}
							onClick={() => setEditing(emptyVersionForm())}
						>
							Create version
						</Button>
					</Buttons>
				}
				columns={[
					{
						id: "version",
						header: "Version",
						cell: (v) => (
							<Button
								variant="link"
								onClick={() => setViewing(v.VersionNumber)}
							>
								{v.VersionNumber}
							</Button>
						),
					},
					{ id: "label", header: "Label", cell: (v) => v.VersionLabel || "-" },
					{
						id: "type",
						header: "Content type",
						cell: (v) => v.ContentType || "-",
					},
					{
						id: "description",
						header: "Description",
						cell: (v) => v.Description || "-",
					},
					{
						id: "actions",
						header: "Actions",
						minWidth: 190,
						cell: (v) => (
							<Buttons>
								<Action onClick={() => setViewing(v.VersionNumber)}>
									View
								</Action>
								<Action disabled={!enabled} onClick={() => setDeleting(v)}>
									Delete
								</Action>
							</Buttons>
						),
					},
				]}
			/>
			{editing && (
				<CreateVersionDialog
					scope={scope}
					application={application}
					profile={profile}
					initial={editing}
					onClose={() => setEditing(undefined)}
					onSaved={() => {
						setEditing(undefined);
						changed();
					}}
				/>
			)}
			{viewing !== undefined && (
				<ViewVersionDialog
					key={viewing}
					scope={scope}
					application={application.Id}
					profile={profile.Id}
					number={viewing}
					onClose={() => setViewing(undefined)}
					onCopy={(form) => {
						setViewing(undefined);
						setEditing(form);
					}}
				/>
			)}
			{deleting && (
				<DeleteDialog
					scope={scope}
					title={`Delete version ${deleting.VersionNumber}`}
					noun="version"
					confirmation={String(deleting.VersionNumber)}
					operation="DeleteHostedConfigurationVersion"
					input={{
						ApplicationId: application.Id,
						ConfigurationProfileId: profile.Id,
						VersionNumber: deleting.VersionNumber,
					}}
					warning={`Delete version ${deleting.VersionNumber} permanently. A version that an environment is running cannot be deleted; stop its deployment first.`}
					onClose={() => setDeleting(undefined)}
					onSaved={() => {
						setDeleting(undefined);
						changed();
					}}
				/>
			)}
		</SpaceBetween>
	);
}
