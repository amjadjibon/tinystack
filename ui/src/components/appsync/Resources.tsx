import {
	Alert,
	Button,
	Link,
	SpaceBetween,
} from "@cloudscape-design/components";
import { useState } from "react";
import { Action, Buttons, Rows } from "#/components/containers/Controls";
import { type AppSyncScope, listAppSync } from "#/lib/appsync";
import {
	appsyncHref,
	type RecordValue,
	type ResourceKind,
	resourceLabel,
} from "#/lib/appsyncHelpers";
import { unixTime } from "#/lib/format";
import { useFollow } from "#/lib/useFollow";
import { usePoll } from "#/lib/usePoll";
import { ResourceDialog } from "./ResourceDialog";

const listing = {
	api: ["ListGraphqlApis", "GraphqlApis"],
	source: ["ListDataSources", "DataSources"],
	resolver: ["ListResolvers", "Resolvers"],
	function: ["ListFunctions", "Functions"],
	key: ["ListApiKeys", "ApiKeys"],
};
const titles = {
	api: "GraphQL APIs",
	source: "Data sources",
	resolver: "Resolvers",
	function: "Functions",
	key: "API keys",
};
export function Resources({
	scope,
	apiId,
	kind,
	typeName,
	enabled,
	onChanged,
}: {
	scope: AppSyncScope;
	apiId?: string;
	kind: ResourceKind;
	typeName?: string;
	enabled: boolean;
	onChanged: () => void;
}) {
	const follow = useFollow();
	const [dialog, setDialog] = useState<{
		row?: RecordValue;
		remove?: boolean;
	}>();
	const poll = usePoll(() =>
		enabled && (kind !== "resolver" || typeName)
			? listAppSync(scope, listing[kind][0], listing[kind][1], {
					...(apiId ? { ApiId: apiId } : {}),
					...(typeName ? { TypeName: typeName } : {}),
				})
			: Promise.resolve([]),
	);
	const columns =
		kind === "api"
			? [
					{
						id: "id",
						header: "API ID",
						cell: (r: RecordValue) => (
							<Link
								href={appsyncHref(scope, String(r.ApiId))}
								onFollow={follow}
							>
								{String(r.ApiId)}
							</Link>
						),
					},
					{
						id: "auth",
						header: "Authentication",
						cell: (r: RecordValue) => String(r.AuthenticationType ?? "-"),
					},
					{
						id: "uris",
						header: "Endpoint URIs",
						cell: (r: RecordValue) => (
							<SpaceBetween size="xxs">
								{Object.entries((r.Uris ?? {}) as Record<string, string>).map(
									([key, value]) => (
										<div key={key}>
											{key}: {value}
										</div>
									),
								)}
							</SpaceBetween>
						),
					},
					{ id: "created", header: "Created", cell: () => "-" },
				]
			: kind === "key"
				? [
						{
							id: "expires",
							header: "Expires",
							cell: (r: RecordValue) =>
								unixTime(r.Expires as number | undefined),
						},
					]
				: kind === "source"
					? [
							{
								id: "type",
								header: "Type",
								cell: (r: RecordValue) => String(r.Type ?? "-"),
							},
							{
								id: "config",
								header: "Configuration",
								cell: (r: RecordValue) =>
									JSON.stringify(
										Object.fromEntries(
											Object.entries(r).filter(([k]) => k.endsWith("Config")),
										),
									),
							},
						]
					: [
							...(kind === "function"
								? [
										{
											id: "functionId",
											header: "Function ID",
											cell: (r: RecordValue) => String(r.FunctionId),
										},
									]
								: []),
							{
								id: "source",
								header: "Data source",
								cell: (r: RecordValue) => String(r.DataSourceName ?? "-"),
							},
							{
								id: "runtime",
								header: "Runtime / kind",
								cell: (r: RecordValue) =>
									r.Runtime
										? "APPSYNC_JS 1.0.0"
										: String(r.Kind ?? "VTL / direct Lambda"),
							},
						];
	return (
		<SpaceBetween size="m">
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			<Rows
				title={titles[kind]}
				items={poll.data ?? []}
				loading={poll.loading}
				emptyText="Choose Create to add one."
				actions={
					<Buttons>
						<Action onClick={poll.reload}>Refresh</Action>
						<Button
							variant="primary"
							disabled={!enabled || (kind === "resolver" && !typeName)}
							onClick={() => setDialog({})}
						>
							Create
						</Button>
					</Buttons>
				}
				columns={[
					{
						id: "name",
						header: kind === "key" ? "Key ID" : "Name",
						cell: (r) => resourceLabel(kind, r),
					},
					...columns,
					{
						id: "actions",
						header: "Actions",
						cell: (r) => (
							<Buttons>
								{kind !== "key" && (
									<Action
										disabled={!enabled}
										onClick={() => setDialog({ row: r })}
									>
										Edit
									</Action>
								)}
								<Action
									disabled={!enabled}
									onClick={() => setDialog({ row: r, remove: true })}
								>
									Delete
								</Action>
							</Buttons>
						),
					},
				]}
			/>
			{dialog && (
				<ResourceDialog
					scope={scope}
					apiId={apiId}
					kind={kind}
					typeName={typeName}
					row={dialog.row}
					remove={dialog.remove}
					onClose={() => setDialog(undefined)}
					onSaved={() => {
						setDialog(undefined);
						poll.reload();
						onChanged();
					}}
				/>
			)}
		</SpaceBetween>
	);
}
