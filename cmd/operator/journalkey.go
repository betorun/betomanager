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

// keyCLI : crée une clé Ed25519 de l'Opérateur. La clé privée va dans un
// fichier (0600, jamais écrasé) ; seule la clé publique s'affiche.
//
//	journal-key : recopie du journal dans l'audit (API :
//	              KUBEMANAGER_IAM_OPERATOR_JOURNAL_KEYS)
//	order-key   : ordres signés aux agents de nœud (order_keys de chaque
//	              config.toml)
func keyCLI(kind string, args []string) error {
	if len(args) != 1 {
		return errors.New("usage : operator " + kind + " <fichier de la clé privée>")
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
	env, where := "OPERATOR_JOURNAL_KEY_FILE", "côté API (KUBEMANAGER_IAM_OPERATOR_JOURNAL_KEYS, séparées par des espaces)"
	if kind == "order-key" {
		env, where = "OPERATOR_ORDER_KEY_FILE", "dans order_keys du config.toml de chaque agent de nœud"
	}
	fmt.Printf("Clé privée écrite dans %s (%s).\n", args[0], env)
	fmt.Printf("Identifiant : %s\n", opjournal.KeyID(pub))
	fmt.Printf("Clé publique, à ajouter %s :\n%s\n", where, base64.StdEncoding.EncodeToString(pub))
	return nil
}
