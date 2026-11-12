import { Alert, SpaceBetween } from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { Action, Buttons, Rows } from "#/components/containers/Controls";
import { Pick, Text } from "#/components/memorydb/Fields";
import {
	type MemoryDBScope,
	memorydbCall,
	memorydbPages,
} from "#/lib/memorydb";
import { usePoll } from "#/lib/usePoll";

type Group = { Name: string; Family: string; Description: string };
type Parameter = { Name: string; Value: string };
export function MemoryDBParameters({
	scope,
	enabled,
	onChanged,
}: {
	scope: MemoryDBScope;
	enabled: boolean;
	onChanged: () => void;
}) {
	const groups = usePoll(
		() =>
			enabled
				? memorydbPages<Group>(
						scope,
						"DescribeParameterGroups",
						"ParameterGroups",
					)
				: Promise.resolve([]),
		0,
	);
	const [dialog, setDialog] = useState<"" | "create" | "edit">("");
	const [name, setName] = useState("");
	const [family, setFamily] = useState("memorydb_redis7");
	const [description, setDescription] = useState("");
	const [parameter, setParameter] = useState("");
	const [value, setValue] = useState("");
	const [selected, setSelected] = useState("");
	const parameters = usePoll(
		() =>
			enabled && selected
				? memorydbPages<Parameter>(scope, "DescribeParameters", "Parameters", {
						ParameterGroupName: selected,
					})
				: Promise.resolve([]),
		0,
	);
	const refresh = () => {
		groups.reload();
		parameters.reload();
		onChanged();
	};
	return (
		<SpaceBetween size="m">
			{(groups.error || parameters.error) && (
				<Alert type="error">{groups.error || parameters.error}</Alert>
			)}
			<Rows
				title="Parameter groups"
				emptyText="No parameter groups"
				items={groups.data ?? []}
				loading={groups.loading}
				actions={
					<Buttons>
						<Action onClick={refresh}>Refresh</Action>
						<Action
							disabled={!enabled}
							onClick={() => {
								setName("");
								setDescription("");
								setDialog("create");
							}}
						>
							Create parameter group
						</Action>
					</Buttons>
				}
				columns={[
					{
						id: "name",
						header: "Name",
						cell: (g) => (
							<Action onClick={() => setSelected(g.Name)}>{g.Name}</Action>
						),
					},
					{ id: "family", header: "Family", cell: (g) => g.Family },
					{
						id: "description",
						header: "Description",
						cell: (g) => g.Description,
					},
					{
						id: "edit",
						header: "Actions",
						cell: (g) => (
							<Action
								disabled={!enabled || g.Name.startsWith("default.")}
								onClick={() => {
									setName(g.Name);
									setSelected(g.Name);
									setParameter("");
									setValue("");
									setDialog("edit");
								}}
							>
								Edit parameter
							</Action>
						),
					},
				]}
			/>
			{selected && (
				<Rows
					title={`Parameters: ${selected}`}
					emptyText="No parameters"
					items={parameters.data ?? []}
					loading={parameters.loading}
					columns={[
						{ id: "name", header: "Name", cell: (p) => p.Name },
						{ id: "value", header: "Value", cell: (p) => p.Value },
					]}
				/>
			)}
			{dialog && (
				<ActionDialog
					title={
						dialog === "create" ? "Create parameter group" : `Edit ${name}`
					}
					submit="Save"
					onClose={() => setDialog("")}
					onSaved={() => {
						setDialog("");
						refresh();
					}}
					plan={async () => {
						if (dialog === "create")
							return [
								{
									label: "Create parameter group",
									run: () =>
										memorydbCall(scope, "CreateParameterGroup", {
											ParameterGroupName: name,
											Family: family,
											Description: description,
										}),
								},
							];
						if (!parameter) throw new Error("Choose a parameter.");
						return [
							{
								label: "Update parameter",
								run: () =>
									memorydbCall(scope, "UpdateParameterGroup", {
										ParameterGroupName: name,
										ParameterNameValues: [
											{ ParameterName: parameter, ParameterValue: value },
										],
									}),
							},
						];
					}}
				>
					<SpaceBetween size="m">
						{dialog === "create" ? (
							<>
								<Text label="Name" value={name} onChange={setName} />
								<Pick
									label="Family"
									value={family}
									onChange={setFamily}
									placeholder="Choose family"
									options={[
										"memorydb_redis6",
										"memorydb_redis7",
										"memorydb_valkey7",
									].map((value) => ({ value }))}
								/>
								<Text
									label="Description"
									value={description}
									onChange={setDescription}
								/>
							</>
						) : (
							<>
								<Pick
									label="Parameter"
									value={parameter}
									onChange={(p) => {
										setParameter(p);
										setValue(
											parameters.data?.find((item) => item.Name === p)?.Value ??
												"",
										);
									}}
									placeholder="Choose parameter"
									options={(parameters.data ?? []).map((p) => ({
										value: p.Name,
									}))}
								/>
								<Text label="Value" value={value} onChange={setValue} />
							</>
						)}
					</SpaceBetween>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
