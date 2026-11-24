package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"

	"github.com/thekrauss/kubemanager/internal/core/opjournal"
)

// journalKeyCLI : crée la clé qui signe la recopie du journal dans l'audit.
// La clé privée va dans un fichier (0600, jamais écrasé) ; seule la clé
// publique s'affiche, à poser côté API (KUBEMANAGER_IAM_OPERATOR_JOURNAL_KEYS).
func journalKeyCLI(args []string) error {
	if len(args) != 1 {
		return errors.New("usage : operator journal-key <fichier de la clé privée>")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(args[0], os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("création du fichier de clé : %w", err)
	}
	if _, err := fmt.Fprintln(f, base64.StdEncoding.EncodeToString(priv.Seed())); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Printf("Clé privée écrite dans %s (OPERATOR_JOURNAL_KEY_FILE).\n", args[0])
	fmt.Printf("Identifiant : %s\n", opjournal.KeyID(pub))
	fmt.Printf("Clé publique, à ajouter côté API (KUBEMANAGER_IAM_OPERATOR_JOURNAL_KEYS, séparées par des espaces) :\n%s\n",
		base64.StdEncoding.EncodeToString(pub))
	return nil
}
