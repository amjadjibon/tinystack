import { DeliveriesSection } from "#/components/DeliveriesSection";

export function BudgetAlerts({ arn }: { arn: string }) {
	return (
		<DeliveriesSection
			title="Recent alerts"
			noun="budget alerts"
			filter={(d) => d.via === "budgets" && d.source === arn}
			info="Each notification fires once per budget period. SNS alerts are published to the topic; email alerts are recorded, not sent. History is bounded and resets on restart."
		/>
	);
}
