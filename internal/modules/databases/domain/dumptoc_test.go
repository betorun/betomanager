package domain

import (
	"bytes"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testdata/pg16.dump et pg17.dump : base « shop » (schéma shop : a, b ;
// public : p partitionnée et sa partition p1), formats d'archive 1.15 et 1.16.
func TestReadDumpTOC(t *testing.T) {
	for _, name := range []string{"pg16.dump", "pg17.dump"} {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile("testdata/" + name)
			require.NoError(t, err)
			got, err := ReadDumpTOC(bytes.NewReader(raw))
			require.NoError(t, err)
			assert.Equal(t, "shop", got.Database)
			assert.True(t, got.Complete)
			assert.Equal(t, []SchemaTables{{Name: "public", Tables: 2}, {Name: "shop", Tables: 2}}, got.Schemas)
			assert.Equal(t, 4, got.Tables)
		})
	}
}

// Fichier coupé au milieu de la table des matières : ce qui a été lu, sans erreur.
func TestReadDumpTOC_Truncated(t *testing.T) {
	raw, err := os.ReadFile("testdata/pg16.dump")
	require.NoError(t, err)
	got, err := ReadDumpTOC(bytes.NewReader(raw[:700]))
	require.NoError(t, err)
	assert.False(t, got.Complete)
	assert.Equal(t, "shop", got.Database)
}

func TestReadDumpTOC_NotADump(t *testing.T) {
	_, err := ReadDumpTOC(bytes.NewReader([]byte("-- PostgreSQL database dump\nCREATE TABLE x();")))
	assert.ErrorIs(t, err, ErrNotADump)
	_, err = ReadDumpTOC(bytes.NewReader([]byte("PGDMP\x01\x0f\x00\x04\x08\x03"))) // format répertoire
	assert.ErrorIs(t, err, ErrDumpUnreadable)
}

// DUMP_TOC_FILE : un vrai dump local, jamais versionné (données privées).
func TestReadDumpTOC_LocalFile(t *testing.T) {
	path := os.Getenv("DUMP_TOC_FILE")
	if path == "" {
		t.Skip("DUMP_TOC_FILE non défini")
	}
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	got, err := ReadDumpTOC(f)
	require.NoError(t, err)
	t.Logf("%+v", got)
}
