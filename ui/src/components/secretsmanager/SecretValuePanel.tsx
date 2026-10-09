import {
	Alert,
	Box,
	Button,
	Container,
	CopyToClipboard,
	Header,
	Select,
	SpaceBetween,
} from "@cloudscape-design/components";
import { useEffect, useRef, useState } from "react";
import { CodeBlock } from "#/components/CodeBlock";
import { pretty } from "#/lib/api";
import { errorText } from "#/lib/consoleActions";
import {
	type Secret,
	type SecretScope,
	type SecretValue,
	type SecretVersion,
	secretCall,
	secretPages,
} from "#/lib/secretsmanager";
import { usePoll } from "#/lib/usePoll";
import { SecretForm } from "./SecretForm";

export function SecretValuePanel({
	scope,
	secret,
	enabled,
	onChanged,
}: {
	scope: SecretScope;
	secret: Secret;
	enabled: boolean;
	onChanged: () => void;
}) {
	const [version, setVersion] = useState(""),
		[value, setValue] = useState<SecretValue>(),
		[busy, setBusy] = useState(false),
		[error, setError] = useState(""),
		[editing, setEditing] = useState(false);
	const versions = usePoll(
		() =>
			enabled
				? secretPages<SecretVersion>(
						scope,
						"ListSecretVersionIds",
						"Versions",
						{ IncludeDeprecated: true },
					)
				: Promise.resolve([]),
		0,
	);
	const generation = useRef(0);
	useEffect(
		() => () => {
			generation.current++;
		},
		[],
	);
	const clear = () => {
		generation.current++;
		setValue(undefined);
		setError("");
		setBusy(false);
	};
	const options = [
		{ label: "Current version (AWSCURRENT)", value: "" },
		...(
			versions.data ??
			Object.entries(secret.VersionIdsToStages ?? {}).map(
				([VersionId, VersionStages]) => ({ VersionId, VersionStages }),
			)
		).map((v) => ({
			label: v.VersionId,
			description:
				v.VersionStages?.join(", ") || "Deprecated (no staging labels)",
			value: v.VersionId,
		})),
	];
	return (
		<Container
			header={
				<Header
					variant="h2"
					actions={
						<Button
							disabled={!enabled}
							onClick={() => {
								clear();
								setEditing(true);
							}}
						>
							Edit secret value
						</Button>
					}
				>
					Secret value
				</Header>
			}
		>
			<SpaceBetween size="m">
				{versions.error && <Alert type="error">{versions.error}</Alert>}
				<Select
					ariaLabel="Secret version"
					options={options}
					selectedOption={
						options.find((o) => o.value === version) ?? options[0]
					}
					disabled={!enabled}
					onChange={({ detail }) => {
						clear();
						setVersion(detail.selectedOption.value ?? "");
					}}
				/>
				{error && <Alert type="error">{error}</Alert>}
				{value ? (
					<>
						<Box>
							Version ID: {value.VersionId} ·{" "}
							{value.VersionStages?.join(", ") || "No staging labels"}
						</Box>
						<Box>
							{value.SecretBinary !== undefined
								? "Binary value (base64)"
								: "Secret string"}
						</Box>
						<div data-testid="revealed-secret-value">
							<CodeBlock
								text={value.SecretBinary ?? pretty(value.SecretString ?? "")}
							/>
						</div>
						<SpaceBetween direction="horizontal" size="xs">
							<Button onClick={clear}>Hide secret value</Button>
							<CopyToClipboard
								variant="button"
								textToCopy={value.SecretBinary ?? value.SecretString ?? ""}
								copyButtonText="Copy secret value"
								copySuccessText="Secret value copied"
								copyErrorText="Could not copy secret value"
							/>
						</SpaceBetween>
					</>
				) : (
					<>
						<Box>
							Secret values are hidden. Retrieve a value explicitly to display
							it here.
						</Box>
						<Button
							loading={busy}
							disabled={!enabled}
							onClick={async () => {
								const request = ++generation.current;
								setBusy(true);
								setError("");
								try {
									const out = await secretCall<SecretValue>(
										scope,
										"GetSecretValue",
										version ? { VersionId: version } : {},
									);
									if (generation.current === request) setValue(out);
								} catch (err) {
									if (generation.current === request) setError(errorText(err));
								} finally {
									if (generation.current === request) setBusy(false);
								}
							}}
						>
							Retrieve secret value
						</Button>
						{busy && <Button onClick={clear}>Cancel retrieval</Button>}
					</>
				)}
			</SpaceBetween>
			{editing && (
				<SecretForm
					scope={scope}
					create={false}
					onClose={() => setEditing(false)}
					onSaved={() => {
						setEditing(false);
						clear();
						onChanged();
					}}
				/>
			)}
		</Container>
	);
}
