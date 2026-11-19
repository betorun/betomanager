package domain

import "time"

// Suppression des données d'une organisation suspendue (ADR 0012) : après
// PurgeDays jours de suspension, ses conteneurs (et leurs disques), ses bases
// (après une copie finale, gardée PurgeDays jours de plus) et ses sites sont
// supprimés. L'organisation, ses projets, son grand livre et ses factures
// restent. Les propriétaires sont prévenus trois fois avant.

// MinPurgeDays : délai minimal réglable (les trois avertissements tiennent).
const MinPurgeDays = 14

// Étapes : chaque avertissement part une fois, dans l'ordre.
const (
	PurgeStageNone = iota
	PurgeStageFirst
	PurgeStageWeek
	PurgeStageLast
	PurgeStageDue
)

// Avertissements : une semaine après la suspension, puis une semaine et deux
// jours avant la suppression.
const (
	purgeFirstAfter = 7 * 24 * time.Hour
	purgeWeekBefore = 7 * 24 * time.Hour
	purgeLastBefore = PurgeLastNotice
)

// PurgeLastNotice : délai minimal entre le dernier avertissement et la
// suppression, même quand des avertissements ont été manqués.
const PurgeLastNotice = 2 * 24 * time.Hour

// PurgeAt : date de suppression des données (zéro : jamais).
func PurgeAt(suspendedAt time.Time, purgeDays int) time.Time {
	if purgeDays <= 0 {
		return time.Time{}
	}
	return suspendedAt.AddDate(0, 0, purgeDays)
}

// PurgeStage : étape atteinte à now (PurgeStageNone si purgeDays vaut 0).
func PurgeStage(suspendedAt time.Time, purgeDays int, now time.Time) int {
	at := PurgeAt(suspendedAt, purgeDays)
	switch {
	case at.IsZero():
		return PurgeStageNone
	case !now.Before(at):
		return PurgeStageDue
	case !now.Before(at.Add(-purgeLastBefore)):
		return PurgeStageLast
	case !now.Before(at.Add(-purgeWeekBefore)):
		return PurgeStageWeek
	case !now.Before(suspendedAt.Add(purgeFirstAfter)):
		return PurgeStageFirst
	}
	return PurgeStageNone
}
