package cognitoidp

import (
	"context"
	"maps"
	"strconv"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/cognitoidp"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	attrSub       = "sub"
	attrEmail     = "email"
	attrPhone     = "phone_number"
	attrPreferred = "preferred_username"
	mfaOff        = "OFF"
	mfaOptional   = "OPTIONAL"
	typeString    = "String"
	typeNumber    = "Number"
	typeBoolean   = "Boolean"
	typeDate      = "DateTime"
	customPrefix  = "custom:"
	maxCustom     = 50
	maxPools      = 60
	codeTemplate  = "{####}"
)

// pool is a user pool record. Settings are what CreateUserPool and
// UpdateUserPool both set; the other fields are fixed at creation, or changed
// by their own operations.
type pool struct {
	Created  time.Time
	Modified time.Time
	Tags     map[string]string `json:",omitempty"`
	Settings settings
	// CaseSensitive is the UsernameConfiguration, if one was given.
	CaseSensitive      *bool `json:",omitempty"`
	ID                 string
	Name               string
	Schema             []api.SchemaAttributeType
	AliasAttributes    []string `json:",omitempty"`
	UsernameAttributes []string `json:",omitempty"`
	// Key is the RSA key (PKCS #1) that signs the pool's tokens, made when the
	// pool first needs it.
	Key              []byte `json:",omitempty"`
	SoftwareTokenMFA bool   `json:",omitempty"`
}

// settings keep the wire names, so requests and responses convert to and from
// them by name. UpdateUserPool replaces them all: an omitted setting returns to
// its default.
type settings struct {
	Policies                    *api.UserPoolPolicyType              `json:",omitempty"`
	VerificationMessageTemplate *api.VerificationMessageTemplateType `json:",omitempty"`
	UserAttributeUpdateSettings *api.UserAttributeUpdateSettingsType `json:",omitempty"`
	DeviceConfiguration         *api.DeviceConfigurationType         `json:",omitempty"`
	EmailConfiguration          *api.EmailConfigurationType          `json:",omitempty"`
	SmsConfiguration            *api.SmsConfigurationType            `json:",omitempty"`
	AdminCreateUserConfig       *api.AdminCreateUserConfigType       `json:",omitempty"`
	UserPoolAddOns              *api.UserPoolAddOnsType              `json:",omitempty"`
	AccountRecoverySetting      *api.AccountRecoverySettingType      `json:",omitempty"`
	SmsVerificationMessage      *string                              `json:",omitempty"`
	EmailVerificationMessage    *string                              `json:",omitempty"`
	EmailVerificationSubject    *string                              `json:",omitempty"`
	SmsAuthenticationMessage    *string                              `json:",omitempty"`
	DeletionProtection          *string                              `json:",omitempty"`
	MfaConfiguration            *string                              `json:",omitempty"`
	UserPoolTier                *string                              `json:",omitempty"`
	AutoVerifiedAttributes      []string                             `json:",omitempty"`
}

// standard are the attributes every pool has, with their types.
var standard = []struct{ name, kind string }{
	{attrSub, typeString},
	{"name", typeString},
	{"given_name", typeString},
	{"family_name", typeString},
	{"middle_name", typeString},
	{"nickname", typeString},
	{attrPreferred, typeString},
	{"profile", typeString},
	{"picture", typeString},
	{"website", typeString},
	{attrEmail, typeString},
	{"email_verified", typeBoolean},
	{"gender", typeString},
	{"birthdate", typeString},
	{"zoneinfo", typeString},
	{"locale", typeString},
	{attrPhone, typeString},
	{"phone_number_verified", typeBoolean},
	{"address", typeString},
	{"updated_at", typeNumber},
	{"identities", typeString},
}

func standardSchema() []api.SchemaAttributeType {
	out := make([]api.SchemaAttributeType, 0, len(standard))
	for _, attr := range standard {
		a := api.SchemaAttributeType{
			Name: new(attr.name), AttributeDataType: new(attr.kind), DeveloperOnlyAttribute: new(false),
			Mutable: new(attr.name != attrSub), Required: new(attr.name == attrSub),
		}

		switch {
		case attr.name == attrSub:
			a.StringAttributeConstraints = &api.StringAttributeConstraintsType{
				MinLength: new("1"),
				MaxLength: new("2048"),
			}
		case attr.name == "birthdate":
			a.StringAttributeConstraints = &api.StringAttributeConstraintsType{
				MinLength: new("10"),
				MaxLength: new("10"),
			}
		case attr.name == "identities":
			a.StringAttributeConstraints = &api.StringAttributeConstraintsType{}
		case attr.kind == typeString:
			a.StringAttributeConstraints = &api.StringAttributeConstraintsType{
				MinLength: new("0"),
				MaxLength: new("2048"),
			}
		case attr.kind == typeNumber:
			a.NumberAttributeConstraints = &api.NumberAttributeConstraintsType{MinValue: new("0")}
		}

		out = append(out, a)
	}

	return out
}

// attribute returns the schema entry for name, if the pool has one.
func (p *pool) attribute(name string) *api.SchemaAttributeType {
	for i := range p.Schema {
		if *p.Schema[i].Name == name {
			return &p.Schema[i]
		}
	}

	return nil
}

// addSchema adds attributes to the pool's schema: standard attributes change
// their settings, and custom ones are added once.
func (p *pool) addSchema(attrs []api.SchemaAttributeType, creating bool) error {
	for i := range attrs {
		a := attrs[i]
		name := strings.TrimPrefix(value(a.Name), customPrefix)
		if name == "" || len(name) > 20 {
			return invalidParameter("Attribute names must be 1 to 20 characters long.")
		}

		if value(a.DeveloperOnlyAttribute) {
			return invalidParameter("TinyStack does not support developer-only attributes.")
		}

		kind := value(a.AttributeDataType)
		if !oneOf(a.AttributeDataType, typeString, typeNumber, typeBoolean, typeDate) {
			return invalidParameter("Invalid AttributeDataType %s.", kind)
		}

		if err := checkConstraints(&a); err != nil {
			return err
		}

		if existing := p.attribute(value(a.Name)); creating && existing != nil {
			// A standard attribute keeps its type; the schema sets the rest.
			if kind != "" && kind != *existing.AttributeDataType {
				return invalidParameter("%s must be of type %s.", name, *existing.AttributeDataType)
			}

			if name == attrSub {
				continue
			}

			existing.Required, existing.Mutable = new(value(a.Required)), new(a.Mutable == nil || *a.Mutable)
			if a.StringAttributeConstraints != nil {
				existing.StringAttributeConstraints = a.StringAttributeConstraints
			}

			if a.NumberAttributeConstraints != nil {
				existing.NumberAttributeConstraints = a.NumberAttributeConstraints
			}

			continue
		}

		if p.attribute(customPrefix+name) != nil {
			return invalidParameter("Existing attribute already has name %s%s.", customPrefix, name)
		}

		if value(a.Required) {
			return invalidParameter("Required custom attributes are not supported currently.")
		}

		if kind == "" {
			kind = typeString
		}

		a.Name, a.AttributeDataType, a.DeveloperOnlyAttribute = new(customPrefix+name), new(kind), new(false)
		a.Required, a.Mutable = new(false), new(a.Mutable == nil || *a.Mutable)
		if kind == typeString && a.StringAttributeConstraints == nil {
			a.StringAttributeConstraints = &api.StringAttributeConstraintsType{}
		}

		if kind == typeNumber && a.NumberAttributeConstraints == nil {
			a.NumberAttributeConstraints = &api.NumberAttributeConstraintsType{}
		}

		p.Schema = append(p.Schema, a)
	}

	custom := 0
	for i := range p.Schema {
		if strings.HasPrefix(*p.Schema[i].Name, customPrefix) {
			custom++
		}
	}

	if custom > maxCustom {
		return invalidParameter("A user pool can have at most %d custom attributes.", maxCustom)
	}

	return nil
}

// checkConstraints validates an attribute's length or value bounds.
func checkConstraints(a *api.SchemaAttributeType) error {
	bounds := []*string{}
	if c := a.StringAttributeConstraints; c != nil {
		bounds = append(bounds, c.MinLength, c.MaxLength)
	}

	if c := a.NumberAttributeConstraints; c != nil {
		bounds = append(bounds, c.MinValue, c.MaxValue)
	}

	for _, b := range bounds {
		if b == nil {
			continue
		}

		if _, err := strconv.ParseInt(*b, 10, 64); err != nil {
			return invalidParameter("Attribute constraint %q is not a number.", *b)
		}
	}

	return nil
}

// validSettings checks and completes the settings CreateUserPool and
// UpdateUserPool take. softwareToken is whether the pool has software token MFA.
func validSettings(st *settings, softwareToken bool) error {
	if !oneOf(st.DeletionProtection, "ACTIVE", "INACTIVE") {
		return invalidParameter("DeletionProtection must be ACTIVE or INACTIVE.")
	}

	for _, attr := range st.AutoVerifiedAttributes {
		if attr != attrEmail && attr != attrPhone {
			return invalidParameter("Invalid auto verified attribute %s.", attr)
		}
	}

	if u := st.UserAttributeUpdateSettings; u != nil && len(u.AttributesRequireVerificationBeforeUpdate) > 0 {
		return invalidParameter("TinyStack does not support keeping the original attribute value until an update " +
			"is verified: AttributesRequireVerificationBeforeUpdate must be empty.")
	}

	if err := checkMFA(st.MfaConfiguration, st.SmsConfiguration != nil || softwareToken); err != nil {
		return err
	}

	if pp := passwordPolicy(st); value(pp.MinimumLength) < 6 || value(pp.MinimumLength) > 99 ||
		value(pp.TemporaryPasswordValidityDays) < 0 || value(pp.TemporaryPasswordValidityDays) > 365 {
		return invalidParameter("Password policies need a MinimumLength of 6 to 99 and TemporaryPasswordValidityDays " +
			"of 0 to 365.")
	}

	if err := checkMessages(st); err != nil {
		return err
	}

	if e := st.EmailConfiguration; e != nil && !oneOf(e.EmailSendingAccount, "COGNITO_DEFAULT", "DEVELOPER") {
		return invalidParameter("EmailSendingAccount must be COGNITO_DEFAULT or DEVELOPER.")
	}

	if a := st.UserPoolAddOns; a != nil && value(a.AdvancedSecurityMode) != mfaOff {
		return invalidParameter("TinyStack does not support threat protection: AdvancedSecurityMode must be OFF.")
	}

	if !oneOf(st.UserPoolTier, "LITE", "ESSENTIALS", "PLUS") {
		return invalidParameter("UserPoolTier must be LITE, ESSENTIALS, or PLUS.")
	}

	if err := checkRecovery(st.AccountRecoverySetting); err != nil {
		return err
	}

	defaults(st)
	return nil
}

func checkMFA(mfa *string, methods bool) error {
	switch value(mfa) {
	case "", mfaOff:
	case mfaOptional:
		if !methods {
			return invalidParameter("Invalid MFA configuration given, can't turn off all MFAs and set MFA to be " +
				"optional or on.")
		}
	case "ON":
		return invalidParameter("TinyStack does not support required MFA: MfaConfiguration must be OFF or OPTIONAL.")
	default:
		return invalidParameter("MfaConfiguration must be OFF, ON, or OPTIONAL.")
	}

	return nil
}

func checkMessages(st *settings) error {
	messages := []*string{st.EmailVerificationMessage, st.SmsVerificationMessage, st.SmsAuthenticationMessage}
	for _, message := range messages {
		if message != nil && !strings.Contains(*message, codeTemplate) {
			return invalidParameter("Verification messages must contain the placeholder %s.", codeTemplate)
		}
	}

	if t := st.VerificationMessageTemplate; t != nil {
		if !oneOf(t.DefaultEmailOption, "CONFIRM_WITH_LINK", "CONFIRM_WITH_CODE") {
			return invalidParameter("DefaultEmailOption must be CONFIRM_WITH_LINK or CONFIRM_WITH_CODE.")
		}

		for _, message := range []*string{t.EmailMessage, t.SmsMessage} {
			if message != nil && !strings.Contains(*message, codeTemplate) {
				return invalidParameter("Verification messages must contain the placeholder %s.", codeTemplate)
			}
		}
	}

	if a := st.AdminCreateUserConfig; a != nil {
		if days := value(a.UnusedAccountValidityDays); days < 0 || days > 365 {
			return invalidParameter("UnusedAccountValidityDays must be between 0 and 365.")
		}

		if t := a.InviteMessageTemplate; t != nil {
			for _, message := range []*string{t.EmailMessage, t.SMSMessage} {
				if message != nil && (!strings.Contains(*message, codeTemplate) ||
					!strings.Contains(*message, "{username}")) {
					return invalidParameter("Invitation messages must contain the placeholders {username} and %s.",
						codeTemplate)
				}
			}
		}
	}

	return nil
}

func checkRecovery(r *api.AccountRecoverySettingType) error {
	if r == nil {
		return nil
	}

	seen := map[int64]bool{}
	for _, m := range r.RecoveryMechanisms {
		name, priority := value(m.Name), value(m.Priority)
		if !oneOf(m.Name, "verified_email", "verified_phone_number", "admin_only") || name == "" ||
			priority < 1 || priority > 2 || seen[priority] {
			return invalidParameter("Invalid account recovery setting.")
		}

		if name == "admin_only" && len(r.RecoveryMechanisms) > 1 {
			return invalidParameter("admin_only cannot be combined with other recovery mechanisms.")
		}

		seen[priority] = true
	}

	return nil
}

// passwordPolicy returns the pool's password policy, with defaults.
func passwordPolicy(st *settings) api.PasswordPolicyType {
	pp := api.PasswordPolicyType{}
	if st.Policies != nil && st.Policies.PasswordPolicy != nil {
		pp = *st.Policies.PasswordPolicy
	}

	if pp.MinimumLength == nil {
		pp.MinimumLength = new(int64(8))
	}

	if pp.TemporaryPasswordValidityDays == nil {
		pp.TemporaryPasswordValidityDays = new(int64(7))
	}

	for _, b := range []**bool{&pp.RequireUppercase, &pp.RequireLowercase, &pp.RequireNumbers, &pp.RequireSymbols} {
		if *b == nil {
			*b = new(false)
		}
	}

	return pp
}

// defaults fills the settings AWS reports even when they were not given.
func defaults(st *settings) {
	pp := passwordPolicy(st)
	st.Policies = &api.UserPoolPolicyType{PasswordPolicy: &pp}
	if st.DeletionProtection == nil {
		st.DeletionProtection = new("INACTIVE")
	}

	if st.MfaConfiguration == nil {
		st.MfaConfiguration = new(mfaOff)
	}

	if st.UserPoolTier == nil {
		st.UserPoolTier = new("ESSENTIALS")
	}

	if st.VerificationMessageTemplate == nil {
		st.VerificationMessageTemplate = &api.VerificationMessageTemplateType{}
	}

	if st.VerificationMessageTemplate.DefaultEmailOption == nil {
		st.VerificationMessageTemplate.DefaultEmailOption = new("CONFIRM_WITH_CODE")
	}

	if st.EmailConfiguration == nil {
		st.EmailConfiguration = &api.EmailConfigurationType{}
	}

	if st.EmailConfiguration.EmailSendingAccount == nil {
		st.EmailConfiguration.EmailSendingAccount = new("COGNITO_DEFAULT")
	}

	if st.AdminCreateUserConfig == nil {
		st.AdminCreateUserConfig = &api.AdminCreateUserConfigType{}
	}

	if st.AdminCreateUserConfig.AllowAdminCreateUserOnly == nil {
		st.AdminCreateUserConfig.AllowAdminCreateUserOnly = new(false)
	}

	if st.AdminCreateUserConfig.UnusedAccountValidityDays == nil {
		st.AdminCreateUserConfig.UnusedAccountValidityDays = new(int64(7))
	}
}

func checkSignIn(alias, username []string) error {
	if len(alias) > 0 && len(username) > 0 {
		return invalidParameter("Only one of the aliasAttributes or usernameAttributes can be set in a User Pool.")
	}

	for _, attr := range alias {
		if attr != attrEmail && attr != attrPhone && attr != attrPreferred {
			return invalidParameter("Invalid alias attribute %s.", attr)
		}
	}

	for _, attr := range username {
		if attr != attrEmail && attr != attrPhone {
			return invalidParameter("Invalid username attribute %s.", attr)
		}
	}

	return nil
}

func (p *pool) output(scope request.Scope, users int) (*api.UserPoolType, error) {
	out, err := convert[api.UserPoolType](p.Settings)
	if err != nil {
		return nil, err
	}

	out.Id, out.Name, out.Arn = new(p.ID), new(p.Name), new(poolARN(scope, p.ID))
	out.CreationDate, out.LastModifiedDate = seconds(p.Created), seconds(p.Modified)
	out.SchemaAttributes, out.AliasAttributes, out.UsernameAttributes = p.Schema, p.AliasAttributes, p.UsernameAttributes
	out.EstimatedNumberOfUsers = new(int64(users))
	out.UserPoolTags = map[string]string{}
	maps.Copy(out.UserPoolTags, p.Tags)
	if p.CaseSensitive != nil {
		out.UsernameConfiguration = &api.UsernameConfigurationType{CaseSensitive: p.CaseSensitive}
	}

	return &out, nil
}

func (s *Service) CreateUserPool(
	ctx context.Context,
	in *api.CreateUserPoolRequest,
) (*api.CreateUserPoolResponse, error) {
	scope, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	if nameErr := checkName(in.PoolName, "PoolName"); nameErr != nil {
		return nil, nameErr
	}

	if signInErr := checkSignIn(in.AliasAttributes, in.UsernameAttributes); signInErr != nil {
		return nil, signInErr
	}

	if tagErr := tagging.Check(in.UserPoolTags); tagErr != nil {
		return nil, awserror.New("UserPoolTaggingException", tagErr.Error())
	}

	if in.UsernameConfiguration != nil && in.UsernameConfiguration.CaseSensitive == nil {
		return nil, invalidParameter("UsernameConfiguration requires CaseSensitive.")
	}

	st, err := convert[settings](in)
	if err != nil {
		return nil, err
	}

	if settingsErr := validSettings(&st, false); settingsErr != nil {
		return nil, settingsErr
	}

	now := s.now().UTC()
	p := &pool{
		ID: scope.Region + "_" + randomString(alphaNum, 9), Name: *in.PoolName, Created: now, Modified: now,
		Settings: st, Tags: in.UserPoolTags, Schema: standardSchema(), AliasAttributes: in.AliasAttributes,
		UsernameAttributes: in.UsernameAttributes,
	}
	if in.UsernameConfiguration != nil {
		p.CaseSensitive = in.UsernameConfiguration.CaseSensitive
	}

	if schemaErr := p.addSchema(in.Schema, true); schemaErr != nil {
		return nil, schemaErr
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if len(tx.Keys(prefix(scope, "pool"))) >= 1000 {
			return awserror.New("LimitExceededException", "The account has reached its limit of 1,000 user pools.")
		}

		tx.Put(poolIndexKey(p.ID), []byte(scope.AccountID))
		return write(tx, poolKey(scope, p.ID), p)
	})
	if err != nil {
		return nil, err
	}

	out, err := p.output(scope, 0)
	if err != nil {
		return nil, err
	}

	return &api.CreateUserPoolResponse{UserPool: out}, nil
}

func (s *Service) DescribeUserPool(
	ctx context.Context,
	in *api.DescribeUserPoolRequest,
) (*api.DescribeUserPoolResponse, error) {
	var out *api.UserPoolType
	err := s.view(ctx, in.UserPoolId, func(tx storage.ReadTx, scope request.Scope, p *pool) error {
		var outErr error
		out, outErr = p.output(scope, len(tx.Keys(userPrefix(scope, p.ID))))
		return outErr
	})
	if err != nil {
		return nil, err
	}

	return &api.DescribeUserPoolResponse{UserPool: out}, nil
}

func (s *Service) ListUserPools(
	ctx context.Context,
	in *api.ListUserPoolsRequest,
) (*api.ListUserPoolsResponse, error) {
	scope, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	if in.MaxResults == nil || *in.MaxResults < 1 || *in.MaxResults > maxPools {
		return nil, invalidParameter("MaxResults must be between 1 and %d.", maxPools)
	}

	var pools []*pool
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, id := range ids(tx, prefix(scope, "pool")) {
			p, _, readErr := read[pool](tx, poolKey(scope, id))
			if readErr != nil {
				return readErr
			}

			pools = append(pools, p)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := page(pools, func(p *pool) string { return p.ID }, in.NextToken, int(*in.MaxResults))
	if err != nil {
		return nil, err
	}

	out := &api.ListUserPoolsResponse{UserPools: []api.UserPoolDescriptionType{}, NextToken: next}
	for _, p := range selected {
		out.UserPools = append(out.UserPools, api.UserPoolDescriptionType{
			Id: new(p.ID), Name: new(p.Name), CreationDate: seconds(p.Created), LastModifiedDate: seconds(p.Modified),
		})
	}

	return out, nil
}

func (s *Service) UpdateUserPool(
	ctx context.Context,
	in *api.UpdateUserPoolRequest,
) (*api.UpdateUserPoolResponse, error) {
	if in.PoolName != nil {
		if err := checkName(in.PoolName, "PoolName"); err != nil {
			return nil, err
		}
	}

	if err := tagging.Check(in.UserPoolTags); err != nil {
		return nil, awserror.New("UserPoolTaggingException", err.Error())
	}

	st, err := convert[settings](in)
	if err != nil {
		return nil, err
	}

	err = s.update(ctx, in.UserPoolId, func(_ storage.WriteTx, _ request.Scope, p *pool) error {
		if settingsErr := validSettings(&st, p.SoftwareTokenMFA); settingsErr != nil {
			return settingsErr
		}

		p.Settings, p.Modified = st, s.now().UTC()
		if in.PoolName != nil {
			p.Name = *in.PoolName
		}

		if in.UserPoolTags != nil {
			p.Tags = in.UserPoolTags
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateUserPoolResponse{}, nil
}

func (s *Service) DeleteUserPool(
	ctx context.Context,
	in *api.DeleteUserPoolRequest,
) (*api.DeleteUserPoolOutput, error) {
	scope, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		p, loadErr := loadPool(tx, scope, in.UserPoolId)
		if loadErr != nil {
			return loadErr
		}

		if value(p.Settings.DeletionProtection) == "ACTIVE" {
			return invalidParameter("The user pool cannot be deleted because deletion protection is activated. " +
				"Deletion protection must be inactivated first.")
		}

		for _, id := range ids(tx, prefix(scope, "client")+p.ID+"/") {
			tx.Delete(clientIndexKey(id))
		}

		for _, kind := range poolKinds {
			for _, k := range tx.Keys(prefix(scope, kind) + p.ID + "/") {
				tx.Delete(k)
			}
		}

		tx.Delete(poolKey(scope, p.ID))
		tx.Delete(poolIndexKey(p.ID))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteUserPoolOutput{}, nil
}

func (s *Service) AddCustomAttributes(
	ctx context.Context,
	in *api.AddCustomAttributesRequest,
) (*api.AddCustomAttributesResponse, error) {
	if len(in.CustomAttributes) == 0 || len(in.CustomAttributes) > 25 {
		return nil, invalidParameter("CustomAttributes must have 1 to 25 attributes.")
	}

	err := s.update(ctx, in.UserPoolId, func(_ storage.WriteTx, _ request.Scope, p *pool) error {
		for i := range in.CustomAttributes {
			name := value(in.CustomAttributes[i].Name)
			if p.attribute(name) != nil && !strings.HasPrefix(name, customPrefix) {
				return invalidParameter("Existing attribute already has name %s.", name)
			}
		}

		p.Modified = s.now().UTC()
		return p.addSchema(in.CustomAttributes, false)
	})
	if err != nil {
		return nil, err
	}

	return &api.AddCustomAttributesResponse{}, nil
}

func (s *Service) GetUserPoolMfaConfig(
	ctx context.Context,
	in *api.GetUserPoolMfaConfigRequest,
) (*api.GetUserPoolMfaConfigResponse, error) {
	var out *api.GetUserPoolMfaConfigResponse
	err := s.view(ctx, in.UserPoolId, func(_ storage.ReadTx, _ request.Scope, p *pool) error {
		out = mfaOutput(p)
		return nil
	})

	return out, err
}

func mfaOutput(p *pool) *api.GetUserPoolMfaConfigResponse {
	return &api.GetUserPoolMfaConfigResponse{
		MfaConfiguration:              p.Settings.MfaConfiguration,
		SoftwareTokenMfaConfiguration: &api.SoftwareTokenMfaConfigType{Enabled: new(p.SoftwareTokenMFA)},
	}
}

// SetUserPoolMfaConfig turns software token MFA on or off. Users never set up
// a token, so an OPTIONAL pool never challenges them.
func (s *Service) SetUserPoolMfaConfig(
	ctx context.Context,
	in *api.SetUserPoolMfaConfigRequest,
) (*api.SetUserPoolMfaConfigResponse, error) {
	var out *api.GetUserPoolMfaConfigResponse
	err := s.update(ctx, in.UserPoolId, func(_ storage.WriteTx, _ request.Scope, p *pool) error {
		token := p.SoftwareTokenMFA
		if in.SoftwareTokenMfaConfiguration != nil {
			token = value(in.SoftwareTokenMfaConfiguration.Enabled)
		}

		mfa := in.MfaConfiguration
		if mfa == nil {
			mfa = p.Settings.MfaConfiguration
		}

		if err := checkMFA(mfa, token || p.Settings.SmsConfiguration != nil); err != nil {
			return err
		}

		p.SoftwareTokenMFA, p.Settings.MfaConfiguration, p.Modified = token, mfa, s.now().UTC()
		out = mfaOutput(p)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.SetUserPoolMfaConfigResponse{
		MfaConfiguration: out.MfaConfiguration, SoftwareTokenMfaConfiguration: out.SoftwareTokenMfaConfiguration,
	}, nil
}
