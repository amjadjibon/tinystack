import {
	Alert,
	Box,
	Button,
	Checkbox,
	ColumnLayout,
	Container,
	CopyToClipboard,
	FormField,
	Header,
	Input,
	SpaceBetween,
	Tabs,
	Textarea,
} from "@cloudscape-design/components";
import { useEffect, useRef, useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { ConsoleTags } from "#/components/ConsoleTags";
import type { Resource } from "#/lib/api";
import { integer } from "#/lib/consoleActions";
import {
	type Secret,
	type SecretScope,
	secretCall,
	secretDate,
	secretTagPlan,
} from "#/lib/secretsmanager";
import { usePoll } from "#/lib/usePoll";
import { SecretPolicy, SecretRotation } from "./SecretSettings";
import { SecretValuePanel } from "./SecretValuePanel";
import { SecretVersions } from "./SecretVersions";
export function SecretDetails({
	scope,
	resource,
	enabled,
	onChanged,
	onDeleted,
}: {
	scope: SecretScope;
	resource?: Resource;
	enabled: boolean;
	onChanged: () => void;
	onDeleted: () => void;
}) {
	const metadata = usePoll(() =>
		enabled
			? secretCall<Secret>(scope, "DescribeSecret")
			: Promise.resolve(undefined),
	);
	const [dialog, setDialog] = useState(""),
		[description, setDescription] = useState(""),
		[key, setKey] = useState(""),
		[days, setDays] = useState("30"),
		[force, setForce] = useState(false),
		[revision, setRevision] = useState(0);
	const alive = useRef(true);
	useEffect(() => {
		alive.current = true;
		return () => {
			alive.current = false;
		};
	}, []);
	const changed = () => {
		if (!alive.current) return;
		setRevision((r) => r + 1);
		metadata.reload();
		onChanged();
	};
	const secret: Secret | undefined =
		metadata.data ??
		(resource
			? {
					ARN: resource.arn,
					Name: resource.name,
					Description: String(resource.details?.Description ?? ""),
					DeletedDate: resource.details?.DeletionDate as number | undefined,
					VersionIdsToStages: resource.details?.VersionIdsToStages as
						| Record<string, string[]>
						| undefined,
					Tags: Object.entries(resource.tags ?? {}).map(([Key, Value]) => ({
						Key,
						Value,
					})),
				}
			: undefined);
	const writable =
		enabled && !!metadata.data && !metadata.error && !secret?.DeletedDate;
	const manageable = enabled && !!metadata.data && !metadata.error;
	const tags = Object.fromEntries(
		(secret?.Tags ?? []).map((t) => [t.Key, t.Value]),
	);
	return (
		<SpaceBetween size="l">
			{metadata.error && (
				<Alert type="error" header="Cannot load secret">
					{metadata.error}
				</Alert>
			)}
			{!secret ? (
				<Box>
					{metadata.loading
						? "Loading secret…"
						: "Secret not found in this account and Region."}
				</Box>
			) : (
				<>
					<Header
						variant="h1"
						actions={
							<SpaceBetween direction="horizontal" size="xs">
								<Button
									iconName="refresh"
									ariaLabel="Refresh secret"
									onClick={() => {
										setRevision((r) => r + 1);
										metadata.reload();
									}}
								/>
								{secret.DeletedDate && (
									<Button
										disabled={!manageable}
										onClick={() => setDialog("restore")}
									>
										Restore secret
									</Button>
								)}
								<Button
									disabled={!manageable}
									onClick={() => {
										setForce(!!secret.DeletedDate);
										setDays("30");
										setDialog("delete");
									}}
								>
									Delete secret
								</Button>
							</SpaceBetween>
						}
					>
						{secret.Name}
					</Header>
					{secret.DeletedDate && (
						<Alert type="warning" header="Secret scheduled for deletion">
							Deletion date: {secretDate(secret.DeletedDate)}. Restore this
							secret to retrieve values or edit its configuration.
						</Alert>
					)}
					<Container
						header={
							<Header
								variant="h2"
								actions={
									<Button
										disabled={!writable}
										onClick={() => {
											setDescription(secret.Description ?? "");
											setKey(secret.KmsKeyId ?? "");
											setDialog("metadata");
										}}
									>
										Edit secret details
									</Button>
								}
							>
								Secret details
							</Header>
						}
					>
						<ColumnLayout columns={2} variant="text-grid">
							<div>
								<Box variant="awsui-key-label">Secret name</Box>
								{secret.Name}
							</div>
							<div>
								<Box variant="awsui-key-label">Secret ARN</Box>
								<CopyToClipboard
									variant="inline"
									textToCopy={secret.ARN}
									copySuccessText="ARN copied"
									copyErrorText="Could not copy ARN"
								/>
							</div>
							<div>
								<Box variant="awsui-key-label">Description</Box>
								{secret.Description || "—"}
							</div>
							<div>
								<Box variant="awsui-key-label">Encryption key</Box>
								{enabled
									? secret.KmsKeyId || "alias/aws/secretsmanager"
									: "Unavailable in inventory"}
							</div>
							<div>
								<Box variant="awsui-key-label">Created</Box>
								{secretDate(secret.CreatedDate)}
							</div>
							<div>
								<Box variant="awsui-key-label">Last changed</Box>
								{secretDate(secret.LastChangedDate)}
							</div>
						</ColumnLayout>
					</Container>
					<Tabs
						tabs={[
							{
								id: "value",
								label: "Secret value",
								content: (
									<SecretValuePanel
										key={`${revision}/${secret.LastChangedDate}/${writable}`}
										scope={scope}
										secret={secret}
										enabled={writable}
										onChanged={changed}
									/>
								),
							},
							{
								id: "versions",
								label: "Versions",
								content: (
									<SecretVersions
										key={revision}
										scope={scope}
										readable={enabled}
										enabled={writable}
										onChanged={changed}
									/>
								),
							},
							{
								id: "rotation",
								label: "Rotation",
								content: enabled ? (
									<SecretRotation
										key={revision}
										scope={scope}
										secret={secret}
										enabled={writable}
										onChanged={changed}
									/>
								) : (
									<Box>
										Use signed AWS CLI or SDK requests to read rotation
										settings.
									</Box>
								),
							},
							{
								id: "permissions",
								label: "Permissions",
								content: (
									<SecretPolicy
										scope={scope}
										readable={enabled}
										enabled={writable}
									/>
								),
							},
							{
								id: "tags",
								label: "Tags",
								content: (
									<ConsoleTags
										key={revision}
										tags={tags}
										enabled={writable}
										plan={(next, removed) =>
											secretTagPlan(scope, next, removed)
										}
										onChanged={changed}
									/>
								),
							},
						]}
					/>
					{dialog && (
						<ActionDialog
							title={
								dialog === "metadata"
									? "Edit secret details"
									: dialog === "restore"
										? "Restore secret"
										: "Delete secret"
							}
							submit={
								dialog === "restore"
									? "Restore secret"
									: dialog === "delete"
										? "Delete secret"
										: "Save changes"
							}
							confirmation={dialog === "delete" ? secret.Name : undefined}
							onClose={() => setDialog("")}
							onSaved={() => {
								if (!alive.current) return;
								setDialog("");
								changed();
								if (dialog === "delete" && force) onDeleted();
							}}
							plan={async () => {
								if (dialog === "restore")
									return [
										{
											label: "Restore secret",
											run: () => secretCall(scope, "RestoreSecret"),
										},
									];
								if (dialog === "delete")
									return [
										{
											label: "Delete secret",
											run: () =>
												secretCall(
													scope,
													"DeleteSecret",
													force
														? { ForceDeleteWithoutRecovery: true }
														: {
																RecoveryWindowInDays: integer(
																	days,
																	"Recovery window",
																	7,
																	30,
																),
															},
												),
										},
									];
								if (description.length > 2048)
									throw new Error(
										"Description must be at most 2,048 characters.",
									);
								return [
									{
										label: "Update secret details",
										run: () =>
											secretCall(scope, "UpdateSecret", {
												Description: description,
												KmsKeyId: key.trim(),
											}),
									},
								];
							}}
						>
							{dialog === "metadata" ? (
								<SpaceBetween size="m">
									<FormField label="Description">
										<Textarea
											ariaLabel="Secret description"
											value={description}
											onChange={({ detail }) => setDescription(detail.value)}
										/>
									</FormField>
									<FormField
										label="Encryption key"
										description="Leave empty for alias/aws/secretsmanager, or enter a KMS key ID, alias, or ARN."
									>
										<Input
											ariaLabel="Encryption key"
											value={key}
											onChange={({ detail }) => setKey(detail.value)}
										/>
									</FormField>
								</SpaceBetween>
							) : dialog === "restore" ? (
								<Box>
									Cancel scheduled deletion and make this secret available
									again.
								</Box>
							) : (
								<SpaceBetween size="m">
									<Alert type={force ? "warning" : "info"}>
										{force
											? "Delete this secret and all versions immediately. This cannot be undone."
											: "The secret becomes unavailable during the recovery window. You can restore it before the deletion date."}
									</Alert>
									<Checkbox
										checked={force}
										disabled={!!secret.DeletedDate}
										onChange={({ detail }) => setForce(detail.checked)}
									>
										Permanently delete without recovery
									</Checkbox>
									{!force && (
										<FormField label="Recovery window (7–30 days)">
											<Input
												ariaLabel="Recovery window"
												value={days}
												onChange={({ detail }) => setDays(detail.value)}
											/>
										</FormField>
									)}
								</SpaceBetween>
							)}
						</ActionDialog>
					)}
				</>
			)}
		</SpaceBetween>
	);
}
