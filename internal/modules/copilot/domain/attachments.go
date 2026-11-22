package domain

import (
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Pièces jointes d'un message : fichiers texte (journaux, configuration,
// Dockerfile, code) lus par la console et envoyés avec le message. Elles ne
// sont jamais stockées : le modèle les lit pour ce message seulement, et la
// mémoire de la conversation n'en garde que les noms.
//
// Elles sont des DONNÉES, comme les résultats d'outils : encadrées, et le
// prompt système interdit d'y lire des consignes.

// Attachment : un fichier joint. Name peut contenir un chemin relatif
// (dossier joint : « api/Dockerfile »).
type Attachment struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

const (
	maxAttachments      = 20
	MaxAttachmentBytes  = 64 << 10  // par fichier
	maxAttachmentsBytes = 128 << 10 // pour le message
	maxAttachmentName   = 200
	// maxChatBody : corps de POST /copilot/chat (message + pièces jointes,
	// échappement JSON compris).
	MaxChatBody = 512 << 10
)

// Codes d'erreur des pièces jointes (réponses JSON avant le flux).
const (
	CodeAttachmentsInvalid  = "COPILOT_ATTACHMENTS_INVALID"
	CodeAttachmentsTooLarge = "COPILOT_ATTACHMENTS_TOO_LARGE"
	CodeAttachmentSecret    = "COPILOT_ATTACHMENT_SECRET"
)

// attachmentError : refus, avec un code stable pour la console.
type AttachmentError struct {
	Code, Message string
}

func (e *AttachmentError) Error() string { return e.Message }

// checkAttachments : refuse ce qui ne doit pas partir chez le modèle.
func CheckAttachments(atts []Attachment) error {
	if len(atts) > maxAttachments {
		return &AttachmentError{CodeAttachmentsTooLarge, "at most 20 attached files per message"}
	}
	total := 0
	for _, a := range atts {
		if !validAttachmentName(a.Name) {
			return &AttachmentError{CodeAttachmentsInvalid, "invalid attached file name"}
		}
		if !utf8.ValidString(a.Content) || strings.ContainsRune(a.Content, 0) {
			return &AttachmentError{CodeAttachmentsInvalid, "attached files must be text files"}
		}
		if len(a.Content) > MaxAttachmentBytes {
			return &AttachmentError{CodeAttachmentsTooLarge, "each attached file must be 64 KB at most"}
		}
		total += len(a.Content)
		if total > maxAttachmentsBytes {
			return &AttachmentError{CodeAttachmentsTooLarge, "attached files must total 128 KB at most"}
		}
		if secretFile(a.Name) || containsPrivateKey(a.Content) {
			return &AttachmentError{CodeAttachmentSecret, "secret files (.env, private keys) cannot be attached"}
		}
	}
	return nil
}

func validAttachmentName(name string) bool {
	if name == "" || utf8.RuneCountInString(name) > maxAttachmentName || !utf8.ValidString(name) {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// secretFile : noms de fichiers qui portent presque toujours des secrets.
func secretFile(name string) bool {
	base := strings.ToLower(path.Base(strings.ReplaceAll(name, "\\", "/")))
	if base == ".env" || strings.HasPrefix(base, ".env.") {
		return !strings.HasSuffix(base, ".example") && !strings.HasSuffix(base, ".sample") && !strings.HasSuffix(base, ".template")
	}
	switch base {
	case "id_rsa", "id_dsa", "id_ecdsa", "id_ed25519", ".npmrc", ".pypirc", ".netrc", "credentials", "kubeconfig":
		return true
	}
	for _, ext := range []string{".pem", ".key", ".p12", ".pfx", ".jks", ".keystore"} {
		if strings.HasSuffix(base, ext) {
			return true
		}
	}
	return false
}

func containsPrivateKey(content string) bool {
	i := strings.Index(content, "-----BEGIN ")
	return i >= 0 && strings.Contains(content[i:], "PRIVATE KEY-----")
}

// withAttachments : le message de la personne suivi des fichiers, encadrés.
func WithAttachments(message string, atts []Attachment) string {
	if len(atts) == 0 {
		return message
	}
	var b strings.Builder
	b.WriteString(message)
	for _, a := range atts {
		b.WriteString("\n\n<attachment name=\"")
		b.WriteString(strings.NewReplacer(`"`, "'", "<", "‹", ">", "›").Replace(a.Name))
		b.WriteString("\">\n")
		// Une fausse balise de fin dans le fichier ne doit pas « sortir » du cadre.
		b.WriteString(strings.ReplaceAll(a.Content, "</attachment>", "</attachment_>"))
		b.WriteString("\n</attachment>")
	}
	return b.String()
}

// historyNote : ce que la mémoire garde des pièces jointes (les noms).
func HistoryNote(message string, atts []Attachment) string {
	if len(atts) == 0 {
		return message
	}
	names := make([]string, len(atts))
	for i, a := range atts {
		names[i] = a.Name
	}
	return message + "\n\n[Attached files, content not kept: " + strings.Join(names, ", ") + "]"
}
