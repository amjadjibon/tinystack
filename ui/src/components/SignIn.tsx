import {
	Alert,
	Box,
	Button,
	ColumnLayout,
	Container,
	Grid,
	Header,
	Link,
	SpaceBetween,
} from "@cloudscape-design/components";
import { useRouter } from "@tanstack/react-router";
import { useEffect, useState } from "react";
import { useHealth } from "#/lib/health";
import { categories, consoleServiceNames, service } from "#/lib/services";
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

// SignIn is the landing page a person sees, instead of the console, while
// single sign-on is configured and nobody has signed in: what this TinyStack
// runs, from its public health endpoint, beside the ways to sign in. The
// sign-in comes first so that it stays above the fold on a narrow screen.
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
		<Box padding={{ vertical: "xxxl", horizontal: "l" }}>
			<div style={{ maxWidth: 1080, margin: "0 auto" }}>
				<Grid
					gridDefinition={[
						{ colspan: { default: 12, s: 5 }, push: { s: 7 } },
						{ colspan: { default: 12, s: 7 }, pull: { s: 5 } },
					]}
				>
					<Container
						header={
							<Header
								variant="h2"
								description="Sign in to manage this TinyStack's resources."
							>
								Sign in
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
					<Overview />
				</Grid>
			</div>
			{keysOpen && (
				<ConsoleSessionDialog onDismiss={() => setKeysOpen(false)} />
			)}
		</Box>
	);
}

// Overview introduces TinyStack and the services this one runs, grouped the
// way the console's service menu groups them.
function Overview() {
	const health = useHealth();
	const names = consoleServiceNames(health.data?.services ?? []);
	const counts = new Map<string, number>();
	for (const name of names) {
		const { category } = service(name);
		counts.set(category, (counts.get(category) ?? 0) + 1);
	}
	const groups = [...categories, "Other"].filter((c) => counts.has(c));

	return (
		<SpaceBetween size="xl">
			<SpaceBetween size="s" direction="horizontal" alignItems="center">
				<img src="/tinystack.svg" alt="" width={48} height={48} />
				<Box variant="h1" fontSize="display-l" padding="n">
					TinyStack
				</Box>
			</SpaceBetween>
			<Box variant="p" fontSize="heading-m" color="text-body-secondary">
				AWS services on one local endpoint, for development and tests. Point the
				AWS SDKs, CLI, Terraform or CDK at it and work against real APIs without
				an AWS account.
			</Box>
			{health.data ? (
				<ColumnLayout columns={3} variant="text-grid" minColumnWidth={120}>
					<Stat label="Services" value={String(names.length)} />
					<Stat label="Storage" value={health.data.storage} />
					<Stat
						label="Signatures"
						value={
							health.data.identity?.mode === "verify" ? "Verified" : "Parsed"
						}
					/>
				</ColumnLayout>
			) : null}
			{groups.length ? (
				<ColumnLayout columns={2} minColumnWidth={220}>
					{groups.map((category) => (
						<Box key={category} color="text-body-secondary">
							<Box variant="strong">{category}</Box>
							{" — "}
							{counts.get(category)}{" "}
							{counts.get(category) === 1 ? "service" : "services"}
						</Box>
					))}
				</ColumnLayout>
			) : null}
			<Link href="https://github.com/amjadjibon/tinystack" external>
				Documentation and source
			</Link>
		</SpaceBetween>
	);
}

function Stat({ label, value }: { label: string; value: string }) {
	return (
		<div>
			<Box variant="awsui-key-label">{label}</Box>
			<Box fontSize="heading-xl" fontWeight="bold">
				{value}
			</Box>
		</div>
	);
}
