import {
	Alert,
	Box,
	Button,
	Checkbox,
	ContentLayout,
	FileUpload,
	FormField,
	Header,
	Hotspot,
	Input,
	KeyValuePairs,
	Link,
	SpaceBetween,
	Table,
	Tabs,
} from "@cloudscape-design/components";
import { useCallback, useEffect, useRef, useState } from "react";
import {
	currentObjects,
	downloadObject,
	fileBody,
	objectID,
	type S3Listing,
	type S3Object,
	type S3Scope,
	type S3Step,
	s3Call,
	s3Href,
	sizeLabel,
} from "#/lib/s3";
import { useFollow } from "#/lib/useFollow";
import { HOTSPOT } from "#/tutorials/hotspots";
import { S3Dialog } from "./S3Dialog";
import { S3ObjectDetail } from "./S3ObjectDetail";
import { S3Settings } from "./S3Settings";

export function S3Bucket({
	scope,
	prefix,
	objectKey,
	version,
	enabled,
	readable,
	onNavigate,
	onChanged,
	onDeleted,
}: {
	scope: S3Scope;
	prefix: string;
	objectKey: string;
	version: string;
	enabled: boolean;
	readable: boolean;
	onNavigate: (prefix: string, key?: string, version?: string) => void;
	onChanged: (message: string) => void;
	onDeleted: () => void;
}) {
	const [tab, setTab] = useState("objects");
	const [dialog, setDialog] = useState(false);
	return (
		<ContentLayout
			header={
				<Header
					variant="h1"
					description={`Account ${scope.account} · ${scope.region}`}
					actions={
						<Button disabled={!enabled} onClick={() => setDialog(true)}>
							Delete bucket
						</Button>
					}
				>
					{scope.bucket}
				</Header>
			}
		>
			<SpaceBetween size="l">
				<KeyValuePairs
					columns={2}
					items={[
						{ label: "Bucket ARN", value: `arn:aws:s3:::${scope.bucket}` },
						{ label: "S3 URI", value: `s3://${scope.bucket}/` },
					]}
				/>
				{!readable ? (
					<Alert type="info">
						Use signed AWS CLI or SDK calls to browse this bucket's objects and
						settings.
					</Alert>
				) : objectKey ? (
					<S3ObjectDetail
						key={`${objectKey}/${version}`}
						scope={scope}
						objectKey={objectKey}
						version={version}
						enabled={enabled}
						onBack={() => onNavigate(prefix)}
						onChanged={onChanged}
					/>
				) : (
					<Tabs
						activeTabId={tab}
						onChange={({ detail }) => setTab(detail.activeTabId)}
						tabs={[
							{
								id: "objects",
								label: "Objects",
								content: (
									<S3Objects
										key={prefix}
										scope={scope}
										prefix={prefix}
										enabled={enabled}
										onNavigate={onNavigate}
										onChanged={onChanged}
									/>
								),
							},
							{
								id: "properties",
								label: "Properties",
								content: (
									<S3Settings
										scope={scope}
										section="properties"
										enabled={enabled}
										onChanged={onChanged}
									/>
								),
							},
							{
								id: "permissions",
								label: "Permissions",
								content: (
									<S3Settings
										scope={scope}
										section="permissions"
										enabled={enabled}
										onChanged={onChanged}
									/>
								),
							},
							{
								id: "management",
								label: "Management",
								content: (
									<S3Settings
										scope={scope}
										section="management"
										enabled={enabled}
										onChanged={onChanged}
									/>
								),
							},
						]}
					/>
				)}
				{dialog && (
					<S3Dialog
						title={`Delete bucket ${scope.bucket}`}
						submit="Delete bucket"
						confirmation={scope.bucket}
						plan={async () => [
							{
								label: "Delete bucket",
								run: () => s3Call(scope, "DeleteBucket"),
							},
						]}
						onClose={() => setDialog(false)}
						onSaved={onDeleted}
					>
						<Alert type="warning">
							The bucket must be empty, including all versions and unfinished
							uploads. Use Empty on the bucket list first.
						</Alert>
					</S3Dialog>
				)}
			</SpaceBetween>
		</ContentLayout>
	);
}

function S3Objects({
	scope,
	prefix,
	enabled,
	onNavigate,
	onChanged,
}: {
	scope: S3Scope;
	prefix: string;
	enabled: boolean;
	onNavigate: (prefix: string, key?: string, version?: string) => void;
	onChanged: (message: string) => void;
}) {
	const follow = useFollow();
	const [versions, setVersions] = useState(false);
	const [page, setPage] = useState<S3Listing>();
	const [loading, setLoading] = useState(true);
	const [error, setError] = useState("");
	const [markers, setMarkers] = useState<Record<string, unknown>[]>([{}]);
	const [selected, setSelected] = useState<S3Object[]>([]);
	const [search, setSearch] = useState("");
	const [filter, setFilter] = useState("");
	const find = (value: string) => {
		setFilter(value);
		setSelected([]);
		setPage(undefined);
		setMarkers([{}]);
	};
	const [dialog, setDialog] = useState<
		"upload" | "folder" | "delete" | "copy" | null
	>(null);
	const [files, setFiles] = useState<File[]>([]);
	const [folder, setFolder] = useState("");
	const [destination, setDestination] = useState("");
	const [transfer, setTransfer] = useState(false);
	const sequence = useRef(0);
	const reload = useCallback(async () => {
		const id = ++sequence.current;
		setLoading(true);
		setError("");
		try {
			const next = await s3Call<S3Listing>(
				scope,
				versions ? "ListObjectVersions" : "ListObjectsV2",
				{
					Prefix: prefix + filter,
					Delimiter: "/",
					MaxKeys: 100,
					...markers[markers.length - 1],
				},
			);
			if (id === sequence.current) {
				setPage(next);
				setSelected([]);
			}
		} catch (err) {
			if (id === sequence.current)
				setError(err instanceof Error ? err.message : String(err));
		} finally {
			if (id === sequence.current) setLoading(false);
		}
	}, [scope, prefix, filter, versions, markers]);
	useEffect(() => {
		reload();
		return () => {
			sequence.current++;
		};
	}, [reload]);
	const objects = [
		...(page?.CommonPrefixes ?? []).map(
			({ Prefix }) => ({ Key: Prefix, folder: true }) as S3Object,
		),
		...(versions
			? [
					...(page?.Versions ?? []),
					...(page?.DeleteMarkers ?? []).map((r) => ({ ...r, marker: true })),
				]
			: (page?.Contents ?? [])
		).filter((r) => r.Key !== prefix),
	];
	const plan = async (): Promise<S3Step[]> => {
		if (dialog === "delete") {
			// One step per key avoids retrying successful current-object deletions in versioned buckets.
			const targets = new Map<string, S3Object>();
			for (const item of selected) {
				const entries = item.folder
					? await currentObjects(scope, item.Key)
					: [item];
				for (const object of entries) targets.set(objectID(object), object);
			}
			return [...targets.values()].map((object) => ({
				label: `Delete ${object.Key}`,
				run: () =>
					s3Call(scope, "DeleteObject", {
						Key: object.Key,
						...(object.VersionId ? { VersionId: object.VersionId } : {}),
					}),
			}));
		}
		if (dialog === "folder") {
			if (!folder || folder.includes("/") || folder === "." || folder === "..")
				throw new Error("Enter a folder name without slashes.");
			const Key = `${prefix}${folder}/`;
			if (new TextEncoder().encode(Key).length > 1024)
				throw new Error("Folder key exceeds 1024 bytes.");
			return [
				{
					label: "Create folder",
					run: () =>
						s3Call(scope, "PutObject", { Key, Body: "", IfNoneMatch: "*" }),
				},
			];
		}
		if (dialog === "copy") {
			if (!destination || new TextEncoder().encode(destination).length > 1024)
				throw new Error("Enter a destination key of 1–1024 UTF-8 bytes.");
			const source = selected[0];
			if (source.Key === destination)
				throw new Error("Choose a different destination key.");
			const CopySource = `${scope.bucket}/${encodeURIComponent(source.Key)}${source.VersionId ? `?versionId=${encodeURIComponent(source.VersionId)}` : ""}`;
			return [
				{
					label: "Copy object",
					run: () =>
						s3Call(scope, "CopyObject", { Key: destination, CopySource }),
				},
			];
		}
		if (!files.length) throw new Error("Select at least one file.");
		// Validate/read all files before making the first write.
		const bodies = await Promise.all(files.map(fileBody));
		const names = new Set<string>();
		return files.map((file, index) => {
			const Key = prefix + file.name;
			if (names.has(Key))
				throw new Error(`Two files have the same destination key: ${Key}`);
			names.add(Key);
			if (new TextEncoder().encode(Key).length > 1024)
				throw new Error(`${Key} exceeds 1024 UTF-8 bytes.`);
			return {
				label: `Upload ${file.name}`,
				run: () =>
					s3Call(scope, "PutObject", {
						Key,
						Body: bodies[index],
						ContentType: file.type || "application/octet-stream",
					}),
			};
		});
	};
	const download = async () => {
		setTransfer(true);
		setError("");
		try {
			await downloadObject(scope, selected[0]);
		} catch (err) {
			setError(err instanceof Error ? err.message : String(err));
		} finally {
			setTransfer(false);
		}
	};
	return (
		<SpaceBetween size="l">
			{!!error && (
				<Alert type="error" header="Cannot complete S3 action">
					{error}
				</Alert>
			)}
			<Table<S3Object>
				items={objects}
				loading={loading}
				loadingText="Loading objects"
				trackBy={objectID}
				selectionType="multi"
				selectedItems={selected}
				isItemDisabled={(r) => !!r.folder && versions}
				ariaLabels={{
					selectionGroupLabel: "Select objects",
					itemSelectionLabel: (_, r) =>
						`Select ${r.Key}${r.VersionId ? ` version ${r.VersionId}` : ""}`,
					allItemsSelectionLabel: () => "Select all objects on this page",
				}}
				onSelectionChange={({ detail }) => setSelected(detail.selectedItems)}
				columnDefinitions={[
					{
						id: "name",
						header: "Name",
						cell: (r) =>
							r.marker ? (
								r.Key.slice(prefix.length)
							) : (
								<Link
									href={
										r.folder
											? s3Href(scope, r.Key)
											: s3Href(scope, prefix, r.Key, r.VersionId)
									}
									onFollow={follow}
								>
									{r.Key.slice(prefix.length)}
								</Link>
							),
					},
					{
						id: "type",
						header: "Type",
						cell: (r) =>
							r.folder ? "Folder" : r.marker ? "Delete marker" : "Object",
					},
					{
						id: "modified",
						header: "Last modified",
						cell: (r) =>
							r.LastModified ? new Date(r.LastModified).toLocaleString() : "—",
					},
					{
						id: "size",
						header: "Size",
						cell: (r) => (r.folder || r.marker ? "—" : sizeLabel(r.Size)),
					},
					...(versions
						? [
								{
									id: "version",
									header: "Version ID",
									cell: (r: S3Object) => r.VersionId || "—",
								},
								{
									id: "latest",
									header: "Latest",
									cell: (r: S3Object) =>
										r.folder ? "—" : r.IsLatest ? "Yes" : "No",
								},
							]
						: []),
				]}
				header={
					<Header
						counter={`(${objects.length})`}
						description={
							prefix ? `Prefix: ${prefix}` : "Objects at the bucket root"
						}
						actions={
							<SpaceBetween direction="horizontal" size="xs">
								<Button
									iconName="refresh"
									ariaLabel="Refresh objects"
									onClick={reload}
								/>
								<Button
									disabled={
										!enabled ||
										selected.length !== 1 ||
										selected[0]?.marker ||
										selected[0]?.folder ||
										transfer ||
										loading
									}
									loading={transfer}
									onClick={download}
								>
									Download
								</Button>
								<Button
									disabled={
										!enabled ||
										selected.length !== 1 ||
										selected[0]?.marker ||
										selected[0]?.folder ||
										loading
									}
									onClick={() => {
										setDestination(prefix);
										setDialog("copy");
									}}
								>
									Copy
								</Button>
								<Button
									disabled={!enabled || !selected.length || loading}
									onClick={() => setDialog("delete")}
								>
									Delete
								</Button>
								<Button
									disabled={!enabled || loading}
									onClick={() => {
										setFolder("");
										setDialog("folder");
									}}
								>
									Create folder
								</Button>
								<Hotspot
									hotspotId={HOTSPOT.s3Upload}
									side="left"
									direction="left"
								>
									<Button
										variant="primary"
										disabled={!enabled || loading}
										onClick={() => {
											setFiles([]);
											setDialog("upload");
										}}
									>
										Upload
									</Button>
								</Hotspot>
							</SpaceBetween>
						}
					>
						Objects
					</Header>
				}
				filter={
					<SpaceBetween size="m">
						<SpaceBetween direction="horizontal" size="xs">
							<Input
								ariaLabel="Object key prefix"
								value={search}
								placeholder="Find objects by key prefix"
								onChange={({ detail }) => setSearch(detail.value)}
								onKeyDown={({ detail }) =>
									detail.key === "Enter" && find(search)
								}
							/>
							<Button onClick={() => find(search)}>Find</Button>
							{!!filter && (
								<Button
									onClick={() => {
										setSearch("");
										find("");
									}}
								>
									Clear filter
								</Button>
							)}
							{prefix && (
								<Button onClick={() => onNavigate("")}>Bucket root</Button>
							)}
						</SpaceBetween>
						<Checkbox
							checked={versions}
							onChange={({ detail }) => {
								setSelected([]);
								setPage(undefined);
								setMarkers([{}]);
								setVersions(detail.checked);
							}}
						>
							Show versions
						</Checkbox>
					</SpaceBetween>
				}
				pagination={
					<SpaceBetween direction="horizontal" size="xs">
						<Button
							disabled={markers.length <= 1 || loading}
							onClick={() => setMarkers((m) => m.slice(0, -1))}
						>
							Previous
						</Button>
						<Box>Page {markers.length}</Box>
						<Button
							disabled={!page?.IsTruncated || loading || !!error}
							onClick={() =>
								setMarkers((m) => [
									...m,
									versions
										? {
												KeyMarker: page?.NextKeyMarker,
												VersionIdMarker: page?.NextVersionIdMarker,
											}
										: { ContinuationToken: page?.NextContinuationToken },
								])
							}
						>
							Next
						</Button>
					</SpaceBetween>
				}
				empty={
					<Box textAlign="center" padding="l">
						<SpaceBetween size="m">
							<Box variant="h3">No objects</Box>
							<Box>
								Upload files or create a folder to start storing objects here.
							</Box>
						</SpaceBetween>
					</Box>
				}
			/>
			{dialog && (
				<S3Dialog
					title={
						dialog === "upload"
							? "Upload files"
							: dialog === "folder"
								? "Create folder"
								: dialog === "copy"
									? "Copy object"
									: "Delete objects"
					}
					submit={
						dialog === "upload"
							? "Upload"
							: dialog === "folder"
								? "Create folder"
								: dialog === "copy"
									? "Copy object"
									: "Delete objects"
					}
					confirmation={dialog === "delete" ? "delete" : undefined}
					plan={plan}
					onClose={() => setDialog(null)}
					onSaved={() => {
						const mode = dialog;
						setDialog(null);
						reload();
						onChanged(
							mode === "upload"
								? "Files uploaded."
								: mode === "folder"
									? "Folder created."
									: mode === "copy"
										? "Object copied."
										: "Selected objects deleted.",
						);
					}}
				>
					{dialog === "upload" ? (
						<SpaceBetween size="l">
							<FormField label="Destination">
								<Box>{`s3://${scope.bucket}/${prefix}`}</Box>
							</FormField>
							<FormField
								label="Files"
								description="Select multiple files. Maximum 1 MiB per file for console uploads. Use the AWS CLI or SDK for larger files."
							>
								<FileUpload
									multiple
									value={files}
									onChange={({ detail }) => setFiles(detail.value)}
									showFileSize
									showFileLastModified
									i18nStrings={{
										uploadButtonText: () => "Add files",
										dropzoneText: () => "Drop files to upload",
										removeFileAriaLabel: (i) => `Remove file ${i + 1}`,
										limitShowFewer: "Show fewer files",
										limitShowMore: "Show more files",
										errorIconAriaLabel: "Error",
									}}
								/>
							</FormField>
							<Alert type="info">
								Uploading an existing key creates a new version when versioning
								is enabled. Otherwise it replaces the current object.
							</Alert>
						</SpaceBetween>
					) : dialog === "folder" ? (
						<FormField
							label="Folder name"
							description={`Create a zero-byte folder marker beneath ${prefix || "the bucket root"}.`}
						>
							<Input
								ariaLabel="Folder name"
								value={folder}
								onChange={({ detail }) => setFolder(detail.value)}
							/>
						</FormField>
					) : dialog === "copy" ? (
						<SpaceBetween size="l">
							<Box>Source: {selected[0]?.Key}</Box>
							<FormField
								label="Destination object key"
								description="Copy within this bucket. An existing destination will be replaced or receive a new version."
							>
								<Input
									ariaLabel="Destination object key"
									value={destination}
									onChange={({ detail }) => setDestination(detail.value)}
								/>
							</FormField>
						</SpaceBetween>
					) : (
						<SpaceBetween size="m">
							<Alert type="warning">
								{versions
									? "Permanently delete the selected versions and delete markers. Deleting a latest version can reveal an older version."
									: "Delete the selected current objects and all current objects inside selected folders. Versioned buckets create delete markers; previous versions remain."}
							</Alert>
							<Box>
								{selected
									.map(
										(r) =>
											`${r.Key}${r.VersionId ? ` (version ${r.VersionId})` : ""}`,
									)
									.join(", ")}
							</Box>
						</SpaceBetween>
					)}
				</S3Dialog>
			)}
		</SpaceBetween>
	);
}
