// unixTime shows a timestamp the API gives in seconds in the viewer's locale.
export function unixTime(seconds?: number) {
	return seconds ? new Date(seconds * 1000).toLocaleString() : "-";
}
