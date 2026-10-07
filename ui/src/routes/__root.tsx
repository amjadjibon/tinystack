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
import { HealthProvider } from "#/lib/health";
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
	return (
		<HealthProvider>
			<ConsoleHeader />
			<Outlet />
		</HealthProvider>
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
