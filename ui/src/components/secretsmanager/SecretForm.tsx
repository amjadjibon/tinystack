import {
	Alert,
	Button,
	ColumnLayout,
	FormField,
	Input,
	Select,
	SpaceBetween,
	Textarea,
} from "@cloudscape-design/components";
import { useEffect, useRef, useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { errorText, stringMap } from "#/lib/consoleActions";
import { type SecretScope, secretCall, valueInput } from "#/lib/secretsmanager";
import { tagList } from "#/lib/tags";

export function ValueEditor({
	scope,
	format,
	setFormat,
	text,
	setText,
	rows,
	setRows,
}: {
	scope: SecretScope;
	format: string;
	setFormat: (v: string) => void;
	text: string;
	setText: (v: string) => void;
	rows: { id: string; key: string; value: string }[];
	setRows: (v: { id: string; key: string; value: string }[]) => void;
}) {
	const [show, setShow] = useState(false),
		[busy, setBusy] = useState(false),
		[error, setError] = useState("");
	const alive = useRef(true);
	useEffect(() => {
		alive.current = true;
		return () => {
			alive.current = false;
		};
	}, []);
	const formats = [
		{ label: "Key/value pairs", value: "pairs" },
		{ label: "Plaintext", value: "text" },
		{ label: "Binary (base64)", value: "binary" },
	];
	return (
		<SpaceBetween size="m">
			<FormField label="Secret value type">
				<Select
					ariaLabel="Secret value type"
					options={formats}
					selectedOption={formats.find((o) => o.value === format) ?? null}
					onChange={({ detail }) =>
						setFormat(detail.selectedOption.value ?? "text")
					}
				/>
			</FormField>
			{format === "pairs" ? (
				<SpaceBetween size="s">
					{rows.map((row, index) => (
						<ColumnLayout key={row.id} columns={3}>
							<FormField label="Key">
								<Input
									ariaLabel={`Secret key ${index + 1}`}
									value={row.key}
									autoComplete="off"
									onChange={({ detail }) =>
										setRows(
											rows.map((r, i) =>
												i === index ? { ...r, key: detail.value } : r,
											),
										)
									}
								/>
							</FormField>
							<FormField label="Value">
								<Input
									ariaLabel={`Secret value ${index + 1}`}
									type={show ? "text" : "password"}
									value={row.value}
									autoComplete="off"
									onChange={({ detail }) =>
										setRows(
											rows.map((r, i) =>
												i === index ? { ...r, value: detail.value } : r,
											),
										)
									}
								/>
							</FormField>
							<Button
								onClick={() => setRows(rows.filter((_, i) => i !== index))}
							>
								Remove pair {index + 1}
							</Button>
						</ColumnLayout>
					))}
					<SpaceBetween direction="horizontal" size="xs">
						<Button
							onClick={() =>
								setRows([
									...rows,
									{ id: crypto.randomUUID(), key: "", value: "" },
								])
							}
						>
							Add row
						</Button>
						<Button onClick={() => setShow(!show)}>
							{show ? "Mask values" : "Show values"}
						</Button>
						<Button
							loading={busy}
							onClick={async () => {
								setBusy(true);
								setError("");
								try {
									const result = await secretCall<{ RandomPassword: string }>(
										{ account: scope.account, region: scope.region },
										"GetRandomPassword",
										{ PasswordLength: 32, RequireEachIncludedType: true },
									);
									if (alive.current)
										setRows(
											rows.some((r) => r.key === "password")
												? rows.map((r) =>
														r.key === "password"
															? { ...r, value: result.RandomPassword }
															: r,
													)
												: rows.length === 1 && !rows[0].key && !rows[0].value
													? [
															{
																...rows[0],
																key: "password",
																value: result.RandomPassword,
															},
														]
													: [
															...rows,
															{
																id: crypto.randomUUID(),
																key: "password",
																value: result.RandomPassword,
															},
														],
										);
								} catch (err) {
									if (alive.current) setError(errorText(err));
								} finally {
									if (alive.current) setBusy(false);
								}
							}}
						>
							Generate password
						</Button>
					</SpaceBetween>
				</SpaceBetween>
			) : (
				<FormField
					label={
						format === "binary" ? "Binary value (base64)" : "Plaintext value"
					}
					description="Enter a value up to 65,536 bytes. This editor displays what you enter."
				>
					<Textarea
						ariaLabel="Secret contents"
						rows={7}
						value={text}
						onChange={({ detail }) => setText(detail.value)}
					/>
				</FormField>
			)}
			{error && <Alert type="error">{error}</Alert>}
		</SpaceBetween>
	);
}
export function SecretForm({
	scope,
	create,
	onClose,
	onSaved,
}: {
	scope: SecretScope;
	create: boolean;
	onClose: () => void;
	onSaved: (arn?: string) => void;
}) {
	const [name, setName] = useState(""),
		[description, setDescription] = useState(""),
		[key, setKey] = useState(""),
		[tags, setTags] = useState("{}"),
		[format, setFormat] = useState("pairs"),
		[text, setText] = useState("");
	const [rows, setRows] = useState<
		{ id: string; key: string; value: string }[]
	>([{ id: crypto.randomUUID(), key: "", value: "" }]);
	const token = useRef(crypto.randomUUID()),
		created = useRef<string | undefined>(undefined);
	return (
		<ActionDialog
			title={create ? "Store a new secret" : "Edit secret value"}
			submit={create ? "Store secret" : "Save new version"}
			onClose={onClose}
			onSaved={() => onSaved(created.current)}
			plan={async () => {
				if (create && !/^[a-zA-Z0-9/_+=.@-]{1,512}$/.test(name))
					throw new Error(
						"Enter a secret name of 1–512 letters, numbers, or /_+=.@- characters.",
					);
				if (description.length > 2048)
					throw new Error("Description must be at most 2,048 characters.");
				const value = valueInput(format, text, rows);
				const input = {
					...value,
					ClientRequestToken: token.current,
					...(create
						? {
								Name: name,
								Description: description,
								...(key.trim() ? { KmsKeyId: key.trim() } : {}),
								Tags: tagList(stringMap(tags, "Tags")),
							}
						: {}),
				};
				return [
					{
						label: create ? "Store secret" : "Save new version",
						run: async () => {
							const out = await secretCall<{ ARN: string }>(
								scope,
								create ? "CreateSecret" : "PutSecretValue",
								input,
							);
							created.current = out.ARN;
						},
					},
				];
			}}
		>
			<SpaceBetween size="l">
				{!create && (
					<Alert type="info">
						Enter the replacement value. Saving creates a new AWSCURRENT version
						and moves the previous current version to AWSPREVIOUS.
					</Alert>
				)}
				<ValueEditor
					{...{ scope, format, setFormat, text, setText, rows, setRows }}
				/>
				{create && (
					<>
						<FormField label="Secret name">
							<Input
								ariaLabel="Secret name"
								value={name}
								onChange={({ detail }) => setName(detail.value)}
							/>
						</FormField>
						<FormField label="Description – optional">
							<Textarea
								ariaLabel="Secret description"
								value={description}
								onChange={({ detail }) => setDescription(detail.value)}
							/>
						</FormField>
						<FormField
							label="Encryption key – optional"
							description="Leave empty for alias/aws/secretsmanager, or enter a customer-managed KMS key ID, alias, or ARN."
						>
							<Input
								ariaLabel="Encryption key"
								value={key}
								onChange={({ detail }) => setKey(detail.value)}
							/>
						</FormField>
						<FormField label="Tags (JSON)">
							<Textarea
								ariaLabel="Secret tags"
								value={tags}
								onChange={({ detail }) => setTags(detail.value)}
							/>
						</FormField>
					</>
				)}
			</SpaceBetween>
		</ActionDialog>
	);
}
