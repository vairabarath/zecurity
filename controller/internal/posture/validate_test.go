package posture

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func validReportInput(now time.Time) ReportInput {
	return ReportInput{
		ReportID:      uuid.NewString(),
		ReportedAt:    now,
		ClientVersion: "1.0.0",
		OSName:        "linux",
		OSVersion:     "test",
		Checks: []CheckInput{{
			CheckID: CheckLUKS,
			Status:  StatusPass,
			Detail:  "encrypted",
		}},
	}
}

func TestValidateReportAcceptsValidInput(t *testing.T) {
	now := time.Now().UTC()
	got, err := ValidateReport(now, validReportInput(now))
	if err != nil {
		t.Fatalf("ValidateReport: %v", err)
	}
	if len(got.Observations) != 1 || got.Observations[0].CheckID != CheckLUKS {
		t.Fatalf("observations = %#v", got.Observations)
	}
}

func TestValidateReportRejectsInvalidReportFields(t *testing.T) {
	now := time.Now().UTC()

	tests := []struct {
		name string
		edit func(*ReportInput)
		want error
	}{
		{
			name: "noncanonical report id",
			edit: func(in *ReportInput) { in.ReportID = strings.ToUpper(in.ReportID) },
			want: ErrInvalidReportID,
		},
		{
			name: "too old",
			edit: func(in *ReportInput) { in.ReportedAt = now.Add(-MaxReportAge - time.Second) },
			want: ErrInvalidReportedTime,
		},
		{
			name: "too far in future",
			edit: func(in *ReportInput) { in.ReportedAt = now.Add(MaxFutureClockSkew + time.Second) },
			want: ErrInvalidReportedTime,
		},
		{
			name: "too many checks",
			edit: func(in *ReportInput) {
				in.Checks = make([]CheckInput, MaxChecks+1)
				for i := range in.Checks {
					in.Checks[i] = CheckInput{CheckID: CheckLUKS, Status: StatusPass}
				}
			},
			want: ErrInvalidReport,
		},
		{
			name: "oversized client version",
			edit: func(in *ReportInput) { in.ClientVersion = strings.Repeat("x", MaxMetadataBytes+1) },
			want: ErrInvalidReport,
		},
		{
			name: "invalid metadata utf8",
			edit: func(in *ReportInput) { in.OSName = string([]byte{0xff}) },
			want: ErrInvalidReport,
		},
		{
			name: "empty checks",
			edit: func(in *ReportInput) { in.Checks = nil },
			want: ErrNoRecognizedChecks,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := validReportInput(now)
			tt.edit(&input)
			_, err := ValidateReport(now, input)
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestValidateReportFiltersUnknownAndInvalidChecksIndividually(t *testing.T) {
	now := time.Now().UTC()
	input := validReportInput(now)
	input.Checks = []CheckInput{
		{CheckID: "linux.future.check", Status: StatusPass},
		{CheckID: CheckFirewall, Status: StatusPass, Detail: "active"},
		{CheckID: CheckSecureBoot, Status: StatusPass, Detail: strings.Repeat("x", MaxDetailBytes+1)},
		{CheckID: CheckOSVersion, Status: "INVALID"},
	}

	got, err := ValidateReport(now, input)
	if err != nil {
		t.Fatalf("ValidateReport: %v", err)
	}
	if len(got.Observations) != 1 || got.Observations[0].CheckID != CheckFirewall {
		t.Fatalf("observations = %#v, want only firewall", got.Observations)
	}
}

func TestValidateReportRejectsNoValidRecognizedChecks(t *testing.T) {
	now := time.Now().UTC()
	input := validReportInput(now)
	input.Checks = []CheckInput{{CheckID: "linux.future.check", Status: StatusPass}}

	_, err := ValidateReport(now, input)
	if !errors.Is(err, ErrNoRecognizedChecks) {
		t.Fatalf("error = %v, want %v", err, ErrNoRecognizedChecks)
	}
}

func TestValidateReportRejectsDuplicateCheckIDs(t *testing.T) {
	now := time.Now().UTC()
	input := validReportInput(now)
	input.Checks = []CheckInput{
		{CheckID: CheckLUKS, Status: StatusPass},
		{CheckID: CheckLUKS, Status: StatusFail},
	}

	_, err := ValidateReport(now, input)
	if !errors.Is(err, ErrDuplicateCheck) {
		t.Fatalf("error = %v, want %v", err, ErrDuplicateCheck)
	}
}
