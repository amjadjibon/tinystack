import {
	Alert,
	Button,
	Link,
	SpaceBetween,
	StatusIndicator,
} from "@cloudscape-design/components";
import { useState } from "react";
import { Action, Buttons, Rows } from "#/components/containers/Controls";
import { type EFSScope, efsHref, listFileSystems } from "#/lib/efs";
import {
	type FileSystem,
	lifeCycleStatusType,
	sizeText,
	throughputText,
} from "#/lib/efsHelpers";
import { unixTime } from "#/lib/format";
import { useFollow } from "#/lib/useFollow";
import { usePoll } from "#/lib/usePoll";
import { CreateFileSystemDialog } from "./FileSystemDialogs";

export function FileSystems({
	scope,
	enabled,
	onCreated,
}: {
	scope: EFSScope;
	enabled: boolean;
	onCreated: (id: string) => void;
}) {
	const follow = useFollow();
	const [creating, setCreating] = useState(false);
	const poll = usePoll(() =>
		enabled ? listFileSystems(scope) : Promise.resolve([]),
	);
	return (
		<SpaceBetween size="m">
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			<Rows<FileSystem>
				title="File systems"
				emptyText="Choose Create file system to add one."
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
							Create file system
						</Button>
					</Buttons>
				}
				columns={[
					{
						id: "id",
						header: "File system ID",
						cell: (f) => (
							<Link
								href={efsHref({ ...scope, filesystem: f.FileSystemId })}
								onFollow={follow}
							>
								{f.FileSystemId}
							</Link>
						),
					},
					{ id: "name", header: "Name", cell: (f) => f.Name || "-" },
					{
						id: "state",
						header: "State",
						cell: (f) => (
							<StatusIndicator type={lifeCycleStatusType(f.LifeCycleState)}>
								{f.LifeCycleState ?? "-"}
							</StatusIndicator>
						),
					},
					{
						id: "performance",
						header: "Performance",
						cell: (f) => f.PerformanceMode || "-",
					},
					{
						id: "throughput",
						header: "Throughput",
						cell: (f) => throughputText(f),
					},
					{ id: "size", header: "Size", cell: (f) => sizeText(f.SizeInBytes) },
					{
						id: "encrypted",
						header: "Encrypted",
						cell: (f) => (f.Encrypted ? "Yes" : "No"),
					},
					{
						id: "created",
						header: "Created",
						cell: (f) => unixTime(f.CreationTime),
					},
				]}
			/>
			{creating && (
				<CreateFileSystemDialog
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
