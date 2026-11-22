package domain

import (
	"errors"
	"fmt"
	"net/textproto"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/thekrauss/kubemanager/internal/infrastructure/mail"
)

func smtpErr(code int, msg string) error {
	return fmt.Errorf("mail: rcpt: %w", &textproto.Error{Code: code, Msg: msg})
}

func TestClassify(t *testing.T) {
	for _, c := range []struct {
		err  error
		want Outcome
	}{
		{nil, Sent},
		{errors.New("mail: connect: dial tcp: i/o timeout"), Retry},
		{smtpErr(421, "4.7.0 try again later"), Retry},
		{smtpErr(451, "4.3.0 temporary failure"), Retry},
		{smtpErr(535, "5.7.8 Username and Password not accepted"), Retry},
		{smtpErr(550, "5.1.1 The email account that you tried to reach does not exist"), Rejected},
		{smtpErr(553, "5.1.3 invalid recipient"), Rejected},
		{smtpErr(550, "5.7.1 rejected: sender reputation"), Retry},
		{smtpErr(550, "5.7.26 unauthenticated mail, DMARC policy"), Retry},
		{smtpErr(552, "5.3.4 message size exceeds fixed limit"), Failed},
		{smtpErr(554, "5.6.0 message content rejected"), Failed},
		{mail.ErrInvalidAttachment, Failed},
	} {
		assert.Equal(t, c.want, Classify(c.err), "%v", c.err)
	}
}

func TestNormalizeAndMask(t *testing.T) {
	assert.Equal(t, "awa@ex.sn", NormalizeEmail(" Awa Diop <AWA@Ex.SN> "))
	assert.Equal(t, "a***@ex.sn", MaskEmail("Awa@ex.sn"))
	assert.Equal(t, "***", MaskEmail("nope"))
	assert.Len(t, NewSuppression("x@y.z", ReasonRejected, SourceSMTP, string(make([]byte, 900))).Detail, maxDetail)
}
