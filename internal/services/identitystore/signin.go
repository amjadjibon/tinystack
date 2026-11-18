package identitystore

import (
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/identitystore"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// instancePrefix maps each identity store ID that holds users to its account, so a start URL, which names only the
// store, finds the account whose users sign in. Store IDs are global, like the start URLs that carry them.
const instancePrefix = "aws/identitystore/instance/"

// StartURL is the AWS access portal URL of the account's Identity Center instance.
func StartURL(account string) string {
	_, storeID := Instance(account)
	return "https://" + storeID + ".awsapps.com/start"
}

// StartURLAccount returns the account whose access portal a start URL names. Only accounts that have created users
// are known, since nobody can sign in to the others.
func StartURLAccount(tx storage.ReadTx, startURL string) (string, bool) {
	rest, ok := strings.CutPrefix(startURL, "https://")
	if !ok {
		return "", false
	}

	host, path, _ := strings.Cut(rest, "/")
	path, _, _ = strings.Cut(path, "#")
	storeID, ok := strings.CutSuffix(host, ".awsapps.com")
	if !ok || strings.TrimSuffix(path, "/") != "start" {
		return "", false
	}

	account, found := tx.Get(instancePrefix + storeID)
	return string(account), found
}

// UserByName finds an account's user by user name, ignoring case as the uniqueness check does.
func UserByName(tx storage.ReadTx, account, userName string) (user *api.User, found bool, err error) {
	users, err := storage.Records[api.User](tx, (request.Scope{AccountID: account}).Prefix(serviceName, "user"))
	if err != nil {
		return nil, false, err
	}

	for _, u := range users {
		if strings.EqualFold(*u.UserName, userName) {
			return u, true, nil
		}
	}

	return nil, false, nil
}

// UserByID returns an account's user; found is false once it has been deleted.
func UserByID(tx storage.ReadTx, account, userID string) (user *api.User, found bool, err error) {
	return storage.Read[api.User](tx, (request.Scope{AccountID: account}).Key(serviceName, "user", userID))
}

// UserGroups returns the IDs of the groups the user belongs to.
func UserGroups(tx storage.ReadTx, account, userID string) ([]string, error) {
	scope := request.Scope{AccountID: account}
	memberships, err := storage.Records[api.GroupMembership](tx, scope.Prefix(serviceName, "membership"))
	if err != nil {
		return nil, err
	}

	var groups []string
	for _, m := range memberships {
		if m.MemberId != nil && m.MemberId.UserId != nil && *m.MemberId.UserId == userID {
			groups = append(groups, *m.GroupId)
		}
	}

	return groups, nil
}
