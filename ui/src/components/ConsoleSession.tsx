import {
	Alert,
	Autosuggest,
	Box,
	Button,
	FormField,
	Input,
	KeyValuePairs,
	Modal,
	SpaceBetween,
} from "@cloudscape-design/components";
import { useEffect, useRef, useState } from "react";
import { useScope } from "#/lib/scope";
import { connectConsoleSession, disconnectConsoleSession } from "#/lib/session";

export function ConsoleSessionDialog({ onDismiss }: { onDismiss: () => void }) {
	const scope = useScope();
	const [account, setAccount] = useState(scope.account);
	const [accessKeyId, setAccessKeyId] = useState("");
	const [secretAccessKey, setSecretAccessKey] = useState("");
	const [sessionToken, setSessionToken] = useState("");
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState("");
	const pending = useRef<AbortController | null>(null);
	useEffect(() => () => pending.current?.abort(), []);
	const connect = async () => {
		const controller = new AbortController();
		pending.current = controller;
		setBusy(true);
		setError("");
		try {
			await connectConsoleSession(
				{
					accessKeyId: accessKeyId.trim(),
					secretAccessKey: secretAccessKey.trim(),
					...(sessionToken.trim() ? { sessionToken: sessionToken.trim() } : {}),
				},
				account,
				controller.signal,
			);
			scope.switchScope({ account });
			onDismiss();
		} catch (e) {
			if (!controller.signal.aborted) {
				setError(e instanceof Error ? e.message : String(e));
				setBusy(false);
			}
		}
	};
	return (
		<Modal
			visible
			header="Console session"
			onDismiss={onDismiss}
			footer={
				<Box float="right">
					<SpaceBetween direction="horizontal" size="xs">
						{scope.session ? (
							<Button
								onClick={() => {
									disconnectConsoleSession();
									onDismiss();
								}}
							>
								Disconnect
							</Button>
						) : null}
						<Button variant="link" onClick={onDismiss}>
							Cancel
						</Button>
						<Button
							variant="primary"
							loading={busy}
							disabled={
								!/^\d{12}$/.test(account) ||
								!accessKeyId.trim() ||
								!secretAccessKey.trim()
							}
							onClick={connect}
						>
							Connect
						</Button>
					</SpaceBetween>
				</Box>
			}
		>
			<SpaceBetween size="m">
				<Alert type="info" header="Administrative root credentials">
					Use a key configured in identity.secret_keys, or temporary root
					credentials. Console actions bypass IAM permission policies. User and
					role credentials cannot connect here. Credentials are kept in this tab
					and are not saved. Disconnect or reload to end the session.
				</Alert>
				{scope.session ? (
					<KeyValuePairs
						items={[
							{ label: "Connected account", value: scope.session.account },
							{ label: "Principal", value: scope.session.arn },
						]}
					/>
				) : null}
				{error ? (
					<Alert type="error" header="Cannot connect">
						{error}
					</Alert>
				) : null}
				<FormField label="Account ID">
					<Autosuggest
						value={account}
						onChange={({ detail }) => setAccount(detail.value)}
						disabled={busy}
						ariaLabel="Session account ID"
						options={scope.accounts.map((value) => ({ value }))}
						enteredTextLabel={(value) => `Use ${value}`}
					/>
				</FormField>
				<FormField label="Access key ID">
					<Input
						value={accessKeyId}
						autoComplete="off"
						disabled={busy}
						onChange={({ detail }) => setAccessKeyId(detail.value)}
					/>
				</FormField>
				<FormField label="Secret access key">
					<Input
						type="password"
						value={secretAccessKey}
						autoComplete="off"
						disabled={busy}
						onChange={({ detail }) => setSecretAccessKey(detail.value)}
					/>
				</FormField>
				<FormField label="Session token — optional">
					<Input
						type="password"
						value={sessionToken}
						autoComplete="off"
						disabled={busy}
						onChange={({ detail }) => setSessionToken(detail.value)}
					/>
				</FormField>
			</SpaceBetween>
		</Modal>
	);
}
