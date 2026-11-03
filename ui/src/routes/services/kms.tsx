import {
	Alert,
	Button,
	Checkbox,
	Container,
	FormField,
	Header,
	Input,
	Select,
	SpaceBetween,
	Table,
	Tabs,
	Textarea,
} from "@cloudscape-design/components";
import { createFileRoute } from "@tanstack/react-router";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { ConsoleLayout } from "#/components/Console";
import { ResourceTable } from "#/components/ResourceTable";
import { pretty, type Resource } from "#/lib/api";
import {
	type KMSAlias,
	type KMSKey,
	type KMSScope,
	type KMSTag,
	kmsCall,
	kmsDecode,
	kmsEncode,
	kmsHref,
	kmsPages,
	kmsSpecs,
	kmsUsages,
} from "#/lib/kms";
import { validRegion } from "#/lib/s3";
import { useScope } from "#/lib/scope";
import { usePoll } from "#/lib/usePoll";

export const Route = createFileRoute("/services/kms")({
	validateSearch: (s: Record<string, unknown>) => ({
		account: String(s.account ?? ""),
		region: String(s.region ?? ""),
		key: String(s.key ?? ""),
	}),
	component: KMSPage,
});
function KMSPage() {
	const search = Route.useSearch(),
		{ account, region, canWrite, capabilities, inventory } = useScope();
	const enabled =
		!!capabilities.data?.pages.kms &&
		canWrite &&
		/^\d{12}$/.test(account) &&
		validRegion(region);
	const scope = { account, region, key: search.key },
		href = kmsHref({ account, region });
	return (
		<ConsoleLayout
			navigationHeader={{ text: "Key Management Service", href }}
			activeHref={href}
			navigationItems={[
				{ type: "link", text: "Keys", href },
				{ type: "link", text: "All services", href: "/services" },
			]}
			breadcrumbs={[
				{ text: "KMS", href },
				...(search.key ? [{ text: search.key, href: kmsHref(scope) }] : []),
			]}
			content={
				<SpaceBetween size="l">
					{(capabilities.error || inventory.error) && (
						<Alert type="error">{capabilities.error || inventory.error}</Alert>
					)}
					{capabilities.data?.requires_signature && !canWrite && (
						<Alert type="info">
							Connect a root console session for this account to manage keys.
						</Alert>
					)}
					{capabilities.data && !capabilities.data.pages.kms && (
						<Alert type="warning">KMS is not enabled on this server.</Alert>
					)}
					<KMSWorkspace
						key={`${account}/${region}/${search.key}/${enabled}`}
						scope={scope}
						enabled={enabled}
						resources={(inventory.data ?? []).filter(
							(r) =>
								r.service === "kms" &&
								r.type === "key" &&
								r.account === account &&
								r.region === region,
						)}
						onChanged={inventory.reload}
					/>
				</SpaceBetween>
			}
		/>
	);
}
function KMSWorkspace({
	scope,
	enabled,
	resources,
	onChanged,
}: {
	scope: KMSScope;
	enabled: boolean;
	resources: Resource[];
	onChanged: () => void;
}) {
	const [action, setAction] = useState("");
	const poll = usePoll(async () =>
		enabled
			? {
					keys: await kmsPages<{ KeyId: string; KeyArn: string }>(
						scope,
						"ListKeys",
						"Keys",
					),
					aliases: await kmsPages<KMSAlias>(scope, "ListAliases", "Aliases"),
				}
			: undefined,
	);
	const changed = () => {
		setAction("");
		poll.reload();
		onChanged();
	};
	const rows: Resource[] =
		poll.data?.keys.map((k) => ({
			service: "kms",
			type: "key",
			name: k.KeyId,
			arn: k.KeyArn,
			account: scope.account,
			region: scope.region,
		})) ?? resources;
	return (
		<SpaceBetween size="l">
			{poll.error && <Alert type="error">{poll.error}</Alert>}
			{scope.key ? (
				<KMSDetails
					scope={scope}
					enabled={enabled}
					resource={resources.find(
						(r) => r.arn === scope.key || r.name === scope.key,
					)}
					onChanged={changed}
				/>
			) : (
				<>
					<Header
						actions={
							<Button
								variant="primary"
								disabled={!enabled}
								onClick={() => setAction("Create key")}
							>
								Create key
							</Button>
						}
					>
						Keys
					</Header>
					<ResourceTable
						title="Keys"
						resources={rows}
						loading={poll.loading}
						onRefresh={poll.reload}
						emptyText="Create a key to encrypt data or manage signing and HMAC workflows."
						extraColumns={[
							{
								id: "aliases",
								header: "Aliases",
								cell: (r) =>
									poll.data?.aliases
										.filter((a) => a.TargetKeyId === r.name)
										.map((a) => a.AliasName)
										.join(", ") ||
									(r.details?.Aliases as string[] | undefined)?.join(", ") ||
									"—",
							},
						]}
					/>
				</>
			)}
			{!!action && (
				<KMSDialog
					scope={scope}
					action={action}
					onClose={() => setAction("")}
					onSaved={changed}
				/>
			)}
		</SpaceBetween>
	);
}
function KMSDetails({
	scope,
	enabled,
	resource,
	onChanged,
}: {
	scope: KMSScope;
	enabled: boolean;
	onChanged: () => void;
	resource?: Resource;
}) {
	const [action, setAction] = useState(""),
		[alias, setAlias] = useState("");
	const poll = usePoll(async () =>
		enabled
			? await kmsCall<{ KeyMetadata: KMSKey }>(scope, "DescribeKey", {
					KeyId: scope.key,
				})
			: undefined,
	);
	const settings = usePoll(async () =>
		enabled
			? {
					policy: await kmsCall<{ Policy: string }>(scope, "GetKeyPolicy", {
						KeyId: scope.key,
						PolicyName: "default",
					}),
					tags: await kmsPages<KMSTag>(scope, "ListResourceTags", "Tags", {
						KeyId: scope.key,
					}),
					aliases: await kmsPages<KMSAlias>(scope, "ListAliases", "Aliases", {
						KeyId: scope.key,
					}),
					rotation: await kmsCall<{ KeyRotationEnabled: boolean }>(
						scope,
						"GetKeyRotationStatus",
						{ KeyId: scope.key },
					),
				}
			: undefined,
	);
	const key =
			poll.data?.KeyMetadata ??
			(resource?.details?.KeyMetadata as KMSKey | undefined),
		data = settings.data;
	const changed = () => {
		setAction("");
		poll.reload();
		settings.reload();
		onChanged();
	};
	const customer = key?.KeyManager === "CUSTOMER",
		pending = key?.KeyState === "PendingDeletion",
		usable = key?.KeyState === "Enabled";
	const button = (name: string, allowed = true) => (
		<Button
			disabled={!enabled || !key || !allowed}
			onClick={() => {
				setAlias("");
				setAction(name);
			}}
		>
			{name}
		</Button>
	);
	return (
		<SpaceBetween size="l">
			{(poll.error || settings.error) && (
				<Alert type="error">{poll.error || settings.error}</Alert>
			)}
			<Header
				actions={
					<Button
						iconName="refresh"
						ariaLabel="Refresh key"
						onClick={() => {
							poll.reload();
							settings.reload();
						}}
					/>
				}
			>
				{key?.Description || scope.key}
			</Header>
			<SpaceBetween size="xs" direction="horizontal">
				{button(
					key?.KeyState === "Disabled" ? "Enable key" : "Disable key",
					customer && !pending,
				)}
				{button(
					pending ? "Cancel key deletion" : "Schedule key deletion",
					customer,
				)}
				{button("Edit description", customer && !pending)}
			</SpaceBetween>
			{pending && (
				<Alert type="warning">
					This key is scheduled for deletion
					{key?.DeletionDate
						? ` on ${new Date(key.DeletionDate * 1000).toISOString()}`
						: ""}
					. Canceling deletion leaves the key disabled.
				</Alert>
			)}
			<Tabs
				tabs={[
					{
						id: "metadata",
						label: "Metadata",
						content: (
							<Container header={<Header>Key metadata</Header>}>
								<pre
									style={{ whiteSpace: "pre-wrap", overflowWrap: "anywhere" }}
								>
									{key
										? JSON.stringify(key, null, 2)
										: "Key metadata is unavailable."}
								</pre>
							</Container>
						),
					},
					{
						id: "policy",
						label: "Key policy",
						content: (
							<Container
								header={
									<Header
										actions={button(
											"Edit key policy",
											customer && !pending && !!data,
										)}
									>
										Default key policy
									</Header>
								}
							>
								<pre
									style={{ whiteSpace: "pre-wrap", overflowWrap: "anywhere" }}
								>
									{data ? pretty(data.policy.Policy) : "Policy is unavailable."}
								</pre>
							</Container>
						),
					},
					{
						id: "aliases",
						label: "Aliases",
						content: (
							<Table
								items={data?.aliases ?? []}
								loading={settings.loading}
								empty="No aliases. Create an alias to give this key a readable name."
								header={
									<Header
										actions={button("Create alias", customer && !pending)}
									>
										Aliases
									</Header>
								}
								columnDefinitions={[
									{ id: "name", header: "Alias", cell: (a) => a.AliasName },
									{
										id: "actions",
										header: "Actions",
										cell: (a) => (
											<SpaceBetween direction="horizontal" size="xs">
												{["Update alias", "Delete alias"].map((name) => (
													<Button
														key={name}
														disabled={
															!enabled || a.AliasName.startsWith("alias/aws/")
														}
														onClick={() => {
															setAlias(a.AliasName);
															setAction(name);
														}}
													>
														{name}
													</Button>
												))}
											</SpaceBetween>
										),
									},
								]}
							/>
						),
					},
					{
						id: "tags",
						label: "Tags",
						content: (
							<Table
								items={data?.tags ?? []}
								empty="No tags. Add a tag to organize this key."
								header={
									<Header actions={button("Add tag", customer && !pending)}>
										Tags
									</Header>
								}
								columnDefinitions={[
									{ id: "key", header: "Key", cell: (t) => t.TagKey },
									{ id: "value", header: "Value", cell: (t) => t.TagValue },
									{
										id: "actions",
										header: "Actions",
										cell: (t) => (
											<SpaceBetween direction="horizontal" size="xs">
												{["Edit tag", "Remove tag"].map((name) => (
													<Button
														key={name}
														disabled={!enabled || !customer || pending}
														onClick={() => {
															setAlias(t.TagKey);
															setAction(name);
														}}
													>
														{name}
													</Button>
												))}
											</SpaceBetween>
										),
									},
								]}
							/>
						),
					},
					{
						id: "rotation",
						label: "Rotation",
						content: (
							<Container
								header={
									<Header
										actions={button(
											data?.rotation.KeyRotationEnabled
												? "Disable rotation"
												: "Enable rotation",
											customer &&
												usable &&
												key?.KeySpec === "SYMMETRIC_DEFAULT" &&
												!!data,
										)}
									>
										Automatic rotation
									</Header>
								}
							>
								<SpaceBetween size="m">
									<div>
										{data
											? data.rotation.KeyRotationEnabled
												? "Enabled"
												: "Disabled"
											: "Unavailable"}
									</div>
									<Alert type="info">
										Rotation settings are recorded locally; key material is not
										rotated.
									</Alert>
								</SpaceBetween>
							</Container>
						),
					},
					{
						id: "tester",
						label: "Encrypt / decrypt",
						content:
							key?.KeyUsage === "ENCRYPT_DECRYPT" ? (
								<KMSTester
									scope={scope}
									keyData={key}
									enabled={enabled && usable}
								/>
							) : (
								<Alert type="info">
									Encryption requires an ENCRYPT_DECRYPT key.
								</Alert>
							),
					},
				]}
			/>
			{!!action && (
				<KMSDialog
					scope={scope}
					action={action}
					alias={alias}
					keyData={key}
					policy={data && pretty(data.policy.Policy)}
					tagValue={data?.tags.find((t) => t.TagKey === alias)?.TagValue}
					onClose={() => setAction("")}
					onSaved={changed}
				/>
			)}
		</SpaceBetween>
	);
}
function Choice({
	label,
	value,
	values,
	onChange,
}: {
	label: string;
	value: string;
	values: string[];
	onChange: (s: string) => void;
}) {
	return (
		<FormField label={label}>
			<Select
				selectedOption={{ label: value, value }}
				options={values.map((value) => ({ label: value, value }))}
				onChange={({ detail }) => onChange(detail.selectedOption.value ?? "")}
			/>
		</FormField>
	);
}
function KMSDialog({
	scope,
	action,
	alias = "",
	keyData,
	policy = "",
	tagValue = "",
	onClose,
	onSaved,
}: {
	scope: KMSScope;
	action: string;
	alias?: string;
	keyData?: KMSKey;
	policy?: string;
	tagValue?: string;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [description, setDescription] = useState(keyData?.Description ?? ""),
		[spec, setSpec] = useState("SYMMETRIC_DEFAULT"),
		[usage, setUsage] = useState("ENCRYPT_DECRYPT");
	const [name, setName] = useState(alias),
		[target, setTarget] = useState(scope.key ?? ""),
		[text, setText] = useState(policy),
		[value, setValue] = useState(tagValue),
		[days, setDays] = useState("30");
	const create = action === "Create key",
		aliasAction = action.endsWith("alias"),
		tagAction = action.endsWith("tag");
	// Typed confirmation guards actions that destroy data or can lock callers out; reversible ones skip it.
	const destructive = [
		"Schedule key deletion",
		"Delete alias",
		"Edit key policy",
	].includes(action);
	return (
		<ActionDialog
			title={action}
			submit={action.startsWith("Edit ") ? "Save changes" : action}
			confirmation={
				destructive ? alias || keyData?.KeyId || scope.key : undefined
			}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				let operation = "",
					input: Record<string, unknown> = { KeyId: scope.key };
				switch (action) {
					case "Create key":
						operation = "CreateKey";
						input = {
							Description: description,
							KeySpec: spec,
							KeyUsage: usage,
						};
						break;
					case "Edit description":
						operation = "UpdateKeyDescription";
						input.Description = description;
						break;
					case "Enable key":
						operation = "EnableKey";
						break;
					case "Disable key":
						operation = "DisableKey";
						break;
					case "Schedule key deletion":
						operation = "ScheduleKeyDeletion";
						input.PendingWindowInDays = Number(days);
						break;
					case "Cancel key deletion":
						operation = "CancelKeyDeletion";
						break;
					case "Enable rotation":
						operation = "EnableKeyRotation";
						break;
					case "Disable rotation":
						operation = "DisableKeyRotation";
						break;
					case "Edit key policy": {
						const parsed = JSON.parse(text);
						if (!parsed || typeof parsed !== "object" || Array.isArray(parsed))
							throw new Error("Enter a JSON policy object.");
						operation = "PutKeyPolicy";
						input.PolicyName = "default";
						input.Policy = text;
						break;
					}
					case "Create alias":
					case "Update alias":
					case "Delete alias":
						if (
							!/^alias\/[a-zA-Z0-9/_-]+$/.test(name) ||
							name.startsWith("alias/aws/")
						)
							throw new Error(
								"Enter an alias beginning with alias/ outside the reserved alias/aws/ prefix.",
							);
						operation =
							action === "Create alias"
								? "CreateAlias"
								: action === "Update alias"
									? "UpdateAlias"
									: "DeleteAlias";
						if (operation !== "DeleteAlias" && !target.trim())
							throw new Error("Enter a target key ID or ARN.");
						input = {
							AliasName: name,
							...(operation !== "DeleteAlias"
								? { TargetKeyId: target.trim() }
								: {}),
						};
						break;
					case "Add tag":
					case "Edit tag":
					case "Remove tag":
						if (!name.trim()) throw new Error("Enter a tag key.");
						operation =
							action === "Remove tag" ? "UntagResource" : "TagResource";
						input = {
							KeyId: scope.key,
							...(operation === "UntagResource"
								? { TagKeys: [name] }
								: { Tags: [{ TagKey: name, TagValue: value }] }),
						};
						break;
					default:
						throw new Error("Unsupported action.");
				}
				return [{ label: action, run: () => kmsCall(scope, operation, input) }];
			}}
		>
			<SpaceBetween size="m">
				{(create || action === "Edit description") && (
					<FormField label="Description">
						<Input
							value={description}
							onChange={({ detail }) => setDescription(detail.value)}
						/>
					</FormField>
				)}
				{create && (
					<>
						<Choice
							label="Key spec"
							value={spec}
							values={kmsSpecs}
							onChange={(s) => {
								setSpec(s);
								setUsage(kmsUsages(s)[0]);
							}}
						/>
						<Choice
							label="Key usage"
							value={usage}
							values={kmsUsages(spec)}
							onChange={setUsage}
						/>
						<Alert type="info">
							Keys are created in a single Region with KMS-generated material.
						</Alert>
					</>
				)}
				{action === "Schedule key deletion" && (
					<>
						<Alert type="warning">
							Deletion permanently removes the key after the waiting period.
							Data encrypted with it will become inaccessible.
						</Alert>
						<Choice
							label="Pending window (days)"
							value={days}
							values={Array.from({ length: 24 }, (_, i) => String(i + 7))}
							onChange={setDays}
						/>
					</>
				)}
				{action === "Cancel key deletion" && (
					<Alert type="info">
						The key will be disabled after cancellation. Enable it separately to
						use it again.
					</Alert>
				)}
				{action === "Edit key policy" && (
					<FormField label="Default key policy (JSON)">
						<Textarea
							rows={16}
							value={text}
							onChange={({ detail }) => setText(detail.value)}
						/>
					</FormField>
				)}
				{(aliasAction || tagAction) && (
					<FormField label={aliasAction ? "Alias name" : "Tag key"}>
						<Input
							value={name}
							disabled={action !== "Create alias" && action !== "Add tag"}
							onChange={({ detail }) => setName(detail.value)}
						/>
					</FormField>
				)}
				{aliasAction && action !== "Delete alias" && (
					<FormField label="Target key ID or ARN">
						<Input
							value={target}
							onChange={({ detail }) => setTarget(detail.value)}
						/>
					</FormField>
				)}
				{tagAction && action !== "Remove tag" && (
					<FormField label="Tag value">
						<Input
							value={value}
							onChange={({ detail }) => setValue(detail.value)}
						/>
					</FormField>
				)}
			</SpaceBetween>
		</ActionDialog>
	);
}
function KMSTester({
	scope,
	keyData,
	enabled,
}: {
	scope: KMSScope;
	keyData: KMSKey;
	enabled: boolean;
}) {
	const algorithms = keyData.EncryptionAlgorithms?.length
		? keyData.EncryptionAlgorithms
		: ["SYMMETRIC_DEFAULT"];
	const [mode, setMode] = useState("Encrypt"),
		[algorithm, setAlgorithm] = useState(algorithms[0]),
		[text, setText] = useState(""),
		[result, setResult] = useState(""),
		[reveal, setReveal] = useState(false),
		[busy, setBusy] = useState(false),
		[error, setError] = useState("");
	const run = async () => {
		if (busy) return;
		setBusy(true);
		setError("");
		setResult("");
		setReveal(false);
		try {
			if (!text) throw new Error("Enter a value to test.");
			if (mode === "Decrypt") atob(text);
			const out = await kmsCall<{
				CiphertextBlob?: string;
				Plaintext?: string;
			}>(scope, mode, {
				KeyId: scope.key,
				EncryptionAlgorithm: algorithm,
				...(mode === "Encrypt"
					? { Plaintext: kmsEncode(text) }
					: { CiphertextBlob: text }),
			});
			setResult(
				mode === "Encrypt"
					? (out.CiphertextBlob ?? "")
					: kmsDecode(out.Plaintext ?? ""),
			);
		} catch (err) {
			setError(err instanceof Error ? err.message : String(err));
		} finally {
			setBusy(false);
		}
	};
	return (
		<Container header={<Header>Encrypt / decrypt tester</Header>}>
			<SpaceBetween size="m">
				{error && <Alert type="error">{error}</Alert>}
				<Choice
					label="Operation"
					value={mode}
					values={["Encrypt", "Decrypt"]}
					onChange={(s) => {
						setMode(s);
						// Carry the last output over so an encrypt can be decrypted straight back.
						setText(result);
						setResult("");
						setReveal(false);
					}}
				/>
				<Choice
					label="Encryption algorithm"
					value={algorithm}
					values={algorithms}
					onChange={(s) => {
						setAlgorithm(s);
						setResult("");
					}}
				/>
				<Checkbox
					checked={reveal}
					onChange={({ detail }) => setReveal(detail.checked)}
				>
					Reveal values
				</Checkbox>
				<FormField
					label={
						mode === "Encrypt" ? "Plaintext (UTF-8)" : "Ciphertext (base64)"
					}
				>
					<Input
						type={reveal ? "text" : "password"}
						value={text}
						onChange={({ detail }) => {
							setText(detail.value);
							setResult("");
						}}
					/>
				</FormField>
				<Button
					variant="primary"
					disabled={!enabled}
					loading={busy}
					onClick={run}
				>
					{mode}
				</Button>
				{result && (
					<FormField
						label={
							mode === "Encrypt" ? "Ciphertext (base64)" : "Plaintext (UTF-8)"
						}
					>
						<Input
							readOnly
							type={reveal ? "text" : "password"}
							value={result}
						/>
					</FormField>
				)}
			</SpaceBetween>
		</Container>
	);
}
