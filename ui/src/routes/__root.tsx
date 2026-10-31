import { TutorialProvider } from "#/tutorials/runtime";
import "@cloudscape-design/global-styles/index.css";
import { Container } from "@cloudscape-design/components";
import { TanStackDevtools } from "@tanstack/react-devtools";
import {
	createRootRoute,
	HeadContent,
	Outlet,
	Scripts,
} from "@tanstack/react-router";
import { TanStackRouterDevtoolsPanel } from "@tanstack/react-router-devtools";
import { ConsoleHeader, ConsoleLayout } from "#/components/Console";
import { EmptyState } from "#/components/EmptyState";
import { SignIn } from "#/components/SignIn";
import { HealthProvider } from "#/lib/health";
import { ScopeProvider, useScope } from "#/lib/scope";
import { ServiceHistoryProvider } from "#/lib/serviceHistory";
import { useConsoleSession } from "#/lib/session";
import appCss from "../styles.css?url";

export const Route = createRootRoute({
	head: () => ({
		meta: [
			{ charSet: "utf-8" },
			{ name: "viewport", content: "width=device-width, initial-scale=1" },
			{ title: "TinyStack Console" },
		],
		links: [
			{ rel: "stylesheet", href: appCss },
			{
				rel: "icon",
				type: "image/svg+xml",
				sizes: "any",
				href: "/tinystack.svg",
			},
		],
	}),
	shellComponent: RootDocument,
	component: Root,
	notFoundComponent: NotFound,
});

function NotFound() {
	return (
		<ConsoleLayout
			contentType="default"
			breadcrumbs={[{ text: "Not found", href: "#" }]}
			content={
				<Container>
					<EmptyState
						title="Page not found"
						subtitle="This console page does not exist."
					/>
				</Container>
			}
		/>
	);
}

function Root() {
	const session = useConsoleSession();
	return (
		<HealthProvider>
			<ScopeProvider key={session?.arn ?? "signed-out"}>
				<ServiceHistoryProvider>
					<TutorialProvider>
						<Signed />
					</TutorialProvider>
				</ServiceHistoryProvider>
			</ScopeProvider>
		</HealthProvider>
	);
}

// Signed shows the console, or only the sign-in page while single sign-on is
// configured and nobody has signed in. Until TinyStack says whether it signs
// people in, it shows nothing, so the console never flashes past the sign-in.
function Signed() {
	const { capabilities, session } = useScope();
	if (!capabilities.data && !capabilities.error) return null;
	const sso = capabilities.data?.sso;
	if (sso && !session) return <SignIn loginURL={sso.login_url} />;
	return (
		<>
			<ConsoleHeader />
			<Outlet />
		</>
	);
}

function RootDocument({ children }: { children: React.ReactNode }) {
	return (
		<html lang="en">
			<head>
				<HeadContent />
			</head>
			<body>
				{children}
				<TanStackDevtools
					config={{ position: "bottom-right" }}
					plugins={[
						{
							name: "Tanstack Router",
							render: <TanStackRouterDevtoolsPanel />,
						},
					]}
				/>
				<Scripts />
			</body>
		</html>
	);
}
