import { DeliveriesSection } from "#/components/DeliveriesSection";

export function BudgetAlerts({ arn }: { arn: string }) {
	return (
		<DeliveriesSection
			title="Recent alerts"
			filter={(d) => d.via === "budgets" && d.source === arn}
			info="Alerts fire once per notification per budget period; SNS alerts are published to the topic, and email alerts are recorded, not sent; this history is bounded and resets on restart."
			loadingText="Loading budget alerts"
			refreshAriaLabel="Refresh budget alerts"
			emptyText="No matching alerts"
			filteringPlaceholder="Find alerts"
			detailsHeader="Alert details"
			closeAriaLabel="Close alert details"
		/>
	);
}
