import { Alert, Checkbox, SpaceBetween } from "@cloudscape-design/components";
import { useRef, useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { Choice } from "#/components/acm/Fields";
import { TagRowsField } from "#/components/elbv2/Shared";
import { Text } from "#/components/memorydb/Fields";
import { callerReference } from "#/lib/cloudfrontHelpers";
import { type EFSScope, efsCall } from "#/lib/efs";
import {
	createFileSystemInput,
	emptyFileSystemForm,
	type FileSystem,
	type FileSystemForm,
	maxProvisionedThroughput,
	performanceModes,
	throughputChoices,
	updateFileSystemInput,
} from "#/lib/efsHelpers";

// ThroughputFields chooses the throughput mode, and the amount a provisioned mode needs.
function ThroughputFields({
	performance,
	mode,
	amount,
	onMode,
	onAmount,
}: {
	performance: string;
	mode: string;
	amount: string;
	onMode: (mode: string) => void;
	onAmount: (amount: string) => void;
}) {
	return (
		<SpaceBetween size="m">
			<Choice
				label="Throughput mode"
				value={mode}
				options={throughputChoices(performance)}
				onChange={onMode}
				description={
					performance === "maxIO"
						? "Elastic throughput is not available with the maxIO performance mode."
						: undefined
				}
			/>
			{mode === "provisioned" && (
				<Text
					label="Provisioned throughput (MiB/s)"
					value={amount}
					onChange={onAmount}
					constraint={`A number from 1 to ${maxProvisionedThroughput}.`}
				/>
			)}
		</SpaceBetween>
	);
}

export function CreateFileSystemDialog({
	scope,
	onClose,
	onSaved,
}: {
	scope: EFSScope;
	onClose: () => void;
	onSaved: (id: string) => void;
}) {
	const [form, setForm] = useState(emptyFileSystemForm);
	const created = useRef("");
	const set = (patch: Partial<FileSystemForm>) =>
		setForm((f) => ({ ...f, ...patch }));
	return (
		<ActionDialog
			title="Create file system"
			submit="Create file system"
			onClose={onClose}
			onSaved={() => onSaved(created.current)}
			plan={async () => {
				const input = createFileSystemInput(form, callerReference("console"));
				return [
					{
						label: "CreateFileSystem",
						run: async () => {
							const out = await efsCall<FileSystem>(
								scope,
								"CreateFileSystem",
								input,
							);
							created.current = out.FileSystemId;
						},
					},
				];
			}}
		>
			<SpaceBetween size="m">
				<Alert type="info">
					TinyStack keeps file systems as records: nothing can mount them, and a
					new file system is available at once.
				</Alert>
				<Text
					label="Name"
					value={form.name}
					onChange={(name) => set({ name })}
					constraint="Optional. Stored as the Name tag."
				/>
				<Text
					label="Creation token"
					value={form.token}
					onChange={(token) => set({ token })}
					description="Identifies this request: a token that another file system already uses is refused."
					constraint="Optional, 1 to 64 printable ASCII characters. Generated when empty."
				/>
				<Choice
					label="Performance mode"
					value={form.performance}
					options={performanceModes}
					onChange={(performance) =>
						set({
							performance,
							throughput: throughputChoices(performance).includes(
								form.throughput,
							)
								? form.throughput
								: "bursting",
						})
					}
				/>
				<ThroughputFields
					performance={form.performance}
					mode={form.throughput}
					amount={form.provisioned}
					onMode={(throughput) => set({ throughput })}
					onAmount={(provisioned) => set({ provisioned })}
				/>
				<Checkbox
					checked={form.encrypted}
					onChange={({ detail }) => set({ encrypted: detail.checked })}
				>
					Encrypt data at rest
				</Checkbox>
				{form.encrypted && (
					<Text
						label="KMS key"
						value={form.kmsKey}
						onChange={(kmsKey) => set({ kmsKey })}
						description="A key ID, key ARN, alias or alias ARN in this account and Region. TinyStack stores it without looking the key up."
						constraint="Optional."
					/>
				)}
				<TagRowsField rows={form.tags} onChange={(tags) => set({ tags })} />
			</SpaceBetween>
		</ActionDialog>
	);
}

export function EditFileSystemDialog({
	scope,
	fileSystem,
	onClose,
	onSaved,
}: {
	scope: EFSScope;
	fileSystem: FileSystem;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [mode, setMode] = useState(fileSystem.ThroughputMode ?? "bursting");
	const [amount, setAmount] = useState(
		fileSystem.ProvisionedThroughputInMibps === undefined
			? ""
			: String(fileSystem.ProvisionedThroughputInMibps),
	);
	return (
		<ActionDialog
			title={`Edit ${fileSystem.FileSystemId}`}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const input = updateFileSystemInput(fileSystem, mode, amount);
				return [
					{
						label: "UpdateFileSystem",
						run: () => efsCall(scope, "UpdateFileSystem", input),
					},
				];
			}}
		>
			<SpaceBetween size="m">
				<Alert type="info">
					Only the throughput can be changed. TinyStack does not support
					lifecycle, backup, file system policy or protection settings; tags are
					edited on the page.
				</Alert>
				<ThroughputFields
					performance={fileSystem.PerformanceMode ?? "generalPurpose"}
					mode={mode}
					amount={amount}
					onMode={setMode}
					onAmount={setAmount}
				/>
			</SpaceBetween>
		</ActionDialog>
	);
}

export function DeleteFileSystemDialog({
	scope,
	fileSystem,
	onClose,
	onSaved,
}: {
	scope: EFSScope;
	fileSystem: FileSystem;
	onClose: () => void;
	onSaved: () => void;
}) {
	const mounts = fileSystem.NumberOfMountTargets ?? 0;
	return (
		<ActionDialog
			title="Delete file system"
			submit="Delete file system"
			confirmation={fileSystem.FileSystemId}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => [
				{
					label: "DeleteFileSystem",
					run: () =>
						efsCall(scope, "DeleteFileSystem", {
							FileSystemId: fileSystem.FileSystemId,
						}),
				},
			]}
		>
			<Alert type="warning">
				Delete {fileSystem.Name || fileSystem.FileSystemId} permanently. A file
				system that has mount targets or access points cannot be deleted, so
				delete them first.
				{mounts > 0 &&
					` This file system has ${mounts} mount ${mounts === 1 ? "target" : "targets"}.`}
			</Alert>
		</ActionDialog>
	);
}
