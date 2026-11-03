import {
	Alert,
	Box,
	Button,
	Container,
	CopyToClipboard,
	Header,
	Hotspot,
	KeyValuePairs,
	SpaceBetween,
	Spinner,
	Tabs,
} from "@cloudscape-design/components";
import { useEffect, useState } from "react";
import { CodeBlock } from "#/components/CodeBlock";
import { downloadObject, type S3Scope, s3Call, sizeLabel } from "#/lib/s3";
import { HOTSPOT } from "#/tutorials/hotspots";
import { S3Dialog } from "./S3Dialog";
import { S3Tags } from "./S3Settings";

export function S3ObjectDetail({
	scope,
	objectKey,
	version,
	enabled,
	onBack,
	onChanged,
}: {
	scope: S3Scope;
	objectKey: string;
	version: string;
	enabled: boolean;
	onBack: () => void;
	onChanged: (message: string) => void;
}) {
	const [metadata, setMetadata] = useState<Record<string, unknown>>();
	const [error, setError] = useState("");
	const [busy, setBusy] = useState(false);
	const [dialog, setDialog] = useState(false);
	const [generation, setGeneration] = useState(0);
	// biome-ignore lint/correctness/useExhaustiveDependencies: generation explicitly refreshes server state.
	useEffect(() => {
		let active = true;
		setError("");
		s3Call(scope, "HeadObject", {
			Key: objectKey,
			...(version ? { VersionId: version } : {}),
			ChecksumMode: "ENABLED",
		})
			.then((result) => {
				if (active) setMetadata(result);
			})
			.catch((err) => {
				if (active) setError(err instanceof Error ? err.message : String(err));
			});
		return () => {
			active = false;
		};
	}, [scope, objectKey, version, generation]);
	const download = async () => {
		setBusy(true);
		try {
			await downloadObject(scope, {
				Key: objectKey,
				...(version ? { VersionId: version } : {}),
			});
		} catch (err) {
			setError(err instanceof Error ? err.message : String(err));
		} finally {
			setBusy(false);
		}
	};
	return (
		<SpaceBetween size="l">
			<Header
				variant="h2"
				actions={
					<SpaceBetween direction="horizontal" size="xs">
						<Button onClick={onBack}>Back to objects</Button>

						<Button
							iconName="refresh"
							ariaLabel="Refresh object"
							onClick={() => setGeneration((g) => g + 1)}
						/>
						<Hotspot hotspotId={HOTSPOT.s3Download}>
							<Button
								disabled={!enabled || !metadata}
								loading={busy}
								onClick={download}
							>
								Download
							</Button>
						</Hotspot>
						<Button disabled={!enabled} onClick={() => setDialog(true)}>
							Delete
						</Button>
					</SpaceBetween>
				}
			>
				{objectKey}
			</Header>
			{!!error && (
				<Alert type="error" header="Cannot complete object action">
					{error}
				</Alert>
			)}
			{!metadata && !error && <Spinner />}
			{metadata && (
				<Tabs
					tabs={[
						{
							id: "properties",
							label: "Properties",
							content: (
								<SpaceBetween size="l">
									<Container
										header={<Header variant="h2">Object overview</Header>}
									>
										<KeyValuePairs
											columns={2}
											items={[
												{ label: "Key", value: objectKey },
												{
													label: "S3 URI",
													value: (
														<CopyToClipboard
															copyButtonText="Copy S3 URI"
															copySuccessText="Copied"
															copyErrorText="Could not copy"
															textToCopy={`s3://${scope.bucket}/${objectKey}`}
														/>
													),
												},
												{
													label: "Size",
													value: sizeLabel(Number(metadata.ContentLength ?? 0)),
												},
												{
													label: "Content type",
													value: String(metadata.ContentType || "—"),
												},
												{
													label: "Last modified",
													value: String(metadata.LastModified || "—"),
												},
												{ label: "ETag", value: String(metadata.ETag || "—") },
												{
													label: "Version ID",
													value:
														version || String(metadata.VersionId || "null"),
												},
												{ label: "Storage class", value: "STANDARD" },
											]}
										/>
									</Container>
									<Container
										header={
											<Header variant="h2">Metadata and checksums</Header>
										}
									>
										<CodeBlock text={JSON.stringify(metadata, null, 2)} />
									</Container>
								</SpaceBetween>
							),
						},
						{
							id: "tags",
							label: "Tags",
							content: (
								<S3Tags
									scope={scope}
									objectKey={objectKey}
									version={version}
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
					title="Delete object"
					submit="Delete object"
					confirmation="delete"
					plan={async () => [
						{
							label: "Delete object",
							run: () =>
								s3Call(scope, "DeleteObject", {
									Key: objectKey,
									...(version ? { VersionId: version } : {}),
								}),
						},
					]}
					onClose={() => setDialog(false)}
					onSaved={() => {
						onChanged(`Object ${objectKey} deleted.`);
						onBack();
					}}
				>
					<SpaceBetween size="m">
						<Box>{objectKey}</Box>
						<Alert type="warning">
							{version
								? `Permanently delete version ${version}. This cannot be undone.`
								: "Delete the current object. If bucket versioning is enabled, previous versions remain and a delete marker is created."}
						</Alert>
					</SpaceBetween>
				</S3Dialog>
			)}
		</SpaceBetween>
	);
}
