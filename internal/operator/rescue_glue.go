package operator

import (
	"errors"

	"github.com/thekrauss/kubemanager/internal/operator/rescue"
)

const rescueTokenPrefix = rescue.TokenPrefix

func isRescueRefusal(err error) bool { return errors.Is(err, rescue.ErrBadCredentials) }
func isRescueLocked(err error) bool  { return errors.Is(err, rescue.ErrLocked) }
func isRescueReason(err error) bool  { return errors.Is(err, rescue.ErrReason) }
