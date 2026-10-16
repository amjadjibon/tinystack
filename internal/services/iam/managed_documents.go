package iam

import (
	"context"
	"fmt"

	iampolicy "github.com/amjadjibon/tinystack/internal/services/iam/policy"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// These exact AWS-managed documents form the local catalog. Unknown AWS-managed
// policies remain unavailable, rather than being approximated with broad allows.
// https://docs.aws.amazon.com/aws-managed-policy/latest/reference/AdministratorAccess.html
// https://docs.aws.amazon.com/aws-managed-policy/latest/reference/AmazonS3FullAccess.html
// https://docs.aws.amazon.com/aws-managed-policy/latest/reference/AmazonSQSFullAccess.html
var awsManagedDocuments = map[string]string{
	"AdministratorAccess": `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`,
	"AmazonS3FullAccess": `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
		`"Action":["s3:*","s3-object-lambda:*"],"Resource":"*"}]}`,
	"AmazonSQSFullAccess": `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"sqs:*","Resource":"*"}]}`,
}

func managedDocument(tx storage.ReadTx, account, policyARN string) (iampolicy.Document, error) {
	ref, err := parsePolicyARN(policyARN)
	if err != nil {
		return iampolicy.Document{}, err
	}

	if ref.awsManaged() {
		document, exists := awsManagedDocuments[ref.name]
		if !exists || ref.path != "/" {
			return iampolicy.Document{}, noSuchEntity(
				"Policy %s does not exist in TinyStack's AWS-managed catalog.",
				policyARN,
			)
		}

		return iampolicy.Document{Name: policyARN, JSON: document}, nil
	}

	managed, _, err := loadPolicy(tx, account, ref)
	if err != nil {
		return iampolicy.Document{}, err
	}

	for _, version := range managed.Versions {
		if version.ID == managed.DefaultVersion {
			return iampolicy.Document{Name: policyARN + "/" + version.ID, JSON: version.Document}, nil
		}
	}

	return iampolicy.Document{}, fmt.Errorf("managed policy %s has no default document", policyARN)
}

// ValidateSessionPolicies shares IAM's identity-document validator and requires
// every managed session policy to exist in the role account (or local AWS catalog).
func (s *Service) ValidateSessionPolicies(ctx context.Context, account, inline string, arns []string) error {
	if inline != "" {
		if err := checkDocument("policyDocument", &inline); err != nil {
			return err
		}
	}

	return s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, arn := range arns {
			if _, err := managedDocument(tx, account, arn); err != nil {
				return err
			}
		}

		return nil
	})
}

func collectLimits(tx storage.ReadTx, account, boundary string, out *Authorization) error {
	if boundary != "" {
		document, err := managedDocument(tx, account, boundary)
		if err != nil {
			return fmt.Errorf("permissions boundary: %w", err)
		}

		out.Boundary = []iampolicy.Document{document}
	}

	p := &out.Principal
	if p.SessionPolicy != "" {
		out.Session = append(out.Session, iampolicy.Document{Name: "session-policy:inline", JSON: p.SessionPolicy})
	}

	for _, arn := range p.SessionPolicyARNs {
		document, err := managedDocument(tx, account, arn)
		if err != nil {
			return fmt.Errorf("session policy: %w", err)
		}

		out.Session = append(out.Session, document)
	}

	return nil
}
