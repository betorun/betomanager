package domain

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParse(t *testing.T) {
	s, err := ParseStatus(" waiting_customer ")
	require.NoError(t, err)
	assert.Equal(t, StatusWaitingCustomer, s)
	_, err = ParseStatus("pending")
	assert.ErrorIs(t, err, ErrInvalidStatus)

	p, err := ParsePriority("urgent")
	require.NoError(t, err)
	assert.Equal(t, PriorityUrgent, p)
	_, err = ParsePriority("")
	assert.ErrorIs(t, err, ErrInvalidPriority)

	c, err := ParseCategory("request")
	require.NoError(t, err)
	assert.Equal(t, CategoryRequest, c)
	c, err = ParseCategory("")
	require.NoError(t, err, "sans catégorie")
	assert.Empty(t, c)
	_, err = ParseCategory("LOGIN")
	assert.ErrorIs(t, err, ErrInvalidCategory, "catégorie propre à SYK")
}

func TestCleanTexts(t *testing.T) {
	s, err := CleanSubject("  Ouvrir   le port 25 ")
	require.NoError(t, err)
	assert.Equal(t, "Ouvrir le port 25", s)
	_, err = CleanSubject("ab")
	assert.ErrorIs(t, err, ErrInvalidSubject)
	_, err = CleanSubject(strings.Repeat("é", 151))
	assert.ErrorIs(t, err, ErrInvalidSubject)
	_, err = CleanSubject(strings.Repeat("é", 150))
	assert.NoError(t, err, "150 caractères, pas 150 octets")

	_, err = CleanBody("   ", ErrInvalidBody)
	assert.ErrorIs(t, err, ErrInvalidBody)
	_, err = CleanBody(strings.Repeat("a", BodyMax+1), ErrInvalidDescription)
	assert.ErrorIs(t, err, ErrInvalidDescription)
	b, err := CleanBody(" bonjour\n ", ErrInvalidBody)
	require.NoError(t, err)
	assert.Equal(t, "bonjour", b)
}

func TestTags(t *testing.T) {
	tags, err := CleanTags([]string{" Port 25 ", "smtp", "SMTP", ""})
	require.NoError(t, err)
	assert.Equal(t, []string{"port 25", "smtp"}, tags)
	_, err = CleanTags([]string{strings.Repeat("x", TagMax+1)})
	assert.ErrorIs(t, err, ErrInvalidTags)
	many := make([]string, MaxTags+1)
	for i := range many {
		many[i] = strings.Repeat("t", i+1)
	}
	_, err = CleanTags(many)
	assert.ErrorIs(t, err, ErrInvalidTags)

	added, removed := TagDiff([]string{"a", "b"}, []string{"b", "c"})
	assert.Equal(t, []string{"c"}, added)
	assert.Equal(t, []string{"a"}, removed)
}

func TestTransitions(t *testing.T) {
	next, err := AfterCustomerMessage(StatusWaitingCustomer)
	require.NoError(t, err)
	assert.Equal(t, StatusInProgress, next, "le client répond : à traiter de nouveau")
	next, _ = AfterCustomerMessage(StatusOpen)
	assert.Equal(t, StatusOpen, next)
	next, _ = AfterCustomerMessage(StatusResolved)
	assert.Equal(t, StatusOpen, next, "répondre à un ticket résolu le rouvre")
	_, err = AfterCustomerMessage(StatusClosed)
	assert.ErrorIs(t, err, ErrTicketClosed)

	assert.Equal(t, StatusWaitingCustomer, AfterAgentMessage(StatusOpen))

	next, changed, err := Reopen(StatusResolved)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, StatusOpen, next)
	_, changed, err = Reopen(StatusInProgress)
	require.NoError(t, err)
	assert.False(t, changed)
	_, _, err = Reopen(StatusClosed)
	assert.ErrorIs(t, err, ErrTicketClosed)

	now := time.Now()
	tk := &Ticket{Status: StatusOpen}
	tk.SetStatus(StatusResolved, now)
	require.NotNil(t, tk.ClosedAt)
	tk.SetStatus(StatusClosed, now.Add(time.Hour))
	assert.Equal(t, now, *tk.ClosedAt, "date de la première clôture gardée")
	tk.SetStatus(StatusOpen, now)
	assert.Nil(t, tk.ClosedAt)
}

func TestFiles(t *testing.T) {
	m, err := DetectMIME([]byte("%PDF-1.7\n"))
	require.NoError(t, err)
	assert.Equal(t, "application/pdf", m)
	m, err = DetectMIME([]byte("bonjour"))
	require.NoError(t, err)
	assert.Equal(t, "text/plain", m)
	_, err = DetectMIME([]byte("<html><script>alert(1)</script>"))
	assert.ErrorIs(t, err, ErrAttachmentType, "pas de HTML")
	_, err = DetectMIME([]byte("MZ\x90\x00\x03\x00\x00\x00"))
	assert.ErrorIs(t, err, ErrAttachmentType)

	assert.Equal(t, "passwd", CleanFileName("../../etc/passwd"))
	assert.Equal(t, "capture.png", CleanFileName(`C:\Users\a\capture.png`))
	assert.Equal(t, "ab.txt", CleanFileName("a\"b\n.txt"))
	assert.Equal(t, "fichier", CleanFileName(".."))
}

func TestPage(t *testing.T) {
	l, o := Page(0, -3)
	assert.Equal(t, DefaultLimit, l)
	assert.Equal(t, 0, o)
	l, _ = Page(500, 0)
	assert.Equal(t, MaxLimit, l)
}
