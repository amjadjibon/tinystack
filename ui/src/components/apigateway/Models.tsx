import {
	Alert,
	Box,
	Button,
	Checkbox,
	FormField,
	Header,
	SpaceBetween,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import {
	apiCall,
	type GatewayModel,
	type GatewayScope,
	type GatewaySnapshot,
	type GatewayValidator,
	scalarPatch,
} from "#/lib/apigateway";
import { jsonObject } from "#/lib/consoleActions";
import { GatewayInput, GatewayText } from "./Fields";
import { GatewayTable } from "./GatewayTable";

const draft4 = "http://json-schema.org/draft-04/schema#";

// Models and request validators of a REST API. Methods pick them in the method form.
export function Models({
	scope,
	snapshot,
	enabled,
	onChanged,
}: {
	scope: GatewayScope;
	snapshot: GatewaySnapshot;
	enabled: boolean;
	onChanged: (message: string) => void;
}) {
	const [model, setModel] = useState<GatewayModel>(),
		[validator, setValidator] = useState<GatewayValidator>(),
		[dialog, setDialog] = useState("");
	const done = (message: string) => () => {
		setDialog("");
		onChanged(message);
	};
	const actions = (edit: () => void, remove: () => void) => (
		<SpaceBetween direction="horizontal" size="xs">
			<Button variant="inline-link" disabled={!enabled} onClick={edit}>
				Edit
			</Button>
			<Button variant="inline-link" disabled={!enabled} onClick={remove}>
				Delete
			</Button>
		</SpaceBetween>
	);
	return (
		<SpaceBetween size="l">
			<GatewayTable
				items={snapshot.models}
				trackBy={(m) => m.Name}
				wrapLines
				header={
					<Header
						description="JSON Schema draft 4 documents that describe request and response bodies."
						actions={
							<Button
								variant="primary"
								disabled={!enabled}
								onClick={() => {
									setModel(undefined);
									setDialog("model");
								}}
							>
								Create model
							</Button>
						}
					>
						Models
					</Header>
				}
				columnDefinitions={[
					{ id: "name", header: "Name", cell: (m) => m.Name },
					{
						id: "type",
						header: "Content type",
						cell: (m) => m.ContentType ?? "—",
					},
					{
						id: "description",
						header: "Description",
						cell: (m) => m.Description || "—",
					},
					{
						id: "actions",
						header: "Actions",
						cell: (m) =>
							actions(
								() => {
									setModel(m);
									setDialog("model");
								},
								() => {
									setModel(m);
									setDialog("delete-model");
								},
							),
					},
				]}
				empty={<Box>No models.</Box>}
			/>
			<GatewayTable
				items={snapshot.validators}
				trackBy={(v) => v.Id}
				header={
					<Header
						description="A method's validator checks its required parameters and its body against the model for the request content type."
						actions={
							<Button
								disabled={!enabled}
								onClick={() => {
									setValidator(undefined);
									setDialog("validator");
								}}
							>
								Create validator
							</Button>
						}
					>
						Request validators
					</Header>
				}
				columnDefinitions={[
					{ id: "name", header: "Name", cell: (v) => v.Name ?? v.Id },
					{
						id: "body",
						header: "Validates body",
						cell: (v) => (v.ValidateRequestBody ? "Yes" : "No"),
					},
					{
						id: "parameters",
						header: "Validates parameters",
						cell: (v) => (v.ValidateRequestParameters ? "Yes" : "No"),
					},
					{
						id: "actions",
						header: "Actions",
						cell: (v) =>
							actions(
								() => {
									setValidator(v);
									setDialog("validator");
								},
								() => {
									setValidator(v);
									setDialog("delete-validator");
								},
							),
					},
				]}
				empty={
					<Box>
						No request validators. Create one, then attach it to a method.
					</Box>
				}
			/>
			{dialog === "model" && (
				<ModelForm
					scope={scope}
					initial={model}
					onClose={() => setDialog("")}
					onSaved={done("Model saved.")}
				/>
			)}
			{dialog === "validator" && (
				<ValidatorForm
					scope={scope}
					initial={validator}
					onClose={() => setDialog("")}
					onSaved={done("Request validator saved.")}
				/>
			)}
			{dialog === "delete-model" && model && (
				<ActionDialog
					title="Delete model"
					submit="Delete model"
					confirmation={model.Name}
					onClose={() => setDialog("")}
					onSaved={done("Model deleted.")}
					plan={async () => [
						{
							label: "Delete model",
							run: () =>
								apiCall(scope, "DeleteModel", { ModelName: model.Name }),
						},
					]}
				>
					<Box>
						Methods and other models that refer to this model must stop using it
						first.
					</Box>
				</ActionDialog>
			)}
			{dialog === "delete-validator" && validator && (
				<ActionDialog
					title="Delete request validator"
					submit="Delete validator"
					confirmation={validator.Name ?? validator.Id}
					onClose={() => setDialog("")}
					onSaved={done("Request validator deleted.")}
					plan={async () => [
						{
							label: "Delete request validator",
							run: () =>
								apiCall(scope, "DeleteRequestValidator", {
									RequestValidatorId: validator.Id,
								}),
						},
					]}
				>
					<Box>Detach this validator from its methods first.</Box>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}

function ModelForm({
	scope,
	initial,
	onClose,
	onSaved,
}: {
	scope: GatewayScope;
	initial?: GatewayModel;
	onClose: () => void;
	onSaved: () => void;
}) {
	const pretty = (text?: string) => {
		try {
			return JSON.stringify(JSON.parse(text ?? ""), null, 2);
		} catch {
			return text ?? "";
		}
	};
	const [name, setName] = useState(initial?.Name ?? ""),
		[contentType, setContentType] = useState(
			initial?.ContentType ?? "application/json",
		),
		[description, setDescription] = useState(initial?.Description ?? ""),
		[schema, setSchema] = useState(
			initial
				? pretty(initial.Schema)
				: JSON.stringify(
						{ $schema: draft4, title: "", type: "object", properties: {} },
						null,
						2,
					),
		);
	return (
		<ActionDialog
			title={initial ? `Edit model ${initial.Name}` : "Create model"}
			submit={initial ? "Save changes" : "Create model"}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const text = JSON.stringify(jsonObject(schema, "Model schema"));
				if (initial)
					return [
						{
							label: "Update model",
							run: () =>
								apiCall(scope, "UpdateModel", {
									ModelName: initial.Name,
									PatchOperations: scalarPatch({ description, schema: text }),
								}),
						},
					];
				if (!/^[a-zA-Z0-9]+$/.test(name))
					throw new Error("Model names are letters and digits only.");
				if (!contentType.trim()) throw new Error("Content type is required.");
				return [
					{
						label: "Create model",
						run: () =>
							apiCall(scope, "CreateModel", {
								Name: name,
								ContentType: contentType,
								Schema: text,
								...(description ? { Description: description } : {}),
							}),
					},
				];
			}}
		>
			<SpaceBetween size="l">
				<GatewayInput
					label="Model name"
					value={name}
					onChange={setName}
					disabled={!!initial}
				/>
				<GatewayInput
					label="Content type"
					value={contentType}
					onChange={setContentType}
					disabled={!!initial}
				/>
				<GatewayInput
					label="Description"
					value={description}
					onChange={setDescription}
				/>
				<GatewayText
					label="Schema (JSON Schema draft 4)"
					value={schema}
					onChange={setSchema}
					rows={14}
					description={`Refer to another model with {"$ref": "https://apigateway.amazonaws.com/restapis/${scope.apiId}/models/Name"}.`}
				/>
				<Alert type="info">
					Deploy the API for stages to validate against model changes.
				</Alert>
			</SpaceBetween>
		</ActionDialog>
	);
}

function ValidatorForm({
	scope,
	initial,
	onClose,
	onSaved,
}: {
	scope: GatewayScope;
	initial?: GatewayValidator;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [name, setName] = useState(initial?.Name ?? ""),
		[body, setBody] = useState(initial?.ValidateRequestBody ?? true),
		[parameters, setParameters] = useState(
			initial?.ValidateRequestParameters ?? true,
		);
	return (
		<ActionDialog
			title={initial ? "Edit request validator" : "Create request validator"}
			submit={initial ? "Save changes" : "Create validator"}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				if (!name.trim()) throw new Error("Validator name is required.");
				return [
					initial
						? {
								label: "Update request validator",
								run: () =>
									apiCall(scope, "UpdateRequestValidator", {
										RequestValidatorId: initial.Id,
										PatchOperations: scalarPatch({
											name,
											validateRequestBody: body,
											validateRequestParameters: parameters,
										}),
									}),
							}
						: {
								label: "Create request validator",
								run: () =>
									apiCall(scope, "CreateRequestValidator", {
										Name: name,
										ValidateRequestBody: body,
										ValidateRequestParameters: parameters,
									}),
							},
				];
			}}
		>
			<SpaceBetween size="l">
				<GatewayInput label="Validator name" value={name} onChange={setName} />
				<FormField label="Checks">
					<SpaceBetween size="xs">
						<Checkbox
							checked={body}
							onChange={({ detail }) => setBody(detail.checked)}
						>
							Validate the request body
						</Checkbox>
						<Checkbox
							checked={parameters}
							onChange={({ detail }) => setParameters(detail.checked)}
						>
							Validate required query string parameters and headers
						</Checkbox>
					</SpaceBetween>
				</FormField>
			</SpaceBetween>
		</ActionDialog>
	);
}
