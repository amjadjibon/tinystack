import {
	Alert,
	Button,
	Header,
	KeyValuePairs,
	SpaceBetween,
	Tabs,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ConsoleTags } from "#/components/ConsoleTags";
import { Choice } from "#/components/containers/Controls";
import { type AppSyncScope, appsyncCall, listAppSync } from "#/lib/appsync";
import type { GraphqlApi } from "#/lib/appsyncHelpers";
import { usePoll } from "#/lib/usePoll";
import { ResourceDialog } from "./ResourceDialog";
import { Resources } from "./Resources";
import { Schema } from "./Schema";

export function ApiDetails({
	scope,
	apiId,
	enabled,
	onChanged,
	onDeleted,
}: {
	scope: AppSyncScope;
	apiId: string;
	enabled: boolean;
	onChanged: () => void;
	onDeleted: () => void;
}) {
	const [tab, setTab] = useState("schema"),
		[edit, setEdit] = useState(false),
		[remove, setRemove] = useState(false);
	const poll = usePoll(() =>
		enabled
			? appsyncCall<{ GraphqlApi?: GraphqlApi }>(scope, "GetGraphqlApi", {
					ApiId: apiId,
				})
			: Promise.resolve({ GraphqlApi: undefined }),
	);
	const api = poll.data?.GraphqlApi;
	return (
		<SpaceBetween size="l">
			<Header>{api?.Name ?? apiId}</Header>
			<KeyValuePairs
				items={[
					{ label: "API ID", value: apiId },
					{
						label: "Authentication type",
						value: api?.AuthenticationType ?? "-",
					},
					{ label: "ARN", value: api?.Arn ?? "-" },
					...Object.entries(api?.Uris ?? {}).map(([label, value]) => ({
						label,
						value,
					})),
				]}
			/>
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			<SpaceBetween direction="horizontal" size="s">
				<Button disabled={!enabled || !api} onClick={() => setEdit(true)}>
					Edit API
				</Button>
				<Button disabled={!enabled || !api} onClick={() => setRemove(true)}>
					Delete API
				</Button>
			</SpaceBetween>
			<Tabs
				activeTabId={tab}
				onChange={({ detail }) => setTab(detail.activeTabId)}
				tabs={[
					{
						id: "schema",
						label: "Schema",
						content:
							tab === "schema" ? (
								<Schema scope={scope} apiId={apiId} enabled={enabled} />
							) : null,
					},
					{
						id: "source",
						label: "Data sources",
						content:
							tab === "source" ? (
								<Resources
									scope={scope}
									apiId={apiId}
									kind="source"
									enabled={enabled}
									onChanged={onChanged}
								/>
							) : null,
					},
					{
						id: "resolver",
						label: "Resolvers",
						content:
							tab === "resolver" ? (
								<Resolvers
									scope={scope}
									apiId={apiId}
									enabled={enabled}
									onChanged={onChanged}
								/>
							) : null,
					},
					{
						id: "function",
						label: "Functions",
						content:
							tab === "function" ? (
								<Resources
									scope={scope}
									apiId={apiId}
									kind="function"
									enabled={enabled}
									onChanged={onChanged}
								/>
							) : null,
					},
					{
						id: "key",
						label: "API keys",
						content:
							tab === "key" ? (
								<Resources
									scope={scope}
									apiId={apiId}
									kind="key"
									enabled={enabled}
									onChanged={onChanged}
								/>
							) : null,
					},
					{
						id: "tags",
						label: "Tags",
						content:
							tab === "tags" ? (
								<ConsoleTags
									tags={api?.Tags ?? {}}
									enabled={enabled && !!api?.Arn}
									onChanged={() => {
										poll.reload();
										onChanged();
									}}
									plan={(tags, removed) => [
										...(Object.keys(tags).length
											? [
													{
														label: "TagResource",
														run: () =>
															appsyncCall(scope, "TagResource", {
																ResourceArn: api?.Arn,
																Tags: tags,
															}),
													},
												]
											: []),
										...(removed.length
											? [
													{
														label: "UntagResource",
														run: () =>
															appsyncCall(scope, "UntagResource", {
																ResourceArn: api?.Arn,
																TagKeys: removed,
															}),
													},
												]
											: []),
									]}
								/>
							) : null,
					},
				]}
			/>
			{(edit || remove) && api && (
				<ResourceDialog
					scope={scope}
					kind="api"
					row={api}
					remove={remove}
					onClose={() => {
						setEdit(false);
						setRemove(false);
					}}
					onSaved={() => {
						setEdit(false);
						setRemove(false);
						onChanged();
						if (remove) onDeleted();
						else poll.reload();
					}}
				/>
			)}
		</SpaceBetween>
	);
}
function Resolvers({
	scope,
	apiId,
	enabled,
	onChanged,
}: {
	scope: AppSyncScope;
	apiId: string;
	enabled: boolean;
	onChanged: () => void;
}) {
	const [type, setType] = useState("");
	const poll = usePoll(() =>
		enabled
			? listAppSync(scope, "ListTypes", "Types", {
					ApiId: apiId,
					Format: "SDL",
				})
			: Promise.resolve([]),
	);
	const selected = type || String(poll.data?.[0]?.Name ?? "");
	return (
		<SpaceBetween size="m">
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			<Choice
				label="Type"
				value={selected}
				options={(poll.data ?? []).map((t) => String(t.Name))}
				onChange={setType}
			/>
			<Resources
				key={selected}
				scope={scope}
				apiId={apiId}
				kind="resolver"
				typeName={selected}
				enabled={enabled}
				onChanged={onChanged}
			/>
		</SpaceBetween>
	);
}
