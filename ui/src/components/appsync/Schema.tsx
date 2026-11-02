import { Alert, Button, SpaceBetween } from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { CodeBlock } from "#/components/CodeBlock";
import { Field } from "#/components/containers/Controls";
import { type AppSyncScope, appsyncCall } from "#/lib/appsync";
import { decodeSDL, encodeSDL } from "#/lib/appsyncHelpers";
import { usePoll } from "#/lib/usePoll";

export function Schema({
	scope,
	apiId,
	enabled,
}: {
	scope: AppSyncScope;
	apiId: string;
	enabled: boolean;
}) {
	const [editing, setEditing] = useState(false),
		[draft, setDraft] = useState("");
	const poll = usePoll(async () => {
		if (!enabled) return { status: "", details: "", sdl: "" };
		const status = await appsyncCall<{ Status?: string; Details?: string }>(
			scope,
			"GetSchemaCreationStatus",
			{ ApiId: apiId },
		).catch((error: unknown) => {
			if (
				error instanceof Error &&
				error.message.startsWith("NotFoundException:")
			)
				return { Status: undefined, Details: undefined };
			throw error;
		});
		let sdl = "";
		if (status.Status === "SUCCESS" || status.Status === "FAILED") {
			// A failed upload retains the last valid schema. A first failed upload has no active schema.
			try {
				const out = await appsyncCall<{ Schema?: string }>(
					scope,
					"GetIntrospectionSchema",
					{ ApiId: apiId, Format: "SDL", IncludeDirectives: true },
				);
				sdl = out.Schema ? decodeSDL(out.Schema) : "";
			} catch (error) {
				if (
					status.Status !== "FAILED" ||
					!(error instanceof Error) ||
					!error.message.startsWith("GraphQLSchemaException:")
				)
					throw error;
			}
		}
		return { status: status.Status ?? "", details: status.Details ?? "", sdl };
	});
	return (
		<SpaceBetween size="m">
			{poll.error && <Alert type="error">{poll.error}</Alert>}
			<Alert type={poll.data?.status === "FAILED" ? "error" : "info"}>
				{poll.data?.status || "No schema"} {poll.data?.details}
			</Alert>
			<Button
				disabled={
					!enabled || poll.loading || poll.data?.status === "PROCESSING"
				}
				onClick={() => {
					setDraft(poll.data?.sdl ?? "");
					setEditing(true);
				}}
			>
				Edit schema
			</Button>
			<CodeBlock text={poll.data?.sdl || "No schema uploaded."} />
			{editing && (
				<ActionDialog
					title="Edit schema"
					onClose={() => setEditing(false)}
					onSaved={() => {
						setEditing(false);
						poll.reload();
					}}
					plan={async () => {
						const Definition = encodeSDL(draft);
						return [
							{
								label: "StartSchemaCreation",
								run: () =>
									appsyncCall(scope, "StartSchemaCreation", {
										ApiId: apiId,
										Definition,
									}),
							},
						];
					}}
				>
					<Field
						label="Schema definition (SDL)"
						value={draft}
						multiline
						onChange={setDraft}
					/>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
