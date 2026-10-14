package gateway

// accountFromKey returns the account an access key names by itself: a key of
// exactly 12 ASCII digits is its own account ID, as in LocalStack and MiniStack.
//
// An access key's account is chosen in this order, and every caller of the
// rule goes through these two steps:
//
//  1. identity.accounts, the configured mapping (identify, kubernetes token);
//  2. credentials TinyStack issued through STS or IAM (resolveCredentials);
//  3. a 12-digit key (this function);
//  4. defaults.account.
//
// The rule picks an account only. In identity.mode: verify the key still needs
// an identity.secret_keys entry, so a 12-digit key never skips a signature.
func accountFromKey(key string) (string, bool) {
	if len(key) != 12 {
		return "", false
	}

	for i := range len(key) {
		if key[i] < '0' || key[i] > '9' {
			return "", false
		}
	}

	return key, true
}

// keyAccount applies steps 1 and 3 of the order above: the configured mapping,
// then the key's own digits. ok is false when neither names an account.
// Issued credentials sit between them, so callers that can look them up do so
// first when mapped is false.
func (s *Server) keyAccount(key string) (account string, mapped, ok bool) {
	if account, ok = s.cfg.Identity.Accounts[key]; ok {
		return account, true, true
	}

	account, ok = accountFromKey(key)
	return account, false, ok
}
