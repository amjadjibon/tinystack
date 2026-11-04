import {
	Alert,
	ColumnLayout,
	Header,
	SpaceBetween,
} from "@cloudscape-design/components";
import { useRef, useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { TagRowsField } from "#/components/elbv2/Shared";
import { Pick, Text } from "#/components/memorydb/Fields";
import { callerReference } from "#/lib/cloudfrontHelpers";
import { type EFSScope, efsCall, listFileSystems } from "#/lib/efs";
import {
	type AccessPoint,
	type AccessPointForm,
	createAccessPointInput,
	emptyAccessPointForm,
	fileSystemLabel,
} from "#/lib/efsHelpers";
import { usePoll } from "#/lib/usePoll";

const idRange = "A whole number from 0 to 4294967295.";

// CreateAccessPointDialog creates an access point for a file system: the given one,
// or one the person picks when the dialog opens from the list of every access point.
export function CreateAccessPointDialog({
	scope,
	fileSystem,
	onClose,
	onSaved,
}: {
	scope: EFSScope;
	fileSystem?: string;
	onClose: () => void;
	onSaved: (id: string) => void;
}) {
	const fileSystems = usePoll(
		() => (fileSystem ? Promise.resolve([]) : listFileSystems(scope)),
		0,
	);
	const [form, setForm] = useState(() => emptyAccessPointForm(fileSystem));
	const created = useRef("");
	const set = (patch: Partial<AccessPointForm>) =>
		setForm((f) => ({ ...f, ...patch }));
	return (
		<ActionDialog
			title="Create access point"
			submit="Create access point"
			onClose={onClose}
			onSaved={() => onSaved(created.current)}
			plan={async () => {
				const input = createAccessPointInput(form, callerReference("console"));
				return [
					{
						label: "CreateAccessPoint",
						run: async () => {
							const out = await efsCall<AccessPoint>(
								scope,
								"CreateAccessPoint",
								input,
							);
							created.current = out.AccessPointId;
						},
					},
				];
			}}
		>
			<SpaceBetween size="l">
				<Alert type="info">
					An access point records a POSIX identity and a root directory for a
					file system. TinyStack does not create or check them on disk.
				</Alert>
				{!fileSystem && (
					<SpaceBetween size="s">
						{!!fileSystems.error && (
							<Alert type="error">{fileSystems.error}</Alert>
						)}
						<Pick
							label="File system"
							value={form.fileSystem}
							placeholder="Choose a file system"
							options={(fileSystems.data ?? []).map((f) => ({
								value: f.FileSystemId,
								label: fileSystemLabel(f),
							}))}
							onChange={(value) => set({ fileSystem: value })}
						/>
					</SpaceBetween>
				)}
				<Text
					label="Name"
					value={form.name}
					onChange={(name) => set({ name })}
					constraint="Optional. Stored as the Name tag."
				/>
				<SpaceBetween size="s">
					<Header
						variant="h3"
						description="Every request through the access point uses this identity. Leave both IDs empty to use the client's own."
					>
						POSIX user
					</Header>
					<ColumnLayout columns={2}>
						<Text
							label="User ID"
							value={form.uid}
							onChange={(uid) => set({ uid })}
							constraint={idRange}
						/>
						<Text
							label="Group ID"
							value={form.gid}
							onChange={(gid) => set({ gid })}
							constraint={idRange}
						/>
					</ColumnLayout>
					<Text
						label="Secondary group IDs"
						value={form.secondaryGids}
						onChange={(secondaryGids) => set({ secondaryGids })}
						constraint="Optional, at most 16, separated by commas or spaces."
					/>
				</SpaceBetween>
				<SpaceBetween size="s">
					<Header
						variant="h3"
						description="The directory that clients of the access point see as the root."
					>
						Root directory
					</Header>
					<Text
						label="Path"
						value={form.path}
						onChange={(path) => set({ path })}
						constraint="Optional. Starts with /, up to four subdirectories and 100 characters. Defaults to /."
					/>
					<ColumnLayout columns={3}>
						<Text
							label="Owner user ID"
							value={form.ownerUid}
							onChange={(ownerUid) => set({ ownerUid })}
							constraint={idRange}
						/>
						<Text
							label="Owner group ID"
							value={form.ownerGid}
							onChange={(ownerGid) => set({ ownerGid })}
							constraint={idRange}
						/>
						<Text
							label="Permissions"
							value={form.permissions}
							onChange={(permissions) => set({ permissions })}
							constraint="3 or 4 octal digits, such as 0755."
						/>
					</ColumnLayout>
					<Alert type="info">
						Creation info is the owner and permissions of the directory when it
						does not exist yet. Enter all three, or none.
					</Alert>
				</SpaceBetween>
				<TagRowsField rows={form.tags} onChange={(tags) => set({ tags })} />
			</SpaceBetween>
		</ActionDialog>
	);
}
