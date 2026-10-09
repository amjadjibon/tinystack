import {
	Box,
	Button,
	Checkbox,
	Container,
	ContentLayout,
	Form,
	FormField,
	Header,
	Input,
	RadioGroup,
	Select,
	SpaceBetween,
	Textarea,
} from "@cloudscape-design/components";
import { useRef, useState } from "react";
import { errorText, stringMap } from "#/lib/consoleActions";
import {
	attributeDefinitions,
	buildIndex,
	capacity,
	type DynamoScope,
	dynamoCall,
	dynamoTags,
	type IndexInput,
	indexDefaults,
	indexDefinitions,
	keySchema,
	streamViews,
} from "#/lib/dynamodb";
import { DynamoIndexFields } from "./DynamoIndexFields";
import { DynamoKeyFields } from "./DynamoKeyFields";

export function DynamoTableForm({
	scope,
	onCancel,
	onSaved,
}: {
	scope: DynamoScope;
	onCancel: () => void;
	onSaved: (name: string) => void;
}) {
	const [name, setName] = useState("");
	const [partition, setPartition] = useState("pk"),
		[partitionType, setPartitionType] = useState("S");
	const [sort, setSort] = useState(""),
		[sortType, setSortType] = useState("S");
	const [mode, setMode] = useState("PAY_PER_REQUEST");
	const [read, setRead] = useState("5"),
		[write, setWrite] = useState("5");
	const [tags, setTags] = useState("{}");
	const [stream, setStream] = useState(false),
		[view, setView] = useState("NEW_AND_OLD_IMAGES");
	const [indexes, setIndexes] = useState<
		{ id: number; local: boolean; value: IndexInput }[]
	>([]);
	const counter = useRef(0);
	const [busy, setBusy] = useState(false),
		[error, setError] = useState("");
	const provisioned = mode === "PROVISIONED";
	const resolved = indexes.map((i) => ({
		...i,
		value: i.local ? { ...i.value, partition, partitionType } : i.value,
	}));
	const save = async () => {
		if (busy) return;
		setBusy(true);
		setError("");
		try {
			if (!/^[A-Za-z0-9_.-]{3,255}$/.test(name))
				throw new Error(
					"Table names use 3–255 letters, numbers, underscores, hyphens or periods.",
				);
			if (new Set(resolved.map((i) => i.value.name)).size !== indexes.length)
				throw new Error("Secondary index names must be unique.");
			if (
				resolved.some(
					(i) => i.local && (!sort || !i.value.sort || i.value.sort === sort),
				)
			)
				throw new Error(
					"Local indexes require a table sort key and a different index sort key.",
				);
			await dynamoCall(scope, "CreateTable", {
				TableName: name,
				KeySchema: keySchema(partition, sort),
				BillingMode: mode,
				AttributeDefinitions: attributeDefinitions([
					{ AttributeName: partition, AttributeType: partitionType },
					...(sort ? [{ AttributeName: sort, AttributeType: sortType }] : []),
					...resolved.flatMap((i) => indexDefinitions(i.value)),
				]),
				...(provisioned
					? { ProvisionedThroughput: capacity(read, write) }
					: {}),
				Tags: dynamoTags(stringMap(tags, "Tags")),
				GlobalSecondaryIndexes: resolved
					.filter((i) => !i.local)
					.map((i) => buildIndex(i.value, provisioned)),
				LocalSecondaryIndexes: resolved
					.filter((i) => i.local)
					.map((i) => buildIndex(i.value, false)),
				...(stream
					? {
							StreamSpecification: {
								StreamEnabled: true,
								StreamViewType: view,
							},
						}
					: {}),
			});
			onSaved(name);
		} catch (err) {
			setError(errorText(err));
		} finally {
			setBusy(false);
		}
	};
	return (
		<ContentLayout header={<Header variant="h1">Create table</Header>}>
			<Form
				errorText={error}
				actions={
					<SpaceBetween direction="horizontal" size="xs">
						<Button disabled={busy} onClick={onCancel}>
							Cancel
						</Button>
						<Button variant="primary" loading={busy} onClick={save}>
							Create table
						</Button>
					</SpaceBetween>
				}
			>
				<fieldset disabled={busy} style={{ border: 0, margin: 0, padding: 0 }}>
					<SpaceBetween size="l">
						<Container header={<Header variant="h2">Table details</Header>}>
							<SpaceBetween size="l">
								<FormField
									label="Table name"
									constraintText="3–255 letters, numbers, underscores, hyphens or periods."
								>
									<Input
										ariaLabel="Table name"
										value={name}
										onChange={({ detail }) => setName(detail.value)}
									/>
								</FormField>
								<DynamoKeyFields
									label="Partition key"
									name={partition}
									type={partitionType}
									onChange={(name, type) => {
										setPartition(name);
										setPartitionType(type);
									}}
								/>
								<DynamoKeyFields
									label="Sort key — optional"
									name={sort}
									type={sortType}
									onChange={(name, type) => {
										setSort(name);
										setSortType(type);
									}}
								/>
								<Box>
									Primary keys cannot be changed after creation. AWS account:{" "}
									{scope.account} · AWS Region: {scope.region}
								</Box>
							</SpaceBetween>
						</Container>
						<Container
							header={
								<Header
									variant="h2"
									description="Capacity settings are stored; TinyStack does not enforce throughput or calculate charges."
								>
									Read/write capacity
								</Header>
							}
						>
							<SpaceBetween size="l">
								<RadioGroup
									value={mode}
									onChange={({ detail }) => setMode(detail.value)}
									items={[
										{ value: "PAY_PER_REQUEST", label: "On-demand" },
										{ value: "PROVISIONED", label: "Provisioned" },
									]}
								/>
								{provisioned && (
									<SpaceBetween direction="horizontal" size="s">
										<FormField label="Read capacity units">
											<Input
												ariaLabel="Table read capacity units"
												type="number"
												value={read}
												onChange={({ detail }) => setRead(detail.value)}
											/>
										</FormField>
										<FormField label="Write capacity units">
											<Input
												ariaLabel="Table write capacity units"
												type="number"
												value={write}
												onChange={({ detail }) => setWrite(detail.value)}
											/>
										</FormField>
									</SpaceBetween>
								)}
							</SpaceBetween>
						</Container>
						<Container
							header={
								<Header
									variant="h2"
									description="Local secondary indexes can only be created with the table."
									actions={
										<SpaceBetween direction="horizontal" size="xs">
											<Button
												disabled={resolved.filter((i) => !i.local).length >= 20}
												onClick={() =>
													setIndexes([
														...indexes,
														{
															id: ++counter.current,
															local: false,
															value: indexDefaults(),
														},
													])
												}
											>
												Add global index
											</Button>
											<Button
												disabled={
													!sort || resolved.filter((i) => i.local).length >= 5
												}
												onClick={() =>
													setIndexes([
														...indexes,
														{
															id: ++counter.current,
															local: true,
															value: indexDefaults(),
														},
													])
												}
											>
												Add local index
											</Button>
										</SpaceBetween>
									}
								>
									Secondary indexes — optional
								</Header>
							}
						>
							<SpaceBetween size="l">
								{!indexes.length && <Box>No secondary indexes configured.</Box>}
								{resolved.map((i) => (
									<Container
										key={i.id}
										header={
											<Header
												variant="h3"
												actions={
													<Button
														onClick={() =>
															setIndexes(indexes.filter((x) => x.id !== i.id))
														}
													>
														Remove
													</Button>
												}
											>
												{i.local
													? "Local secondary index"
													: "Global secondary index"}
											</Header>
										}
									>
										<DynamoIndexFields
											value={i.value}
											local={i.local}
											provisioned={provisioned}
											onChange={(value) =>
												setIndexes(
													indexes.map((x) =>
														x.id === i.id ? { ...x, value } : x,
													),
												)
											}
										/>
									</Container>
								))}
							</SpaceBetween>
						</Container>
						<Container
							header={<Header variant="h2">DynamoDB Streams — optional</Header>}
						>
							<SpaceBetween size="l">
								<Checkbox
									checked={stream}
									onChange={({ detail }) => setStream(detail.checked)}
								>
									Enable streams
								</Checkbox>
								{stream && (
									<FormField label="Stream view type">
										<Select
											selectedOption={{ value: view, label: view }}
											options={streamViews.map((v) => ({ value: v, label: v }))}
											onChange={({ detail }) =>
												setView(
													detail.selectedOption.value ?? "NEW_AND_OLD_IMAGES",
												)
											}
										/>
									</FormField>
								)}
							</SpaceBetween>
						</Container>
						<Container header={<Header variant="h2">Tags — optional</Header>}>
							<FormField
								label="Tags (JSON)"
								description='Object of string values, for example {"team":"platform"}.'
							>
								<Textarea
									ariaLabel="Table tags"
									rows={4}
									value={tags}
									onChange={({ detail }) => setTags(detail.value)}
								/>
							</FormField>
						</Container>
					</SpaceBetween>
				</fieldset>
			</Form>
		</ContentLayout>
	);
}
