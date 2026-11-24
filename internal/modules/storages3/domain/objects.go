package domain

import (
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Explorateur de fichiers de la console (ADR 0015, point 10) : la console
// lit et écrit par l'API, qui vérifie les droits du projet puis agit avec
// l'identité du module. Aucune clé client n'arrive dans le navigateur.

const (
	// MaxConsoleUpload : envoi d'un fichier depuis la console (au-delà : un
	// outil S3, avec une clé).
	MaxConsoleUpload = 100 << 20
	// MaxObjectKey : longueur maximale d'un nom d'objet (octets).
	MaxObjectKey = 1024
	// DefaultPageSize / MaxPageSize : objets par page de l'explorateur.
	DefaultPageSize = 200
	MaxPageSize     = 1000
	// Liens partagés : URL signée, d'une heure à 7 jours (limite SigV4).
	DefaultLinkHours = 24
	MaxLinkHours     = 7 * 24
	// MaxDeleteBatch : objets supprimés en une requête.
	MaxDeleteBatch = 1000
	// ObjectFormField : champ multipart du fichier envoyé.
	ObjectFormField = "file"
)

// ValidObjectKey : nom d'objet sûr (UTF-8, sans caractère de contrôle, sans
// « / » de tête ni segment « .. »). Un nom terminé par « / » désigne un dossier.
func ValidObjectKey(k string) bool {
	if k == "" || len(k) > MaxObjectKey || !utf8.ValidString(k) || strings.HasPrefix(k, "/") {
		return false
	}
	for _, r := range k {
		if unicode.IsControl(r) {
			return false
		}
	}
	for seg := range strings.SplitSeq(k, "/") {
		if seg == ".." || seg == "." {
			return false
		}
	}
	return true
}

// ValidPrefixPath : dossier parcouru ("" pour la racine, sinon terminé par « / »).
func ValidPrefixPath(p string) bool {
	return p == "" || (strings.HasSuffix(p, "/") && ValidObjectKey(p))
}

// Object : un fichier, ou un dossier (Dir : préfixe commun, sans taille).
type Object struct {
	Key          string     `json:"key"`
	Size         int64      `json:"size"`
	LastModified *time.Time `json:"last_modified,omitempty"`
	ContentType  string     `json:"content_type,omitempty"`
	Dir          bool       `json:"dir"`
}

// ObjectPage : contenu d'un dossier, dossiers d'abord. Next : à passer en
// « after » pour la page suivante ("" : fin).
type ObjectPage struct {
	Prefix  string   `json:"prefix"`
	Objects []Object `json:"objects"`
	Next    string   `json:"next,omitempty"`
}

type ListObjectsRequest struct {
	ProjectID string `path:"id" binding:"required" description:"ID du projet"`
	BucketID  string `path:"bucketID" binding:"required" description:"ID du bucket"`
	Prefix    string `query:"prefix" description:"Dossier parcouru (vide : racine ; sinon terminé par /)"`
	After     string `query:"after" description:"Reprise après ce nom (champ next de la page précédente)"`
	Limit     int    `query:"limit" description:"Objets par page (200 par défaut, 1000 au plus)"`
}

type ObjectRequest struct {
	ProjectID string `path:"id" binding:"required" description:"ID du projet"`
	BucketID  string `path:"bucketID" binding:"required" description:"ID du bucket"`
	Key       string `query:"key" binding:"required" description:"Nom complet du fichier (ex. images/logo.png)"`
}

type UploadObjectRequest struct {
	ProjectID string `path:"id" binding:"required" description:"ID du projet"`
	BucketID  string `path:"bucketID" binding:"required" description:"ID du bucket"`
	Key       string `query:"key" binding:"required" description:"Nom complet du fichier à écrire (un fichier existant est remplacé)"`
}

type DeleteObjectsRequest struct {
	ProjectID string   `path:"id" binding:"required" description:"ID du projet"`
	BucketID  string   `path:"bucketID" binding:"required" description:"ID du bucket"`
	Keys      []string `json:"keys" binding:"required" description:"Fichiers à supprimer ; un nom terminé par / supprime tout le dossier"`
}

// DeleteObjectsResponse : nombre d'objets effacés.
type DeleteObjectsResponse struct {
	Deleted int `json:"deleted"`
}

type ShareLinkRequest struct {
	ProjectID    string `path:"id" binding:"required" description:"ID du projet"`
	BucketID     string `path:"bucketID" binding:"required" description:"ID du bucket"`
	Key          string `json:"key" binding:"required" description:"Nom complet du fichier partagé"`
	ExpiresHours int    `json:"expires_hours" description:"Durée de validité en heures : 1 à 168 (24 par défaut)"`
}

// ShareLink : lien de lecture d'un seul fichier, valable jusqu'à ExpiresAt,
// sans compte ni clé.
type ShareLink struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

type SetPublicRequest struct {
	ProjectID string `path:"id" binding:"required" description:"ID du projet"`
	BucketID  string `path:"bucketID" binding:"required" description:"ID du bucket"`
	Public    bool   `json:"public" description:"true : chaque fichier est lisible par tous à son adresse (lister reste refusé) ; false : privé"`
}
