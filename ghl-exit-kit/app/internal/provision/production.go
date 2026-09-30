package provision

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	sestypes "github.com/aws/aws-sdk-go-v2/service/sesv2/types"

	"mailroom/internal/core"
)

// Limits of the production access use-case text.
const (
	// MinUseCaseLength rejects placeholder texts; AWS reviewers expect a
	// real description of consent, volume and bounce handling.
	MinUseCaseLength = 200
	// MaxUseCaseLength is SES's limit for UseCaseDescription.
	MaxUseCaseLength = 5000
)

// ProductionInput configures RequestProduction.
type ProductionInput struct {
	Domain     string // DOMAIN; the website is https://DOMAIN
	OwnerEmail string // OWNER_EMAIL, the additional contact address
	UseCase    string // the reviewed use-case text
}

// ProductionResult is the outcome of RequestProduction.
type ProductionResult struct {
	// Submitted is false when production access was already enabled.
	Submitted               bool   `json:"submitted"`
	ProductionAccessEnabled bool   `json:"production_access_enabled"`
	ReviewStatus            string `json:"review_status,omitempty"`
	CaseID                  string `json:"case_id,omitempty"`
}

// RequestProduction submits the SES production access request (L-2):
// marketing mail, the website https://DOMAIN, English contact language,
// the owner as additional contact and the use-case text. It does nothing
// when the account already has production access.
func (p *Provisioner) RequestProduction(ctx context.Context, in ProductionInput) (ProductionResult, error) {
	domain := normalizeDomain(in.Domain)
	if err := validateDomain(domain); err != nil {
		return ProductionResult{}, err
	}
	if strings.TrimSpace(in.OwnerEmail) == "" {
		return ProductionResult{}, errors.New("OWNER_EMAIL is required: AWS sends review questions to it")
	}
	owner, err := core.NormalizeEmail(in.OwnerEmail)
	if err != nil {
		return ProductionResult{}, fmt.Errorf("OWNER_EMAIL is not a valid email address")
	}
	useCase, err := validateUseCase(in.UseCase)
	if err != nil {
		return ProductionResult{}, err
	}

	acct, err := p.Clients.SES.GetAccount(ctx, &sesv2.GetAccountInput{})
	if err != nil {
		return ProductionResult{}, fmt.Errorf("ses get-account: %w", err)
	}
	if acct.ProductionAccessEnabled {
		return ProductionResult{ProductionAccessEnabled: true, ReviewStatus: reviewStatus(acct)}, nil
	}
	_, err = p.Clients.SES.PutAccountDetails(ctx, &sesv2.PutAccountDetailsInput{
		MailType:                        sestypes.MailTypeMarketing,
		WebsiteURL:                      aws.String("https://" + domain),
		ContactLanguage:                 sestypes.ContactLanguageEn,
		UseCaseDescription:              aws.String(useCase),
		AdditionalContactEmailAddresses: []string{owner},
		ProductionAccessEnabled:         aws.Bool(true),
	})
	if err != nil {
		return ProductionResult{}, fmt.Errorf("ses put-account-details: %w", err)
	}
	res := ProductionResult{Submitted: true}
	if after, err := p.Clients.SES.GetAccount(ctx, &sesv2.GetAccountInput{}); err == nil {
		res.ProductionAccessEnabled = after.ProductionAccessEnabled
		res.ReviewStatus = reviewStatus(after)
		if after.Details != nil && after.Details.ReviewDetails != nil {
			res.CaseID = aws.ToString(after.Details.ReviewDetails.CaseId)
		}
	}
	return res, nil
}

func reviewStatus(acct *sesv2.GetAccountOutput) string {
	if acct.Details == nil || acct.Details.ReviewDetails == nil {
		return ""
	}
	return string(acct.Details.ReviewDetails.Status)
}

// validateUseCase trims the text and checks its length.
func validateUseCase(s string) (string, error) {
	t := strings.TrimSpace(s)
	n := utf8.RuneCountInString(t)
	switch {
	case n == 0:
		return "", errors.New("the use-case text is empty")
	case n < MinUseCaseLength:
		return "", fmt.Errorf("the use-case text is too short (%d characters, at least %d): describe how contacts opted in, the expected volume, bounce and complaint handling and unsubscribing", n, MinUseCaseLength)
	case n > MaxUseCaseLength:
		return "", fmt.Errorf("the use-case text is too long (%d characters, at most %d)", n, MaxUseCaseLength)
	}
	return t, nil
}
