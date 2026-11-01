import { createFileRoute } from "@tanstack/react-router";
import { StreamingPage } from "#/components/streaming/StreamingPage";
export const Route = createFileRoute("/services/firehose")({
	validateSearch: (search: Record<string, unknown>) => ({
		account: String(search.account ?? ""),
		region: String(search.region ?? ""),
		stream: String(search.stream ?? ""),
	}),
	component: Page,
});
function Page() {
	const search = Route.useSearch();
	return <StreamingPage service="firehose" stream={search.stream} />;
}
