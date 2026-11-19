package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Appareils déjà vus à la connexion (table user_devices). L'appareil est une
// famille (client, navigateur, système) tirée du User-Agent : deux PC Windows
// sous Chrome se confondent, mais une connexion depuis un autre navigateur,
// un téléphone ou la CLI se voit. Sans cookie d'appareil, c'est le meilleur
// compromis entre e-mails inutiles et connexions passées inaperçues.

type deviceRow struct {
	UserID      uuid.UUID `gorm:"type:uuid;primaryKey"`
	Fingerprint string    `gorm:"primaryKey"`
	Label       string
	FirstSeen   time.Time
	LastSeen    time.Time
}

func (deviceRow) TableName() string { return "user_devices" }

// Devices : appareils connus des comptes.
type Devices struct {
	DB  *gorm.DB
	Now func() time.Time
}

// DeviceSeen : résultat d'une connexion.
type DeviceSeen struct {
	Label string
	// New : appareil jamais vu pour ce compte.
	New bool
	// First : premier appareil du compte (première connexion) : rien à signaler.
	First bool
}

// Seen : enregistre l'appareil de cette connexion.
func (d *Devices) Seen(ctx context.Context, userID uuid.UUID, kind, userAgent string) (DeviceSeen, error) {
	fp, label := DescribeDevice(kind, userAgent)
	now := time.Now().UTC()
	if d.Now != nil {
		now = d.Now()
	}
	out := DeviceSeen{Label: label}
	err := d.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var known int64
		if err := tx.Model(&deviceRow{}).Where("user_id = ?", userID).Count(&known).Error; err != nil {
			return err
		}
		res := tx.Model(&deviceRow{}).Where("user_id = ? AND fingerprint = ?", userID, fp).
			Updates(map[string]any{"last_seen": now, "label": label})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected > 0 {
			return nil
		}
		out.New, out.First = true, known == 0
		// Deux connexions simultanées du même appareil : la seconde ne fait rien.
		return tx.Exec(`INSERT INTO user_devices (user_id, fingerprint, label, first_seen, last_seen)
			VALUES (?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`, userID, fp, label, now, now).Error
	})
	return out, err
}

// DescribeDevice : empreinte (hachée) et libellé lisible (« Chrome · Windows »,
// « CLI betorun · Linux »).
func DescribeDevice(kind, userAgent string) (fingerprint, label string) {
	ua := strings.ToLower(userAgent)
	client := browserOf(ua)
	if kind == KindCLI || strings.HasPrefix(ua, "betorun-cli") {
		kind, client = KindCLI, "CLI betorun"
	}
	system := systemOf(ua)
	label = client
	if system != "" {
		label += " · " + system
	}
	sum := sha256.Sum256([]byte(kind + "|" + client + "|" + system))
	return hex.EncodeToString(sum[:16]), label
}

func browserOf(ua string) string {
	switch {
	case ua == "":
		return "?"
	case strings.Contains(ua, "edg/"):
		return "Edge"
	case strings.Contains(ua, "opr/"), strings.Contains(ua, "opera"):
		return "Opera"
	case strings.Contains(ua, "samsungbrowser"):
		return "Samsung Internet"
	case strings.Contains(ua, "firefox/"), strings.Contains(ua, "fxios/"):
		return "Firefox"
	case strings.Contains(ua, "chrome/"), strings.Contains(ua, "crios/"):
		return "Chrome"
	case strings.Contains(ua, "safari/"):
		return "Safari"
	}
	return "Web"
}

func systemOf(ua string) string {
	switch {
	case strings.Contains(ua, "android"):
		return "Android"
	case strings.Contains(ua, "iphone"):
		return "iPhone"
	case strings.Contains(ua, "ipad"):
		return "iPad"
	case strings.Contains(ua, "windows"):
		return "Windows"
	case strings.Contains(ua, "cros"):
		return "ChromeOS"
	case strings.Contains(ua, "mac os"), strings.Contains(ua, "macintosh"), strings.Contains(ua, "darwin"):
		return "macOS"
	case strings.Contains(ua, "linux"):
		return "Linux"
	}
	return ""
}
