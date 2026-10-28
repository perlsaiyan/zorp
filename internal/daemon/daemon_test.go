package daemon

import (
	"errors"
	"testing"

	"github.com/perlsaiyan/zorp/internal/ipc"
)

func TestShownErrorWaitsOutOneNetworkFailure(t *testing.T) {
	err := errors.New("context deadline exceeded")
	if got := shownError(ipc.StateOffline, 1, err); got != "" {
		t.Errorf("first network failure shown: %q", got)
	}
	if got := shownError(ipc.StateOffline, 2, err); got != err.Error() {
		t.Errorf("second network failure = %q", got)
	}
	if got := shownError(ipc.StateIdle, 0, err); got != err.Error() {
		t.Errorf("other failures should show at once, got %q", got)
	}
}
