// unixTime shows a timestamp the API gives in seconds in the viewer's locale.
export function unixTime(seconds?: number) {
	return seconds ? new Date(seconds * 1000).toLocaleString() : "-";
}

// isoTime shows an ISO 8601 timestamp, as console actions give times (with up to
// nine fractional digits), in the viewer's locale.
export function isoTime(value?: string) {
	if (!value) return "-";
	const date = new Date(value.replace(/(\.\d{3})\d+/, "$1"));
	return Number.isNaN(date.getTime()) ? value : date.toLocaleString();
}
