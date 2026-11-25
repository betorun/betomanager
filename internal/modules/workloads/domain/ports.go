package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
)

// ExtraPort : port interne supplémentaire d'un conteneur, joignable par les
// autres conteneurs du projet seulement (nom-du-conteneur:port), jamais
// publié (ni adresse publique, ni domaine). TCP seulement.
type ExtraPort struct {
	Name string `json:"name" description:"Nom du port (1 à 15 caractères : minuscules, chiffres et -, commence par une lettre), ex. smtp"`
	Port int    `json:"port" description:"Numéro du port (1 à 65535), différent du port principal"`
}

const (
	// MaxExtraPorts : ports internes supplémentaires au plus par conteneur.
	MaxExtraPorts = 5
	// maxPortNameLen : longueur maximale d'un nom de port (IANA_SVC_NAME).
	maxPortNameLen = 15
	// maxPortNumber : plus grand numéro de port TCP.
	maxPortNumber = 65535
	// webServicePort : port de l'adresse interne d'un conteneur web
	// (http://<nom>, port 80) : déjà pris sur son adresse.
	webServicePort = 80
)

// reservedPortNames : noms du port principal (http : web, tcp : service).
var reservedPortNames = map[string]bool{"http": true, "tcp": true}

var portNameRe = regexp.MustCompile(`^[a-z]([a-z0-9-]*[a-z0-9])?$`)

// ErrInvalidExtraPorts : liste extra_ports refusée (le détail suit).
var ErrInvalidExtraPorts = errors.New("invalid extra_ports")

// ValidateExtraPorts : ports internes supplémentaires d'un conteneur de la
// sorte kind, dont le port principal est targetPort (0 : aucun). Aucune
// règle de quota : un port ne consomme rien.
func ValidateExtraPorts(kind string, targetPort int, ports []ExtraPort) error {
	if len(ports) == 0 {
		return nil
	}
	if kind == KindCron {
		return fmt.Errorf("%w: a cron container has no network address, it cannot listen on a port", ErrInvalidExtraPorts)
	}
	if len(ports) > MaxExtraPorts {
		return fmt.Errorf("%w: at most %d ports", ErrInvalidExtraPorts, MaxExtraPorts)
	}
	names, numbers := map[string]bool{}, map[int]bool{}
	for _, p := range ports {
		if len(p.Name) > maxPortNameLen || !portNameRe.MatchString(p.Name) {
			return fmt.Errorf("%w: %q is not a valid port name (1 to 15 characters: lowercase letters, digits and -, starting with a letter)", ErrInvalidExtraPorts, p.Name)
		}
		if reservedPortNames[p.Name] {
			return fmt.Errorf("%w: the name %q is reserved for the main port", ErrInvalidExtraPorts, p.Name)
		}
		if names[p.Name] {
			return fmt.Errorf("%w: the name %q is used twice", ErrInvalidExtraPorts, p.Name)
		}
		if p.Port < 1 || p.Port > maxPortNumber {
			return fmt.Errorf("%w: port %d must be between 1 and 65535", ErrInvalidExtraPorts, p.Port)
		}
		if p.Port == targetPort && targetPort > 0 {
			return fmt.Errorf("%w: port %d is already the main port (target_port)", ErrInvalidExtraPorts, p.Port)
		}
		if (kind == KindWeb || kind == "") && p.Port == webServicePort {
			return fmt.Errorf("%w: port 80 is already the internal address of a web container", ErrInvalidExtraPorts)
		}
		if numbers[p.Port] {
			return fmt.Errorf("%w: port %d is used twice", ErrInvalidExtraPorts, p.Port)
		}
		names[p.Name], numbers[p.Port] = true, true
	}
	return nil
}

// InternalAddresses : adresse nom:port de chaque port supplémentaire (jamais
// nil).
func InternalAddresses(name string, ports []ExtraPort) []string {
	out := make([]string, 0, len(ports))
	if name == "" {
		return out
	}
	for _, p := range ports {
		out = append(out, name+":"+strconv.Itoa(p.Port))
	}
	return out
}

// SpecExtraPorts : ports supplémentaires enregistrés dans une spec (jamais
// nil ; spec absente ou illisible : aucun).
func SpecExtraPorts(spec []byte) []ExtraPort {
	out := []ExtraPort{}
	if len(spec) == 0 {
		return out
	}
	var s struct {
		ExtraPorts []ExtraPort `json:"extra_ports"`
	}
	if json.Unmarshal(spec, &s) == nil && len(s.ExtraPorts) > 0 {
		out = s.ExtraPorts
	}
	return out
}

// MarshalJSON : extra_ports et internal_addresses rendus depuis la spec
// enregistrée, jamais null (fiche, liste).
func (w Workload) MarshalJSON() ([]byte, error) {
	type plain Workload // sans méthode : pas de récursion
	p := plain(w)
	if len(w.Spec) > 0 || p.ExtraPorts == nil {
		p.ExtraPorts = SpecExtraPorts(w.Spec)
	}
	p.InternalAddresses = InternalAddresses(w.Name, p.ExtraPorts)
	return json.Marshal(p)
}
