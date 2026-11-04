import {
	Alert,
	Button,
	SpaceBetween,
	Tabs,
} from "@cloudscape-design/components";
import { useState } from "react";
import type { GlueScope } from "#/lib/glue";
import type { GlueKind } from "#/lib/glueHelpers";
import { GlueResources } from "./GlueResources";
export function GlueConsole({
	scope,
	enabled,
	onChanged,
}: {
	scope: GlueScope;
	enabled: boolean;
	onChanged: () => void;
}) {
	const [tab, setTab] = useState("catalog");
	const [database, setDatabase] = useState<string>();
	const [table, setTable] = useState<string>();
	return (
		<Tabs
			activeTabId={tab}
			onChange={({ detail }) => setTab(detail.activeTabId)}
			tabs={[
				{
					id: "catalog",
					label: "Data Catalog",
					content: (
						<SpaceBetween size="m">
							<GlueResources
								scope={scope}
								kind="Database"
								context={{}}
								enabled={enabled}
								onChanged={onChanged}
								onSelect={(r) => {
									setDatabase(r.Name);
									setTable(undefined);
								}}
							/>
							{!!database && (
								<SpaceBetween size="m">
									<Alert type="info">
										Database: {database}{" "}
										<Button
											variant="inline-link"
											onClick={() => {
												setDatabase(undefined);
												setTable(undefined);
											}}
										>
											Close tables
										</Button>
									</Alert>
									<GlueResources
										key={database}
										scope={scope}
										kind="Table"
										context={{ DatabaseName: database }}
										enabled={enabled}
										onChanged={onChanged}
										onSelect={(r) => setTable(r.Name)}
									/>
								</SpaceBetween>
							)}
							{!!database && !!table && (
								<SpaceBetween size="m">
									<Alert type="info">
										Table: {table}{" "}
										<Button
											variant="inline-link"
											onClick={() => setTable(undefined)}
										>
											Close partitions
										</Button>
									</Alert>
									<GlueResources
										key={`${database}/${table}`}
										scope={scope}
										kind="Partition"
										context={{ DatabaseName: database, TableName: table }}
										enabled={enabled}
										onChanged={onChanged}
									/>
								</SpaceBetween>
							)}
						</SpaceBetween>
					),
				},
				...(["Crawler", "Job", "Trigger", "Workflow"] as GlueKind[]).map(
					(kind) => ({
						id: kind,
						label: `${kind}s`,
						content: (
							<GlueResources
								scope={scope}
								kind={kind}
								context={{}}
								enabled={enabled}
								onChanged={onChanged}
							/>
						),
					}),
				),
			]}
		/>
	);
}
