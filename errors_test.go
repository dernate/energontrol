package energontrol

// The error types and how a caller classifies a failure through them.

import (
	"errors"
	"fmt"
	"testing"
)

func TestSessionStateErrorClassification(t *testing.T) {
	cases := []struct {
		got  SessionState
		want error
	}{
		{SessionOccupied, ErrSessionOccupied},
		{SessionBlocked, ErrSessionBlocked},
		{SessionAccessDenied, ErrAccessDenied},
		{SessionInsufficientRights, ErrInsufficientRights},
		{SessionIncorrectUserID, ErrIncorrectUserID},
		{SessionValueError, ErrSessionValue},
		{SessionReserved, ErrSessionState},
		{SessionState(234), ErrSessionState},
	}
	for _, tc := range cases {
		err := &SessionStateError{PlantNo: 3, Want: SessionFree, Got: tc.got}
		if !errors.Is(err, tc.want) {
			t.Errorf("state %d: err = %v, want it to wrap %v", tc.got, err, tc.want)
		}
	}
}

func TestItemErrorMessage(t *testing.T) {
	err := &ItemError{ItemName: ctrlItem(4), Reason: ErrBadQuality, Detail: "quality=bad"}
	want := fmt.Sprintf("energontrol: item %q: item quality is not good (quality=bad)", ctrlItem(4))
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if !errors.Is(err, ErrBadQuality) {
		t.Error("ItemError should unwrap to its reason")
	}
}
