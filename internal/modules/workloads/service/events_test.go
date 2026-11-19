package service

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestEventTimes(t *testing.T) {
	t1 := time.Date(2026, 11, 19, 1, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Minute)

	// Ancien format : FirstTimestamp / LastTimestamp.
	f, l := eventTimes(corev1.Event{FirstTimestamp: metav1.NewTime(t1), LastTimestamp: metav1.NewTime(t2)})
	if !f.Equal(t1) || !l.Equal(t2) {
		t.Fatalf("ancien format : %v %v", f, l)
	}
	// Nouveau format (ex. « Scheduled ») : EventTime seul.
	f, l = eventTimes(corev1.Event{EventTime: metav1.NewMicroTime(t1)})
	if !f.Equal(t1) || !l.Equal(t1) {
		t.Fatalf("EventTime : %v %v", f, l)
	}
	// Série : dernier passage dans Series.
	f, l = eventTimes(corev1.Event{EventTime: metav1.NewMicroTime(t1), Series: &corev1.EventSeries{LastObservedTime: metav1.NewMicroTime(t2)}})
	if !f.Equal(t1) || !l.Equal(t2) {
		t.Fatalf("série : %v %v", f, l)
	}
	// Rien : la date de création de l'objet.
	f, _ = eventTimes(corev1.Event{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(t1)}})
	if !f.Equal(t1) {
		t.Fatalf("création : %v", f)
	}
}
