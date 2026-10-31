import { api } from "./api";
import type { Attribute, CognitoUser } from "./cognitoHelpers";
export type CognitoScope = { account: string; region: string; id?: string };
export type Pool = Record<string, unknown> & {
	Id: string;
	Name: string;
	CreationDate?: number;
	EstimatedNumberOfUsers?: number;
	MfaConfiguration?: string;
	Domain?: string;
	CustomDomain?: string;
};
export type Group = {
	GroupName: string;
	Description?: string;
	Precedence?: number;
	RoleArn?: string;
};
export type Client = Record<string, unknown> & {
	ClientId: string;
	ClientName: string;
	ClientSecret?: string;
	CallbackURLs?: string[];
	LogoutURLs?: string[];
	AllowedOAuthFlows?: string[];
	ExplicitAuthFlows?: string[];
};
type UserInput = { Username: string };
type Inputs = {
	CreateUserPool: Record<string, unknown>;
	UpdateUserPool: Record<string, unknown>;
	DeleteUserPool: Record<string, never>;
	GetUserPoolMfaConfig: Record<string, never>;
	SetUserPoolMfaConfig: Record<string, unknown>;
	AddCustomAttributes: { CustomAttributes: Record<string, unknown>[] };
	ListTagsForResource: { ResourceArn: string };
	TagResource: { ResourceArn: string; Tags: Record<string, string> };
	UntagResource: { ResourceArn: string; TagKeys: string[] };
	CreateUserPoolClient: Record<string, unknown>;
	UpdateUserPoolClient: Record<string, unknown>;
	DeleteUserPoolClient: { ClientId: string };
	CreateUserPoolDomain: Record<string, unknown>;
	UpdateUserPoolDomain: Record<string, unknown>;
	DeleteUserPoolDomain: { Domain: string };
	GetGroup: { GroupName: string };
	UpdateGroup: Record<string, unknown>;
	AdminDeleteUserAttributes: UserInput & { UserAttributeNames: string[] };
	AdminUserGlobalSignOut: UserInput;
	ListUserPools: Record<string, never>;
	DescribeUserPool: Record<string, never>;
	ListUsers: Record<string, never>;
	AdminGetUser: UserInput;
	AdminCreateUser: UserInput & {
		TemporaryPassword: string;
		UserAttributes: Attribute[];
	};
	AdminConfirmSignUp: UserInput;
	AdminEnableUser: UserInput;
	AdminDisableUser: UserInput;
	AdminResetUserPassword: UserInput;
	AdminSetUserPassword: UserInput & { Password: string; Permanent: boolean };
	AdminDeleteUser: UserInput;
	AdminUpdateUserAttributes: UserInput & { UserAttributes: Attribute[] };
	AdminListGroupsForUser: UserInput;
	AdminAddUserToGroup: UserInput & { GroupName: string };
	AdminRemoveUserFromGroup: UserInput & { GroupName: string };
	ListGroups: Record<string, never>;
	CreateGroup: { GroupName: string; Description: string };
	DeleteGroup: { GroupName: string };
	ListUsersInGroup: { GroupName: string };
	ListUserPoolClients: Record<string, never>;
	DescribeUserPoolClient: { ClientId: string };
	DescribeUserPoolDomain: { Domain: string };
};
type Outputs = {
	CreateUserPool: { UserPool: Pool };
	GetUserPoolMfaConfig: Record<string, unknown>;
	ListTagsForResource: { Tags?: Record<string, string> };
	GetGroup: { Group: Group };
	ListUserPools: { UserPools?: Pool[] };
	DescribeUserPool: { UserPool: Pool };
	ListUsers: { Users?: CognitoUser[] };
	AdminGetUser: Omit<CognitoUser, "Attributes"> & {
		UserAttributes?: Attribute[];
	};
	AdminListGroupsForUser: { Groups?: Group[] };
	ListGroups: { Groups?: Group[] };
	ListUsersInGroup: { Users?: CognitoUser[] };
	ListUserPoolClients: { UserPoolClients?: Client[] };
	DescribeUserPoolClient: { UserPoolClient: Client };
	DescribeUserPoolDomain: {
		DomainDescription?: { Domain?: string; Status?: string };
	};
};
const unscopedOperations: string[] = [
	"ListUserPools",
	"CreateUserPool",
	"DescribeUserPoolDomain",
	"TagResource",
	"UntagResource",
	"ListTagsForResource",
];
export function cognitoCall<K extends keyof Inputs>(
	s: CognitoScope,
	operation: K,
	input: Inputs[K],
): Promise<K extends keyof Outputs ? Outputs[K] : Record<string, unknown>> {
	return api.action(
		"cognitoidp",
		operation,
		s.account,
		{
			...(s.id && !unscopedOperations.includes(operation)
				? { UserPoolId: s.id }
				: {}),
			...input,
		},
		s.region,
	);
}
export async function cognitoPages<T>(
	s: CognitoScope,
	operation:
		| "ListUserPools"
		| "ListUsers"
		| "ListGroups"
		| "AdminListGroupsForUser"
		| "ListUsersInGroup"
		| "ListUserPoolClients",
	key: string,
	input: Record<string, string> = {},
): Promise<T[]> {
	const items: T[] = [],
		seen = new Set<string>();
	const tokenKey = operation === "ListUsers" ? "PaginationToken" : "NextToken";
	let token: string | undefined;
	do {
		const out = await api.action<Record<string, unknown>>(
			"cognitoidp",
			operation,
			s.account,
			{
				...(s.id && operation !== "ListUserPools" ? { UserPoolId: s.id } : {}),
				...input,
				...(["ListUserPools", "ListUserPoolClients"].includes(operation)
					? { MaxResults: 60 }
					: { Limit: 60 }),
				...(token ? { [tokenKey]: token } : {}),
			},
			s.region,
		);
		items.push(...((out[key] as T[] | undefined) ?? []));
		token = out[tokenKey] as string | undefined;
		if (token && seen.has(token))
			throw new Error("Repeated Cognito pagination token.");
		if (token) seen.add(token);
	} while (token);
	return items;
}
export function cognitoHref(s: CognitoScope, username = "") {
	return `/services/cognitoidp?${new URLSearchParams({ account: s.account, region: s.region, id: s.id ?? "", username })}`;
}
