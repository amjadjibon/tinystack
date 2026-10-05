package cognitoidp

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/cognitoidp"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	statusUnconfirmed = "UNCONFIRMED"
	statusConfirmed   = "CONFIRMED"
	statusForceChange = "FORCE_CHANGE_PASSWORD"
	statusReset       = "RESET_REQUIRED"
	verifiedSuffix    = "_verified"
	valueTrue         = "true"
	valueFalse        = "false"
	maxAttribute      = 2048
)

// user is a user record. Attributes include sub; Username is the name the user
// was created with, or its sub in a pool whose users sign in with an email
// address or phone number.
type user struct {
	Created  time.Time
	Modified time.Time
	// PasswordExpires is when a temporary password stops working.
	PasswordExpires time.Time         `json:",omitzero"`
	Attributes      map[string]string `json:",omitempty"`
	// Codes are the pending confirmation codes, by purpose.
	Codes    map[string]code `json:",omitempty"`
	Username string
	Status   string
	Password []byte `json:",omitempty"`
	// Salt and Verifier let the user sign in with SRP: the verifier is
	// g^x for the password, in hex.
	Salt     string `json:",omitempty"`
	Verifier string `json:",omitempty"`
	// History holds the hashes of earlier passwords the pool's password
	// history keeps, newest first.
	History [][]byte `json:",omitempty"`
	Groups  []string `json:",omitempty"`
	// Sessions are the origin token IDs of the user's signed-in sessions.
	Sessions []string `json:",omitempty"`
	Disabled bool     `json:",omitempty"`
}

var (
	usernamePattern = regexp.MustCompile(`^[\p{L}\p{M}\p{S}\p{N}\p{P}]+$`)
	phonePattern    = regexp.MustCompile(`^\+\d{1,15}$`)
)

func userNotFound() error { return awserror.New("UserNotFoundException", "User does not exist.") }

func (p *pool) caseInsensitive() bool { return p.CaseSensitive != nil && !*p.CaseSensitive }

// userKey places a user by name, folded when the pool's names ignore case.
func userKey(scope request.Scope, p *pool, name string) string {
	if p.caseInsensitive() {
		name = strings.ToLower(name)
	}

	return userPrefix(scope, p.ID) + name
}

// aliasKey indexes the user whose attr is v; attribute values ignore case.
func aliasKey(scope request.Scope, pool, attr, v string) string {
	return scope.Prefix(service, "alias") + pool + "/" + attr + "/" + strings.ToLower(v)
}

// aliases returns the attributes a user can sign in with and their values: the
// username attributes, a preferred username, and verified alias attributes.
func (p *pool) aliases(u *user) map[string]string {
	out := map[string]string{}
	for _, attr := range p.UsernameAttributes {
		if v := u.Attributes[attr]; v != "" {
			out[attr] = v
		}
	}

	for _, attr := range p.AliasAttributes {
		v := u.Attributes[attr]
		if v != "" && (attr == attrPreferred || u.Attributes[attr+verifiedSuffix] == valueTrue) {
			out[attr] = v
		}
	}

	return out
}

func checkUsername(name *string) error {
	if name == nil || utf8.RuneCountInString(*name) > 128 || !usernamePattern.MatchString(*name) {
		return invalidParameter("1 validation error detected: Value at 'username' failed to satisfy constraint: " +
			"Member must satisfy regular expression pattern: [\\p{L}\\p{M}\\p{S}\\p{N}\\p{P}]+")
	}

	return nil
}

// findUser returns the user name names: by username, or by an alias.
func findUser(tx storage.ReadTx, scope request.Scope, p *pool, name *string) (*user, error) {
	if err := checkUsername(name); err != nil {
		return nil, err
	}

	u, ok, err := storage.Read[user](tx, userKey(scope, p, *name))
	if err != nil || ok {
		return u, err
	}

	for _, attr := range append(slices.Clone(p.UsernameAttributes), p.AliasAttributes...) {
		owner, found := tx.Get(aliasKey(scope, p.ID, attr, *name))
		if !found {
			continue
		}

		if u, ok, err = storage.Read[user](tx, userKey(scope, p, string(owner))); err != nil || ok {
			return u, err
		}
	}

	return nil, userNotFound()
}

// saveUser writes u and moves its alias index entries from what before had.
// force takes an alias from the user that holds it (ForceAliasCreation).
func saveUser(tx storage.WriteTx, scope request.Scope, p *pool, before, u *user, force bool) error {
	old := map[string]string{}
	if before != nil {
		old = p.aliases(before)
	}

	current := p.aliases(u)
	for attr, v := range old {
		if !strings.EqualFold(current[attr], v) {
			tx.Delete(aliasKey(scope, p.ID, attr, v))
		}
	}

	for attr, v := range current {
		if strings.EqualFold(old[attr], v) {
			continue
		}

		if err := claimAlias(tx, scope, p, u, attr, v, force); err != nil {
			return err
		}
	}

	return storage.Write(tx, userKey(scope, p, u.Username), u)
}

func claimAlias(tx storage.WriteTx, scope request.Scope, p *pool, u *user, attr, v string, force bool) error {
	k := aliasKey(scope, p.ID, attr, v)
	if owner, taken := tx.Get(k); taken && string(owner) != u.Username {
		holder, ok, err := storage.Read[user](tx, userKey(scope, p, string(owner)))
		if err != nil {
			return err
		}

		switch {
		case !ok:
		case slices.Contains(p.UsernameAttributes, attr):
			return awserror.New("UsernameExistsException", "An account with the given "+attr+" already exists.")
		case !force || attr == attrPreferred:
			return awserror.New("AliasExistsException", "An account with the given "+attr+" already exists.")
		default:
			holder.Attributes[attr+verifiedSuffix] = valueFalse
			if writeErr := storage.Write(tx, userKey(scope, p, holder.Username), holder); writeErr != nil {
				return writeErr
			}
		}
	}

	tx.Put(k, []byte(u.Username))
	return nil
}

// deleteUser removes u and its alias index entries.
func deleteUser(tx storage.WriteTx, scope request.Scope, p *pool, u *user) {
	for attr, v := range p.aliases(u) {
		tx.Delete(aliasKey(scope, p.ID, attr, v))
	}

	tx.Delete(userKey(scope, p, u.Username))
}

// newSub returns a random UUID, the form of a user's sub.
func newSub() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// newUser returns a user named name with attrs, before it is saved. In a pool
// whose users sign in with an email address or phone number, name must be
// one, and the user's username is its sub.
func newUser(p *pool, name *string, attrs []api.AttributeType, now time.Time) (*user, error) {
	if err := checkUsername(name); err != nil {
		return nil, err
	}

	sub := newSub()
	u := &user{
		Username: *name, Created: now, Modified: now, Attributes: map[string]string{attrSub: sub},
		Status: statusUnconfirmed,
	}
	if _, err := setAttributes(p, u, attrs, true); err != nil {
		return nil, err
	}

	if len(p.UsernameAttributes) > 0 {
		attr := ""
		switch {
		case slices.Contains(p.UsernameAttributes, attrEmail) && strings.Contains(*name, "@"):
			attr = attrEmail
		case slices.Contains(p.UsernameAttributes, attrPhone) && phonePattern.MatchString(*name):
			attr = attrPhone
		default:
			return nil, invalidParameter("Username should be either an email or a phone number.")
		}

		if _, err := setAttributes(p, u, []api.AttributeType{{Name: &attr, Value: name}}, true); err != nil {
			return nil, err
		}

		u.Username = sub
	}

	return u, checkRequired(p, u)
}

func checkRequired(p *pool, u *user) error {
	for i := range p.Schema {
		a := &p.Schema[i]
		if common.Value(a.Required) && *a.Name != attrSub && u.Attributes[*a.Name] == "" {
			return invalidParameter("Attributes did not conform to the schema: %s: The attribute is required", *a.Name)
		}
	}

	return nil
}

// setAttributes validates attrs against the pool's schema and applies them.
// An empty value removes an attribute. Changing an email address or phone
// number marks it unverified unless attrs also verify it. It returns the
// names of the attributes that changed.
func setAttributes(p *pool, u *user, attrs []api.AttributeType, creating bool) ([]string, error) {
	var changed []string
	given := map[string]bool{}
	for _, a := range attrs {
		name, v := common.Value(a.Name), common.Value(a.Value)
		given[name] = true
		schema := p.attribute(name)
		if schema == nil {
			return nil, invalidParameter("Attributes did not conform to the schema: Type for attribute {%s} could "+
				"not be determined", name)
		}

		if name == attrSub || (!creating && !common.Value(schema.Mutable) && u.Attributes[name] != v) {
			return nil, invalidParameter("Cannot modify the non-mutable attribute %s", name)
		}

		if err := checkValue(schema, name, v); err != nil {
			return nil, err
		}

		if u.Attributes[name] == v {
			continue
		}

		if v == "" {
			if common.Value(schema.Required) {
				return nil, invalidParameter("Attributes did not conform to the schema: %s: The attribute is "+
					"required", name)
			}

			delete(u.Attributes, name)
		} else {
			u.Attributes[name] = v
		}

		changed = append(changed, name)
	}

	for _, attr := range []string{attrEmail, attrPhone} {
		if slices.Contains(changed, attr) && !given[attr+verifiedSuffix] {
			if u.Attributes[attr] == "" {
				delete(u.Attributes, attr+verifiedSuffix)
			} else {
				u.Attributes[attr+verifiedSuffix] = valueFalse
			}
		}
	}

	return changed, nil
}

// checkValue validates an attribute value against its type and constraints.
func checkValue(schema *api.SchemaAttributeType, name, v string) error {
	if v == "" {
		return nil
	}

	switch common.Value(schema.AttributeDataType) {
	case typeBoolean:
		if v != valueTrue && v != valueFalse {
			return invalidParameter("%s must be true or false.", name)
		}
	case typeNumber:
		n, err := strconv.ParseInt(v, 10, 64)
		c := schema.NumberAttributeConstraints
		if err != nil || (c != nil && outside(n, c.MinValue, c.MaxValue)) {
			return invalidParameter("%s must be a number within its constraints.", name)
		}
	default:
		highest := int64(maxAttribute)
		if c := schema.StringAttributeConstraints; c != nil && c.MaxLength != nil {
			highest, _ = strconv.ParseInt(*c.MaxLength, 10, 64)
		}

		n := int64(utf8.RuneCountInString(v))
		if c := schema.StringAttributeConstraints; n > highest || (c != nil && outside(n, c.MinLength, nil)) {
			return invalidParameter("%s must be within its length constraints.", name)
		}
	}

	switch {
	case name == attrEmail && (strings.Count(v, "@") != 1 || strings.ContainsAny(v, " \t")):
		return invalidParameter("Invalid email address format.")
	case name == attrPhone && !phonePattern.MatchString(v):
		return invalidParameter("Invalid phone number format.")
	}

	return nil
}

func outside(n int64, lowest, highest *string) bool {
	if lowest != nil {
		if low, err := strconv.ParseInt(*lowest, 10, 64); err == nil && n < low {
			return true
		}
	}

	if highest != nil {
		if high, err := strconv.ParseInt(*highest, 10, 64); err == nil && n > high {
			return true
		}
	}

	return false
}

// attributes returns the user's attributes, sub first and the rest by name,
// limited to names when it is not empty.
func (u *user) attributes(names []string) []api.AttributeType {
	keys := make([]string, 0, len(u.Attributes))
	for k := range u.Attributes {
		if len(names) == 0 || slices.Contains(names, k) {
			keys = append(keys, k)
		}
	}

	slices.SortFunc(keys, func(a, b string) int {
		if (a == attrSub) != (b == attrSub) {
			if a == attrSub {
				return -1
			}

			return 1
		}

		return strings.Compare(a, b)
	})
	out := make([]api.AttributeType, 0, len(keys))
	for _, k := range keys {
		out = append(out, api.AttributeType{Name: new(k), Value: new(u.Attributes[k])})
	}

	return out
}

func (u *user) output(names []string) api.UserType {
	return api.UserType{
		Username: new(u.Username), Attributes: u.attributes(names), UserCreateDate: common.SecondsMilli(u.Created),
		UserLastModifiedDate: common.SecondsMilli(u.Modified), Enabled: new(!u.Disabled), UserStatus: new(u.Status),
	}
}

// setPassword sets u's password, temporary until the user changes it unless
// permanent. The pool's password history forbids reusing a recent password.
func setPassword(p *pool, u *user, password string, permanent bool, now time.Time) error {
	size := int(common.Value(passwordPolicy(&p.Settings).PasswordHistorySize))
	if size > 0 {
		recent := append([][]byte{u.Password}, u.History...)
		for _, hash := range recent[:min(size, len(recent))] {
			if checkPassword(hash, password) {
				return awserror.New("PasswordHistoryPolicyViolationException",
					"The password can't be the same as a previous password.")
			}
		}
	}

	hash, err := hashPassword(password)
	if err != nil {
		return err
	}

	if size > 1 && len(u.Password) > 0 {
		u.History = append([][]byte{u.Password}, u.History...)
		u.History = u.History[:min(size-1, len(u.History))]
	} else {
		u.History = nil
	}

	u.Password, u.PasswordExpires = hash, time.Time{}
	u.Salt, u.Verifier = verifier(p.ID, u.Username, password)
	delete(u.Codes, purposePassword)
	if permanent {
		u.Status = statusConfirmed
		return nil
	}

	days := common.Value(passwordPolicy(&p.Settings).TemporaryPasswordValidityDays)
	u.Status, u.PasswordExpires = statusForceChange, now.Add(time.Duration(days)*24*time.Hour)
	return nil
}

// setCode stores a new code for purpose, sent to attr and valid for ttl, and
// returns it.
func (u *user) setCode(purpose, attr string, now time.Time, ttl time.Duration) string {
	if u.Codes == nil {
		u.Codes = map[string]code{}
	}

	c := newCode()
	u.Codes[purpose] = code{Value: c, Expires: now.Add(ttl), Attribute: attr}
	return c
}

// useCode checks and consumes the code for purpose.
func (u *user) useCode(purpose, given string, now time.Time) error {
	c, ok := u.Codes[purpose]
	switch {
	case !ok || c.Value != given:
		return awserror.New("CodeMismatchException", "Invalid verification code provided, please try again.")
	case !now.Before(c.Expires):
		return awserror.New("ExpiredCodeException", "Invalid code provided, please request a code again.")
	}

	delete(u.Codes, purpose)
	return nil
}
