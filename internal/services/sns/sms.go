package sns

import (
	"context"
	"regexp"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/sns"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/delivery"
)

// protocolSMS subscriptions, and Publish to a PhoneNumber, send no text
// message: the delivery log records it, with the phone number as its target.
const protocolSMS = "sms"

// phoneNumber matches an E.164 phone number.
var phoneNumber = regexp.MustCompile(`^\+[1-9]\d{4,14}$`)

// smsAttributes are the reserved message attributes that set an SMS
// message's sender, type, price, and origination; SNS accepts them despite
// their AWS. prefix.
var smsAttributes = map[string]bool{
	"AWS.SNS.SMS.SenderID": true, "AWS.SNS.SMS.SMSType": true, "AWS.SNS.SMS.MaxPrice": true,
	"AWS.MM.SMS.OriginationNumber": true, "AWS.MM.SMS.EntityId": true, "AWS.MM.SMS.TemplateId": true,
}

func checkPhoneNumber(number string) error {
	if !phoneNumber.MatchString(number) {
		return invalid("PhoneNumber Reason: %s is not valid to publish to", number)
	}

	return nil
}

// publishSMS sends a message straight to a phone number.
func (s *Service) publishSMS(ctx context.Context, in *api.PublishInput) (*api.PublishResponse, error) {
	switch {
	case in.TopicArn != nil:
		return nil, invalid("TopicArn and PhoneNumber are mutually exclusive")
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

	s.deliveries.Add(delivery.ViaSNS, "", number, []byte(textFor(protocolSMS)), nil)
	return &api.PublishResponse{MessageId: new(newID())}, nil
}
