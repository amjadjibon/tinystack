import {
	Alert,
	Badge,
	Box,
	Button,
	Checkbox,
	CopyToClipboard,
	FormField,
	Header,
	Input,
	Link,
	Multiselect,
	RadioGroup,
	SpaceBetween,
	Table,
} from "@cloudscape-design/components";
import { useCallback, useEffect, useRef, useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { errorText } from "#/lib/consoleActions";
import { useHealth } from "#/lib/health";
import { functionCall, type LambdaScope } from "#/lib/lambda";
import {
	ALLOWED_CORS_METHODS,
	buildPublicPermissionInputs,
	buildRemovePermissionInputs,
	checkPublicPermissions,
	configToFormState,
	corsSummary,
	defaultFormState,
	extractQualifier,
	type FunctionUrlConfig,
	type FunctionUrlFormState,
	formStateToCreatePayload,
	formStateToUpdatePayload,
	PUBLIC_INVOKE_STATEMENT_ID,
	PUBLIC_URL_STATEMENT_ID,
	validateForm,
} from "#/lib/lambdaUrl";

export function LambdaUrls({
	scope,
	enabled,
	loading: parentLoading,
	onRefresh: parentRefresh,
	onChanged,
}: {
	scope: LambdaScope;
	enabled: boolean;
	loading: boolean;
	onRefresh: () => void;
	onChanged: (message: string) => void;
}) {
	const health = useHealth();
	const authMode = health.data?.identity?.authorization;
	const isAuthEnabled = authMode === "soft" || authMode === "enforce";

	const [urls, setUrls] = useState<FunctionUrlConfig[]>([]);
	const [policies, setPolicies] = useState<
		Record<string, { policy: string | null; missingSids: string[] }>
	>({});
	const [loading, setLoading] = useState(false);

	const [dialog, setDialog] = useState<"create" | "edit" | "delete" | null>(
		null,
	);
	const [selected, setSelected] = useState<{
		config: FunctionUrlConfig;
		qualifier: string;
	} | null>(null);
	const [form, setForm] = useState<FunctionUrlFormState>(defaultFormState());
	const [infoAlert, setInfoAlert] = useState<{
		qualifier: string;
		title: string;
		message: string;
	} | null>(null);

	const addedPermissionsRef = useRef(false);

	const loadData = useCallback(async () => {
		if (!scope.functionName) return;
		setLoading(true);
		try {
			const res = await functionCall<{
				FunctionUrlConfigs?: FunctionUrlConfig[];
			}>(scope, "ListFunctionUrlConfigs");
			const items = res.FunctionUrlConfigs ?? [];
			setUrls(items);

			const policyMap: Record<
				string,
				{ policy: string | null; missingSids: string[] }
			> = {};
			for (const item of items) {
				const q = extractQualifier(item.FunctionArn);
				try {
					const polRes = await functionCall<{ Policy?: string }>(
						scope,
						"GetPolicy",
						q === "$LATEST" ? {} : { Qualifier: q },
					);
					const polStr = polRes.Policy ?? null;
					const checked = checkPublicPermissions(polStr);
					policyMap[q] = {
						policy: polStr,
						missingSids: checked.missingSids,
					};
				} catch (err) {
					if (errorText(err).includes("ResourceNotFoundException")) {
						policyMap[q] = {
							policy: null,
							missingSids: [
								PUBLIC_URL_STATEMENT_ID,
								PUBLIC_INVOKE_STATEMENT_ID,
							],
						};
					}
				}
			}
			setPolicies(policyMap);
		} catch (err) {
			console.error("Failed to load function URLs", err);
		} finally {
			setLoading(false);
		}
	}, [scope]);

	useEffect(() => {
		loadData();
	}, [loadData]);

	const handleRefresh = async () => {
		parentRefresh();
		await loadData();
	};

	const handleAddPublicPermissions = async (
		qualifier: string,
		missingSids: string[],
	) => {
		try {
			const inputs = buildPublicPermissionInputs(
				scope.functionName ?? "",
				qualifier,
				missingSids,
			);
			for (const input of inputs) {
				try {
					await functionCall(scope, "AddPermission", input);
				} catch (err) {
					if (!errorText(err).includes("ResourceConflictException")) {
						throw err;
					}
				}
			}
			await loadData();
			onChanged("Public permissions added.");
		} catch (err) {
			console.error("Failed to add permissions", err);
		}
	};

	const handleRemovePublicPermissions = async (qualifier: string) => {
		try {
			const inputs = buildRemovePermissionInputs(
				scope.functionName ?? "",
				qualifier,
			);
			for (const input of inputs) {
				try {
					await functionCall(scope, "RemovePermission", input);
				} catch (err) {
					if (!errorText(err).includes("ResourceNotFoundException")) {
						throw err;
					}
				}
			}
			setInfoAlert(null);
			await loadData();
			onChanged("Public permissions removed.");
		} catch (err) {
			console.error("Failed to remove permissions", err);
		}
	};

	const openCreate = () => {
		setSelected(null);
		setForm(defaultFormState());
		setDialog("create");
	};

	const openEdit = (config: FunctionUrlConfig) => {
		const q = extractQualifier(config.FunctionArn);
		setSelected({ config, qualifier: q });
		setForm(configToFormState(config, q));
		setDialog("edit");
	};

	const openDelete = (config: FunctionUrlConfig) => {
		const q = extractQualifier(config.FunctionArn);
		setSelected({ config, qualifier: q });
		setDialog("delete");
	};

	const warningUrls = urls
		.map((u) => {
			const q = extractQualifier(u.FunctionArn);
			const polInfo = policies[q];
			const missing = polInfo?.missingSids ?? [];
			return { config: u, qualifier: q, missingSids: missing };
		})
		.filter(
			(item) =>
				item.config.AuthType === "NONE" &&
				item.missingSids.length > 0 &&
				isAuthEnabled,
		);

	return (
		<SpaceBetween size="l">
			{warningUrls.map(({ qualifier, missingSids }) => (
				<Alert
					key={`warning-${qualifier}`}
					type="warning"
					header="This URL returns 403 while IAM authorization is enabled: the function policy does not allow public access"
					action={
						<Button
							disabled={!enabled}
							onClick={() => handleAddPublicPermissions(qualifier, missingSids)}
						>
							Add public permissions
						</Button>
					}
				>
					AWS requires a NONE URL's function policy to grant
					lambda:InvokeFunctionUrl and lambda:InvokeFunction to everyone;
					TinyStack enforces this while IAM authorization is enabled.
				</Alert>
			))}

			{infoAlert && (
				<Alert
					type="info"
					header={infoAlert.title}
					dismissible
					onDismiss={() => setInfoAlert(null)}
					action={
						<Button
							disabled={!enabled}
							onClick={() => handleRemovePublicPermissions(infoAlert.qualifier)}
						>
							Remove public permissions
						</Button>
					}
				>
					{infoAlert.message}
				</Alert>
			)}

			<Table
				items={urls}
				trackBy="FunctionArn"
				loading={loading || parentLoading}
				loadingText="Loading function URLs"
				wrapLines
				header={
					<Header
						variant="h2"
						counter={`(${urls.length})`}
						description="Public or IAM-authenticated HTTPS endpoints for calling this function directly."
						actions={
							<SpaceBetween direction="horizontal" size="xs">
								<Button
									iconName="refresh"
									ariaLabel="Refresh function URLs"
									onClick={handleRefresh}
								/>
								<Button
									variant="primary"
									disabled={!enabled}
									onClick={openCreate}
								>
									Create function URL
								</Button>
							</SpaceBetween>
						}
					>
						Function URLs
					</Header>
				}
				columnDefinitions={[
					{
						id: "url",
						header: "Function URL",
						cell: (item) => (
							<SpaceBetween direction="horizontal" size="xs">
								<Link href={item.FunctionUrl} external>
									{item.FunctionUrl}
								</Link>
								<CopyToClipboard
									textToCopy={item.FunctionUrl ?? ""}
									copyButtonAriaLabel="Copy function URL"
									copySuccessText="URL copied"
									copyErrorText="Failed to copy URL"
								/>
							</SpaceBetween>
						),
					},
					{
						id: "target",
						header: "Qualifier",
						cell: (item) => extractQualifier(item.FunctionArn),
					},
					{
						id: "authType",
						header: "Auth type",
						cell: (item) => (
							<Badge color={item.AuthType === "NONE" ? "green" : "blue"}>
								{item.AuthType ?? "NONE"}
							</Badge>
						),
					},
					{
						id: "invokeMode",
						header: "Invoke mode",
						cell: (item) => item.InvokeMode ?? "BUFFERED",
					},
					{
						id: "cors",
						header: "CORS",
						cell: (item) => corsSummary(item.Cors),
					},
					{
						id: "actions",
						header: "Actions",
						cell: (item) => (
							<SpaceBetween direction="horizontal" size="xs">
								<Button disabled={!enabled} onClick={() => openEdit(item)}>
									Edit
								</Button>
								<Button disabled={!enabled} onClick={() => openDelete(item)}>
									Delete
								</Button>
							</SpaceBetween>
						),
					},
				]}
				empty={
					<Box padding="l" textAlign="center">
						No function URLs. Create a function URL to assign a dedicated
						HTTP(S) endpoint to this function or alias.
					</Box>
				}
			/>

			{dialog === "create" && (
				<ActionDialog
					title="Create function URL"
					submit="Create"
					onClose={() => setDialog(null)}
					onSaved={async () => {
						setDialog(null);
						await loadData();
						if (addedPermissionsRef.current) {
							onChanged("Function URL created and public permissions granted.");
						} else {
							onChanged("Function URL created.");
						}
					}}
					plan={async () => {
						const err = validateForm(form, false);
						if (err) throw new Error(err);
						const qualifier =
							form.qualifierType === "alias"
								? form.aliasName.trim()
								: "$LATEST";
						const payload = formStateToCreatePayload(
							form,
							scope.functionName ?? "",
						);
						addedPermissionsRef.current = false;

						return [
							{
								label: "Create function URL config",
								run: async () => {
									await functionCall(scope, "CreateFunctionUrlConfig", payload);
								},
							},
							...(form.authType === "NONE"
								? [
										{
											label: "Add public policy permissions",
											run: async () => {
												let polStr: string | null = null;
												try {
													const polRes = await functionCall<{
														Policy?: string;
													}>(
														scope,
														"GetPolicy",
														qualifier === "$LATEST"
															? {}
															: {
																	Qualifier: qualifier,
																},
													);
													polStr = polRes.Policy ?? null;
												} catch (e) {
													if (
														!errorText(e).includes("ResourceNotFoundException")
													) {
														throw e;
													}
												}
												const checked = checkPublicPermissions(polStr);
												if (checked.missingSids.length > 0) {
													const inputs = buildPublicPermissionInputs(
														scope.functionName ?? "",
														qualifier,
														checked.missingSids,
													);
													for (const input of inputs) {
														try {
															await functionCall(scope, "AddPermission", input);
														} catch (addErr) {
															if (
																!errorText(addErr).includes(
																	"ResourceConflictException",
																)
															) {
																throw addErr;
															}
														}
													}
													addedPermissionsRef.current = true;
												}
											},
										},
									]
								: []),
						];
					}}
				>
					<SpaceBetween size="m">
						<FormField
							label="Target qualifier"
							description="Choose $LATEST or specify an alias name."
						>
							<RadioGroup
								value={form.qualifierType}
								onChange={({ detail }) =>
									setForm({
										...form,
										qualifierType: detail.value as "latest" | "alias",
									})
								}
								items={[
									{ value: "latest", label: "$LATEST" },
									{ value: "alias", label: "Alias" },
								]}
							/>
							{form.qualifierType === "alias" && (
								<Box margin={{ top: "xs" }}>
									<Input
										value={form.aliasName}
										onChange={({ detail }) =>
											setForm({
												...form,
												aliasName: detail.value,
											})
										}
										placeholder="Alias name (e.g. prod)"
										ariaLabel="Alias name"
									/>
								</Box>
							)}
						</FormField>

						<FormField
							label="Auth type"
							description="NONE allows public unauthenticated requests. AWS_IAM requires SigV4-signed requests."
						>
							<RadioGroup
								value={form.authType}
								onChange={({ detail }) =>
									setForm({
										...form,
										authType: detail.value as "NONE" | "AWS_IAM",
									})
								}
								items={[
									{
										value: "NONE",
										label: "NONE",
										description: "Public access without IAM credentials.",
									},
									{
										value: "AWS_IAM",
										label: "AWS_IAM",
										description:
											"Requires IAM credentials with lambda:InvokeFunctionUrl and lambda:InvokeFunction permissions.",
									},
								]}
							/>
						</FormField>

						<FormField
							label="Invoke mode"
							description="BUFFERED waits for response completion. RESPONSE_STREAM streams responses as they are produced."
						>
							<RadioGroup
								value={form.invokeMode}
								onChange={({ detail }) =>
									setForm({
										...form,
										invokeMode: detail.value as "BUFFERED" | "RESPONSE_STREAM",
									})
								}
								items={[
									{
										value: "BUFFERED",
										label: "BUFFERED",
										description: "Buffer function responses until completion.",
									},
									{
										value: "RESPONSE_STREAM",
										label: "RESPONSE_STREAM",
										description: "Stream responses back to the client.",
									},
								]}
							/>
						</FormField>

						<FormField>
							<Checkbox
								checked={form.enableCors}
								onChange={({ detail }) =>
									setForm({
										...form,
										enableCors: detail.checked,
									})
								}
							>
								Configure Cross-Origin Resource Sharing (CORS)
							</Checkbox>
						</FormField>

						{form.enableCors && (
							<SpaceBetween size="m">
								<FormField
									label="Allow origins"
									description="Origins allowed to access the URL. Enter * or comma-separated origins (e.g. https://example.com)."
								>
									<Input
										value={form.allowOrigins}
										onChange={({ detail }) =>
											setForm({
												...form,
												allowOrigins: detail.value,
											})
										}
										placeholder="*"
										ariaLabel="Allow origins"
									/>
								</FormField>

								<FormField
									label="Allow methods"
									description="HTTP methods allowed for cross-origin requests."
								>
									<Multiselect
										selectedOptions={form.allowMethods.map((m) => ({
											label: m,
											value: m,
										}))}
										options={ALLOWED_CORS_METHODS.map((m) => ({
											label: m,
											value: m,
										}))}
										onChange={({ detail }) =>
											setForm({
												...form,
												allowMethods: detail.selectedOptions.map(
													(o) => o.value ?? "",
												),
											})
										}
										placeholder="Select allowed HTTP methods"
										ariaLabel="Allow methods"
									/>
								</FormField>

								<FormField
									label="Allow headers"
									description="Headers allowed in requests. Enter * or comma-separated headers (e.g. content-type, authorization)."
								>
									<Input
										value={form.allowHeaders}
										onChange={({ detail }) =>
											setForm({
												...form,
												allowHeaders: detail.value,
											})
										}
										placeholder="*"
										ariaLabel="Allow headers"
									/>
								</FormField>

								<FormField>
									<Checkbox
										checked={form.allowCredentials}
										onChange={({ detail }) =>
											setForm({
												...form,
												allowCredentials: detail.checked,
											})
										}
									>
										Allow credentials (cookies, authorization headers)
									</Checkbox>
								</FormField>

								<FormField
									label="Max age (seconds)"
									description="Cache duration for preflight OPTIONS requests in the browser (0 to 86400)."
								>
									<Input
										type="number"
										value={form.maxAge}
										onChange={({ detail }) =>
											setForm({
												...form,
												maxAge: detail.value,
											})
										}
										ariaLabel="Max age"
									/>
								</FormField>

								<FormField
									label="Expose headers — optional"
									description="Response headers browsers can expose. Comma-separated list."
								>
									<Input
										value={form.exposeHeaders}
										onChange={({ detail }) =>
											setForm({
												...form,
												exposeHeaders: detail.value,
											})
										}
										placeholder="x-custom-header, x-trace-id"
										ariaLabel="Expose headers"
									/>
								</FormField>
							</SpaceBetween>
						)}
					</SpaceBetween>
				</ActionDialog>
			)}

			{dialog === "edit" && selected && (
				<ActionDialog
					title={`Edit function URL (${selected.qualifier})`}
					submit="Save changes"
					onClose={() => setDialog(null)}
					onSaved={async () => {
						const prevAuth = selected.config.AuthType;
						const newAuth = form.authType;
						const q = selected.qualifier;
						setDialog(null);
						await loadData();
						if (addedPermissionsRef.current) {
							onChanged("Function URL updated and public permissions granted.");
						} else {
							onChanged("Function URL updated.");
						}
						if (prevAuth === "NONE" && newAuth === "AWS_IAM") {
							let polStr: string | null = null;
							try {
								const polRes = await functionCall<{
									Policy?: string;
								}>(scope, "GetPolicy", q === "$LATEST" ? {} : { Qualifier: q });
								polStr = polRes.Policy ?? null;
							} catch {
								// ignore
							}
							const checked = checkPublicPermissions(polStr);
							if (checked.hasUrlAccess || checked.hasInvokeAccess) {
								setInfoAlert({
									qualifier: q,
									title: "Public permissions remain on function policy",
									message: `Function URL auth type was changed to AWS_IAM for ${q}, but public permissions remain on the function resource policy. AWS does not remove these automatically.`,
								});
							}
						}
					}}
					plan={async () => {
						const err = validateForm(form, true);
						if (err) throw new Error(err);
						const qualifier = selected.qualifier;
						const payload = formStateToUpdatePayload(
							form,
							scope.functionName ?? "",
							qualifier,
						);
						const changedToNone =
							selected.config.AuthType !== "NONE" && form.authType === "NONE";
						addedPermissionsRef.current = false;

						return [
							{
								label: "Update function URL config",
								run: async () => {
									await functionCall(scope, "UpdateFunctionUrlConfig", payload);
								},
							},
							...(changedToNone
								? [
										{
											label: "Add public policy permissions",
											run: async () => {
												let polStr: string | null = null;
												try {
													const polRes = await functionCall<{
														Policy?: string;
													}>(
														scope,
														"GetPolicy",
														qualifier === "$LATEST"
															? {}
															: {
																	Qualifier: qualifier,
																},
													);
													polStr = polRes.Policy ?? null;
												} catch (e) {
													if (
														!errorText(e).includes("ResourceNotFoundException")
													) {
														throw e;
													}
												}
												const checked = checkPublicPermissions(polStr);
												if (checked.missingSids.length > 0) {
													const inputs = buildPublicPermissionInputs(
														scope.functionName ?? "",
														qualifier,
														checked.missingSids,
													);
													for (const input of inputs) {
														try {
															await functionCall(scope, "AddPermission", input);
														} catch (addErr) {
															if (
																!errorText(addErr).includes(
																	"ResourceConflictException",
																)
															) {
																throw addErr;
															}
														}
													}
													addedPermissionsRef.current = true;
												}
											},
										},
									]
								: []),
						];
					}}
				>
					<SpaceBetween size="m">
						<FormField
							label="Target"
							description="The qualifier this URL is attached to."
						>
							<Box variant="strong">{selected.qualifier}</Box>
						</FormField>

						<FormField
							label="Auth type"
							description="NONE allows public unauthenticated requests. AWS_IAM requires SigV4-signed requests."
						>
							<RadioGroup
								value={form.authType}
								onChange={({ detail }) =>
									setForm({
										...form,
										authType: detail.value as "NONE" | "AWS_IAM",
									})
								}
								items={[
									{
										value: "NONE",
										label: "NONE",
										description: "Public access without IAM credentials.",
									},
									{
										value: "AWS_IAM",
										label: "AWS_IAM",
										description:
											"Requires IAM credentials with lambda:InvokeFunctionUrl and lambda:InvokeFunction permissions.",
									},
								]}
							/>
						</FormField>

						<FormField
							label="Invoke mode"
							description="BUFFERED waits for response completion. RESPONSE_STREAM streams responses as they are produced."
						>
							<RadioGroup
								value={form.invokeMode}
								onChange={({ detail }) =>
									setForm({
										...form,
										invokeMode: detail.value as "BUFFERED" | "RESPONSE_STREAM",
									})
								}
								items={[
									{
										value: "BUFFERED",
										label: "BUFFERED",
										description: "Buffer function responses until completion.",
									},
									{
										value: "RESPONSE_STREAM",
										label: "RESPONSE_STREAM",
										description: "Stream responses back to the client.",
									},
								]}
							/>
						</FormField>

						<FormField>
							<Checkbox
								checked={form.enableCors}
								onChange={({ detail }) =>
									setForm({
										...form,
										enableCors: detail.checked,
									})
								}
							>
								Configure Cross-Origin Resource Sharing (CORS)
							</Checkbox>
						</FormField>

						{form.enableCors && (
							<SpaceBetween size="m">
								<FormField
									label="Allow origins"
									description="Origins allowed to access the URL. Enter * or comma-separated origins (e.g. https://example.com)."
								>
									<Input
										value={form.allowOrigins}
										onChange={({ detail }) =>
											setForm({
												...form,
												allowOrigins: detail.value,
											})
										}
										placeholder="*"
										ariaLabel="Allow origins"
									/>
								</FormField>

								<FormField
									label="Allow methods"
									description="HTTP methods allowed for cross-origin requests."
								>
									<Multiselect
										selectedOptions={form.allowMethods.map((m) => ({
											label: m,
											value: m,
										}))}
										options={ALLOWED_CORS_METHODS.map((m) => ({
											label: m,
											value: m,
										}))}
										onChange={({ detail }) =>
											setForm({
												...form,
												allowMethods: detail.selectedOptions.map(
													(o) => o.value ?? "",
												),
											})
										}
										placeholder="Select allowed HTTP methods"
										ariaLabel="Allow methods"
									/>
								</FormField>

								<FormField
									label="Allow headers"
									description="Headers allowed in requests. Enter * or comma-separated headers (e.g. content-type, authorization)."
								>
									<Input
										value={form.allowHeaders}
										onChange={({ detail }) =>
											setForm({
												...form,
												allowHeaders: detail.value,
											})
										}
										placeholder="*"
										ariaLabel="Allow headers"
									/>
								</FormField>

								<FormField>
									<Checkbox
										checked={form.allowCredentials}
										onChange={({ detail }) =>
											setForm({
												...form,
												allowCredentials: detail.checked,
											})
										}
									>
										Allow credentials (cookies, authorization headers)
									</Checkbox>
								</FormField>

								<FormField
									label="Max age (seconds)"
									description="Cache duration for preflight OPTIONS requests in the browser (0 to 86400)."
								>
									<Input
										type="number"
										value={form.maxAge}
										onChange={({ detail }) =>
											setForm({
												...form,
												maxAge: detail.value,
											})
										}
										ariaLabel="Max age"
									/>
								</FormField>

								<FormField
									label="Expose headers — optional"
									description="Response headers browsers can expose. Comma-separated list."
								>
									<Input
										value={form.exposeHeaders}
										onChange={({ detail }) =>
											setForm({
												...form,
												exposeHeaders: detail.value,
											})
										}
										placeholder="x-custom-header, x-trace-id"
										ariaLabel="Expose headers"
									/>
								</FormField>
							</SpaceBetween>
						)}
					</SpaceBetween>
				</ActionDialog>
			)}

			{dialog === "delete" && selected && (
				<ActionDialog
					title={`Delete function URL (${selected.qualifier})`}
					submit="Delete"
					confirmation="delete"
					onClose={() => setDialog(null)}
					onSaved={async () => {
						const wasNone = selected.config.AuthType === "NONE";
						const q = selected.qualifier;
						setDialog(null);
						await loadData();
						onChanged("Function URL deleted.");
						if (wasNone) {
							let polStr: string | null = null;
							try {
								const polRes = await functionCall<{
									Policy?: string;
								}>(scope, "GetPolicy", q === "$LATEST" ? {} : { Qualifier: q });
								polStr = polRes.Policy ?? null;
							} catch {
								// ignore
							}
							const checked = checkPublicPermissions(polStr);
							if (checked.hasUrlAccess || checked.hasInvokeAccess) {
								setInfoAlert({
									qualifier: q,
									title: "Public permissions remain on function policy",
									message: `The function URL for ${q} was deleted, but public permissions remain on the function resource policy. AWS does not remove these automatically.`,
								});
							}
						}
					}}
					plan={async () => {
						const q = selected.qualifier;
						return [
							{
								label: "Delete function URL config",
								run: async () => {
									await functionCall(
										scope,
										"DeleteFunctionUrlConfig",
										q === "$LATEST" ? {} : { Qualifier: q },
									);
								},
							},
						];
					}}
				>
					<Box variant="p">
						Are you sure you want to delete the function URL for{" "}
						<strong>{selected.qualifier}</strong>? This action cannot be undone.
						Requests to this URL will immediately fail.
					</Box>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
