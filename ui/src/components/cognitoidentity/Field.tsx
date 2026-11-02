import { Box, CopyToClipboard } from "@cloudscape-design/components";
import type { ReactNode } from "react";

export function Field({
	label,
	children,
}: {
	label: string;
	children: ReactNode;
}) {
	return (
		<div>
			<Box variant="awsui-key-label">{label}</Box>
			<div>{children}</div>
		</div>
	);
}

// Copy shows text that has a copy button; label names it in the button's messages.
export function Copy({ text, label }: { text: string; label: string }) {
	return (
		<CopyToClipboard
			variant="inline"
			textToCopy={text}
			copySuccessText={`${label} copied`}
			copyErrorText={`${label} failed to copy`}
		/>
	);
}
