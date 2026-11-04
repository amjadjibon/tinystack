import { Alert, SpaceBetween, Tabs } from "@cloudscape-design/components";
import { useState } from "react";
import { Action, Buttons, Choice } from "#/components/containers/Controls";
import { type AthenaScope, athenaPages } from "#/lib/athena";
import type {
	NamedQuery,
	QueryExecution,
	WorkGroup,
} from "#/lib/athenaHelpers";
import { usePoll } from "#/lib/usePoll";
import { AthenaCatalog } from "./AthenaCatalog";
import { AthenaQueries } from "./AthenaQueries";
import { AthenaWorkgroups } from "./AthenaWorkgroups";
export function AthenaWorkspace({
	scope,
	enabled,
	onChanged,
}: {
	scope: AthenaScope;
	enabled: boolean;
	onChanged: () => void;
}) {
	const [tab, setTab] = useState("editor"),
		[group, setGroup] = useState("primary"),
		[draft, setDraft] = useState<QueryExecution | NamedQuery>(),
		[revision, setRevision] = useState(0);
	const groups = usePoll(() =>
		enabled
			? athenaPages<WorkGroup>(scope, "ListWorkGroups", "WorkGroups")
			: Promise.resolve([]),
	);
	const open = (q: QueryExecution | NamedQuery) => {
		setDraft(q);
		setRevision((v) => v + 1);
		setGroup(q.WorkGroup ?? "primary");
		setTab("editor");
	};
	return (
		<SpaceBetween size="m">
			{!!groups.error && <Alert type="error">{groups.error}</Alert>}
			<Buttons>
				<Action
					disabled={!enabled}
					onClick={() => {
						groups.reload();
						onChanged();
					}}
				>
					Refresh
				</Action>
			</Buttons>
			<Choice
				label="Workgroup"
				value={group}
				options={(groups.data ?? []).map((g) => g.Name)}
				onChange={setGroup}
			/>
			<Tabs
				activeTabId={tab}
				onChange={({ detail }) => setTab(detail.activeTabId)}
				tabs={[
					{
						id: "editor",
						label: "Query editor",
						content: (
							<AthenaQueries
								key={`${group}/${revision}`}
								scope={scope}
								enabled={enabled}
								group={group}
								draft={draft}
								mode="editor"
								onOpen={open}
								onChanged={onChanged}
							/>
						),
					},
					{
						id: "history",
						label: "Query history",
						content: (
							<AthenaQueries
								key={group}
								scope={scope}
								enabled={enabled}
								group={group}
								mode="history"
								onOpen={open}
								onChanged={onChanged}
							/>
						),
					},
					{
						id: "saved",
						label: "Saved queries",
						content: (
							<AthenaQueries
								key={group}
								scope={scope}
								enabled={enabled}
								group={group}
								mode="saved"
								onOpen={open}
								onChanged={onChanged}
							/>
						),
					},
					{
						id: "groups",
						label: "Workgroups",
						content: (
							<AthenaWorkgroups
								scope={scope}
								enabled={enabled}
								groups={groups.data ?? []}
								onChanged={() => {
									groups.reload();
									onChanged();
								}}
							/>
						),
					},
					{
						id: "catalog",
						label: "Data catalogs",
						content: (
							<AthenaCatalog
								key={group}
								scope={scope}
								enabled={enabled}
								group={group}
							/>
						),
					},
				]}
			/>
		</SpaceBetween>
	);
}
