import {
	Alert,
	Box,
	Button,
	SpaceBetween,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { api } from "#/lib/api";
import { arn, type Context } from "./checks";
import type { Tutorial } from "./types";
export function Cleanup({
	tutorial,
	ctx,
}: {
	tutorial: Tutorial;
	ctx: Context;
}) {
	const [rows, setRows] = useState<string[] | null>(null);
	const [error, setError] = useState("");
	const [busy, setBusy] = useState(false);
	const [done, setDone] = useState(false);
	const n = ctx.names;
	const labels =
		tutorial.id === "s3-first-bucket"
			? [
					`Object s3://${n.bucket}/${n.key}`,
					`Bucket ${n.bucket} (must otherwise be empty)`,
				]
			: [
					`Topic ${arn("sns", n.topic, ctx)} (including all its subscriptions)`,
					`Queue ${arn("sqs", n.queue, ctx)} and its messages`,
				];
	const call = <T,>(
		service: string,
		operation: string,
		input: Record<string, unknown>,
	) => api.action<T>(service, operation, ctx.account, input, ctx.region);
	const prepare = async () => {
		setBusy(true);
		setError("");
		try {
			const subscriptions: string[] = [];
			if (tutorial.id === "sns-sqs-fanout") {
				let token: string | undefined;
				do {
					const page: {
						Subscriptions?: {
							SubscriptionArn: string;
							Protocol: string;
							Endpoint: string;
						}[];
						NextToken?: string;
					} = await call("sns", "ListSubscriptionsByTopic", {
						TopicArn: arn("sns", n.topic, ctx),
						...(token ? { NextToken: token } : {}),
					});
					for (const s of page.Subscriptions ?? [])
						if (s.Protocol === "sqs" && s.Endpoint === arn("sqs", n.queue, ctx))
							subscriptions.push(s.SubscriptionArn);
					token = page.NextToken;
				} while (token);
			}
			setRows(subscriptions);
		} catch (e) {
			setError(e instanceof Error ? e.message : String(e));
		} finally {
			setBusy(false);
		}
	};
	return (
		<SpaceBetween size="s">
			{!!error && <Alert type="error">{error}</Alert>}
			<Button disabled={done} loading={busy} onClick={prepare}>
				{done ? "Cleaned up" : "Clean up"}
			</Button>
			{rows && (
				<ActionDialog
					title="Clean up tutorial resources"
					submit="Delete listed resources"
					confirmation="delete"
					onClose={() => setRows(null)}
					onSaved={() => {
						setRows(null);
						setDone(true);
					}}
					plan={async () => {
						if (tutorial.id === "s3-first-bucket")
							return [
								{
									label: labels[0],
									run: () =>
										call("s3", "DeleteObject", {
											Bucket: n.bucket,
											Key: n.key,
										}),
								},
								{
									label: labels[1],
									run: () => call("s3", "DeleteBucket", { Bucket: n.bucket }),
								},
							];
						return [
							...rows.map((id) => ({
								label: `Subscription ${id}`,
								run: () => call("sns", "Unsubscribe", { SubscriptionArn: id }),
							})),
							{
								label: labels[0],
								run: () =>
									call("sns", "DeleteTopic", {
										TopicArn: arn("sns", n.topic, ctx),
									}),
							},
							{
								label: labels[1],
								run: () =>
									call("sqs", "DeleteQueue", {
										QueueUrl: `/${ctx.account}/${n.queue}`,
									}),
							},
						];
					}}
				>
					<Box>
						Only these generated resources in account {ctx.account}, Region{" "}
						{ctx.region} will be removed:
					</Box>
					<ul>
						{[...rows.map((id) => `Subscription ${id}`), ...labels].map(
							(label) => (
								<li key={label}>{label}</li>
							),
						)}
					</ul>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
