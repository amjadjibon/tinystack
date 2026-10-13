import {
	Alert,
	Box,
	Button,
	Container,
	Header,
	Link,
	SpaceBetween,
} from "@cloudscape-design/components";
import { useRouter } from "@tanstack/react-router";
import { useEffect, useState } from "react";
import { collectSSOSession, lastSignOut } from "#/lib/session";
import {
	applyConsoleTheme,
	applyPreferences,
	loadPreferences,
} from "#/lib/theme";
import { ConsoleSessionDialog, collectSSOReturn } from "./ConsoleSession";

// nextKey keeps the page a person asked for while they sign in at the
// identity provider, which always sends them back to the console's root.
const nextKey = "tinystack.signin.next";

// returnPath is a saved page to go back to: only a path on this origin.
function returnPath() {
	try {
		const next = sessionStorage.getItem(nextKey) ?? "";
		sessionStorage.removeItem(nextKey);
		return next.startsWith("/") && !next.startsWith("//") ? next : "";
	} catch {
		return "";
	}
}

// SignIn is the page a person sees, instead of the console, while single
// sign-on is configured and nobody has signed in.
export function SignIn({ loginURL }: { loginURL: string }) {
	const router = useRouter();
	const [notice, setNotice] = useState("");
	const [keysOpen, setKeysOpen] = useState(false);
	const signedOut = lastSignOut();
	// Until the server says whether this browser is signed in, show nothing.
	const [checking, setChecking] = useState(signedOut === undefined);

	useEffect(() => {
		const theme = applyConsoleTheme();
		applyPreferences(loadPreferences());
		return theme.reset;
	}, []);
	// Pick up the session of a person already signed in with this browser:
	// after a reload, or coming back from the identity provider, which then
	// returns them to the page they first asked for.
	// biome-ignore lint/correctness/useExhaustiveDependencies: runs once per page load.
	useEffect(() => {
		if (signedOut !== undefined) return;
		collectSSOReturn().then(async (result) => {
			if (result === null) {
				await collectSSOSession().catch(() => false);
				setChecking(false);
				return;
			}

			setChecking(false);
			if (result) {
				setNotice(result);
				return;
			}

			const next = returnPath();
			if (next) router.history.replace(next);
		});
	}, []);

	const signIn = () => {
		try {
			const here = window.location.pathname + window.location.search;
			if (here !== "/") sessionStorage.setItem(nextKey, here);
		} catch {
			// Without session storage the person lands on the console home.
		}
		window.location.assign(loginURL);
	};

	if (checking) return null;

	return (
		<Box padding={{ top: "xxxl", horizontal: "l" }}>
			<div style={{ maxWidth: 420, margin: "0 auto" }}>
				<SpaceBetween size="l">
					<Box textAlign="center">
						<img src="/tinystack.svg" alt="" width={56} height={56} />
					</Box>
					<Container
						header={
							<Header
								variant="h1"
								description="Sign in to manage this TinyStack's resources."
							>
								Sign in to TinyStack
							</Header>
						}
					>
						<SpaceBetween size="m">
							{signedOut !== undefined && !notice ? (
								<Alert type="success" header="You signed out">
									{signedOut ? (
										<Link href={signedOut} external>
											Also sign out of the identity provider
										</Link>
									) : null}
								</Alert>
							) : null}
							{notice ? (
								<Alert type="error" header="Sign-in did not finish">
									{notice}
								</Alert>
							) : null}
							<Button variant="primary" fullWidth onClick={signIn}>
								Sign in with SSO
							</Button>
							<Box textAlign="center" color="text-body-secondary">
								Your organization's identity provider signs you in, and your
								groups choose the role whose permissions apply.
							</Box>
							<Box textAlign="center">
								<Button variant="link" onClick={() => setKeysOpen(true)}>
									Sign in with root access keys
								</Button>
							</Box>
						</SpaceBetween>
					</Container>
				</SpaceBetween>
			</div>
			{keysOpen && (
				<ConsoleSessionDialog onDismiss={() => setKeysOpen(false)} />
			)}
		</Box>
	);
}
