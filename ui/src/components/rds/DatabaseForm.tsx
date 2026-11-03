import {
	Alert,
	Cards,
	Checkbox,
	FormField,
	Input,
	Select,
	SpaceBetween,
	Textarea,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { stringMap } from "#/lib/consoleActions";
import {
	type DBGroup,
	engines,
	type RDSScope,
	rdsCall,
	rdsPages,
	runtimeNote,
	validateName,
	validatePassword,
} from "#/lib/rds";
import { tagList } from "#/lib/tags";
import { usePoll } from "#/lib/usePoll";
export function DatabaseForm({
	scope,
	onClose,
	onSaved,
}: {
	scope: RDSScope;
	onClose: () => void;
	onSaved: (id: string) => void;
}) {
	const [engine, setEngine] = useState(engines[0]),
		[version, setVersion] = useState("16");
	const [id, setId] = useState(""),
		[user, setUser] = useState("admin"),
		[password, setPassword] = useState(""),
		[confirm, setConfirm] = useState(""),
		[show, setShow] = useState(false);
	const [db, setDB] = useState(""),
		[dbClass, setClass] = useState("db.t3.micro"),
		[storage, setStorage] = useState("20"),
		[subnet, setSubnet] = useState(""),
		[parameter, setParameter] = useState(""),
		[tags, setTags] = useState("{}");
	const groups = usePoll(async () => {
		const [subnets, parameters] = await Promise.all([
			rdsPages<DBGroup>(scope, "DescribeDBSubnetGroups", "DBSubnetGroups"),
			rdsPages<DBGroup>(
				scope,
				"DescribeDBParameterGroups",
				"DBParameterGroups",
			),
		]);
		return { subnets, parameters };
	}, 0);
	const field = (
		label: string,
		value: string,
		update: (v: string) => void,
		type: "text" | "password" = "text",
	) => (
		<FormField label={label}>
			<Input
				ariaLabel={label}
				value={value}
				type={type}
				onChange={({ detail }) => update(detail.value)}
			/>
		</FormField>
	);
	return (
		<ActionDialog
			title="Create database"
			submit="Create database"
			onClose={onClose}
			onSaved={() => onSaved(id.toLowerCase())}
			plan={async () => {
				validateName(id);
				validatePassword(password);
				if (password !== confirm) throw new Error("Passwords must match.");
				if (
					!/^[a-zA-Z][a-zA-Z0-9_]*$/.test(user) ||
					user.length > (engine.value === "postgres" ? 63 : 16)
				)
					throw new Error(
						"Enter a valid master username (letters, numbers, underscore; starts with a letter).",
					);
				if (db && (!/^[a-zA-Z][a-zA-Z0-9_]*$/.test(db) || db.length > 63))
					throw new Error(
						"Enter a valid initial database name (up to 63 characters).",
					);
				if (!dbClass.startsWith("db.") || dbClass.length < 4)
					throw new Error("Instance class must start with db.");
				if (
					!/^\d+$/.test(storage) ||
					Number(storage) < 20 ||
					Number(storage) > 65536
				)
					throw new Error("Storage must be 20–65,536 GiB.");
				const input = {
					DBInstanceIdentifier: id,
					Engine: engine.value,
					EngineVersion: version,
					MasterUsername: user,
					MasterUserPassword: password,
					DBInstanceClass: dbClass,
					AllocatedStorage: Number(storage),
					...(db ? { DBName: db } : {}),
					...(subnet ? { DBSubnetGroupName: subnet } : {}),
					...(parameter ? { DBParameterGroupName: parameter } : {}),
					Tags: tagList(stringMap(tags, "Tags")),
				};
				return [
					{
						label: "Create database",
						run: () => rdsCall(scope, "CreateDBInstance", input),
					},
				];
			}}
		>
			<SpaceBetween size="l">
				<Alert type="info">{runtimeNote}</Alert>
				{groups.error && <Alert type="error">{groups.error}</Alert>}
				<FormField label="Engine">
					<Cards
						items={engines}
						selectionType="single"
						selectedItems={[engine]}
						onSelectionChange={({ detail }) => {
							const next = detail.selectedItems[0];
							if (next) {
								setEngine(next);
								setVersion(
									next.versions.includes("16") ? "16" : next.versions[0],
								);
								setParameter("");
							}
						}}
						cardDefinition={{
							header: (e) => e.label,
							sections: [
								{
									id: "versions",
									content: (e) => `Versions: ${e.versions.join(", ")}`,
								},
							],
						}}
					/>
				</FormField>
				<FormField label="Engine version">
					<Select
						selectedOption={{ label: version, value: version }}
						options={engine.versions.map((v) => ({ label: v, value: v }))}
						onChange={({ detail }) => {
							setVersion(detail.selectedOption.value ?? "");
							setParameter("");
						}}
					/>
				</FormField>
				{field("DB instance identifier", id, setId)}
				{field("Master username", user, setUser)}
				{field(
					"Master password",
					password,
					setPassword,
					show ? "text" : "password",
				)}
				{field(
					"Confirm password",
					confirm,
					setConfirm,
					show ? "text" : "password",
				)}
				<Checkbox
					checked={show}
					onChange={({ detail }) => setShow(detail.checked)}
				>
					Show passwords
				</Checkbox>
				{field("Initial database name (optional)", db, setDB)}
				{field("DB instance class", dbClass, setClass)}
				{field("Allocated storage (GiB)", storage, setStorage)}
				<FormField label="DB subnet group">
					<Select
						selectedOption={{ label: subnet || "None", value: subnet }}
						options={[
							{ label: "None", value: "" },
							...(groups.data?.subnets ?? []).map((g) => ({
								label: g.DBSubnetGroupName,
								value: g.DBSubnetGroupName,
							})),
						]}
						onChange={({ detail }) =>
							setSubnet(detail.selectedOption.value ?? "")
						}
					/>
				</FormField>
				<FormField label="DB parameter group">
					<Select
						selectedOption={{ label: parameter || "None", value: parameter }}
						options={[
							{ label: "None", value: "" },
							...(groups.data?.parameters ?? [])
								.filter(
									(g) => g.DBParameterGroupFamily === engine.value + version,
								)
								.map((g) => ({
									label: g.DBParameterGroupName,
									value: g.DBParameterGroupName,
								})),
						]}
						onChange={({ detail }) =>
							setParameter(detail.selectedOption.value ?? "")
						}
					/>
				</FormField>
				<FormField label="Tags (JSON)">
					<Textarea
						ariaLabel="Database tags"
						value={tags}
						onChange={({ detail }) => setTags(detail.value)}
					/>
				</FormField>
			</SpaceBetween>
		</ActionDialog>
	);
}
