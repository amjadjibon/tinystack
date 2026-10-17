import {
	Alert,
	Autosuggest,
	Box,
	Button,
	FormField,
	Input,
	KeyValuePairs,
	Link,
	Modal,
	SpaceBetween,
} from "@cloudscape-design/components";
import { useEffect, useRef, useState } from "react";
import { useScope } from "#/lib/scope";
import {
	collectSSOSession,
	connectConsoleSession,
	disconnectConsoleSession,
	signOutSSO,
} from "#/lib/session";

// collectSSOReturn finishes a sign-in the identity provider sent the browser
// back from: the callback redirects to /?signin=sso. The marker is removed from
// the address either way, through the router's history so that it does not
// write the marker back; null means there was none.
export async function collectSSOReturn(replace: (path: string) => void) {
	const url = new URL(window.location.href);
	if (url.searchParams.get("signin") !== "sso") return null;
	url.searchParams.delete("signin");
	replace(url.pathname + url.search + url.hash);
	try {
		return (await collectSSOSession()) ? "" : "No sign-in was waiting.";
	} catch (e) {
		return e instanceof Error ? e.message : String(e);
	}
}

export function ConsoleSessionDialog({
	onDismiss,
	notice = "",
}: {
	onDismiss: () => void;
	notice?: string;
}) {
	const scope = useScope();
	const [account, setAccount] = useState(scope.account);
	const [accessKeyId, setAccessKeyId] = useState("");
	const [secretAccessKey, setSecretAccessKey] = useState("");
	const [sessionToken, setSessionToken] = useState("");
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState(notice);
	const [endSession, setEndSession] = useState<string | undefined>();
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
	const sso = scope.session?.sso;
	const signOut = async () => {
		setEndSession(await signOutSSO());
		setBusy(false);
	};
	if (sso) {
		return (
			<Modal
				visible
				header="Console session"
				onDismiss={onDismiss}
				footer={
					<Box float="right">
						<SpaceBetween direction="horizontal" size="xs">
							<Button variant="link" onClick={onDismiss}>
								Close
							</Button>
							<Button variant="primary" onClick={signOut}>
								Sign out
							</Button>
						</SpaceBetween>
					</Box>
				}
			>
				<SpaceBetween size="m">
					<Alert type="info" header="Signed in with SSO">
						IAM permission policies of your role apply to every console action.
						The credentials are kept in this tab only and are not saved. Signing
						out ends this session; the temporary credentials expire at the time
						shown.
					</Alert>
					<KeyValuePairs
						columns={2}
						items={[
							{ label: "Signed in as", value: sso.name },
							{ label: "Account", value: scope.session?.account },
							{ label: "Role", value: sso.role },
							{ label: "Principal", value: scope.session?.arn },
							{
								label: "Administrator",
								value: sso.admin ? "Yes" : "No",
							},
							{
								label: "Expires",
								value: new Date(sso.expiresAt).toLocaleString(),
							},
						]}
					/>
				</SpaceBetween>
			</Modal>
		);
	}
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
				{endSession ? (
					<Alert type="success" header="Signed out of TinyStack">
						<Link href={endSession} external>
							Also sign out of the identity provider
						</Link>
					</Alert>
				) : null}
				{scope.capabilities.data?.sso && !scope.session ? (
					<Alert
						type="info"
						header="Single sign-on"
						action={
							<Button
								variant="primary"
								onClick={() =>
									window.location.assign(
										scope.capabilities.data?.sso?.login_url ??
											"/_tinystack/auth/login",
									)
								}
							>
								Sign in with SSO
							</Button>
						}
					>
						Sign in with your organization's identity provider. Your groups
						choose the role whose permissions apply.
					</Alert>
				) : null}
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
