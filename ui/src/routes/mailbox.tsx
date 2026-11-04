import { useCollection } from "@cloudscape-design/collection-hooks";
import {
	Alert,
	Button,
	FormField,
	Header,
	Input,
	KeyValuePairs,
	Link,
	Pagination,
	Select,
	SpaceBetween,
	SplitPanel,
	Table,
	Toggle,
} from "@cloudscape-design/components";
import { createFileRoute } from "@tanstack/react-router";
import { useEffect, useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { CodeBlock } from "#/components/CodeBlock";
import { ConsoleLayout } from "#/components/Console";
import { EmptyState } from "#/components/EmptyState";
import { mailbox } from "#/lib/mailbox";
import {
	emptyMessageFilters,
	type MailMessage,
	type MessageContent,
	mailboxPoolHref,
	messageContent,
} from "#/lib/mailboxHelpers";
import { useScope } from "#/lib/scope";
import { useFollow } from "#/lib/useFollow";
import { usePoll } from "#/lib/usePoll";

export const Route = createFileRoute("/mailbox")({ component: Mailbox });
function Mailbox() {
	const [filters, setFilters] = useState(emptyMessageFilters);
	const [applied, setApplied] = useState(emptyMessageFilters);
	const [live, setLive] = useState(true);
	const messages = usePoll(() => mailbox.list(applied), live ? 2000 : 0);
	const deliveries = usePoll(mailbox.deliveries, live ? 2000 : 0);
	const [selected, setSelected] = useState<MailMessage>();
	const [panelOpen, setPanelOpen] = useState(false);
	const [clear, setClear] = useState(false);
	const [source, setSource] = useState(false);
	const follow = useFollow();
	const scope = useScope();
	const reload = messages.reload;
	useEffect(() => {
		void applied;
		reload();
		setSelected(undefined);
	}, [applied, reload]);
	const forbidden = messages.error?.includes("administrator access required");
	const records = forbidden
		? []
		: [...(messages.data ?? [])].sort(
				(a, b) => Date.parse(b.time) - Date.parse(a.time),
			);
	const { items, collectionProps, paginationProps } = useCollection(records, {
		pagination: { pageSize: 50 },
	});
	const content: MessageContent = selected
		? {
				...selected,
				to: undefined,
				...messageContent(selected, deliveries.data ?? []),
			}
		: {};
	return (
		<ConsoleLayout
			breadcrumbs={[{ text: "Mailbox", href: "/mailbox" }]}
			splitPanelOpen={panelOpen && !forbidden}
			onSplitPanelToggle={setPanelOpen}
			splitPanel={
				<SplitPanel header="Message details" hidePreferencesButton>
					{selected && (
						<SpaceBetween size="l">
							<KeyValuePairs
								columns={2}
								items={[
									{
										label: "Time",
										value: new Date(selected.time).toLocaleString(),
									},
									{ label: "Channel", value: selected.channel },
									{ label: "Service", value: selected.service },
									{
										label: selected.channel === "sms" ? "Sender" : "From",
										value: content.from ?? selected.source ?? "—",
									},
									{
										label: selected.channel === "sms" ? "Number" : "To",
										value: content.to?.join(", ") || selected.to,
									},
									...(content.cc?.length
										? [{ label: "Cc", value: content.cc.join(", ") }]
										: []),
									...(content.bcc?.length
										? [{ label: "Bcc", value: content.bcc.join(", ") }]
										: []),
									{ label: "Subject", value: selected.subject || "—" },
									...(content.userPoolId
										? [
												{
													label: "User pool",
													value: (
														<Link
															href={mailboxPoolHref(
																selected,
																content.userPoolId,
																scope.inventory.data,
															)}
															onFollow={follow}
														>
															{content.userPoolId}
														</Link>
													),
												},
											]
										: []),
									...(content.username
										? [{ label: "User", value: content.username }]
										: []),
								]}
							/>
							{!!deliveries.error && (
								<Alert type="error" header="Rich message details unavailable">
									{deliveries.error}
								</Alert>
							)}
							<FormField
								label={
									content.html || content.raw
										? "Text body (when present)"
										: "Body"
								}
							>
								<CodeBlock
									text={
										content.text ??
										(content.html || content.raw ? "" : (selected.body ?? ""))
									}
								/>
							</FormField>
							{!!content.raw && (
								<FormField label="Raw message">
									<CodeBlock text={content.raw} />
								</FormField>
							)}
							{!!content.html && (
								<SpaceBetween size="s">
									<Toggle
										checked={source}
										onChange={({ detail }) => setSource(detail.checked)}
									>
										View HTML source
									</Toggle>
									{source ? (
										<CodeBlock text={content.html} />
									) : (
										<iframe
											title="Email HTML body"
											sandbox=""
											referrerPolicy="no-referrer"
											srcDoc={content.html}
											style={{ width: "100%", minHeight: 300, border: 0 }}
										/>
									)}
								</SpaceBetween>
							)}
							{!!content.attachments?.length && (
								<KeyValuePairs
									items={content.attachments.map((a) => ({
										label: a.name,
										value: `${a.size} bytes`,
									}))}
								/>
							)}
						</SpaceBetween>
					)}
				</SplitPanel>
			}
			content={
				<SpaceBetween size="l">
					<Header
						variant="h1"
						description="Captured email and SMS, newest first. Follow watches for new verification and sign-up messages."
						actions={
							<SpaceBetween direction="horizontal" size="xs">
								<Toggle
									checked={live}
									onChange={({ detail }) => setLive(detail.checked)}
								>
									Follow
								</Toggle>
								<Button
									onClick={() => {
										messages.reload();
										deliveries.reload();
									}}
								>
									Refresh
								</Button>
								<Button disabled={forbidden} onClick={() => setClear(true)}>
									Clear mailbox
								</Button>
							</SpaceBetween>
						}
					>
						Mailbox
					</Header>
					<SpaceBetween direction="horizontal" size="m" alignItems="end">
						{(["channel", "service"] as const).map((field) => (
							<FormField
								key={field}
								label={field === "channel" ? "Channel" : "Service"}
							>
								<Select
									selectedOption={{
										value: filters[field],
										label: filters[field] || "All",
									}}
									options={[
										{ value: "", label: "All" },
										...(field === "channel"
											? ["email", "sms"]
											: ["ses", "sns", "cognito-idp", "budgets"]
										).map((value) => ({ value, label: value })),
									]}
									onChange={({ detail }) =>
										setFilters({
											...filters,
											[field]: detail.selectedOption.value ?? "",
										})
									}
								/>
							</FormField>
						))}
						<FormField label="To">
							<Input
								value={filters.to}
								onChange={({ detail }) =>
									setFilters({ ...filters, to: detail.value })
								}
							/>
						</FormField>
						<FormField
							label="Since (RFC 3339)"
							description="Example: 2026-10-30T12:00:00Z"
						>
							<Input
								value={filters.since}
								onChange={({ detail }) =>
									setFilters({ ...filters, since: detail.value })
								}
							/>
						</FormField>
						<Button onClick={() => setApplied({ ...filters })}>
							Apply filters
						</Button>
					</SpaceBetween>
					{!!messages.error && <Alert type="error">{messages.error}</Alert>}
					{!forbidden && (
						<Table
							{...collectionProps}
							loading={messages.loading}
							loadingText="Loading messages"
							items={items}
							trackBy={(m) => `${m.time}/${m.id}`}
							selectionType="single"
							selectedItems={selected ? [selected] : []}
							onSelectionChange={({ detail }) => {
								setSelected(detail.selectedItems[0]);
								setSource(false);
								setPanelOpen(true);
							}}
							empty={
								<EmptyState
									title="No messages"
									subtitle="Send an email or SMS, or start a Cognito verification flow. Check the filters if messages are missing."
								/>
							}
							pagination={<Pagination {...paginationProps} />}
							columnDefinitions={[
								{
									id: "time",
									header: "Time",
									cell: (m) => new Date(m.time).toLocaleString(),
								},
								{ id: "channel", header: "Channel", cell: (m) => m.channel },
								{ id: "service", header: "Service", cell: (m) => m.service },
								{
									id: "source",
									header: "From / sender",
									cell: (m) => m.source || "—",
								},
								{ id: "to", header: "To", cell: (m) => m.to },
								{
									id: "subject",
									header: "Subject / first line",
									cell: (m) => m.subject || m.body?.split("\n")[0] || "—",
								},
							]}
						/>
					)}
					{clear && (
						<ActionDialog
							title="Clear mailbox"
							submit="Clear mailbox"
							plan={async () => [
								{ label: "Clear mailbox", run: mailbox.clear },
							]}
							onClose={() => setClear(false)}
							onSaved={() => {
								setClear(false);
								setSelected(undefined);
								setPanelOpen(false);
								messages.reload();
								deliveries.reload();
							}}
						>
							<p>
								Remove all captured email and SMS messages across every account
								and Region, including Cognito codes. Other delivery records are
								kept. This cannot be undone.
							</p>
						</ActionDialog>
					)}
				</SpaceBetween>
			}
		/>
	);
}
