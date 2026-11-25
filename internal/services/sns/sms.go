package sns

import (
	"context"
	"crypto/rand"
	"fmt"
	"maps"
	"math/big"
	"regexp"
	"strconv"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/sns"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/delivery"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// protocolSMS subscriptions, and Publish to a PhoneNumber, send no text
// message: the delivery log records it, with the phone number as its target.
const (
	protocolSMS           = "sms"
	kindSMS               = "sms"
	smsAttributesRecord   = "attributes"
	smsOptedOutPrefix     = "optedout/"
	smsSandboxPrefix      = "sandbox/"
	sandboxStatusPending  = "Pending"
	sandboxStatusVerified = "Verified"
	maxSandboxListResults = 100
	maxOriginationResults = 30
	otpUpperBound         = 1000000
)

var (
	// phoneNumber matches an E.164 phone number.
	phoneNumber = regexp.MustCompile(`^\+[1-9]\d{4,14}$`)
	senderID    = regexp.MustCompile(`^[A-Za-z0-9]{1,11}$`)
	hasLetter   = regexp.MustCompile(`[A-Za-z]`)

	// smsAttributes are the reserved message attributes that set an SMS
	// message's sender, type, price, and origination; SNS accepts them despite
	// their AWS. prefix.
	smsAttributes = map[string]bool{
		"AWS.SNS.SMS.SenderID": true, "AWS.SNS.SMS.SMSType": true, "AWS.SNS.SMS.MaxPrice": true,
		"AWS.MM.SMS.OriginationNumber": true, "AWS.MM.SMS.EntityId": true, "AWS.MM.SMS.TemplateId": true,
	}

	validLanguageCodes = map[string]bool{
		"en-US": true, "en-GB": true, "es-419": true, "es-ES": true, "de-DE": true,
		"fr-CA": true, "fr-FR": true, "it-IT": true, "ja-JP": true, "pt-BR": true,
		"kr-KR": true, "zh-CN": true, "zh-TW": true,
	}
)

type smsAttributesDoc struct {
	Attributes map[string]string `json:",omitempty"`
}

type optedOutRecord struct {
	PhoneNumber string
}

type smsSandboxRecord struct {
	PhoneNumber  string
	Status       string
	OTP          string `json:",omitempty"`
	LanguageCode string `json:",omitempty"`
}

func checkPhoneNumber(number string) error {
	if !phoneNumber.MatchString(number) {
		return invalid("PhoneNumber Reason: %s is not valid to publish to", number)
	}

	return nil
}

func newOTP() string {
	n, err := rand.Int(rand.Reader, big.NewInt(otpUpperBound))
	if err != nil {
		return "000000"
	}

	return fmt.Sprintf("%06d", n.Int64())
}

// publishSMS sends a message straight to a phone number.
func (s *Service) publishSMS(ctx context.Context, in *api.PublishInput) (*api.PublishResponse, error) {
	switch {
	case in.TopicArn != nil || in.TargetArn != nil:
		return nil, invalid("TopicArn, TargetArn, and PhoneNumber are mutually exclusive")
	case in.MessageGroupId != nil || in.MessageDeduplicationId != nil:
		return nil, invalid("MessageGroupId and MessageDeduplicationId are only supported for FIFO topics")
	}

	if _, err := request.Regional(ctx); err != nil {
		return nil, err
	}

	number := common.Value(in.PhoneNumber)
	if err := checkPhoneNumber(number); err != nil {
		return nil, err
	}

	textFor, err := checkMessage(&api.PublishBatchRequestEntry{
		Message: in.Message, Subject: in.Subject, MessageStructure: in.MessageStructure,
		MessageAttributes: in.MessageAttributes,
	})
	if err != nil {
		return nil, err
	}

	if s.deliveries != nil {
		s.deliveries.Add(delivery.ViaSNS, "", number, []byte(textFor(protocolSMS)), nil)
	}

	return &api.PublishResponse{MessageId: new(newID())}, nil
}

func checkSMSAttribute(name, v string) error {
	switch name {
	case "MonthlySpendLimit":
		if v == "" {
			return nil
		}

		n, err := strconv.ParseFloat(v, 64)
		if err != nil || n < 0 {
			return invalid("MonthlySpendLimit must be a non-negative number")
		}
	case "DeliveryStatusIAMRole":
		if v != "" && !iamRoleARN.MatchString(v) {
			return invalid("DeliveryStatusIAMRole must be a valid IAM role ARN")
		}
	case "DeliveryStatusSuccessSamplingRate":
		if v == "" {
			return nil
		}

		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 100 {
			return invalid("DeliveryStatusSuccessSamplingRate must be an integer from 0 to 100")
		}
	case "DefaultSenderID":
		if v != "" && (!senderID.MatchString(v) || !hasLetter.MatchString(v)) {
			return invalid("DefaultSenderID must be 1 to 11 alphanumeric characters with at least one letter")
		}
	case "DefaultSMSType":
		if v != "" && v != "Promotional" && v != "Transactional" {
			return invalid("DefaultSMSType must be Promotional or Transactional")
		}
	case "UsageReportS3Bucket":
	default:
		return invalid("SMS attribute %s is not supported", name)
	}

	return nil
}

func (s *Service) SetSMSAttributes(
	ctx context.Context,
	in *api.SetSMSAttributesInput,
) (*api.SetSMSAttributesResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	for k, v := range in.Attributes {
		if guardErr := checkSMSAttribute(k, v); guardErr != nil {
			return nil, guardErr
		}
	}

	key := m.Key(serviceName, kindSMS, smsAttributesRecord)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		doc, found, readErr := storage.Read[smsAttributesDoc](tx, key)
		if readErr != nil {
			return fmt.Errorf("decode sms attributes: %w", readErr)
		}

		if !found || doc.Attributes == nil {
			doc = &smsAttributesDoc{Attributes: map[string]string{}}
		}

		for k, v := range in.Attributes {
			if v == "" {
				delete(doc.Attributes, k)
				continue
			}

			doc.Attributes[k] = v
		}

		return storage.Write(tx, key, doc)
	})
	if err != nil {
		return nil, err
	}

	return &api.SetSMSAttributesResponse{}, nil
}

func (s *Service) GetSMSAttributes(
	ctx context.Context,
	in *api.GetSMSAttributesInput,
) (*api.GetSMSAttributesResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	key := m.Key(serviceName, kindSMS, smsAttributesRecord)
	stored := map[string]string{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		doc, found, readErr := storage.Read[smsAttributesDoc](tx, key)
		if readErr != nil {
			return fmt.Errorf("decode sms attributes: %w", readErr)
		}

		if found && doc.Attributes != nil {
			stored = maps.Clone(doc.Attributes)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	if len(in.Attributes) == 0 {
		return &api.GetSMSAttributesResponse{Attributes: stored}, nil
	}

	filtered := map[string]string{}
	for _, name := range in.Attributes {
		if v, ok := stored[name]; ok {
			filtered[name] = v
		}
	}

	return &api.GetSMSAttributesResponse{Attributes: filtered}, nil
}

func (s *Service) CheckIfPhoneNumberIsOptedOut(
	ctx context.Context,
	in *api.CheckIfPhoneNumberIsOptedOutInput,
) (*api.CheckIfPhoneNumberIsOptedOutResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	number := common.Value(in.PhoneNumber)
	if guardErr := checkPhoneNumber(number); guardErr != nil {
		return nil, guardErr
	}

	var optedOut bool
	key := m.Key(serviceName, kindSMS, smsOptedOutPrefix+number)
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		_, optedOut = tx.Get(key)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.CheckIfPhoneNumberIsOptedOutResponse{IsOptedOut: new(optedOut)}, nil
}

func (s *Service) ListPhoneNumbersOptedOut(
	ctx context.Context,
	in *api.ListPhoneNumbersOptedOutInput,
) (*api.ListPhoneNumbersOptedOutResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var numbers []string
	prefix := m.Key(serviceName, kindSMS, smsOptedOutPrefix)
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys(prefix) {
			numbers = append(numbers, strings.TrimPrefix(k, prefix))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := page(numbers, in.NextToken)
	if err != nil {
		return nil, err
	}

	if selected == nil {
		selected = []string{}
	}

	return &api.ListPhoneNumbersOptedOutResponse{PhoneNumbers: selected, NextToken: next}, nil
}

func (s *Service) OptInPhoneNumber(
	ctx context.Context,
	in *api.OptInPhoneNumberInput,
) (*api.OptInPhoneNumberResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	number := common.Value(in.PhoneNumber)
	if guardErr := checkPhoneNumber(number); guardErr != nil {
		return nil, guardErr
	}

	key := m.Key(serviceName, kindSMS, smsOptedOutPrefix+number)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		tx.Delete(key)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.OptInPhoneNumberResponse{}, nil
}

func (s *Service) CreateSMSSandboxPhoneNumber(
	ctx context.Context,
	in *api.CreateSMSSandboxPhoneNumberInput,
) (*api.CreateSMSSandboxPhoneNumberResult, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	number := common.Value(in.PhoneNumber)
	if guardErr := checkPhoneNumber(number); guardErr != nil {
		return nil, guardErr
	}

	lang := common.Value(in.LanguageCode)
	if lang != "" && !validLanguageCodes[lang] {
		return nil, invalid("LanguageCode %q is not supported", lang)
	}

	otp := newOTP()
	optOutKey := m.Key(serviceName, kindSMS, smsOptedOutPrefix+number)
	key := m.Key(serviceName, kindSMS, smsSandboxPrefix+number)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, optedOut := tx.Get(optOutKey); optedOut {
			return awserror.New("OptedOutException", "The specified phone number is opted out.")
		}

		existing, found, readErr := storage.Read[smsSandboxRecord](tx, key)
		if readErr != nil {
			return fmt.Errorf("decode sms sandbox phone number: %w", readErr)
		}

		if found && existing.Status == sandboxStatusVerified {
			return awserror.New("UserErrorException", "The specified phone number is already verified.")
		}

		rec := &smsSandboxRecord{
			PhoneNumber:  number,
			Status:       sandboxStatusPending,
			OTP:          otp,
			LanguageCode: lang,
		}
		return storage.Write(tx, key, rec)
	})
	if err != nil {
		return nil, err
	}

	if s.deliveries != nil {
		s.deliveries.Add(delivery.ViaSNS, "", number, []byte("Your AWS SNS verification code is "+otp), nil)
	}

	return &api.CreateSMSSandboxPhoneNumberResult{}, nil
}

func (s *Service) VerifySMSSandboxPhoneNumber(
	ctx context.Context,
	in *api.VerifySMSSandboxPhoneNumberInput,
) (*api.VerifySMSSandboxPhoneNumberResult, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	number := common.Value(in.PhoneNumber)
	if guardErr := checkPhoneNumber(number); guardErr != nil {
		return nil, guardErr
	}

	otp := common.Value(in.OneTimePassword)
	if otp == "" {
		return nil, invalid("OneTimePassword is required")
	}

	key := m.Key(serviceName, kindSMS, smsSandboxPrefix+number)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rec, found, readErr := storage.Read[smsSandboxRecord](tx, key)
		if readErr != nil {
			return fmt.Errorf("decode sms sandbox phone number: %w", readErr)
		}

		if !found {
			return awserror.New("ResourceNotFoundException",
				"The specified phone number does not exist in the SMS sandbox.")
		}

		if rec.Status == sandboxStatusVerified {
			return nil
		}

		if rec.OTP != otp {
			return awserror.New("VerificationException", "The one-time password is invalid.")
		}

		rec.Status = sandboxStatusVerified
		rec.OTP = ""
		return storage.Write(tx, key, rec)
	})
	if err != nil {
		return nil, err
	}

	return &api.VerifySMSSandboxPhoneNumberResult{}, nil
}

func (s *Service) DeleteSMSSandboxPhoneNumber(
	ctx context.Context,
	in *api.DeleteSMSSandboxPhoneNumberInput,
) (*api.DeleteSMSSandboxPhoneNumberResult, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	number := common.Value(in.PhoneNumber)
	if guardErr := checkPhoneNumber(number); guardErr != nil {
		return nil, guardErr
	}

	key := m.Key(serviceName, kindSMS, smsSandboxPrefix+number)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, found := tx.Get(key); !found {
			return awserror.New("ResourceNotFoundException",
				"The specified phone number does not exist in the SMS sandbox.")
		}

		tx.Delete(key)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteSMSSandboxPhoneNumberResult{}, nil
}

func (s *Service) ListSMSSandboxPhoneNumbers(
	ctx context.Context,
	in *api.ListSMSSandboxPhoneNumbersInput,
) (*api.ListSMSSandboxPhoneNumbersResult, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	limit := maxSandboxListResults
	if in.MaxResults != nil {
		if *in.MaxResults < 1 || *in.MaxResults > maxSandboxListResults {
			return nil, invalid("MaxResults must be between 1 and %d", maxSandboxListResults)
		}

		limit = int(*in.MaxResults)
	}

	var items []api.SMSSandboxPhoneNumber
	prefix := m.Key(serviceName, kindSMS, smsSandboxPrefix)
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		recs, readErr := storage.Records[smsSandboxRecord](tx, prefix)
		if readErr != nil {
			return readErr
		}

		for _, rec := range recs {
			items = append(items, api.SMSSandboxPhoneNumber{
				PhoneNumber: new(rec.PhoneNumber),
				Status:      new(rec.Status),
			})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := common.Page(
		items,
		func(p api.SMSSandboxPhoneNumber) string { return common.Value(p.PhoneNumber) },
		in.NextToken,
		limit,
		invalid("NextToken"),
	)
	if err != nil {
		return nil, err
	}

	if selected == nil {
		selected = []api.SMSSandboxPhoneNumber{}
	}

	return &api.ListSMSSandboxPhoneNumbersResult{PhoneNumbers: selected, NextToken: next}, nil
}

func (s *Service) GetSMSSandboxAccountStatus(
	ctx context.Context,
	_ *api.GetSMSSandboxAccountStatusInput,
) (*api.GetSMSSandboxAccountStatusResult, error) {
	if _, err := request.Regional(ctx); err != nil {
		return nil, err
	}

	return &api.GetSMSSandboxAccountStatusResult{IsInSandbox: new(true)}, nil
}

func (s *Service) ListOriginationNumbers(
	ctx context.Context,
	in *api.ListOriginationNumbersRequest,
) (*api.ListOriginationNumbersResult, error) {
	if _, err := request.Regional(ctx); err != nil {
		return nil, err
	}

	if in.MaxResults != nil && (*in.MaxResults < 1 || *in.MaxResults > maxOriginationResults) {
		return nil, invalid("MaxResults must be between 1 and %d", maxOriginationResults)
	}

	if common.Value(in.NextToken) != "" {
		return nil, invalid("NextToken")
	}

	return &api.ListOriginationNumbersResult{PhoneNumbers: []api.PhoneNumberInformation{}}, nil
}
