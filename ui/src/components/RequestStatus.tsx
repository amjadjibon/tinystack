import { StatusIndicator } from "@cloudscape-design/components";

export function RequestStatus({ status }: { status: number }) {
	return (
		<StatusIndicator type={status < 400 ? "success" : "error"}>
			{status}
		</StatusIndicator>
	);
}
