import { Box } from "@cloudscape-design/components";

// CodeBlock shows text such as a request body verbatim. React escapes it, so
// client-supplied content is never parsed as HTML.
export function CodeBlock({ text }: { text: string }) {
	return (
		<Box variant="code" display="block">
			<pre
				style={{
					margin: 0,
					whiteSpace: "pre-wrap",
					wordBreak: "break-all",
					maxHeight: 480,
					overflow: "auto",
				}}
			>
				{text || "(empty)"}
			</pre>
		</Box>
	);
}
