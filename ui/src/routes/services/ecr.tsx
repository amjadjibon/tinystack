import {
	Alert,
	Checkbox,
	Container,
	Header,
	SpaceBetween,
	Tabs,
} from "@cloudscape-design/components";
import { createFileRoute } from "@tanstack/react-router";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import {
	Action,
	Buttons,
	Choice,
	ContainerLayout,
	Field,
	Rows,
} from "#/components/containers/Controls";
import { pretty } from "#/lib/api";
import {
	type ContainerScope,
	containerPages,
	ecrCall,
	type Image,
	jsonObject,
	type Repository,
} from "#/lib/ecr";
import { validRegion } from "#/lib/s3";
import { useScope } from "#/lib/scope";
import { usePoll } from "#/lib/usePoll";
export const Route = createFileRoute("/services/ecr")({
	validateSearch: (search: Record<string, unknown>) => ({
		account: String(search.account ?? ""),
		region: String(search.region ?? ""),
	}),
	component: Page,
});
function Page() {
	const s = useScope();
	const enabled =
		!!s.capabilities.data?.pages.ecr &&
		s.canWrite &&
		/^\d{12}$/.test(s.account) &&
		validRegion(s.region);
	return (
		<ContainerLayout service="ecr" title="Elastic Container Registry">
			<Workspace
				key={`${s.account}/${s.region}/${enabled}`}
				scope={s}
				enabled={enabled}
			/>
		</ContainerLayout>
	);
}
function Workspace({
	scope,
	enabled,
}: {
	scope: ContainerScope;
	enabled: boolean;
}) {
	const call = (op: string, input: Record<string, unknown> = {}) =>
		ecrCall(scope, op, input);
	const repos = usePoll(() =>
		enabled
			? containerPages<Repository>(call, "DescribeRepositories", "Repositories")
			: Promise.resolve([]),
	);
	const [selected, setSelected] = useState(""),
		[dialog, setDialog] = useState("");
	const repo = repos.data?.find((r) => r.RepositoryName === selected);
	const [name, setName] = useState(""),
		[mutability, setMutability] = useState("MUTABLE"),
		[scan, setScan] = useState("Disabled"),
		[encryption, setEncryption] = useState("AES256"),
		[force, setForce] = useState(false);
	const changed = () => {
		setDialog("");
		repos.reload();
	};
	return (
		<SpaceBetween size="l">
			{repos.error && <Alert type="error">{repos.error}</Alert>}
			<Rows
				title="Repositories"
				emptyText="Choose Create repository to get started."
				items={repos.data ?? []}
				loading={repos.loading}
				actions={
					<Buttons>
						<Action onClick={repos.reload}>Refresh</Action>
						<Action
							disabled={!enabled}
							onClick={() => {
								setName("");
								setDialog("Create repository");
							}}
						>
							Create repository
						</Action>
					</Buttons>
				}
				columns={[
					{
						id: "name",
						header: "Repository",
						cell: (r) => (
							<Action onClick={() => setSelected(r.RepositoryName)}>
								{r.RepositoryName}
							</Action>
						),
					},
					{ id: "uri", header: "Repository URI", cell: (r) => r.RepositoryUri },
					{
						id: "mutability",
						header: "Tag mutability",
						cell: (r) => r.ImageTagMutability,
					},
					{
						id: "scan",
						header: "Scan on push",
						cell: (r) =>
							r.ImageScanningConfiguration?.ScanOnPush ? "Enabled" : "Disabled",
					},
					{
						id: "encryption",
						header: "Encryption",
						cell: (r) => r.EncryptionConfiguration?.EncryptionType ?? "AES256",
					},
				]}
			/>
			{repo && (
				<>
					<Header
						actions={
							<Buttons>
								<Action
									onClick={() => {
										setMutability(repo.ImageTagMutability);
										setScan(
											repo.ImageScanningConfiguration?.ScanOnPush
												? "Enabled"
												: "Disabled",
										);
										setDialog("Edit repository");
									}}
								>
									Edit settings
								</Action>
								<Action
									onClick={() => {
										setForce(false);
										setDialog("Delete repository");
									}}
								>
									Delete repository
								</Action>
							</Buttons>
						}
					>
						{repo.RepositoryName}
					</Header>
					<RepositoryDetails
						key={repo.RepositoryArn}
						scope={scope}
						repo={repo}
					/>
				</>
			)}
			{dialog && (
				<ActionDialog
					title={dialog}
					submit={dialog === "Edit repository" ? "Save changes" : dialog}
					confirmation={dialog === "Delete repository" ? selected : undefined}
					onClose={() => setDialog("")}
					onSaved={changed}
					plan={async () => {
						if (dialog === "Delete repository")
							return [
								{
									label: dialog,
									run: () =>
										call("DeleteRepository", {
											repositoryName: selected,
											force,
										}),
								},
							];
						if (
							dialog === "Create repository" &&
							!/^(?:[a-z0-9]+(?:[._-][a-z0-9]+)*\/)*[a-z0-9]+(?:[._-][a-z0-9]+)*$/.test(
								name,
							)
						)
							throw new Error("Enter a valid lowercase repository name.");
						const input = {
							repositoryName: dialog === "Create repository" ? name : selected,
							imageTagMutability: mutability,
							imageScanningConfiguration: { scanOnPush: scan === "Enabled" },
						};
						return dialog === "Create repository"
							? [
									{
										label: dialog,
										run: () =>
											call("CreateRepository", {
												...input,
												encryptionConfiguration: {
													encryptionType: encryption,
												},
											}),
									},
								]
							: [
									{
										label: "Tag mutability",
										run: () =>
											call("PutImageTagMutability", {
												repositoryName: selected,
												imageTagMutability: mutability,
											}),
									},
									{
										label: "Scan on push",
										run: () =>
											call("PutImageScanningConfiguration", {
												repositoryName: selected,
												imageScanningConfiguration:
													input.imageScanningConfiguration,
											}),
									},
								];
					}}
				>
					<SpaceBetween size="m">
						{dialog === "Delete repository" ? (
							<Checkbox
								checked={force}
								onChange={({ detail }) => setForce(detail.checked)}
							>
								Force deletion, including all images
							</Checkbox>
						) : (
							<>
								{dialog === "Create repository" && (
									<Field
										label="Repository name"
										value={name}
										onChange={setName}
									/>
								)}
								<Choice
									label="Tag mutability"
									value={mutability}
									options={["MUTABLE", "IMMUTABLE"]}
									onChange={setMutability}
								/>
								<Choice
									label="Scan on push"
									value={scan}
									options={["Enabled", "Disabled"]}
									onChange={setScan}
								/>
								{dialog === "Create repository" && (
									<Choice
										label="Encryption"
										value={encryption}
										options={["AES256"]}
										onChange={setEncryption}
									/>
								)}
							</>
						)}
					</SpaceBetween>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
function RepositoryDetails({
	scope,
	repo,
}: {
	scope: ContainerScope;
	repo: Repository;
}) {
	const call = (op: string, input: Record<string, unknown> = {}) =>
		ecrCall(scope, op, { repositoryName: repo.RepositoryName, ...input });
	const images = usePoll(() =>
		containerPages<Image>(call, "DescribeImages", "ImageDetails"),
	);
	const [image, setImage] = useState<Image>();
	const host = repo.RepositoryUri.slice(0, -(repo.RepositoryName.length + 1));
	const [endpoint, setEndpoint] = useState(
		`${globalThis.location?.protocol ?? "http:"}//${host}`,
	);
	const quote = (v: string) => `'${v.replaceAll("'", "'\\''")}'`;
	return (
		<Tabs
			tabs={[
				{
					id: "images",
					label: "Images",
					content: (
						<SpaceBetween size="m">
							{images.error && <Alert type="error">{images.error}</Alert>}
							<Rows
								title="Images"
								emptyText="Push an image using the commands in the Push commands tab."
								items={images.data ?? []}
								actions={<Action onClick={images.reload}>Refresh</Action>}
								columns={[
									{
										id: "digest",
										header: "Digest",
										cell: (i) => i.ImageDigest,
									},
									{
										id: "tags",
										header: "Tags",
										cell: (i) => i.ImageTags?.join(", ") || "Untagged",
									},
									{
										id: "size",
										header: "Bytes",
										cell: (i) => i.ImageSizeInBytes ?? "—",
									},
									{
										id: "actions",
										header: "Actions",
										cell: (i) => (
											<Action onClick={() => setImage(i)}>Delete image</Action>
										),
									},
								]}
							/>
							{image && (
								<ActionDialog
									title="Delete image"
									submit="Delete image"
									confirmation={image.ImageDigest}
									onClose={() => setImage(undefined)}
									onSaved={() => {
										setImage(undefined);
										images.reload();
									}}
									plan={async () => [
										{
											label: "Delete image",
											run: async () => {
												const out = await call("BatchDeleteImage", {
													imageIds: [{ imageDigest: image.ImageDigest }],
												});
												if ((out.Failures as unknown[])?.length)
													throw new Error(JSON.stringify(out.Failures));
											},
										},
									]}
								>
									Deleting by digest removes all tags for this image.
								</ActionDialog>
							)}
						</SpaceBetween>
					),
				},
				{
					id: "lifecycle",
					label: "Lifecycle policy",
					content: <Policy scope={scope} repo={repo} lifecycle />,
				},
				{
					id: "policy",
					label: "Repository policy",
					content: <Policy scope={scope} repo={repo} />,
				},
				{
					id: "tags",
					label: "Tags",
					content: <Tags scope={scope} repo={repo} />,
				},
				{
					id: "commands",
					label: "Push commands",
					content: (
						<Container header={<Header>Push to the local registry</Header>}>
							<SpaceBetween size="m">
								<Field
									label="Local gateway endpoint (include scheme)"
									value={endpoint}
									onChange={setEndpoint}
								/>
								<BoxCommands
									text={`aws --endpoint-url ${quote(endpoint)} --region ${quote(scope.region)} ecr get-login-password | docker login --username AWS --password-stdin ${quote(host)}\ndocker tag IMAGE:TAG ${quote(`${repo.RepositoryUri}:TAG`)}\ndocker push ${quote(`${repo.RepositoryUri}:TAG`)}`}
								/>
								<Alert type="info">
									Use CLI credentials for account {scope.account}. For an HTTP
									endpoint, configure this registry as an insecure registry in
									your Docker client.
								</Alert>
							</SpaceBetween>
						</Container>
					),
				},
			]}
		/>
	);
}
function BoxCommands({ text }: { text: string }) {
	return (
		<pre style={{ whiteSpace: "pre-wrap", overflowWrap: "anywhere" }}>
			{text}
		</pre>
	);
}
function Policy({
	scope,
	repo,
	lifecycle = false,
}: {
	scope: ContainerScope;
	repo: Repository;
	lifecycle?: boolean;
}) {
	const get = lifecycle ? "GetLifecyclePolicy" : "GetRepositoryPolicy",
		put = lifecycle ? "PutLifecyclePolicy" : "SetRepositoryPolicy",
		del = lifecycle ? "DeleteLifecyclePolicy" : "DeleteRepositoryPolicy",
		key = lifecycle ? "LifecyclePolicyText" : "PolicyText";
	const poll = usePoll(async () => {
		try {
			return await ecrCall(scope, get, { repositoryName: repo.RepositoryName });
		} catch (e) {
			if (
				e instanceof Error &&
				e.message.includes(
					lifecycle
						? "LifecyclePolicyNotFoundException"
						: "RepositoryPolicyNotFoundException",
				)
			)
				return {};
			throw e;
		}
	});
	const [dialog, setDialog] = useState(""),
		[text, setText] = useState("{}");
	return (
		<SpaceBetween size="m">
			{!lifecycle && (
				<Alert type="info">
					Repository policies are stored as metadata. TinyStack does not
					evaluate these policies.
				</Alert>
			)}
			{poll.error && <Alert type="error">{poll.error}</Alert>}
			<BoxCommands
				text={
					poll.data?.[key]
						? pretty(String(poll.data[key]))
						: "No policy. Choose Edit to add one."
				}
			/>
			<Buttons>
				<Action
					disabled={!poll.data || !!poll.error}
					onClick={() => {
						setText(pretty(String(poll.data?.[key] ?? "{}")));
						setDialog("Edit");
					}}
				>
					Edit
				</Action>
				<Action
					disabled={!poll.data?.[key]}
					onClick={() => setDialog("Delete")}
				>
					Delete policy
				</Action>
			</Buttons>
			{dialog && (
				<ActionDialog
					title={`${dialog} policy`}
					submit={dialog === "Delete" ? "Delete policy" : "Save policy"}
					confirmation={dialog === "Delete" ? repo.RepositoryName : undefined}
					onClose={() => setDialog("")}
					onSaved={() => {
						setDialog("");
						poll.reload();
					}}
					plan={async () => {
						if (dialog === "Edit") jsonObject(text);
						return [
							{
								label: dialog,
								run: () =>
									ecrCall(scope, dialog === "Delete" ? del : put, {
										repositoryName: repo.RepositoryName,
										...(dialog === "Edit" ? { [key]: text } : {}),
									}),
							},
						];
					}}
				>
					{dialog === "Edit" ? (
						<Field
							multiline
							label="Policy JSON"
							value={text}
							onChange={setText}
						/>
					) : (
						"Remove the policy from this repository."
					)}
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
function Tags({ scope, repo }: { scope: ContainerScope; repo: Repository }) {
	const poll = usePoll(() =>
		ecrCall<{ Tags?: { Key: string; Value: string }[] }>(
			scope,
			"ListTagsForResource",
			{ resourceArn: repo.RepositoryArn },
		),
	);
	const [edit, setEdit] = useState(false),
		[text, setText] = useState("{}");
	return (
		<SpaceBetween size="m">
			{poll.error && <Alert type="error">{poll.error}</Alert>}
			<Rows
				title="Tags"
				emptyText="Choose Edit tags to add repository tags."
				items={poll.data?.Tags ?? []}
				columns={[
					{ id: "key", header: "Key", cell: (t) => t.Key },
					{ id: "value", header: "Value", cell: (t) => t.Value },
				]}
				actions={
					<Action
						disabled={!poll.data || !!poll.error}
						onClick={() => {
							setText(
								JSON.stringify(
									Object.fromEntries(
										(poll.data?.Tags ?? []).map((t) => [t.Key, t.Value]),
									),
									null,
									2,
								),
							);
							setEdit(true);
						}}
					>
						Edit tags
					</Action>
				}
			/>
			{edit && (
				<ActionDialog
					title="Edit tags"
					onClose={() => setEdit(false)}
					onSaved={() => {
						setEdit(false);
						poll.reload();
					}}
					plan={async () => {
						const value = jsonObject(text);
						if (
							Object.entries(value).some(
								([k, v]) => !k || typeof v !== "string",
							)
						)
							throw new Error(
								"Tags must be an object with nonempty keys and string values.",
							);
						const removed = (poll.data?.Tags ?? [])
							.filter((t) => !(t.Key in value))
							.map((t) => t.Key);
						return [
							...(removed.length
								? [
										{
											label: "Remove tags",
											run: () =>
												ecrCall(scope, "UntagResource", {
													resourceArn: repo.RepositoryArn,
													tagKeys: removed,
												}),
										},
									]
								: []),
							...(Object.keys(value).length
								? [
										{
											label: "Save tags",
											run: () =>
												ecrCall(scope, "TagResource", {
													resourceArn: repo.RepositoryArn,
													tags: Object.entries(value).map(([Key, Value]) => ({
														Key,
														Value,
													})),
												}),
										},
									]
								: []),
						];
					}}
				>
					<Field
						label="Tags JSON (key/value object; omitted keys are removed)"
						multiline
						value={text}
						onChange={setText}
					/>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
