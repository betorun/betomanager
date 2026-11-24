package apierr

import (
	"errors"
	"fmt"
	"testing"

	betoerrors "github.com/thekrauss/beto-shared/pkg/errors"
)

func TestReason(t *testing.T) {
	base := betoerrors.New(betoerrors.CodeConflict, "already open")
	err := WithReason(base, "ASSISTANCE_ALREADY_OPEN")
	wrapped := fmt.Errorf("approve: %w", err)
	if Reason(wrapped) != "ASSISTANCE_ALREADY_OPEN" || Reason(base) != "" || Reason(nil) != "" {
		t.Fatal("raison perdue")
	}
	var typed *betoerrors.Error
	if !errors.As(wrapped, &typed) || typed.Code != betoerrors.CodeConflict || !errors.Is(wrapped, err) {
		t.Fatal("code perdu")
	}
}
