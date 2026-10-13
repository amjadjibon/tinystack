import {
	Box,
	Container,
	Header,
	KeyValuePairs,
	Link,
	SpaceBetween,
} from "@cloudscape-design/components";
import { AuthorizationModeStatus } from "#/components/Authorization";
import { useHealth } from "#/lib/health";
import { useFollow } from "#/lib/useFollow";

export function IAMAuthorization() {
	const health = useHealth();
	const follow = useFollow();
	const identity = health.data?.identity;
	return (
		<Container header={<Header variant="h2">Permission checks</Header>}>
			<SpaceBetween size="m">
				<KeyValuePairs
					columns={2}
					items={[
						{
							label: "Authorization",
							value: <AuthorizationModeStatus mode={identity?.authorization} />,
						},
						{
							label: "Authentication",
							value:
								identity?.mode === "verify"
									? "SigV4 signatures verified"
									: identity?.mode === "parse"
										? "Access keys parsed; signatures not verified"
										: "Unavailable",
						},
					]}
				/>
				<Box variant="p">
					{identity?.authorization === "enforce"
						? "AWS API requests from IAM users and role sessions must be allowed by their identity policies. Explicit denies take precedence."
						: identity?.authorization === "soft"
							? "IAM policies are evaluated and decisions are recorded. Requests continue even when permission checks would deny them."
							: identity?.authorization === "off"
								? "IAM policies are stored, but permission checks are off. Start TinyStack with --authorization soft to inspect decisions, or --authorization enforce to block denied requests."
								: "This server has not reported its authorization mode."}
				</Box>
				<Link href="/requests" onFollow={follow}>
					Inspect authorization decisions
				</Link>
				<Box variant="p" color="text-body-secondary">
					Console actions are administrative and bypass these permission checks.
					Configured root keys also bypass them. Use IAM user or role
					credentials with the AWS CLI or SDK to test policies.
				</Box>
				<Box variant="p" color="text-body-secondary">
					When enabled, identity checks evaluate user, group and role policies.
					AWS-managed policy attachments grant no permissions because their
					documents are unavailable. Resource policies, permission boundaries,
					session policies and organization policies are not evaluated.
				</Box>
			</SpaceBetween>
		</Container>
	);
}
