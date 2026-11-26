package repository

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDelta(t *testing.T) {
	assert.Equal(t, int64(50), delta(150, 100), "écart depuis le relevé précédent")
	assert.Equal(t, int64(30), delta(30, 100), "compteur remis à zéro : tout compte")
	assert.Equal(t, int64(0), delta(100, 100))
	assert.Equal(t, int64(0), delta(-1, 0), "valeur aberrante ignorée")
}
