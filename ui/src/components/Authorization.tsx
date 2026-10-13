import {
	Alert,
	Box,
	Container,
	Header,
	KeyValuePairs,
	SpaceBetween,
	StatusIndicator,
} from "@cloudscape-design/components";
import type { AuthorizationMode, AuthorizationRecord } from "#/lib/api";

export function AuthorizationModeStatus({
	mode,
}: {
	mode?: AuthorizationMode;
}) {
	return (
		<StatusIndicator type={mode === "enforce" ? "success" : "info"}>
			{mode === "enforce"
				? "Enforced"
				: mode === "soft"
					? "Soft mode"
					: mode === "off"
						? "Off"
						: "Unavailable"}
		</StatusIndicator>
	);
}

export function authorizationLabel(decision?: AuthorizationRecord) {
	if (!decision) return "Not evaluated";
	if (decision.allowed) return "Allowed";
	return decision.mode === "soft" ? "Would deny" : "Denied";
}

export function AuthorizationStatus({
	decision,
}: {
	decision?: AuthorizationRecord;
}) {
	return (
		<StatusIndicator
			type={
				!decision
					? "not-started"
					: decision.allowed
						? "success"
						: decision.mode === "soft"
							? "warning"
							: "error"
			}
		>
			{authorizationLabel(decision)}
		</StatusIndicator>
	);
}

export function RequestAuthorization({
	decision,
}: {
	decision?: AuthorizationRecord;
}) {
	return (
		<Container header={<Header variant="h2">IAM authorization</Header>}>
			{decision ? (
				<SpaceBetween size="m">
					<KeyValuePairs
						columns={2}
						items={[
							{
								label: "Decision",
								value: <AuthorizationStatus decision={decision} />,
							},
							{
								label: "Mode",
								value: <AuthorizationModeStatus mode={decision.mode} />,
							},
							{ label: "Principal", value: decision.principal },
							{ label: "Explanation", value: decision.message },
						]}
					/>
					{decision.mode === "soft" && !decision.allowed ? (
						<Alert type="warning">
							Soft mode let this request continue. Enforce mode would block it.
						</Alert>
					) : null}
					{decision.checks?.map((check, index) => (
						<Container
							// biome-ignore lint/suspicious/noArrayIndexKey: Checks are an immutable request snapshot; duplicate action/resource pairs have no IDs.
							key={`${check.action}/${check.resource}/${index}`}
							header={<Header variant="h3">{check.action}</Header>}
						>
							<SpaceBetween size="s">
								<KeyValuePairs
									items={[
										{ label: "Resource", value: check.resource },
										{
											label: "Policy result",
											value: check.allowed
												? "Allowed"
												: check.explicit_deny
													? "Explicit deny"
													: "No matching allow",
										},
										{
											label: "Matched statements",
											value: check.matched?.length ? (
												<ul>
													{check.matched.map((statement) => (
														<li key={statement}>{statement}</li>
													))}
												</ul>
											) : (
												"None"
											),
										},
									]}
								/>
								{check.wildcard_only ? (
									<Alert type="info">
										This operation checks Resource "*" only. Resource-level
										evaluation is unavailable.
									</Alert>
								) : null}
								{check.unsupported?.length ? (
									<Alert type="warning" header="Evaluation limits">
										<ul>
											{check.unsupported.map((issue) => (
												<li key={issue}>{issue}</li>
											))}
										</ul>
									</Alert>
								) : null}
							</SpaceBetween>
						</Container>
					))}
				</SpaceBetween>
			) : (
				<Box color="text-body-secondary">
					No IAM decision was recorded. Permission checks may be off, the caller
					may be root, or this request may bypass identity-policy evaluation.
				</Box>
			)}
		</Container>
	);
}
