import { useCollection } from "@cloudscape-design/collection-hooks";
import {
	Alert,
	Badge,
	Box,
	Button,
	Container,
	ExpandableSection,
	FormField,
	Header,
	Input,
	Pagination,
	SegmentedControl,
	Select,
	SpaceBetween,
	StatusIndicator,
	Table,
	Textarea,
	TextFilter,
} from "@cloudscape-design/components";
import { useId, useMemo, useState } from "react";
import type { Resource } from "#/lib/api";
import { type IAMKind, iamLabels, iamPages } from "#/lib/iam";
import {
	buildContextEntries,
	CONTEXT_KEY_TYPES,
	type ContextEntryRow,
	type ContextKeyType,
	defaultPolicyDocument,
	formatPolicyJson,
	isIamPrincipalArn,
	parseActions,
	parseResources,
	type SimulationEvaluationResult,
	validatePolicyJson,
} from "#/lib/simulator";
import { IAMSelection } from "./IAMSelection";

export function SimulationDecisionStatus({ decision }: { decision?: string }) {
	switch (decision) {
		case "allowed":
			return <StatusIndicator type="success">Allowed</StatusIndicator>;
		case "explicitDeny":
			return <StatusIndicator type="error">Explicit deny</StatusIndicator>;
		case "implicitDeny":
			return <StatusIndicator type="error">Implicit deny</StatusIndicator>;
		default:
			return (
				<StatusIndicator type="not-started">
					{decision ?? "Not evaluated"}
				</StatusIndicator>
			);
	}
}

export function IAMSimulator({
	resources,
	account,
	initialPrincipal = "",
	initialAction = "",
	initialResource = "",
}: {
	resources: Resource[];
	account: string;
	initialPrincipal?: string;
	initialAction?: string;
	initialResource?: string;
}) {
	const defaultId = useId();
	const [mode, setMode] = useState<"principal" | "custom">("principal");
	const [principalArn, setPrincipalArn] = useState(initialPrincipal);
	const [customPolicies, setCustomPolicies] = useState<string[]>([
		defaultPolicyDocument,
	]);
	const [actionsText, setActionsText] = useState(initialAction);
	const [resourcesText, setResourcesText] = useState(initialResource || "*");
	const [boundaryJson, setBoundaryJson] = useState("");
	const [resourcePolicyJson, setResourcePolicyJson] = useState("");
	const [callerArn, setCallerArn] = useState("");
	const [resourceOwner, setResourceOwner] = useState("");
	const [contextRows, setContextRows] = useState<ContextEntryRow[]>([]);

	const [actionError, setActionError] = useState("");
	const [policyErrors, setPolicyErrors] = useState<Record<number, string>>({});
	const [boundaryError, setBoundaryError] = useState("");
	const [resourcePolicyError, setResourcePolicyError] = useState("");
	const [contextError, setContextError] = useState("");

	const [simulating, setSimulating] = useState(false);
	const [error, setError] = useState<string | null>(null);
	const [results, setResults] = useState<SimulationEvaluationResult[] | null>(
		null,
	);

	const principalOptions = useMemo(() => {
		const list = resources
			.filter(
				(r) =>
					r.service === "iam" &&
					(r.account === account || !r.account) &&
					(r.type === "user" || r.type === "role" || r.type === "group"),
			)
			.map((r) => ({
				label: r.name,
				value: r.arn,
				description: `${iamLabels[r.type as IAMKind]?.singular ?? r.type} • ${r.arn}`,
			}));

		if (principalArn && !list.some((o) => o.value === principalArn)) {
			const kind = principalArn.includes(":user/")
				? "User"
				: principalArn.includes(":role/")
					? "Role"
					: principalArn.includes(":group/")
						? "User group"
						: "Principal";
			list.unshift({
				label: principalArn.split("/").pop() || principalArn,
				value: principalArn,
				description: `${kind} • ${principalArn}`,
			});
		}
		return list;
	}, [resources, account, principalArn]);

	const addContextRow = () => {
		setContextRows((prev) => [
			...prev,
			{
				id: `${defaultId}-${Date.now()}-${prev.length}`,
				key: "",
				type: "string",
				value: "",
			},
		]);
	};

	const removeContextRow = (id: string) => {
		setContextRows((prev) => prev.filter((r) => r.id !== id));
	};

	const updateContextRow = (
		id: string,
		field: "key" | "type" | "value",
		val: string,
	) => {
		setContextRows((prev) =>
			prev.map((r) =>
				r.id === id ? { ...r, [field]: val as ContextKeyType } : r,
			),
		);
	};

	const runSimulation = async () => {
		setError(null);
		setActionError("");
		setPolicyErrors({});
		setBoundaryError("");
		setResourcePolicyError("");
		setContextError("");

		let hasError = false;

		if (mode === "principal") {
			if (!principalArn) {
				setError("Please select a principal (user, role, or group).");
				return;
			}
			if (!isIamPrincipalArn(principalArn)) {
				setError(
					`Principal ARN "${principalArn}" is not a valid IAM user, role, or group ARN.`,
				);
				return;
			}
		} else {
			if (customPolicies.length === 0) {
				setError("At least one custom policy document is required.");
				return;
			}
			const errors: Record<number, string> = {};
			for (let i = 0; i < customPolicies.length; i++) {
				const err = validatePolicyJson(
					customPolicies[i],
					`Policy document #${i + 1}`,
				);
				if (err) {
					errors[i] = err;
					hasError = true;
				}
			}
			if (hasError) {
				setPolicyErrors(errors);
				return;
			}
		}

		const parsedActions = parseActions(actionsText);
		if (parsedActions.error) {
			setActionError(parsedActions.error);
			return;
		}

		const parsedResources = parseResources(resourcesText);

		if (boundaryJson.trim()) {
			const bErr = validatePolicyJson(
				boundaryJson,
				"Permissions boundary policy",
			);
			if (bErr) {
				setBoundaryError(bErr);
				return;
			}
		}

		if (resourcePolicyJson.trim()) {
			const rpErr = validatePolicyJson(
				resourcePolicyJson,
				"Resource policy document",
			);
			if (rpErr) {
				setResourcePolicyError(rpErr);
				return;
			}
			if (!callerArn.trim()) {
				setResourcePolicyError(
					"Caller ARN is required when simulating a resource policy.",
				);
				return;
			}
		}

		const contextResult = buildContextEntries(contextRows);
		if (contextResult.error) {
			setContextError(contextResult.error);
			return;
		}

		setSimulating(true);
		setResults(null);

		try {
			const payload: Record<string, unknown> = {
				ActionNames: parsedActions.actions,
				ResourceArns: parsedResources,
			};

			if (boundaryJson.trim()) {
				payload.PermissionsBoundaryPolicyInputList = [boundaryJson.trim()];
			}
			if (resourcePolicyJson.trim()) {
				payload.ResourcePolicy = resourcePolicyJson.trim();
				payload.CallerArn = callerArn.trim();
				if (resourceOwner.trim()) {
					payload.ResourceOwner = resourceOwner.trim();
				}
			}
			if (contextResult.entries.length > 0) {
				payload.ContextEntries = contextResult.entries;
			}

			let response: SimulationEvaluationResult[];
			if (mode === "principal") {
				payload.PolicySourceArn = principalArn;
				response = await iamPages<SimulationEvaluationResult>(
					account,
					"SimulatePrincipalPolicy",
					payload,
					"EvaluationResults",
				);
			} else {
				payload.PolicyInputList = customPolicies;
				response = await iamPages<SimulationEvaluationResult>(
					account,
					"SimulateCustomPolicy",
					payload,
					"EvaluationResults",
				);
			}
			setResults(response);
		} catch (e) {
			setError(e instanceof Error ? e.message : String(e));
		} finally {
			setSimulating(false);
		}
	};

	const {
		items: resultItems,
		collectionProps: resultCollectionProps,
		filterProps: resultFilterProps,
		paginationProps: resultPaginationProps,
	} = useCollection(results ?? [], {
		filtering: {
			empty: <Box>No simulation results available.</Box>,
			noMatch: <Box>No matching evaluation results.</Box>,
		},
		pagination: { pageSize: 10 },
		sorting: {},
	});

	return (
		<SpaceBetween size="l">
			<Header
				variant="h1"
				description="Test and debug IAM policies, permissions boundaries, resource policies, and request conditions."
				actions={
					<Button
						variant="primary"
						loading={simulating}
						onClick={runSimulation}
					>
						Run simulation
					</Button>
				}
			>
				Policy simulator
			</Header>

			{!!error && (
				<Alert
					type="error"
					header="Simulation failed"
					dismissible
					onDismiss={() => setError(null)}
				>
					{error}
				</Alert>
			)}

			<Container
				header={
					<Header
						variant="h2"
						description="Choose whether to evaluate stored principal permissions or custom policy documents."
					>
						Mode and policies
					</Header>
				}
			>
				<SpaceBetween size="m">
					<FormField label="Simulation mode">
						<SegmentedControl
							selectedId={mode}
							onChange={({ detail }) =>
								setMode(detail.selectedId as "principal" | "custom")
							}
							options={[
								{ id: "principal", text: "Principal mode" },
								{ id: "custom", text: "Custom mode" },
							]}
						/>
					</FormField>

					{mode === "principal" ? (
						<SpaceBetween size="s">
							<IAMSelection
								title="Select principal"
								single
								options={principalOptions}
								selected={principalArn ? [principalArn] : []}
								onChange={(selected) => setPrincipalArn(selected[0] ?? "")}
							/>
							{!!principalArn && (
								<Box variant="p" color="text-body-secondary">
									Simulating: <strong>{principalArn}</strong>
								</Box>
							)}
						</SpaceBetween>
					) : (
						<SpaceBetween size="m">
							{customPolicies.map((doc, idx) => (
								<Container
									// biome-ignore lint/suspicious/noArrayIndexKey: List of editable policy documents
									key={`policy-${idx}`}
									header={
										<Header
											variant="h3"
											actions={
												<SpaceBetween direction="horizontal" size="xs">
													<Button
														onClick={() =>
															setCustomPolicies((prev) =>
																prev.map((p, i) =>
																	i === idx ? formatPolicyJson(p) : p,
																),
															)
														}
													>
														Format JSON
													</Button>
													{customPolicies.length > 1 && (
														<Button
															onClick={() =>
																setCustomPolicies((prev) =>
																	prev.filter((_, i) => i !== idx),
																)
															}
														>
															Remove
														</Button>
													)}
												</SpaceBetween>
											}
										>
											Policy document {idx + 1}
										</Header>
									}
								>
									<FormField
										label="Policy document (JSON)"
										errorText={policyErrors[idx]}
									>
										<Textarea
											value={doc}
											rows={8}
											spellcheck={false}
											onChange={({ detail }) => {
												setCustomPolicies((prev) =>
													prev.map((p, i) => (i === idx ? detail.value : p)),
												);
												setPolicyErrors((prev) => {
													const next = { ...prev };
													delete next[idx];
													return next;
												});
											}}
										/>
									</FormField>
								</Container>
							))}
							<Button
								onClick={() =>
									setCustomPolicies((prev) => [...prev, defaultPolicyDocument])
								}
							>
								Add policy document
							</Button>
						</SpaceBetween>
					)}
				</SpaceBetween>
			</Container>

			<Container
				header={
					<Header
						variant="h2"
						description="Specify the actions and resources to test."
					>
						Actions and resources
					</Header>
				}
			>
				<SpaceBetween size="m">
					<FormField
						label="Actions"
						description="Action names in service:action format (e.g. s3:GetObject, dynamodb:PutItem). Separate multiple actions with newlines or commas. Wildcards are not supported."
						errorText={actionError}
					>
						<Textarea
							value={actionsText}
							placeholder="s3:GetObject"
							rows={3}
							spellcheck={false}
							onChange={({ detail }) => {
								setActionsText(detail.value);
								setActionError("");
							}}
						/>
					</FormField>

					<FormField
						label="Resource ARNs"
						description="Resource ARNs to test against. Separate multiple ARNs with newlines or commas. Defaults to * if left empty."
					>
						<Textarea
							value={resourcesText}
							placeholder="*"
							rows={2}
							spellcheck={false}
							onChange={({ detail }) => setResourcesText(detail.value)}
						/>
					</FormField>
				</SpaceBetween>
			</Container>

			<Container
				header={
					<Header
						variant="h2"
						description="Optional permissions boundary, resource policy, and request conditions."
					>
						Advanced configuration
					</Header>
				}
			>
				<SpaceBetween size="m">
					<ExpandableSection headerText="Permissions boundary (optional)">
						<SpaceBetween size="s">
							<FormField
								label="Permissions boundary policy JSON"
								description="Evaluate how an explicit permissions boundary intersects identity grants."
								errorText={boundaryError}
							>
								<Textarea
									value={boundaryJson}
									rows={6}
									spellcheck={false}
									placeholder='{\n  "Version": "2012-10-17",\n  "Statement": [...]\n}'
									onChange={({ detail }) => {
										setBoundaryJson(detail.value);
										setBoundaryError("");
									}}
								/>
							</FormField>
							{!!boundaryJson.trim() && (
								<Button
									onClick={() =>
										setBoundaryJson(formatPolicyJson(boundaryJson))
									}
								>
									Format JSON
								</Button>
							)}
						</SpaceBetween>
					</ExpandableSection>

					<ExpandableSection headerText="Resource policy & caller (optional)">
						<SpaceBetween size="m">
							<FormField
								label="Resource policy JSON"
								description="Evaluate against a resource policy (e.g. S3 bucket, SQS queue, SNS topic, Lambda function, Secrets Manager secret, EventBridge bus)."
								errorText={resourcePolicyError}
							>
								<Textarea
									value={resourcePolicyJson}
									rows={6}
									spellcheck={false}
									placeholder='{\n  "Version": "2012-10-17",\n  "Statement": [...]\n}'
									onChange={({ detail }) => {
										setResourcePolicyJson(detail.value);
										setResourcePolicyError("");
									}}
								/>
							</FormField>
							{!!resourcePolicyJson.trim() && (
								<Button
									onClick={() =>
										setResourcePolicyJson(formatPolicyJson(resourcePolicyJson))
									}
								>
									Format JSON
								</Button>
							)}

							<FormField
								label="Caller ARN"
								description="Required when ResourcePolicy is provided. Must be an IAM user in the same account."
							>
								<Input
									value={callerArn}
									placeholder={`arn:aws:iam::${account}:user/alice`}
									onChange={({ detail }) => {
										setCallerArn(detail.value);
										setResourcePolicyError("");
									}}
								/>
							</FormField>

							<FormField
								label="Resource owner account ID (optional)"
								description="Account owning the resource. Defaults to caller account if omitted."
							>
								<Input
									value={resourceOwner}
									placeholder={account}
									onChange={({ detail }) => setResourceOwner(detail.value)}
								/>
							</FormField>
						</SpaceBetween>
					</ExpandableSection>

					<ExpandableSection headerText="Context keys (optional)">
						<SpaceBetween size="m">
							<Box variant="p" color="text-body-secondary">
								Supply condition keys such as aws:CurrentTime,
								aws:PrincipalTag/department, or s3:prefix evaluated in policy
								Condition blocks.
							</Box>
							{!!contextError && (
								<Alert type="error" header="Invalid context key configuration">
									{contextError}
								</Alert>
							)}
							{contextRows.map((row) => (
								<SpaceBetween
									key={row.id}
									direction="horizontal"
									size="s"
									alignItems="center"
								>
									<FormField label="Context key">
										<Input
											value={row.key}
											placeholder="aws:PrincipalTag/team"
											onChange={({ detail }) =>
												updateContextRow(row.id, "key", detail.value)
											}
										/>
									</FormField>
									<FormField label="Type">
										<Select
											selectedOption={{ label: row.type, value: row.type }}
											options={CONTEXT_KEY_TYPES.map((t) => ({
												label: t,
												value: t,
											}))}
											onChange={({ detail }) =>
												updateContextRow(
													row.id,
													"type",
													detail.selectedOption.value as ContextKeyType,
												)
											}
										/>
									</FormField>
									<FormField
										label="Value(s)"
										description={
											row.type.endsWith("List")
												? "Comma-separated values"
												: "Single value"
										}
									>
										<Input
											value={row.value}
											placeholder={
												row.type.endsWith("List") ? "val1, val2" : "value"
											}
											onChange={({ detail }) =>
												updateContextRow(row.id, "value", detail.value)
											}
										/>
									</FormField>
									<Button
										variant="icon"
										iconName="remove"
										ariaLabel={`Remove context key ${row.key || "entry"}`}
										onClick={() => removeContextRow(row.id)}
									/>
								</SpaceBetween>
							))}
							<Button onClick={addContextRow}>Add context entry</Button>
						</SpaceBetween>
					</ExpandableSection>
				</SpaceBetween>
			</Container>

			{results && (
				<Table
					{...resultCollectionProps}
					items={resultItems}
					trackBy={(item) => `${item.EvalActionName}/${item.EvalResourceName}`}
					header={
						<Header
							variant="h2"
							counter={`(${results.length})`}
							description="Policy evaluation results for tested action and resource pairs."
						>
							Evaluation results
						</Header>
					}
					filter={
						<TextFilter
							{...resultFilterProps}
							filteringPlaceholder="Find evaluation results"
							filteringAriaLabel="Filter simulation results"
						/>
					}
					pagination={<Pagination {...resultPaginationProps} />}
					columnDefinitions={[
						{
							id: "action",
							header: "Action",
							cell: (item) => <strong>{item.EvalActionName}</strong>,
							sortingField: "EvalActionName",
							isRowHeader: true,
						},
						{
							id: "resource",
							header: "Resource",
							cell: (item) => item.EvalResourceName ?? "*",
							sortingField: "EvalResourceName",
						},
						{
							id: "decision",
							header: "Decision",
							cell: (item) => (
								<SimulationDecisionStatus decision={item.EvalDecision} />
							),
							sortingField: "EvalDecision",
						},
						{
							id: "matched",
							header: "Matched statements",
							cell: (item) =>
								item.MatchedStatements && item.MatchedStatements.length > 0 ? (
									<SpaceBetween direction="horizontal" size="xxs">
										{item.MatchedStatements.map((stmt, i) => (
											// biome-ignore lint/suspicious/noArrayIndexKey: Statement items have no unique identifier
											<Badge key={i} color="blue">
												{stmt.SourcePolicyId || "Policy"} (
												{stmt.SourcePolicyType || "identity"})
											</Badge>
										))}
									</SpaceBetween>
								) : (
									<Box color="text-status-inactive">—</Box>
								),
						},
						{
							id: "missing",
							header: "Missing context values",
							cell: (item) =>
								item.MissingContextValues &&
								item.MissingContextValues.length > 0 ? (
									<SpaceBetween direction="horizontal" size="xxs">
										{item.MissingContextValues.map((k) => (
											<Badge key={k} color="red">
												{k}
											</Badge>
										))}
									</SpaceBetween>
								) : (
									<Box color="text-status-inactive">—</Box>
								),
						},
						{
							id: "boundary",
							header: "Boundary allowed",
							cell: (item) =>
								item.PermissionsBoundaryDecisionDetail
									?.AllowedByPermissionsBoundary !== undefined ? (
									item.PermissionsBoundaryDecisionDetail
										.AllowedByPermissionsBoundary ? (
										<StatusIndicator type="success">Yes</StatusIndicator>
									) : (
										<StatusIndicator type="error">No</StatusIndicator>
									)
								) : (
									<Box color="text-status-inactive">—</Box>
								),
						},
					]}
				/>
			)}
		</SpaceBetween>
	);
}
