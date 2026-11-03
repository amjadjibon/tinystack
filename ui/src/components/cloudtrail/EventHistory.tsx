import {
	Alert,
	Button,
	ExpandableSection,
	Header,
	SpaceBetween,
	Table,
} from "@cloudscape-design/components";
import { useState } from "react";
import { CodeBlock } from "#/components/CodeBlock";
import { Buttons, Choice, Field } from "#/components/containers/Controls";
import { type CloudTrailScope, cloudtrailCall } from "#/lib/cloudtrail";
import {
	eventJSON,
	type HistoryFilter,
	lookupAttributes,
	lookupInput,
	type TrailEvent,
} from "#/lib/cloudtrailHelpers";
import { errorText } from "#/lib/consoleActions";
import { unixTime } from "#/lib/format";
import { usePoll } from "#/lib/usePoll";
export function EventHistory({
	scope,
	enabled,
}: {
	scope: CloudTrailScope;
	enabled: boolean;
}) {
	const [filter, setFilter] = useState<HistoryFilter>({
		attribute: "",
		value: "",
		start: "",
		end: "",
	});
	const [query, setQuery] = useState<Record<string, unknown>>({
		MaxResults: 50,
	});
	const [error, setError] = useState("");
	return (
		<SpaceBetween size="m">
			<Alert type="info">
				History contains up to 1,000 external AWS API events from the last 90
				days, independently of trail logging and selectors. Console actions are
				excluded. Username is the caller access key.
			</Alert>
			<Choice
				label="Lookup attribute"
				value={filter.attribute || "All events"}
				options={["All events", ...lookupAttributes]}
				onChange={(attribute) =>
					setFilter((f) => ({
						...f,
						attribute: attribute === "All events" ? "" : attribute,
					}))
				}
			/>
			<Field
				label="Attribute value"
				value={filter.value}
				onChange={(value) => setFilter((f) => ({ ...f, value }))}
			/>
			<Field
				label="Start time (ISO 8601 with timezone, optional)"
				value={filter.start}
				onChange={(start) => setFilter((f) => ({ ...f, start }))}
			/>
			<Field
				label="End time (ISO 8601 with timezone, optional)"
				value={filter.end}
				onChange={(end) => setFilter((f) => ({ ...f, end }))}
			/>
			<Button
				disabled={!enabled}
				onClick={() => {
					try {
						setQuery(lookupInput(filter));
						setError("");
					} catch (err) {
						setError(errorText(err));
					}
				}}
			>
				Apply filters
			</Button>
			{!!error && <Alert type="error">{error}</Alert>}
			<HistoryPages
				key={JSON.stringify(query)}
				scope={scope}
				enabled={enabled}
				query={query}
			/>
		</SpaceBetween>
	);
}
function HistoryPages({
	scope,
	enabled,
	query,
}: {
	scope: CloudTrailScope;
	enabled: boolean;
	query: Record<string, unknown>;
}) {
	const [tokens, setTokens] = useState<string[]>([]);
	const token = tokens.at(-1);
	return (
		<HistoryPage
			key={token ?? "first"}
			scope={scope}
			enabled={enabled}
			query={query}
			token={token}
			back={() => setTokens((t) => t.slice(0, -1))}
			next={(token) => setTokens((t) => [...t, token])}
		/>
	);
}
function HistoryPage({
	scope,
	enabled,
	query,
	token,
	back,
	next,
}: {
	scope: CloudTrailScope;
	enabled: boolean;
	query: Record<string, unknown>;
	token?: string;
	back: () => void;
	next: (token: string) => void;
}) {
	const poll = usePoll(() =>
		enabled
			? cloudtrailCall<{ Events?: TrailEvent[]; NextToken?: string }>(
					scope,
					"LookupEvents",
					{ ...query, ...(token ? { NextToken: token } : {}) },
				)
			: Promise.resolve({ Events: [] }),
	);
	return (
		<SpaceBetween size="m">
			{poll.error && <Alert type="error">{poll.error}</Alert>}
			<Table<TrailEvent>
				items={poll.data?.Events ?? []}
				loading={poll.loading}
				empty="No events. Adjust the filters or make an AWS API request."
				header={
					<Header
						actions={
							<Buttons>
								<Button disabled={!enabled} onClick={poll.reload}>
									Refresh
								</Button>
								<Button disabled={!token} onClick={back}>
									Previous page
								</Button>
								<Button
									disabled={
										!poll.data?.NextToken || poll.data.NextToken === token
									}
									onClick={() => {
										if (poll.data?.NextToken) next(poll.data.NextToken);
									}}
								>
									Next page
								</Button>
							</Buttons>
						}
					>
						Event history
					</Header>
				}
				columnDefinitions={[
					{ id: "time", header: "Time", cell: (e) => unixTime(e.EventTime) },
					{
						id: "name",
						header: "Event",
						cell: (e) => (
							<ExpandableSection headerText={e.EventName ?? "-"}>
								<CodeBlock text={eventJSON(e.CloudTrailEvent)} />
							</ExpandableSection>
						),
					},
					{ id: "source", header: "Source", cell: (e) => e.EventSource || "-" },
					{ id: "user", header: "Username", cell: (e) => e.Username || "-" },
					{ id: "read", header: "Read only", cell: (e) => e.ReadOnly || "-" },
					{
						id: "resources",
						header: "Resources",
						cell: (e) =>
							e.Resources?.map(
								(r) => `${r.ResourceType}: ${r.ResourceName}`,
							).join(", ") || "-",
					},
				]}
			/>
		</SpaceBetween>
	);
}
