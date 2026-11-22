package domain

import (
	"errors"

	"github.com/google/uuid"
)

// Recipient : destinataire d'une alerte.
type Recipient struct {
	ID       uuid.UUID
	Email    string
	FullName string
	// Locale : langue choisie par la personne ("" : celle de la requête).
	Locale string
}

func ValidCategory(c string) bool {
	for _, x := range Categories {
		if x == c {
			return true
		}
	}
	return false
}

// Erreurs des préférences.
var (
	ErrUnknownCategory = errors.New("unknown alert category")
	ErrInvalidLocale   = errors.New("unsupported language")
	ErrInvalidToken    = errors.New("this unsubscribe link is invalid")
)

// Preferences : réponse et requête de /identity/me/preferences.
type Preferences struct {
	// Locale : "fr", "en" ou "" (pas encore choisie).
	Locale string `json:"locale"`
	// Alerts : catégorie → reçue (true) ou coupée (false).
	Alerts map[string]bool `json:"alerts"`
}

// PreferencesUpdate : champs absents inchangés.
type PreferencesUpdate struct {
	Locale *string         `json:"locale,omitempty" description:"Langue des e-mails : fr ou en"`
	Alerts map[string]bool `json:"alerts,omitempty" description:"Catégorie → reçue (true) ou coupée (false) : containers, builds, disk, api_keys"`
}

// Catégories d'alertes de projet que l'on peut couper.
const (
	CategoryWorkloads = "containers" // conteneur en incident
	CategoryBuilds    = "builds"     // build échoué
	CategoryDisk      = "disk"       // disque presque plein, plein, arrêt
	CategoryAPIKeys   = "api_keys"   // nouvelle clé API d'un compte de service
)

// Categories : dans l'ordre d'affichage.
var Categories = []string{CategoryWorkloads, CategoryBuilds, CategoryDisk, CategoryAPIKeys}
