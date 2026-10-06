package provision

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sestypes "github.com/aws/aws-sdk-go-v2/service/sesv2/types"
)

var goodUseCase = strings.Repeat("Our contacts opted in through our website forms with documented consent. ", 4)

func TestRequestProduction(t *testing.T) {
	f := newFakeAWS()
	res, err := New(f.clients()).RequestProduction(context.Background(), ProductionInput{
		Domain: "Example.COM.", OwnerEmail: " Owner@Example.com ", UseCase: "  " + goodUseCase + "\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Submitted || res.ReviewStatus != "PENDING" || res.CaseID != "case-1" {
		t.Errorf("result = %+v", res)
	}
	d := f.details
	if d == nil {
		t.Fatal("PutAccountDetails not called")
	}
	if d.MailType != sestypes.MailTypeMarketing || aws.ToString(d.WebsiteURL) != "https://example.com" ||
		d.ContactLanguage != sestypes.ContactLanguageEn || !aws.ToBool(d.ProductionAccessEnabled) ||
		len(d.AdditionalContactEmailAddresses) != 1 || d.AdditionalContactEmailAddresses[0] != "owner@example.com" ||
		aws.ToString(d.UseCaseDescription) != strings.TrimSpace(goodUseCase) {
		t.Errorf("details = %+v", d)
	}
}

func TestRequestProductionAlreadyEnabled(t *testing.T) {
	f := newFakeAWS()
	f.account.ProductionAccessEnabled = true
	res, err := New(f.clients()).RequestProduction(context.Background(), ProductionInput{Domain: testDomain, OwnerEmail: "o@example.com", UseCase: goodUseCase})
	if err != nil {
		t.Fatal(err)
	}
	if res.Submitted || !res.ProductionAccessEnabled || len(f.mutations) != 0 {
		t.Errorf("result %+v mutations %v", res, f.mutations)
	}
}

func TestRequestProductionValidation(t *testing.T) {
	tests := []struct {
		name string
		in   ProductionInput
		want string
	}{
		{"empty use case", ProductionInput{Domain: testDomain, OwnerEmail: "o@example.com", UseCase: "  \n"}, "empty"},
		{"short use case", ProductionInput{Domain: testDomain, OwnerEmail: "o@example.com", UseCase: "We send newsletters."}, "too short"},
		{"long use case", ProductionInput{Domain: testDomain, OwnerEmail: "o@example.com", UseCase: strings.Repeat("x", MaxUseCaseLength+1)}, "too long"},
		{"no owner", ProductionInput{Domain: testDomain, UseCase: goodUseCase}, "OWNER_EMAIL is required"},
		{"bad owner", ProductionInput{Domain: testDomain, OwnerEmail: "nope", UseCase: goodUseCase}, "OWNER_EMAIL is not a valid"},
		{"no domain", ProductionInput{OwnerEmail: "o@example.com", UseCase: goodUseCase}, "DOMAIN"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeAWS()
			_, err := New(f.clients()).RequestProduction(context.Background(), tt.in)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
			if len(f.calls) != 0 {
				t.Errorf("called AWS: %v", f.calls)
			}
		})
	}
}
