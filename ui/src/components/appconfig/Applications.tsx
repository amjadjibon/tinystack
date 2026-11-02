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
	listApplications,
} from "#/lib/appconfig";
import type { Application } from "#/lib/appconfigHelpers";
import { useFollow } from "#/lib/useFollow";
import { usePoll } from "#/lib/usePoll";
import { CreateApplicationDialog } from "./ApplicationDialogs";

export function Applications({
	scope,
	enabled,
	onCreated,
}: {
	scope: AppConfigScope;
	enabled: boolean;
	onCreated: (id: string) => void;
}) {
	const follow = useFollow();
	const [creating, setCreating] = useState(false);
	const poll = usePoll(
		() => (enabled ? listApplications(scope) : Promise.resolve([])),
		0,
	);
	return (
		<SpaceBetween size="m">
			{poll.error && <Alert type="error">{poll.error}</Alert>}
			<Rows<Application>
				title="Applications"
				emptyText="Choose Create application to add one."
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
							Create application
						</Button>
					</Buttons>
				}
				columns={[
					{
						id: "name",
						header: "Name",
						cell: (a) => (
							<Link
								href={appconfigHref({ ...scope, application: a.Id })}
								onFollow={follow}
							>
								{a.Name ?? a.Id}
							</Link>
						),
					},
					{
						id: "id",
						header: "Application ID",
						cell: (a) => <Copy text={a.Id} label="ID" />,
					},
					{
						id: "description",
						header: "Description",
						cell: (a) => a.Description || "-",
					},
				]}
			/>
			{creating && (
				<CreateApplicationDialog
					scope={scope}
					onClose={() => setCreating(false)}
					onSaved={(id) => {
						setCreating(false);
						onCreated(id);
					}}
				/>
			)}
		</SpaceBetween>
	);
}
