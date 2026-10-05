package cognitoidp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"math/big"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Cognito's SRP-6a uses the 3072-bit group of RFC 5054 with SHA-256, and
// derives the session key with HKDF as the AWS client libraries do.
const (
	srpN = "FFFFFFFFFFFFFFFFC90FDAA22168C234C4C6628B80DC1CD129024E088A67CC74020BBEA63B139B22514A08798E3404DD" +
		"EF9519B3CD3A431B302B0A6DF25F14374FE1356D6D51C245E485B576625E7EC6F44C42E9A637ED6B0BFF5CB6F406B7EDEE386BFB" +
		"5A899FA5AE9F24117C4B1FE649286651ECE45B3DC2007CB8A163BF0598DA48361C55D39A69163FA8FD24CF5F83655D23DCA3AD96" +
		"1C62F356208552BB9ED529077096966D670C354E4ABC9804F1746C08CA18217C32905E462E36CE3BE39E772C180E86039B2783A2" +
		"EC07A28FB5C55DF06F4C52C9DE2BCBF6955817183995497CEA956AE515D2261898FA051015728E5A8AAAC42DAD33170D04507A33" +
		"A85521ABDF1CBA64ECFB850458DBEF0A8AEA71575D060C7DB3970F85A6E1E4C7ABF5AE8CDB0933D71E8C94E04A25619DCEE3D226" +
		"1AD2EE6BF12FFA06D98A0864D87602733EC86A64521F2B18177B200CBBE117577A615D6C770988C0BAD946E208E24FA074E5AB31" +
		"43DB5BFCE0FD108E4B82D120A93AD2CAFFFFFFFFFFFFFFFF"
	challengePasswordVerifier = "PASSWORD_VERIFIER"
)

var (
	groupN, _  = new(big.Int).SetString(srpN, 16)
	groupG     = big.NewInt(2)
	multiplier = hexHash(padHex(groupN.Text(16)) + padHex(groupG.Text(16)))
)

// padHex pads a hex number so that it decodes to a positive two's-complement
// value, as the AWS SRP clients do.
func padHex(h string) string {
	if len(h)%2 == 1 {
		return "0" + h
	}

	if strings.ContainsRune("89abcdefABCDEF", rune(h[0])) {
		return "00" + h
	}

	return h
}

func padInt(n *big.Int) string { return padHex(n.Text(16)) }

// hexHash returns SHA-256 of the bytes a hex string encodes, as a number.
func hexHash(h string) *big.Int {
	b, _ := hex.DecodeString(h)
	sum := sha256.Sum256(b)
	return new(big.Int).SetBytes(sum[:])
}

// passwordExponent is SRP's x for a user: the hash of its salt and of the
// pool name, username, and password.
func passwordExponent(poolID, username, password, salt string) *big.Int {
	_, name, _ := strings.Cut(poolID, "_")
	inner := sha256.Sum256([]byte(name + username + ":" + password))
	return hexHash(padHex(salt) + hex.EncodeToString(inner[:]))
}

// verifier returns a new salt and SRP verifier for a user's password.
func verifier(poolID, username, password string) (salt, v string) {
	var b [16]byte
	_, _ = rand.Read(b[:])
	salt = hex.EncodeToString(b[:])
	x := passwordExponent(poolID, username, password, salt)
	return salt, new(big.Int).Exp(groupG, x, groupN).Text(16)
}

// srpChallenge is the server's half of an SRP exchange.
type srpChallenge struct {
	A string // the client's public value
	B string // the server's public value
	b string // the server's private value
}

// startSRP answers a client's public value A for a user's verifier, or
// reports that A is invalid.
func startSRP(clientA, v string) (*srpChallenge, bool) {
	a, ok := new(big.Int).SetString(clientA, 16)
	if !ok || new(big.Int).Mod(a, groupN).Sign() == 0 {
		return nil, false
	}

	verifierInt, ok := new(big.Int).SetString(v, 16)
	if !ok {
		return nil, false
	}

	for {
		private, err := rand.Int(rand.Reader, groupN)
		if err != nil {
			return nil, false
		}

		// B = k·v + g^b
		public := new(big.Int).Mul(multiplier, verifierInt)
		public.Add(public, new(big.Int).Exp(groupG, private, groupN)).Mod(public, groupN)
		if public.Sign() != 0 {
			return &srpChallenge{A: clientA, B: public.Text(16), b: private.Text(16)}, true
		}
	}
}

// srpKey derives the session key both sides share: HKDF of S with u as salt.
func srpKey(c *srpChallenge, v string) []byte {
	a, _ := new(big.Int).SetString(c.A, 16)
	b, _ := new(big.Int).SetString(c.B, 16)
	private, _ := new(big.Int).SetString(c.b, 16)
	verifierInt, _ := new(big.Int).SetString(v, 16)
	u := hexHash(padInt(a) + padInt(b))
	// S = (A · v^u)^b
	s := new(big.Int).Exp(verifierInt, u, groupN)
	s.Mul(s, a).Mod(s, groupN).Exp(s, private, groupN)
	ikm, _ := hex.DecodeString(padInt(s))
	salt, _ := hex.DecodeString(padInt(u))
	prk := hmac.New(sha256.New, salt)
	prk.Write(ikm)
	okm := hmac.New(sha256.New, prk.Sum(nil))
	okm.Write([]byte("Caldera Derived Key\x01"))
	return okm.Sum(nil)[:16]
}

// srpSignature is the PASSWORD_CLAIM_SIGNATURE a client that knows the
// password sends.
func srpSignature(key []byte, poolID, username string, secretBlock []byte, timestamp string) string {
	_, name, _ := strings.Cut(poolID, "_")
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(name + username))
	mac.Write(secretBlock)
	mac.Write([]byte(timestamp))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// srpSession is a PASSWORD_VERIFIER challenge's record, under
// session/<pool ID>/<secret block>.
type srpSession struct {
	authSession
	A string
	B string
	// Private is the server's private value b.
	Private string
}

// beginSRP starts a USER_SRP_AUTH sign-in and returns the challenge.
func (s *Service) beginSRP(
	tx storage.WriteTx, scope request.Scope, p *pool, c *client, u *user, clientA string,
) (*outcome, error) {
	challenge, ok := startSRP(clientA, u.Verifier)
	if u.Verifier == "" || !ok {
		return nil, notAuthorized("Incorrect username or password.")
	}

	block := make([]byte, 64)
	_, _ = rand.Read(block)
	sess := srpSession{
		authSession: authSession{
			Username: u.Username, Client: c.ID, Challenge: challengePasswordVerifier,
			Expires: s.now().UTC().Add(time.Duration(common.Value(c.Settings.AuthSessionValidity)) * time.Minute),
		},
		A: challenge.A, B: challenge.B, Private: challenge.b,
	}
	if err := storage.Write(tx, sessionKey(scope, p.ID, b64.EncodeToString(block)), sess); err != nil {
		return nil, err
	}

	return &outcome{
		ChallengeName: new(challengePasswordVerifier),
		ChallengeParameters: map[string]string{
			"SALT": u.Salt, "SECRET_BLOCK": base64.StdEncoding.EncodeToString(block), "SRP_B": challenge.B,
			paramUsername: u.Username, "USER_ID_FOR_SRP": u.Username,
		},
	}, nil
}

// checkSRP verifies a PASSWORD_VERIFIER answer and returns the user it signs
// in.
func (s *Service) checkSRP(
	tx storage.WriteTx, scope request.Scope, p *pool, c *client, responses map[string]string,
) (*user, error) {
	incorrect := notAuthorized("Incorrect username or password.")
	block, err := base64.StdEncoding.DecodeString(responses["PASSWORD_CLAIM_SECRET_BLOCK"])
	if err != nil || len(block) == 0 {
		return nil, incorrect
	}

	k := sessionKey(scope, p.ID, b64.EncodeToString(block))
	sess, ok, err := storage.Read[srpSession](tx, k)
	switch {
	case err != nil:
		return nil, err
	case !ok || sess.Client != c.ID || sess.Challenge != challengePasswordVerifier:
		return nil, incorrect
	case !s.now().Before(sess.Expires):
		return nil, notAuthorized("Invalid session for the user, session is expired.")
	}

	// A challenge that is answered is gone.
	tx.Delete(k)
	u, ok, err := storage.Read[user](tx, userKey(scope, p, sess.Username))
	if err != nil || !ok {
		return nil, common.ErrorsOr(err, userNotFound())
	}

	key := srpKey(&srpChallenge{A: sess.A, B: sess.B, b: sess.Private}, u.Verifier)
	want := srpSignature(key, p.ID, u.Username, block, responses["TIMESTAMP"])
	if !hmac.Equal([]byte(want), []byte(responses["PASSWORD_CLAIM_SIGNATURE"])) {
		return nil, incorrect
	}

	return u, nil
}
