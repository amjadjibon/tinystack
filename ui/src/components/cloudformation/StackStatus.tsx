import { StatusIndicator } from "@cloudscape-design/components";
import { statusType } from "#/lib/cloudformationHelpers";

// StackStatus shows an icon and the status text, so state never relies on color alone.
export function StackStatus({ status }: { status?: string }) {
	if (!status) return <>—</>;
	return <StatusIndicator type={statusType(status)}>{status}</StatusIndicator>;
}
