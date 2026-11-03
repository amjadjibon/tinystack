import { api } from "./api";
export type ContainerScope = { account: string; region: string };
export function ecrCall<T = Record<string, unknown>>(
	s: ContainerScope,
	operation: string,
	input: Record<string, unknown> = {},
) {
	return api.action<T>("ecr", operation, s.account, input, s.region);
}
export type Repository = {
	RepositoryName: string;
	RepositoryArn: string;
	RepositoryUri: string;
	ImageTagMutability: string;
	ImageScanningConfiguration?: { ScanOnPush: boolean };
	EncryptionConfiguration?: { EncryptionType: string; KmsKey?: string };
};
export type Image = {
	ImageDigest: string;
	ImageTags?: string[];
	ImageSizeInBytes?: number;
};
