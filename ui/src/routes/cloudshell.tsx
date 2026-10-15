import { Container, Header, Spinner } from "@cloudscape-design/components";
import { createFileRoute } from "@tanstack/react-router";
import { lazy, Suspense, useEffect, useState } from "react";
import { ConsoleLayout } from "#/components/Console";
import { useScope } from "#/lib/scope";

// The terminal (xterm.js) is its own chunk, fetched only when this page opens.
const Terminal = lazy(() => import("#/components/CloudShellTerminal"));

export const Route = createFileRoute("/cloudshell")({ component: CloudShell });

function CloudShell() {
	const { account, region } = useScope();
	// The terminal needs a browser; the prerendered page holds only the frame.
	const [mounted, setMounted] = useState(false);
	useEffect(() => setMounted(true), []);
	return (
		<ConsoleLayout
			breadcrumbs={[{ text: "CloudShell", href: "#" }]}
			content={
				<Container
					header={
						<Header
							variant="h1"
							description={`AWS CLI-style commands run inside TinyStack as your console identity, in account ${account || "..."} and ${region || "..."}. Pipes, variables and && are not supported; use aws-local in a real shell for those.`}
						>
							CloudShell
						</Header>
					}
				>
					{mounted && account && region ? (
						<Suspense fallback={<Spinner size="large" />}>
							<Terminal scope={{ account, region }} />
						</Suspense>
					) : (
						<Spinner size="large" />
					)}
				</Container>
			}
		/>
	);
}
