package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/thekrauss/kubemanager/internal/operator"
	"github.com/thekrauss/kubemanager/internal/operator/rescue"
	"github.com/thekrauss/kubemanager/internal/operator/store"
)

// rescueCLI : comptes de secours, gérés sur le serveur de gestion (jamais
// par l'API). Le mot de passe ne passe jamais en argument.
func rescueCLI(args []string) error {
	if len(args) == 0 {
		return errors.New("usage : operator rescue add|disable <e-mail> | list")
	}
	cfg, err := operator.LoadConfig(os.Getenv)
	if err != nil {
		return err
	}
	if cfg.RescueKey == "" {
		return errors.New("OPERATOR_RESCUE_KEY_FILE est obligatoire pour l'accès de secours")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	svc, err := rescue.New(db, cfg.RescueKey)
	if err != nil {
		return err
	}
	in := bufio.NewReader(os.Stdin)

	switch {
	case args[0] == "list" && len(args) == 1:
		accounts, err := db.RescueAccounts(ctx)
		if err != nil {
			return err
		}
		for _, a := range accounts {
			state := "actif"
			if a.DisabledAt != nil {
				state = "désactivé"
			} else if a.LockedUntil != nil && a.LockedUntil.After(time.Now()) {
				state = "verrouillé"
			}
			last := "jamais"
			if a.LastLoginAt != nil {
				last = a.LastLoginAt.Format(time.RFC3339)
			}
			fmt.Printf("%s\t%s\tdernière connexion : %s\n", a.Email, state, last)
		}
		return nil

	case args[0] == "disable" && len(args) == 2:
		if err := db.DisableRescueAccount(ctx, strings.ToLower(args[1])); err != nil {
			return err
		}
		_ = db.Journal(ctx, "cli", "rescue.account.disabled", nil, map[string]any{"email": strings.ToLower(args[1])})
		fmt.Println("Compte désactivé, sessions révoquées.")
		return nil

	case args[0] == "add" && len(args) == 2:
		pw, err := secret(in, "Mot de passe (16 caractères au moins) : ")
		if err != nil {
			return err
		}
		again, err := secret(in, "Encore une fois : ")
		if err != nil {
			return err
		}
		if pw != again {
			return errors.New("les deux saisies diffèrent")
		}
		p, err := svc.Prepare(args[1], pw)
		if err != nil {
			return err
		}
		fmt.Println("Ajoutez ce compte dans l'application d'authentification de la personne :")
		fmt.Println("  " + p.URI)
		fmt.Println("  ou à la main : " + p.Secret)
		fmt.Print("Code affiché par l'application : ")
		code, err := in.ReadString('\n')
		if err != nil {
			return err
		}
		if err := svc.Commit(ctx, p, strings.TrimSpace(code)); err != nil {
			return err
		}
		fmt.Println("Compte de secours créé.")
		return nil
	}
	return errors.New("usage : operator rescue add|disable <e-mail> | list")
}

// secret : saisie sans écho dans un terminal ; une ligne de l'entrée sinon.
func secret(in *bufio.Reader, prompt string) (string, error) {
	fmt.Print(prompt)
	if term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		return string(b), err
	}
	line, err := in.ReadString('\n')
	return strings.TrimRight(line, "\r\n"), err
}
