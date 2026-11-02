import {
	Alert,
	Box,
	Button,
	Modal,
	SpaceBetween,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { CodeBlock } from "#/components/CodeBlock";
import { ConsoleTags } from "#/components/ConsoleTags";
import { Field } from "#/components/containers/Controls";
import { type CloudTrailScope, cloudtrailCall } from "#/lib/cloudtrail";
import { selectorInput } from "#/lib/cloudtrailHelpers";
import { unixTime } from "#/lib/format";
import { usePoll } from "#/lib/usePoll";

type Selectors = {
	EventSelectors?: unknown[];
	AdvancedEventSelectors?: unknown[];
};
export function TrailDetails({
	scope,
	name,
	enabled,
	onClose,
}: {
	scope: CloudTrailScope;
	name: string;
	enabled: boolean;
	onClose: () => void;
}) {
	const poll = usePoll(async () => {
		const [status, selectors, tags] = await Promise.all([
			cloudtrailCall<{
				IsLogging?: boolean;
				StartLoggingTime?: number;
				StopLoggingTime?: number;
			}>(scope, "GetTrailStatus", { Name: name }),
			cloudtrailCall<Selectors>(scope, "GetEventSelectors", {
				TrailName: name,
			}),
			cloudtrailCall<{
				ResourceTagList?: { TagsList?: { Key: string; Value: string }[] }[];
			}>(scope, "ListTags", { ResourceIdList: [name] }),
		]);
		return {
			status,
			selectors,
			tags: Object.fromEntries(
				(tags.ResourceTagList?.[0]?.TagsList ?? []).map((t) => [
					t.Key,
					t.Value,
				]),
			),
		};
	});
	const [editing, setEditing] = useState<string>();
	const selectors = poll.data?.selectors;
	const text = JSON.stringify(
		selectors?.AdvancedEventSelectors?.length
			? { AdvancedEventSelectors: selectors.AdvancedEventSelectors }
			: { EventSelectors: selectors?.EventSelectors ?? [] },
		null,
		2,
	);
	return (
		<>
			<Modal
				visible={editing === undefined}
				size="large"
				header={`Trail: ${name}`}
				closeAriaLabel="Close trail details"
				onDismiss={onClose}
			>
				<SpaceBetween size="m">
					{poll.error && <Alert type="error">{poll.error}</Alert>}
					{poll.data && (
						<>
							<Box>
								Logging: {poll.data.status.IsLogging ? "Started" : "Stopped"}
							</Box>
							<Box>
								Logging started: {unixTime(poll.data.status.StartLoggingTime)}
							</Box>
							<Box>
								Logging stopped: {unixTime(poll.data.status.StopLoggingTime)}
							</Box>
							<Alert type="info">
								Logging stores status only; no log files are delivered. Event
								selectors are stored configuration and do not filter event
								history.
							</Alert>
							<CodeBlock text={text} />
							<Button disabled={!enabled} onClick={() => setEditing(text)}>
								Edit event selectors
							</Button>
							<ConsoleTags
								tags={poll.data.tags}
								enabled={enabled}
								onChanged={poll.reload}
								plan={(next, removed) => [
									...(Object.keys(next).length
										? [
												{
													label: "AddTags",
													run: () =>
														cloudtrailCall(scope, "AddTags", {
															ResourceId: name,
															TagsList: Object.entries(next).map(
																([Key, Value]) => ({ Key, Value }),
															),
														}),
												},
											]
										: []),
									...(removed.length
										? [
												{
													label: "RemoveTags",
													run: () =>
														cloudtrailCall(scope, "RemoveTags", {
															ResourceId: name,
															TagsList: removed.map((Key) => ({ Key })),
														}),
												},
											]
										: []),
								]}
							/>
						</>
					)}
				</SpaceBetween>
			</Modal>
			{editing !== undefined && (
				<ActionDialog
					title="Edit event selectors"
					onClose={() => setEditing(undefined)}
					onSaved={() => {
						setEditing(undefined);
						poll.reload();
					}}
					plan={async () => {
						const input = selectorInput(editing);
						return [
							{
								label: "PutEventSelectors",
								run: () =>
									cloudtrailCall(scope, "PutEventSelectors", {
										TrailName: name,
										...input,
									}),
							},
						];
					}}
				>
					<SpaceBetween size="m">
						<Box>
							Enter either EventSelectors or AdvancedEventSelectors (at most
							five). Basic selectors accept ReadWriteType,
							IncludeManagementEvents, DataResources and
							ExcludeManagementEventSources. Advanced selectors accept Name and
							FieldSelectors.
						</Box>
						<Field
							label="Event selectors (JSON)"
							multiline
							value={editing}
							onChange={setEditing}
						/>
					</SpaceBetween>
				</ActionDialog>
			)}
		</>
	);
}
