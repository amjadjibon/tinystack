import { Alert, Box, SpaceBetween } from "@cloudscape-design/components";
import { useState } from "react";
import { CodeBlock } from "#/components/CodeBlock";
import { Action, Buttons, Rows } from "#/components/containers/Controls";
import { mailbox } from "#/lib/mailbox";
import type { MailMessage } from "#/lib/mailboxHelpers";
import type { SESScope } from "#/lib/sesv2";
import { sesMessages } from "#/lib/sesv2Helpers";
import { usePoll } from "#/lib/usePoll";

// SESMessages lists the mail TinyStack captured from /_tinystack/messages for
// one account and Region. Reading it needs administrator access.
export function SESMessages({
	scope,
	enabled,
}: {
	scope: SESScope;
	enabled: boolean;
}) {
	const poll = usePoll(
		async () =>
			enabled
				? sesMessages(
						await mailbox.list({
							channel: "email",
							to: "",
							service: "ses",
							since: "",
						}),
						scope.account,
						scope.region,
					)
				: [],
		5000,
	);
	const [selected, setSelected] = useState<MailMessage>();
	return (
		<SpaceBetween size="m">
			{poll.error && (
				<Alert type="error" header="Sent messages are unavailable">
					{poll.error}
				</Alert>
			)}
			<Rows
				title="Sent messages"
				emptyText="Use the Send test email tab, then choose Refresh."
				items={poll.data ?? []}
				loading={poll.loading}
				actions={<Action onClick={poll.reload}>Refresh</Action>}
				columns={[
					{
						id: "time",
						header: "Time",
						cell: (m) => new Date(m.time).toLocaleString(),
					},
					{ id: "from", header: "From", cell: (m) => m.source },
					{ id: "to", header: "To", cell: (m) => m.to },
					{ id: "subject", header: "Subject", cell: (m) => m.subject },
					{
						id: "actions",
						header: "Actions",
						cell: (m) => (
							<Buttons>
								<Action onClick={() => setSelected(m)}>View</Action>
							</Buttons>
						),
					},
				]}
			/>
			{selected && (
				<SpaceBetween size="xs">
					<Box variant="awsui-key-label">
						{selected.subject || "(no subject)"} - message {selected.id}
					</Box>
					<CodeBlock text={selected.body ?? ""} />
				</SpaceBetween>
			)}
		</SpaceBetween>
	);
}
