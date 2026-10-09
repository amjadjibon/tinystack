import {
	Alert,
	Box,
	Button,
	Container,
	Header,
	Link,
	SpaceBetween,
	Table,
} from "@cloudscape-design/components";
import { useEffect, useRef, useState } from "react";
import { api } from "#/lib/api";
import { errorText } from "#/lib/consoleActions";
import type { LambdaScope } from "#/lib/lambda";
import { logsHref } from "#/lib/logs";
import { useFollow } from "#/lib/useFollow";

type LogEvent = {
	Timestamp?: number;
	Message?: string;
	LogStreamName?: string;
	EventId?: string;
};
export function LambdaLogs({
	scope,
	enabled,
}: {
	scope: LambdaScope;
	enabled: boolean;
}) {
	const follow = useFollow();
	const [events, setEvents] = useState<LogEvent[]>([]);
	const [next, setNext] = useState<string>();
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState("");
	const [empty, setEmpty] = useState(false);
	const windowStart = useRef(Date.now() - 60 * 60 * 1000);
	const request = useRef(0);
	const locked = useRef(false);
	const group = `/aws/lambda/${scope.functionName}`;
	const load = async (append = false) => {
		if (!enabled || locked.current) return;
		locked.current = true;
		setBusy(true);
		setError("");
		const id = ++request.current;
		try {
			if (!append) windowStart.current = Date.now() - 60 * 60 * 1000;
			const result = await api.action<{
				Events?: LogEvent[];
				NextToken?: string;
			}>(
				"logs",
				"FilterLogEvents",
				scope.account,
				{
					LogGroupName: group,
					StartTime: windowStart.current,
					Limit: 100,
					...(append && next ? { NextToken: next } : {}),
				},
				scope.region,
			);
			if (id !== request.current) return;
			setEvents((previous) =>
				append
					? [...previous, ...(result.Events ?? [])]
					: (result.Events ?? []),
			);
			setNext(result.NextToken);
			setEmpty(false);
		} catch (err) {
			if (id !== request.current) return;
			if (errorText(err).startsWith("ResourceNotFoundException:")) {
				setEmpty(true);
				setEvents([]);
				setNext(undefined);
			} else setError(errorText(err));
		} finally {
			if (id === request.current) {
				setBusy(false);
				locked.current = false;
			}
		}
	};
	// biome-ignore lint/correctness/useExhaustiveDependencies: this component is keyed by function and server mode.
	useEffect(() => {
		load();
		return () => {
			request.current++;
			locked.current = false;
		};
	}, []);
	return (
		<Container
			header={
				<Header
					variant="h2"
					description={`Log group ${group} · Events from the last hour.`}
					actions={
						<SpaceBetween direction="horizontal" size="xs">
							<Link
								href={logsHref({
									account: scope.account,
									region: scope.region,
									group,
								})}
								onFollow={follow}
							>
								Open in CloudWatch Logs
							</Link>
							<Button
								iconName="refresh"
								ariaLabel="Refresh function logs"
								disabled={!enabled}
								loading={busy}
								onClick={() => load()}
							>
								Refresh
							</Button>
							{next && (
								<Button
									disabled={!enabled}
									loading={busy}
									onClick={() => load(true)}
								>
									Load more
								</Button>
							)}
						</SpaceBetween>
					}
				>
					CloudWatch logs
				</Header>
			}
		>
			<SpaceBetween size="l">
				{!enabled && (
					<Alert type="info">
						Log reads require the CloudWatch Logs service and unsigned console
						actions. Use a signed AWS CLI or SDK request when this server
						requires signatures.
					</Alert>
				)}
				{error && <Alert type="error">{error}</Alert>}
				<Table
					items={events}
					loading={busy}
					loadingText="Loading log events"
					wrapLines
					trackBy="EventId"
					columnDefinitions={[
						{
							id: "time",
							header: "Timestamp",
							cell: (e) =>
								e.Timestamp ? new Date(e.Timestamp).toLocaleString() : "—",
							width: 200,
						},
						{
							id: "message",
							header: "Message",
							cell: (e) => (
								<span
									style={{ fontFamily: "monospace", whiteSpace: "pre-wrap" }}
								>
									{e.Message}
								</span>
							),
						},
						{
							id: "stream",
							header: "Log stream",
							cell: (e) => e.LogStreamName,
							width: 280,
						},
					]}
					empty={
						<Box textAlign="center" padding="l">
							{empty
								? "The log group is created after the first completed invocation."
								: "No log events loaded for the last hour. Invoke the function or refresh its logs."}
						</Box>
					}
				/>
			</SpaceBetween>
		</Container>
	);
}
