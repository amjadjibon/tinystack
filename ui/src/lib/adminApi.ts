import { adminFetch } from "./session";

export async function adminRequest<T>(
	path: string,
	method = "GET",
	body?: unknown,
): Promise<T> {
	const response = await adminFetch(path, method, body);
	if (!response.ok) {
		const message = await response.text();
		throw new Error(
			`${response.status === 403 ? "administrator access required — " : ""}${response.status}: ${message}`,
		);
	}
	if (response.status === 204) return undefined as T;
	return response.json();
}
