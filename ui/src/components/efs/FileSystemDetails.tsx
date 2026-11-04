import {
	Alert,
	Container,
	Header,
	SpaceBetween,
	StatusIndicator,
} from "@cloudscape-design/components";
import { useState } from "react";
import { Action, Buttons } from "#/components/containers/Controls";
import { Details } from "#/components/elbv2/Shared";
import { type EFSScope, getFileSystem } from "#/lib/efs";
import {
	bytesText,
	lifeCycleStatusType,
	sizeText,
	throughputText,
} from "#/lib/efsHelpers";
import { unixTime } from "#/lib/format";
import { usePoll } from "#/lib/usePoll";
import { AccessPoints } from "./AccessPoints";
import {
	DeleteFileSystemDialog,
	EditFileSystemDialog,
} from "./FileSystemDialogs";
import { MountTargets } from "./MountTargets";
import { Copy, ResourceTags } from "./Shared";

type Dialog = "" | "edit" | "delete";

export function FileSystemDetails({
	scope,
	id,
	enabled,
	onChanged,
	onBack,
}: {
	scope: EFSScope;
	id: string;
	enabled: boolean;
	onChanged: () => void;
	onBack: () => void;
}) {
	const [dialog, setDialog] = useState<Dialog>("");
	const poll = usePoll(() =>
		enabled ? getFileSystem(scope, id) : Promise.resolve(undefined),
	);
	const fs = poll.data;
	// Mount targets and tags change what the description shows, such as the number of mount targets.
	const refresh = () => {
		poll.reload();
		onChanged();
	};
	const edited = () => {
		setDialog("");
		refresh();
	};
	return (
		<SpaceBetween size="l">
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			{!enabled && (
				<Alert type="info">
					Connect a console session to load file system details.
				</Alert>
			)}
			<Header
				variant="h1"
				description={fs?.Name ? id : undefined}
				actions={
					<Buttons>
						<Action onClick={poll.reload}>Refresh</Action>
						<Action onClick={onBack}>Back to file systems</Action>
						<Action
							disabled={!enabled || !fs}
							onClick={() => setDialog("edit")}
						>
							Edit
						</Action>
						<Action
							disabled={!enabled || !fs}
							onClick={() => setDialog("delete")}
						>
							Delete
						</Action>
					</Buttons>
				}
			>
				{fs?.Name || id}
			</Header>
			{fs && (
				<Container header={<Header variant="h2">General</Header>}>
					<Details
						items={[
							["File system ID", <Copy key="id" text={id} label="ID" />],
							["ARN", <Copy key="arn" text={fs.FileSystemArn} label="ARN" />],
							["Name", fs.Name],
							[
								"State",
								<StatusIndicator
									key="state"
									type={lifeCycleStatusType(fs.LifeCycleState)}
								>
									{fs.LifeCycleState ?? "-"}
								</StatusIndicator>,
							],
							["Creation token", fs.CreationToken],
							["Owner", fs.OwnerId],
							["Created", unixTime(fs.CreationTime)],
							["Performance mode", fs.PerformanceMode],
							["Throughput mode", throughputText(fs)],
							["Encrypted", fs.Encrypted ? "Yes" : "No"],
							["KMS key", fs.KmsKeyId],
							["Size", sizeText(fs.SizeInBytes)],
							["Size in Standard", bytesText(fs.SizeInBytes?.ValueInStandard)],
							[
								"Size in Infrequent Access",
								bytesText(fs.SizeInBytes?.ValueInIA),
							],
							["Size in Archive", bytesText(fs.SizeInBytes?.ValueInArchive)],
							["Mount targets", String(fs.NumberOfMountTargets ?? 0)],
						]}
					/>
				</Container>
			)}
			{fs && (
				<ResourceTags
					scope={scope}
					id={id}
					tags={fs.Tags}
					enabled={enabled}
					onChanged={refresh}
				/>
			)}
			{fs && (
				<MountTargets
					scope={scope}
					fileSystem={id}
					enabled={enabled}
					onChanged={refresh}
				/>
			)}
			{fs && (
				<AccessPoints
					scope={scope}
					enabled={enabled}
					fileSystem={id}
					onChanged={onChanged}
				/>
			)}
			{dialog === "edit" && fs && (
				<EditFileSystemDialog
					scope={scope}
					fileSystem={fs}
					onClose={() => setDialog("")}
					onSaved={edited}
				/>
			)}
			{dialog === "delete" && fs && (
				<DeleteFileSystemDialog
					scope={scope}
					fileSystem={fs}
					onClose={() => setDialog("")}
					onSaved={() => {
						setDialog("");
						onChanged();
						onBack();
					}}
				/>
			)}
		</SpaceBetween>
	);
}
