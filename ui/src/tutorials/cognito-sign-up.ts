import { HOTSPOT } from "./hotspots.ts";
import type { Tutorial } from "./types.ts";
export const cognitoSignUp: Tutorial = {
	id: "cognito-sign-up",
	title: "Sign up a user with Cognito",
	description:
		"Create a user pool and app client, sign up a user, and confirm their email using the local mailbox.",
	services: ["cognitoidp"],
	duration: "10 min",
	prerequisites:
		"Requires Cognito pool/client creation, SignUp, ConfirmSignUp and console reads; cleanup requires ListUserPools and DeleteUserPool. Sign-up and code confirmation use CloudShell because the console offers administrative user operations. Replace POOL_ID, CLIENT_ID and CODE manually. Client checks use the pool client count; sign-up uses user count plus verification mail. Confirmation requires manual Next because diagnostic user records are redacted.",
	learnMore: "https://amjadjibon.github.io/tinystack/guides/console-tutorials/",
	cleanup: async (c, call) => {
		const ids: string[] = [];
		let token: string | undefined;
		do {
			const page: {
				UserPools?: { Id: string; Name: string }[];
				NextToken?: string;
			} = await call("cognitoidp", "ListUserPools", {
				MaxResults: 60,
				...(token ? { NextToken: token } : {}),
			});
			for (const pool of page.UserPools ?? [])
				if (pool.Name === c.names.pool) ids.push(pool.Id);
			token = page.NextToken;
		} while (token);
		return ids.map((id) => ({
			label: `User pool ${c.names.pool} (${id}), its clients and users`,
			run: () => call("cognitoidp", "DeleteUserPool", { UserPoolId: id }),
		}));
	},
	tasks: [
		{
			title: "Sign up a user with Cognito",
			steps: [
				{
					title: "Create an email-verifying pool",
					content:
						"Choose Create user pool and name it {pool}. Set Auto-verified attributes to email. Verification proves access to the email destination; TinyStack captures the code locally. Copy the returned pool ID as POOL_ID for later commands.",
					command:
						"aws-local cognito-idp create-user-pool --pool-name {pool} --auto-verified-attributes email",
					hotspotId: HOTSPOT.cognitoPool,
					route: "/services/cognitoidp",
					check: "cognitoPool",
				},
				{
					title: "Create an app client",
					content:
						"Open {pool}, select App clients and Create app client. Name it {client} and leave Generate client secret off. Copy its ID as CLIENT_ID. Inventory reports the client count on this generated pool, not client names.",
					command:
						"aws-local cognito-idp create-user-pool-client --user-pool-id POOL_ID --client-name {client} --no-generate-secret",
					hotspotId: HOTSPOT.cognitoClient,
					route: "/services/cognitoidp",
					check: "cognitoClient",
				},
				{
					title: "Sign up a user",
					content:
						"Open the pool Users tab. Its search control helps find users but Create user uses AdminCreateUser. For self-service sign-up, open CloudShell and run this command with CLIENT_ID replaced. Use username {username} and email {email}. The check waits for a user and a verification email in this pool.",
					command:
						"aws-local cognito-idp sign-up --client-id CLIENT_ID --username {username} --password 'Tutorial123!' --user-attributes Name=email,Value={email}",
					hotspotId: HOTSPOT.cognitoUsers,
					route: "/services/cognitoidp",
					check: "cognitoSignup",
				},
				{
					title: "Read the verification email",
					content:
						"Open Mailbox, choose Refresh, and open the verification email to {email} from your pool. Copy its six-digit code as CODE. The CLI below lists the user; the captured code is available in the mailbox, not in that response.",
					command: "aws-local cognito-idp list-users --user-pool-id POOL_ID",
					hotspotId: HOTSPOT.mailboxRefresh,
					route: "/mailbox",
					check: "cognitoMail",
				},
				{
					title: "Confirm with the code",
					content:
						"Run the command in CloudShell with CLIENT_ID and CODE replaced. Reopen {pool}, Users, and your user to verify CONFIRMED. The highlighted Confirm button performs AdminConfirmSignUp without a code; use CloudShell for this exercise. Choose Next manually after verification: user status and confirmation requests are redacted from diagnostics, so this check cannot auto-advance.",
					command:
						"aws-local cognito-idp confirm-sign-up --client-id CLIENT_ID --username {username} --confirmation-code CODE",
					hotspotId: HOTSPOT.cognitoConfirm,
					route: "/services/cognitoidp",
					check: "cognitoConfirmed",
				},
			],
		},
	],
};
