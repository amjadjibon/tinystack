import {
	Alert,
	Container,
	CopyToClipboard,
	Header,
	KeyValuePairs,
	SpaceBetween,
	StatusIndicator,
	Tabs,
} from "@cloudscape-design/components";
import { CodeBlock } from "#/components/CodeBlock";
import { ConsoleTags } from "#/components/ConsoleTags";
import {
	type DBInstance,
	type RDSScope,
	rdsCall,
	rdsDate,
	rdsTags,
	runtimeNote,
} from "#/lib/rds";
import { DatabaseActions } from "./DatabaseActions";
export function DatabaseStatus({ status }: { status: string }) {
	const type =
		status === "available"
			? "success"
			: status === "failed"
				? "error"
				: status === "stopped"
					? "stopped"
					: [
								"creating",
								"rebooting",
								"modifying",
								"starting",
								"stopping",
								"deleting",
							].includes(status)
						? "loading"
						: "pending";
	return <StatusIndicator type={type}>{status}</StatusIndicator>;
}
const copy = (text: string) => (
	<CopyToClipboard
		copyButtonAriaLabel="Copy"
		copyErrorText="Copy failed"
		copySuccessText="Copied"
		textToCopy={text}
	/>
);
export function DatabaseDetails({
	scope,
	instance: d,
	enabled,
	onChanged,
	onDeleted,
}: {
	scope: RDSScope;
	instance: DBInstance;
	enabled: boolean;
	onChanged: () => void;
	onDeleted: () => void;
}) {
	const host = d.Endpoint?.Address ?? "",
		port = d.Endpoint?.Port ?? 0,
		endpoint = port ? `${host}:${port}` : "Pending";
	const postgres = d.Engine === "postgres" || d.Engine === "aurora-postgresql",
		db = d.DBName || (postgres ? d.MasterUsername : "");
	let command = postgres
		? `psql -h '${host}' -p ${port} -U '${d.MasterUsername}' -d '${db}' -W`
		: `mysql -h '${host}' -P ${port} -u '${d.MasterUsername}' -p${db ? ` '${db}'` : ""}`;
	let url = `${postgres ? "postgresql" : "mysql"}://${encodeURIComponent(d.MasterUsername)}:<PASSWORD>@${host}:${port}/${encodeURIComponent(db)}`;
	if (d.Engine === "docdb") {
		url = `mongodb://${encodeURIComponent(d.MasterUsername)}:<PASSWORD>@${host}:${port}/?authSource=admin&tls=false`;
		command = `mongosh 'mongodb://${host}:${port}/?authSource=admin&tls=false' --username '${d.MasterUsername}' --password`;
	} else if (d.Engine === "neptune") {
		url = `http://${host}:${port}/gremlin`;
		command = `curl '${url}' -H 'Content-Type: application/json' -d '{"gremlin":"g.inject(1)"}'`;
	}

	return (
		<SpaceBetween size="l">
			<Header
				variant="h1"
				actions={
					<DatabaseActions
						{...{ scope, enabled, onChanged, onDeleted }}
						instance={d}
					/>
				}
			>
				{d.DBInstanceIdentifier}
			</Header>
			<Container header={<Header variant="h2">Summary</Header>}>
				<KeyValuePairs
					columns={3}
					items={[
						{
							label: "Status",
							value: <DatabaseStatus status={d.DBInstanceStatus} />,
						},
						{ label: "Engine", value: `${d.Engine} ${d.EngineVersion}` },
						{ label: "Class", value: d.DBInstanceClass },
						{
							label: "Endpoint",
							value: (
								<SpaceBetween direction="horizontal" size="xs">
									{endpoint}
									{port > 0 && copy(endpoint)}
								</SpaceBetween>
							),
						},
						{ label: "Port", value: port || "Pending" },
					]}
				/>
			</Container>
			<Container header={<Header variant="h2">Connectivity</Header>}>
				<SpaceBetween size="m">
					<Alert type="info">{runtimeNote}</Alert>
					{d.DBInstanceStatus === "available" && port > 0 ? (
						<>
							<Header variant="h3" actions={copy(command)}>
								Command (prompts for password)
							</Header>
							<CodeBlock text={command} />
							<Header variant="h3" actions={copy(url)}>
								Connection URL
							</Header>
							<CodeBlock text={url} />
							<Alert type="info">
								Replace &lt;PASSWORD&gt; with a URL-encoded password when
								connecting.
							</Alert>
						</>
					) : (
						<Alert type="warning">
							Connection commands are available once the database is available.
						</Alert>
					)}
				</SpaceBetween>
			</Container>
			<Tabs
				tabs={[
					{
						id: "configuration",
						label: "Configuration",
						content: (
							<Container>
								<KeyValuePairs
									columns={2}
									items={[
										{ label: "ARN", value: d.DBInstanceArn },
										{ label: "Master username", value: d.MasterUsername },
										{ label: "Database name", value: d.DBName || "—" },
										{
											label: "Allocated storage",
											value: `${d.AllocatedStorage ?? "—"} GiB`,
										},
										{
											label: "Subnet group",
											value: d.DBSubnetGroup?.DBSubnetGroupName ?? "None",
										},
										{
											label: "Parameter groups",
											value:
												d.DBParameterGroups?.map(
													(g) => g.DBParameterGroupName,
												).join(", ") || "None",
										},
										{
											label: "Created",
											value: rdsDate(d.InstanceCreateTime),
										},
									]}
								/>
							</Container>
						),
					},
					{
						id: "tags",
						label: "Tags",
						content: (
							<ConsoleTags
								tags={Object.fromEntries(
									(d.TagList ?? []).map((t) => [t.Key, t.Value]),
								)}
								enabled={enabled}
								onChanged={onChanged}
								plan={(tags, removed) => [
									...(removed.length
										? [
												{
													label: "Remove tags",
													run: () =>
														rdsCall(scope, "RemoveTagsFromResource", {
															ResourceName: d.DBInstanceArn,
															TagKeys: removed,
														}),
												},
											]
										: []),
									...(Object.keys(tags).length
										? [
												{
													label: "Save tags",
													run: () =>
														rdsCall(scope, "AddTagsToResource", {
															ResourceName: d.DBInstanceArn,
															Tags: rdsTags(tags),
														}),
												},
											]
										: []),
								]}
							/>
						),
					},
				]}
			/>
		</SpaceBetween>
	);
}
