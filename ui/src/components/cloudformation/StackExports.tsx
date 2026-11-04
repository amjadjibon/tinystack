import {
	Alert,
	Button,
	Header,
	Link,
	SpaceBetween,
	Table,
} from "@cloudscape-design/components";
import { useState } from "react";
import { type CfnScope, cfnPages } from "#/lib/cloudformation";
import { usePoll } from "#/lib/usePoll";

type Export = { Name: string; Value: string; ExportingStackId: string };
export function StackExports({ scope }: { scope: CfnScope }) {
	const poll = usePoll(
		() => cfnPages<Export>(scope, "ListExports", "Exports"),
		0,
	);
	const [imports, setImports] = useState<string[]>(),
		[error, setError] = useState("");
	return (
		<SpaceBetween size="s">
			{!!(poll.error || error) && (
				<Alert type="error">{poll.error || error}</Alert>
			)}
			<Table
				items={poll.data ?? []}
				loading={poll.loading}
				header={
					<Header
						actions={<Button onClick={poll.reload}>Refresh exports</Button>}
					>
						Exports
					</Header>
				}
				columnDefinitions={[
					{
						id: "name",
						header: "Name (select to list imports)",
						cell: (e) => (
							<Link
								onFollow={async () => {
									setImports(undefined);
									setError("");
									try {
										setImports(
											await cfnPages<string>(scope, "ListImports", "Imports", {
												ExportName: e.Name,
											}),
										);
									} catch (err) {
										setError(err instanceof Error ? err.message : String(err));
									}
								}}
							>
								{e.Name}
							</Link>
						),
					},
					{ id: "value", header: "Value", cell: (e) => e.Value },
					{ id: "stack", header: "Stack ID", cell: (e) => e.ExportingStackId },
				]}
			/>
			{imports && (
				<Alert header="Importing stacks">
					{imports.join(", ") || "No importing stacks."}
				</Alert>
			)}
		</SpaceBetween>
	);
}
