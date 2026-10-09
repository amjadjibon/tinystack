import {
	Checkbox,
	FormField,
	SpaceBetween,
} from "@cloudscape-design/components";
import { useRef, useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import {
	apiCall,
	type GatewayAPI,
	type GatewayScope,
	gatewayCall,
	isHTTP,
	scalarPatch,
} from "#/lib/apigateway";
import { type ActionStep, jsonObject, stringMap } from "#/lib/consoleActions";
import { GatewayInput, GatewayText } from "./Fields";
export function ApiForm({
	scope,
	initial,
	onClose,
	onSaved,
}: {
	scope: GatewayScope;
	initial?: GatewayAPI;
	onClose: () => void;
	onSaved: (id: string) => void;
}) {
	const createdId = useRef("");
	const http = isHTTP(scope),
		[name, setName] = useState(initial?.Name ?? ""),
		[description, setDescription] = useState(initial?.Description ?? "");
	const [disabled, setDisabled] = useState(
			initial?.DisableExecuteApiEndpoint ?? false,
		),
		[cors, setCors] = useState(
			JSON.stringify(initial?.CorsConfiguration ?? {}, null, 2),
		),
		[tags, setTags] = useState("{}"),
		[auto, setAuto] = useState(true);
	return (
		<ActionDialog
			title={initial ? "Edit API" : `Create ${http ? "HTTP" : "REST"} API`}
			submit={initial ? "Save changes" : "Create API"}
			onClose={onClose}
			onSaved={() => onSaved(scope.apiId || createdId.current)}
			plan={async () => {
				if (!name.trim()) throw new Error("API name is required.");
				const config = http
					? jsonObject(cors, "CORS configuration")
					: undefined;
				const steps: ActionStep[] = [];
				if (initial)
					steps.push({
						label: "Update API",
						run: () =>
							apiCall(
								scope,
								http ? "UpdateApi" : "UpdateRestApi",
								http
									? {
											Name: name,
											Description: description,
											DisableExecuteApiEndpoint: disabled,
											...(Object.keys(config ?? {}).length
												? { CorsConfiguration: config }
												: {}),
										}
									: {
											PatchOperations: scalarPatch({
												name,
												description,
												disableExecuteApiEndpoint: disabled,
											}),
										},
							),
					});
				else {
					const next = stringMap(tags, "Tags");
					steps.push({
						label: "Create API",
						run: async () => {
							const out = await gatewayCall<GatewayAPI>(
								scope,
								http ? "CreateApi" : "CreateRestApi",
								{
									Name: name,
									Description: description,
									Tags: next,
									DisableExecuteApiEndpoint: disabled,
									...(http
										? {
												ProtocolType: "HTTP",
												...(Object.keys(config ?? {}).length
													? { CorsConfiguration: config }
													: {}),
											}
										: { EndpointConfiguration: { Types: ["REGIONAL"] } }),
								},
							);
							createdId.current = out.ApiId ?? out.Id ?? "";
						},
					});
					if (http && auto)
						steps.push({
							label: "Create default stage",
							run: () =>
								apiCall({ ...scope, apiId: createdId.current }, "CreateStage", {
									StageName: "$default",
									AutoDeploy: true,
								}),
						});
				}
				if (initial && http && !Object.keys(config ?? {}).length)
					steps.push({
						label: "Remove CORS configuration",
						run: () => apiCall(scope, "DeleteCorsConfiguration"),
					});
				return steps;
			}}
		>
			<SpaceBetween size="l">
				<GatewayInput label="API name" value={name} onChange={setName} />
				<GatewayInput
					label="Description"
					value={description}
					onChange={setDescription}
				/>
				<FormField label="Default endpoint">
					<Checkbox
						checked={disabled}
						onChange={({ detail }) => setDisabled(detail.checked)}
					>
						Disable execute-api endpoint
					</Checkbox>
				</FormField>
				{http && (
					<GatewayText
						label="CORS configuration (JSON)"
						value={cors}
						onChange={setCors}
						description="SDK field names: AllowOrigins, AllowMethods, AllowHeaders, ExposeHeaders, AllowCredentials, MaxAge. Use {} to disable CORS."
						rows={8}
					/>
				)}
				{!initial && http && (
					<Checkbox
						checked={auto}
						onChange={({ detail }) => setAuto(detail.checked)}
					>
						Create a $default stage with automatic deployments
					</Checkbox>
				)}
				{!initial && (
					<GatewayText
						label="API tags (JSON)"
						value={tags}
						onChange={setTags}
					/>
				)}
			</SpaceBetween>
		</ActionDialog>
	);
}
