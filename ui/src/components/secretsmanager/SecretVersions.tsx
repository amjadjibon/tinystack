import { useCollection } from "@cloudscape-design/collection-hooks";
import {
	Alert,
	Box,
	Button,
	FormField,
	Header,
	Input,
	Pagination,
	Select,
	SpaceBetween,
	Table,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import {
	type Secret,
	type SecretScope,
	type SecretVersion,
	secretCall,
	secretDate,
	secretPages,
} from "#/lib/secretsmanager";
import { usePoll } from "#/lib/usePoll";
export function SecretVersions({
	scope,
	enabled,
	readable,
	onChanged,
}: {
	scope: SecretScope;
	enabled: boolean;
	readable: boolean;
	onChanged: () => void;
}) {
	const versions = usePoll(
		() =>
			readable
				? secretPages<SecretVersion>(
						scope,
						"ListSecretVersionIds",
						"Versions",
						{ IncludeDeprecated: true },
					)
				: Promise.resolve([]),
		0,
	);
	const [selected, setSelected] = useState<SecretVersion[]>([]),
		[mode, setMode] = useState(""),
		[label, setLabel] = useState("");
	const { items, collectionProps, paginationProps } = useCollection(
		versions.data ?? [],
		{
			sorting: {
				defaultState: {
					sortingColumn: { sortingField: "CreatedDate" },
					isDescending: true,
				},
			},
			pagination: { pageSize: 20 },
		},
	);
	const version = selected[0];
	const removeOptions = (version?.VersionStages ?? [])
		.filter((s) => s !== "AWSCURRENT")
		.map((s) => ({ label: s, value: s }));
	return (
		<SpaceBetween size="m">
			{!!versions.error && <Alert type="error">{versions.error}</Alert>}
			<Table
				{...collectionProps}
				items={items}
				trackBy="VersionId"
				selectionType="single"
				selectedItems={selected}
				onSelectionChange={({ detail }) => setSelected(detail.selectedItems)}
				loading={versions.loading}
				loadingText="Loading versions"
				empty={<Box>No versions</Box>}
				header={
					<Header
						variant="h2"
						counter={`(${versions.data?.length ?? 0})`}
						actions={
							<SpaceBetween direction="horizontal" size="xs">
								<Button
									iconName="refresh"
									ariaLabel="Refresh versions"
									onClick={versions.reload}
								/>
								<Button
									disabled={
										!enabled ||
										!version ||
										!!versions.error ||
										!!version.VersionStages?.includes("AWSCURRENT")
									}
									onClick={() => {
										setLabel("AWSCURRENT");
										setMode("move");
									}}
								>
									Make current
								</Button>
								<Button
									disabled={!enabled || !version || !!versions.error}
									onClick={() => {
										setLabel("");
										setMode("move");
									}}
								>
									Add or move label
								</Button>
								<Button
									disabled={
										!enabled ||
										!version ||
										!!versions.error ||
										!removeOptions.length
									}
									onClick={() => {
										setLabel(removeOptions[0].value);
										setMode("remove");
									}}
								>
									Remove label
								</Button>
							</SpaceBetween>
						}
					>
						Versions
					</Header>
				}
				pagination={<Pagination {...paginationProps} />}
				columnDefinitions={[
					{
						id: "id",
						header: "Version ID",
						cell: (v) => v.VersionId,
						isRowHeader: true,
					},
					{
						id: "stages",
						header: "Staging labels",
						cell: (v) =>
							v.VersionStages?.join(", ") || "Deprecated (no labels)",
					},
					{
						id: "created",
						header: "Created",
						cell: (v) => secretDate(v.CreatedDate),
						sortingField: "CreatedDate",
					},
				]}
			/>
			{!!mode && version && (
				<ActionDialog
					title={
						mode === "remove"
							? "Remove staging label"
							: "Add or move staging label"
					}
					onClose={() => setMode("")}
					onSaved={() => {
						setMode("");
						setSelected([]);
						versions.reload();
						onChanged();
					}}
					plan={async () => {
						if (!label || label.length > 256)
							throw new Error("Enter a staging label of 1–256 characters.");
						if (mode === "remove" && label === "AWSCURRENT")
							throw new Error("AWSCURRENT must be moved to another version.");
						const fresh = await secretCall<Secret>(scope, "DescribeSecret");
						const holder = Object.entries(fresh.VersionIdsToStages ?? {}).find(
							([, stages]) => stages.includes(label),
						)?.[0];
						if (mode === "remove" && holder !== version.VersionId)
							throw new Error(
								"This label has changed. Refresh versions before editing it.",
							);
						if (mode === "move" && holder === version.VersionId) return [];
						return [
							{
								label: "Update staging label",
								run: () =>
									secretCall(scope, "UpdateSecretVersionStage", {
										VersionStage: label,
										...(holder ? { RemoveFromVersionId: holder } : {}),
										...(mode === "move"
											? { MoveToVersionId: version.VersionId }
											: {}),
									}),
							},
						];
					}}
				>
					<SpaceBetween size="m">
						<Box>Version: {version.VersionId}</Box>
						{label === "AWSCURRENT" && (
							<Alert type="info">
								The previous current version becomes AWSPREVIOUS.
							</Alert>
						)}
						<FormField label="Staging label">
							{mode === "remove" ? (
								<Select
									ariaLabel="Staging label"
									options={removeOptions}
									selectedOption={
										removeOptions.find((o) => o.value === label) ?? null
									}
									onChange={({ detail }) =>
										setLabel(detail.selectedOption.value ?? "")
									}
								/>
							) : (
								<Input
									ariaLabel="Staging label"
									value={label}
									onChange={({ detail }) => setLabel(detail.value)}
								/>
							)}
						</FormField>
					</SpaceBetween>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
