package cognitoidp

import (
	"crypto/rand"
	"encoding/json"
	"math/big"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/delivery"
)

const (
	mediumEmail = "EMAIL"
	mediumSMS   = "SMS"
	maxPassword = 256
	// symbols are the special characters Cognito's password policy counts.
	symbols = "^$*.[]{}()?\"!@#%&/\\,><':;|_~`=+- "

	purposeSignUp   = "signup"
	purposePassword = "password"
	purposeVerify   = "verify:"
)

// code is a confirmation code the user must send back before it expires.
type code struct {
	Expires time.Time
	Value   string
	// Attribute is the email or phone attribute a sign-up code went to, which
	// confirming verifies.
	Attribute string `json:",omitempty"`
}

// newCode returns a random six-digit code.
func newCode() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(1_000_000))
	return strings.Repeat("0", 6-len(n.String())) + n.String()
}

// hashPassword keeps passwords out of stored state. TinyStack is a local
// emulator, so it hashes at bcrypt's lowest cost to keep sign-ins fast.
func hashPassword(password string) ([]byte, error) {
	return bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
}

func checkPassword(hash []byte, password string) bool {
	return len(hash) > 0 && bcrypt.CompareHashAndPassword(hash, []byte(password)) == nil
}

func invalidPassword(reason string) error {
	return awserror.New("InvalidPasswordException", "Password did not conform with policy: "+reason)
}

// checkPolicy validates password against the pool's password policy.
func checkPolicy(p *pool, password *string) error {
	pw := value(password)
	pp := passwordPolicy(&p.Settings)
	if n := utf8.RuneCountInString(pw); n < int(value(pp.MinimumLength)) || n > maxPassword {
		return invalidPassword("Password not long enough")
	}

	classes := []struct {
		test     func(rune) bool
		reason   string
		required bool
	}{
		{unicode.IsUpper, "Password must have uppercase characters", value(pp.RequireUppercase)},
		{unicode.IsLower, "Password must have lowercase characters", value(pp.RequireLowercase)},
		{unicode.IsDigit, "Password must have numeric characters", value(pp.RequireNumbers)},
		{
			func(r rune) bool { return strings.ContainsRune(symbols, r) }, "Password must have symbol characters",
			value(pp.RequireSymbols),
		},
	}
	for _, class := range classes {
		if class.required && !strings.ContainsFunc(pw, class.test) {
			return invalidPassword(class.reason)
		}
	}

	return nil
}

// temporaryPassword returns a random password that meets any policy.
func temporaryPassword() string {
	return randomString("ABCDEFGHJKLMNPQRSTUVWXYZ", 4) + randomString("abcdefghijkmnopqrstuvwxyz", 4) +
		randomString("23456789", 3) + randomString("!#%&*+-=?@^_", 1)
}

// mask hides most of a delivery destination, as Cognito does.
func mask(destination string) string {
	if local, domain, ok := strings.Cut(destination, "@"); ok && local != "" && domain != "" {
		return local[:1] + "***@" + domain[:1] + "***"
	}

	if len(destination) > 4 {
		return "+" + strings.Repeat("*", len(destination)-5) + destination[len(destination)-4:]
	}

	return "***"
}

// message is what the delivery log records for a message Cognito would have
// sent. Code is the confirmation code or temporary password it carries.
type message struct {
	Pool        string `json:"userPoolId"`
	Username    string `json:"username"`
	Destination string `json:"destination"`
	Subject     string `json:"subject,omitempty"`
	Message     string `json:"message"`
	Code        string `json:"code"`
}

// outbox collects the messages an operation sends, so they are recorded only
// once its transaction commits.
type outbox struct {
	source   string
	messages []message
	targets  []string
}

// sent is where a message went, masked as Cognito reports it.
type sent struct {
	Destination string
	Medium      string
	Attribute   string
}

// send queues a message with template's {username} and {####} filled in and
// returns where it goes.
func (o *outbox) send(scope request.Scope, p *pool, u *user, medium, subject, template, secret string) sent {
	attr := attrEmail
	if medium == mediumSMS {
		attr = attrPhone
	}

	destination := u.Attributes[attr]
	text := strings.ReplaceAll(strings.ReplaceAll(template, "{username}", u.Username), codeTemplate, secret)
	if medium == mediumSMS {
		subject = ""
	}

	o.source = poolARN(scope, p.ID)
	o.messages = append(o.messages, message{
		Pool: p.ID, Username: u.Username, Destination: destination, Subject: subject, Message: text, Code: secret,
	})
	o.targets = append(o.targets, strings.ToLower(medium)+":"+destination)
	return sent{Destination: mask(destination), Medium: medium, Attribute: attr}
}

// flush records the queued messages in the delivery log.
func (o *outbox) flush(log *delivery.Log) {
	if log == nil {
		return
	}

	for i, m := range o.messages {
		payload, _ := json.Marshal(m)
		log.Add(delivery.ViaCognito, o.source, o.targets[i], payload, nil)
	}
}

// verificationMessage returns the pool's verification subject and template
// for medium.
func verificationMessage(p *pool, medium string) (subject, template string) {
	st := &p.Settings
	subject, template = "Your verification code", "Your verification code is {####}. "
	if medium == mediumSMS {
		template = first(st.VerificationMessageTemplate.SmsMessage, st.SmsVerificationMessage, &template)
		return "", template
	}

	return first(st.VerificationMessageTemplate.EmailSubject, st.EmailVerificationSubject, &subject),
		first(st.VerificationMessageTemplate.EmailMessage, st.EmailVerificationMessage, &template)
}

// inviteMessage returns the pool's invitation subject and template for medium.
func inviteMessage(p *pool, medium string) (subject, template string) {
	subject, template = "Your temporary password", "Your username is {username} and temporary password is {####}. "
	if t := p.Settings.AdminCreateUserConfig.InviteMessageTemplate; t != nil {
		if medium == mediumSMS {
			return "", first(t.SMSMessage, &template)
		}

		return first(t.EmailSubject, &subject), first(t.EmailMessage, &template)
	}

	return subject, template
}

func first(values ...*string) string {
	for _, v := range values {
		if v != nil && *v != "" {
			return *v
		}
	}

	return ""
}

// verificationMedium returns how to send u a code: to its email if the pool
// verifies email and u has one, else to its phone number, else "".
func verificationMedium(p *pool, u *user) string {
	for _, attr := range []string{attrEmail, attrPhone} {
		if hasAutoVerify(p, attr) && u.Attributes[attr] != "" {
			return mediumOf(attr)
		}
	}

	return ""
}

func hasAutoVerify(p *pool, attr string) bool {
	for _, a := range p.Settings.AutoVerifiedAttributes {
		if a == attr {
			return true
		}
	}

	return false
}

func mediumOf(attr string) string {
	if attr == attrPhone {
		return mediumSMS
	}

	return mediumEmail
}
