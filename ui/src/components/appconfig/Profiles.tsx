import {
	Alert,
	Button,
	Link,
	SpaceBetween,
} from "@cloudscape-design/components";
import { useState } from "react";
import { Action, Buttons, Rows } from "#/components/containers/Controls";
import { Copy } from "#/components/efs/Shared";
import {
	type AppConfigScope,
	appconfigHref,
	listProfiles,
} from "#/lib/appconfig";
import type { Application, ProfileSummary } from "#/lib/appconfigHelpers";
import { useFollow } from "#/lib/useFollow";
import { usePoll } from "#/lib/usePoll";
import { CreateProfileDialog } from "./ProfileDialogs";

export function Profiles({
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
		() => (enabled ? listProfiles(scope, application.Id) : Promise.resolve([])),
		0,
	);
	return (
		<SpaceBetween size="m">
			{poll.error && <Alert type="error">{poll.error}</Alert>}
			<Rows<ProfileSummary>
				title="Configuration profiles"
				emptyText="Choose Create profile to add one."
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
							Create profile
						</Button>
					</Buttons>
				}
				columns={[
					{
						id: "name",
						header: "Name",
						cell: (p) => (
							<Link
								href={appconfigHref({
									...scope,
									application: application.Id,
									profile: p.Id,
								})}
								onFollow={follow}
							>
								{p.Name ?? p.Id}
							</Link>
						),
					},
					{
						id: "id",
						header: "Profile ID",
						cell: (p) => <Copy text={p.Id} label="ID" />,
					},
					{ id: "type", header: "Type", cell: (p) => p.Type || "-" },
					{ id: "source", header: "Source", cell: (p) => p.LocationUri || "-" },
					{
						id: "validators",
						header: "Validators",
						cell: (p) => p.ValidatorTypes?.join(", ") || "-",
					},
				]}
			/>
			{creating && (
				<CreateProfileDialog
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
