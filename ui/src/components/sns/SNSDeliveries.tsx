import { DeliveriesSection } from "#/components/DeliveriesSection";

export function SNSDeliveries({ arn }: { arn: string }) {
	return (
		<DeliveriesSection
			title="Deliveries"
			filter={(d) => d.via === "sns" && d.source === arn}
			info="Recent deliveries from this topic, including confirmation requests. HTTP delivery is asynchronous; a successful publish does not guarantee delivery. Filtered-out messages do not produce delivery records. History is bounded and resets on restart."
			loadingText="Loading topic deliveries"
			refreshAriaLabel="Refresh topic deliveries"
			emptyText="No matching topic deliveries"
		/>
	);
}
