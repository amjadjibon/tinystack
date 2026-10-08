import {
	Alert,
	Box,
	Button,
	Checkbox,
	Container,
	FormField,
	Header,
	Input,
	Select,
	SpaceBetween,
	Spinner,
	Table,
	Textarea,
} from "@cloudscape-design/components";
import { useEffect, useState } from "react";
import { CodeBlock } from "#/components/CodeBlock";
import { type S3Scope, s3Call } from "#/lib/s3";
import { S3Dialog } from "./S3Dialog";

type Setting = {
	id: string;
	title: string;
	get: string;
	put: string;
	remove?: string;
	member: string;
	output?: string;
	mode?:
		| "versioning"
		| "tags"
		| "block"
		| "ownership"
		| "encryption"
		| "policy";
	fallback: unknown;
	description?: string;
	missing?: string[];
};
const settings: Record<"properties" | "permissions" | "management", Setting[]> =
	{
		properties: [
			{
				id: "versioning",
				title: "Bucket versioning",
				get: "GetBucketVersioning",
				put: "PutBucketVersioning",
				member: "VersioningConfiguration",
				mode: "versioning",
				fallback: {},
			},
			{
				id: "tags",
				title: "Tags",
				get: "GetBucketTagging",
				put: "PutBucketTagging",
				remove: "DeleteBucketTagging",
				member: "Tagging",
				mode: "tags",
				fallback: { TagSet: [] },
				missing: ["NoSuchTagSet"],
			},
			{
				id: "encryption",
				title: "Default encryption",
				get: "GetBucketEncryption",
				put: "PutBucketEncryption",
				remove: "DeleteBucketEncryption",
				member: "ServerSideEncryptionConfiguration",
				output: "ServerSideEncryptionConfiguration",
				mode: "encryption",
				fallback: {
					Rules: [
						{
							ApplyServerSideEncryptionByDefault: { SSEAlgorithm: "AES256" },
							BucketKeyEnabled: false,
						},
					],
				},
				description:
					"Encryption settings are stored as metadata. TinyStack stores object bytes as uploaded.",
			},
			{
				id: "notifications",
				title: "Event notifications",
				get: "GetBucketNotificationConfiguration",
				put: "PutBucketNotificationConfiguration",
				member: "NotificationConfiguration",
				fallback: {},
				description:
					"Configure notifications to local SQS queues, SNS topics, Lambda functions or EventBridge.",
			},
			{
				id: "website",
				title: "Static website hosting",
				get: "GetBucketWebsite",
				put: "PutBucketWebsite",
				remove: "DeleteBucketWebsite",
				member: "WebsiteConfiguration",
				fallback: { IndexDocument: { Suffix: "index.html" } },
				missing: ["NoSuchWebsiteConfiguration"],
				description:
					"Website configuration is stored. TinyStack does not serve a website endpoint.",
			},
		],
		permissions: [
			{
				id: "block",
				title: "Block public access (bucket settings)",
				get: "GetPublicAccessBlock",
				put: "PutPublicAccessBlock",
				member: "PublicAccessBlockConfiguration",
				output: "PublicAccessBlockConfiguration",
				mode: "block",
				fallback: {
					BlockPublicAcls: false,
					IgnorePublicAcls: false,
					BlockPublicPolicy: false,
					RestrictPublicBuckets: false,
				},
				missing: ["NoSuchPublicAccessBlockConfiguration"],
			},
			{
				id: "policy",
				title: "Bucket policy",
				get: "GetBucketPolicy",
				put: "PutBucketPolicy",
				remove: "DeleteBucketPolicy",
				member: "Policy",
				output: "Policy",
				mode: "policy",
				fallback: { Version: "2012-10-17", Statement: [] },
				missing: ["NoSuchBucketPolicy"],
				description:
					"Policies are stored. TinyStack does not evaluate them for general access.",
			},
			{
				id: "ownership",
				title: "Object ownership",
				get: "GetBucketOwnershipControls",
				put: "PutBucketOwnershipControls",
				member: "OwnershipControls",
				output: "OwnershipControls",
				mode: "ownership",
				fallback: { Rules: [{ ObjectOwnership: "BucketOwnerEnforced" }] },
				missing: ["OwnershipControlsNotFoundError"],
			},
			{
				id: "cors",
				title: "Cross-origin resource sharing (CORS)",
				get: "GetBucketCors",
				put: "PutBucketCors",
				remove: "DeleteBucketCors",
				member: "CORSConfiguration",
				fallback: { CORSRules: [] },
				missing: ["NoSuchCORSConfiguration"],
			},
		],
		management: [
			{
				id: "lifecycle",
				title: "Lifecycle rules",
				get: "GetBucketLifecycleConfiguration",
				put: "PutBucketLifecycleConfiguration",
				remove: "DeleteBucketLifecycle",
				member: "LifecycleConfiguration",
				fallback: { Rules: [] },
				missing: ["NoSuchLifecycleConfiguration"],
				description:
					"Rules are stored and validated. They do not expire objects or transition storage classes.",
			},
		],
	};

export function S3Settings({
	scope,
	section,
	enabled,
	onChanged,
}: {
	scope: S3Scope;
	section: keyof typeof settings;
	enabled: boolean;
	onChanged: (message: string) => void;
}) {
	return (
		<SpaceBetween size="l">
			{settings[section].map((setting) => (
				<S3Setting
					key={setting.id}
					scope={scope}
					setting={setting}
					enabled={enabled}
					onChanged={onChanged}
				/>
			))}
		</SpaceBetween>
	);
}
export function S3Tags({
	scope,
	objectKey,
	version,
	enabled,
	onChanged,
}: {
	scope: S3Scope;
	objectKey: string;
	version: string;
	enabled: boolean;
	onChanged: (message: string) => void;
}) {
	return (
		<S3Setting
			scope={scope}
			setting={{
				id: "object-tags",
				title: "Tags",
				get: "GetObjectTagging",
				put: "PutObjectTagging",
				remove: "DeleteObjectTagging",
				member: "Tagging",
				mode: "tags",
				fallback: { TagSet: [] },
			}}
			input={{ Key: objectKey, ...(version ? { VersionId: version } : {}) }}
			enabled={enabled}
			onChanged={onChanged}
		/>
	);
}
type Config = Record<string, unknown>;
type Tag = { Key: string; Value: string };
const blockLabels: Record<string, string> = {
	BlockPublicAcls:
		"Block public access granted through new access control lists (ACLs)",
	IgnorePublicAcls:
		"Block public access granted through any access control lists (ACLs)",
	BlockPublicPolicy:
		"Block public access granted through new public bucket policies",
	RestrictPublicBuckets:
		"Block public and cross-account access through any public bucket policies",
};
function S3Setting({
	scope,
	setting,
	enabled,
	input = {},
	onChanged,
}: {
	scope: S3Scope;
	setting: Setting;
	enabled: boolean;
	input?: Record<string, unknown>;
	onChanged: (message: string) => void;
}) {
	const [value, setValue] = useState<unknown>();
	const [configured, setConfigured] = useState(false);
	const [error, setError] = useState("");
	const [loading, setLoading] = useState(true);
	const [generation, setGeneration] = useState(0);
	const [dialog, setDialog] = useState<"edit" | "delete" | null>(null);
	const [draft, setDraft] = useState<Config>({});
	const [text, setText] = useState("");
	// biome-ignore lint/correctness/useExhaustiveDependencies: request members define scope; generation refreshes server state.
	useEffect(() => {
		let active = true;
		setLoading(true);
		setError("");
		s3Call<Config>(scope, setting.get, input)
			.then((result) => {
				if (!active) return;
				let next = setting.output ? result[setting.output] : result;
				if (setting.mode === "policy" && typeof next === "string")
					next = JSON.parse(next);
				if (setting.mode === "tags")
					next = { TagSet: (next as Config).TagSet ?? [] };
				if (setting.id === "lifecycle")
					next = { Rules: (next as Config).Rules ?? [] };
				setValue(next);
				setConfigured(true);
			})
			.catch((err) => {
				if (!active) return;
				const message = err instanceof Error ? err.message : String(err);
				if (setting.missing?.some((code) => message.startsWith(`${code}:`))) {
					setValue(undefined);
					setConfigured(false);
				} else setError(message);
			})
			.finally(() => {
				if (active) setLoading(false);
			});
		return () => {
			active = false;
		};
	}, [
		scope,
		setting.get,
		setting.output,
		setting.mode,
		setting.missing,
		input.Key,
		input.VersionId,
		generation,
	]);
	const current = (value ?? setting.fallback) as Config;
	const edit = () => {
		setDraft(structuredClone(current));
		setText(JSON.stringify(current, null, 2));
		setDialog("edit");
	};
	const plan = async () => {
		if (dialog === "delete")
			return [
				{
					label: `Delete ${setting.title}`,
					run: () => s3Call(scope, setting.remove ?? "", input),
				},
			];
		let next: unknown = draft;
		if (!setting.mode || setting.mode === "policy") {
			next = JSON.parse(text);
			if (!next || typeof next !== "object" || Array.isArray(next))
				throw new Error("Enter a JSON object.");
		}
		if (setting.mode === "versioning" && !draft.Status)
			throw new Error("Choose Enable or Suspend.");
		if (setting.mode === "tags") {
			const tags = (draft.TagSet ?? []) as Tag[];
			if (
				tags.some((tag) => !tag.Key) ||
				new Set(tags.map((tag) => tag.Key)).size !== tags.length
			) {
				throw new Error("Tag keys must be nonempty and unique.");
			}
		}
		return [
			{
				label: `Save ${setting.title}`,
				run: () =>
					s3Call(scope, setting.put, {
						...input,
						[setting.member]:
							setting.mode === "policy" ? JSON.stringify(next) : next,
					}),
			},
		];
	};
	return (
		<Container
			header={
				<Header
					variant="h2"
					description={setting.description}
					actions={
						<SpaceBetween direction="horizontal" size="xs">
							<Button
								iconName="refresh"
								ariaLabel={`Refresh ${setting.title}`}
								onClick={() => setGeneration((g) => g + 1)}
							/>
							{setting.remove && (
								<Button
									disabled={!enabled || loading || !configured || !!error}
									onClick={() => setDialog("delete")}
								>
									Delete
								</Button>
							)}
							<Button
								disabled={!enabled || loading || !!error}
								ariaLabel={`Edit ${setting.title}`}
								onClick={edit}
							>
								Edit
							</Button>
						</SpaceBetween>
					}
				>
					{setting.title}
				</Header>
			}
		>
			<SpaceBetween size="m">
				{error ? (
					<Alert type="error">{error}</Alert>
				) : loading ? (
					<Spinner />
				) : setting.mode === "versioning" ? (
					<Box>{String(current.Status || "Disabled")}</Box>
				) : !configured ? (
					<Box>No configuration.</Box>
				) : setting.mode === "tags" ? (
					<Table<Tag>
						items={(current.TagSet ?? []) as Tag[]}
						columnDefinitions={[
							{ id: "key", header: "Key", cell: (tag) => tag.Key },
							{ id: "value", header: "Value", cell: (tag) => tag.Value },
						]}
						empty={<Box>No tags.</Box>}
					/>
				) : setting.mode === "block" ? (
					<SpaceBetween size="xs">
						{Object.entries(blockLabels).map(([key, label]) => (
							<Box key={key}>
								{label}: <strong>{current[key] ? "On" : "Off"}</strong>
							</Box>
						))}
					</SpaceBetween>
				) : (
					<CodeBlock text={JSON.stringify(current, null, 2)} />
				)}
				{dialog && (
					<S3Dialog
						title={`${dialog === "delete" ? "Delete" : "Edit"} ${setting.title.toLowerCase()}`}
						submit={
							dialog === "delete" ? "Delete configuration" : "Save changes"
						}
						confirmation={dialog === "delete" ? "delete" : undefined}
						plan={plan}
						onClose={() => setDialog(null)}
						onSaved={() => {
							setDialog(null);
							setGeneration((g) => g + 1);
							onChanged(`${setting.title} updated.`);
						}}
					>
						{dialog === "delete" ? (
							<Alert type="warning">
								Remove this configuration from {scope.bucket}.
								{setting.mode === "encryption" &&
									" This restores the default SSE-S3 encryption setting."}
							</Alert>
						) : setting.mode === "versioning" ? (
							<FormField
								label="Bucket versioning"
								description="Once enabled, versioning can only be suspended. Existing versions remain."
							>
								<Select
									selectedOption={
										draft.Status
											? {
													value: String(draft.Status),
													label: String(draft.Status),
												}
											: null
									}
									options={[
										{ value: "Enabled", label: "Enable" },
										{ value: "Suspended", label: "Suspend" },
									]}
									onChange={({ detail }) =>
										setDraft({ Status: detail.selectedOption.value })
									}
								/>
							</FormField>
						) : setting.mode === "block" ? (
							<SpaceBetween size="m">
								<Checkbox
									checked={Object.keys(blockLabels).every(
										(key) => !!draft[key],
									)}
									onChange={({ detail }) =>
										setDraft(
											Object.fromEntries(
												Object.keys(blockLabels).map((key) => [
													key,
													detail.checked,
												]),
											),
										)
									}
								>
									Block all public access
								</Checkbox>
								{Object.entries(blockLabels).map(([key, label]) => (
									<Checkbox
										key={key}
										checked={!!draft[key]}
										onChange={({ detail }) =>
											setDraft({ ...draft, [key]: detail.checked })
										}
									>
										{label}
									</Checkbox>
								))}
							</SpaceBetween>
						) : setting.mode === "ownership" ? (
							<FormField label="Object ownership">
								<Select
									selectedOption={{
										value: (draft.Rules as { ObjectOwnership: string }[])[0]
											.ObjectOwnership,
									}}
									options={[
										{
											value: "BucketOwnerEnforced",
											label: "Bucket owner enforced (ACLs disabled)",
										},
										{
											value: "BucketOwnerPreferred",
											label: "Bucket owner preferred",
										},
										{ value: "ObjectWriter", label: "Object writer" },
									]}
									onChange={({ detail }) =>
										setDraft({
											Rules: [{ ObjectOwnership: detail.selectedOption.value }],
										})
									}
								/>
							</FormField>
						) : setting.mode === "encryption" ? (
							<EncryptionForm value={draft} onChange={setDraft} />
						) : setting.mode === "tags" ? (
							<TagForm
								tags={(draft.TagSet ?? []) as Tag[]}
								onChange={(TagSet) => setDraft({ TagSet })}
							/>
						) : (
							<FormField
								label={
									setting.mode === "policy"
										? "Policy JSON"
										: "Configuration JSON"
								}
							>
								<Textarea
									ariaLabel={`${setting.title} JSON`}
									rows={16}
									value={text}
									onChange={({ detail }) => setText(detail.value)}
								/>
							</FormField>
						)}
					</S3Dialog>
				)}
			</SpaceBetween>
		</Container>
	);
}
function EncryptionForm({
	value,
	onChange,
}: {
	value: Config;
	onChange: (value: Config) => void;
}) {
	const rule = (
		value.Rules as {
			ApplyServerSideEncryptionByDefault: {
				SSEAlgorithm: string;
				KMSMasterKeyID?: string;
			};
			BucketKeyEnabled?: boolean;
		}[]
	)[0];
	const defaults = rule.ApplyServerSideEncryptionByDefault;
	const update = (changes: Partial<typeof defaults>) =>
		onChange({
			Rules: [
				{
					...rule,
					ApplyServerSideEncryptionByDefault: { ...defaults, ...changes },
				},
			],
		});
	return (
		<SpaceBetween size="l">
			<FormField label="Encryption type">
				<Select
					selectedOption={{ value: defaults.SSEAlgorithm }}
					options={[
						{ value: "AES256", label: "Amazon S3 managed keys (SSE-S3)" },
						{ value: "aws:kms", label: "AWS KMS keys (SSE-KMS)" },
						{
							value: "aws:kms:dsse",
							label: "Dual-layer encryption (DSSE-KMS)",
						},
					]}
					onChange={({ detail }) =>
						update({
							SSEAlgorithm: detail.selectedOption.value || "AES256",
							KMSMasterKeyID: undefined,
						})
					}
				/>
			</FormField>
			{defaults.SSEAlgorithm !== "AES256" && (
				<FormField label="KMS key ID or ARN (optional)">
					<Input
						value={defaults.KMSMasterKeyID || ""}
						onChange={({ detail }) =>
							update({ KMSMasterKeyID: detail.value || undefined })
						}
					/>
				</FormField>
			)}
			<Checkbox
				checked={!!rule.BucketKeyEnabled}
				onChange={({ detail }) =>
					onChange({ Rules: [{ ...rule, BucketKeyEnabled: detail.checked }] })
				}
			>
				Enable bucket key
			</Checkbox>
		</SpaceBetween>
	);
}
function TagForm({
	tags,
	onChange,
}: {
	tags: Tag[];
	onChange: (tags: Tag[]) => void;
}) {
	return (
		<SpaceBetween size="m">
			{tags.map((tag, index) => (
				// biome-ignore lint/suspicious/noArrayIndexKey: editable rows contain only controlled inputs.
				<SpaceBetween key={index} direction="horizontal" size="xs">
					<FormField label="Key">
						<Input
							ariaLabel={`Tag ${index + 1} key`}
							value={tag.Key}
							onChange={({ detail }) =>
								onChange(
									tags.map((t, i) =>
										i === index ? { ...t, Key: detail.value } : t,
									),
								)
							}
						/>
					</FormField>
					<FormField label="Value">
						<Input
							ariaLabel={`Tag ${index + 1} value`}
							value={tag.Value}
							onChange={({ detail }) =>
								onChange(
									tags.map((t, i) =>
										i === index ? { ...t, Value: detail.value } : t,
									),
								)
							}
						/>
					</FormField>
					<Button
						ariaLabel={`Remove tag ${index + 1}`}
						onClick={() => onChange(tags.filter((_, i) => i !== index))}
					>
						Remove
					</Button>
				</SpaceBetween>
			))}
			<Button onClick={() => onChange([...tags, { Key: "", Value: "" }])}>
				Add tag
			</Button>
		</SpaceBetween>
	);
}
