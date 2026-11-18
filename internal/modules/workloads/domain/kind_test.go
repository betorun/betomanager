package domain

import (
	"errors"
	"testing"
)

func TestNormalizeProcess(t *testing.T) {
	cases := []struct {
		name string
		in   CreateWorkloadRequest
		want error
		chk  func(t *testing.T, r CreateWorkloadRequest)
	}{
		{"web par défaut, port 8080", CreateWorkloadRequest{}, nil, func(t *testing.T, r CreateWorkloadRequest) {
			if r.Kind != KindWeb || r.TargetPort != 8080 {
				t.Fatalf("%+v", r)
			}
		}},
		{"web avec horaire refusé", CreateWorkloadRequest{Schedule: "@daily"}, ErrNotCron, nil},
		{"sorte inconnue", CreateWorkloadRequest{Kind: "batch"}, ErrInvalidKind, nil},
		{"worker sans port ni TLS ni sondes", CreateWorkloadRequest{Kind: KindWorker, TargetPort: 3000, TLSEnabled: true, HealthPath: "/h"}, nil,
			func(t *testing.T, r CreateWorkloadRequest) {
				if r.TargetPort != 0 || r.TLSEnabled || r.HealthPath != "" {
					t.Fatalf("%+v", r)
				}
			}},
		{"worker avec fuseau refusé", CreateWorkloadRequest{Kind: KindWorker, TimeZone: "UTC"}, ErrNotCron, nil},
		{"cron sans horaire", CreateWorkloadRequest{Kind: KindCron}, ErrScheduleRequired, nil},
		{"cron horaire invalide", CreateWorkloadRequest{Kind: KindCron, Schedule: "tous les jours"}, ErrInvalidSchedule, nil},
		{"cron fuseau invalide", CreateWorkloadRequest{Kind: KindCron, Schedule: "0 2 * * *", TimeZone: "Mars/Olympus"}, ErrInvalidTimeZone, nil},
		{"cron durée trop longue", CreateWorkloadRequest{Kind: KindCron, Schedule: "0 2 * * *", MaxDurationSeconds: 90000}, ErrMaxDuration, nil},
		{"cron : une exécution, défauts", CreateWorkloadRequest{Kind: KindCron, Schedule: " */15 * * * * ", Replicas: 4, PDBEnabled: true, TimeZone: "Africa/Dakar"}, nil,
			func(t *testing.T, r CreateWorkloadRequest) {
				if r.Replicas != 1 || r.PDBEnabled || r.MaxDurationSeconds != DefaultCronMaxDuration || r.Schedule != "*/15 * * * *" {
					t.Fatalf("%+v", r)
				}
			}},
		{"commande trop longue", CreateWorkloadRequest{Command: make([]string, MaxCommandParts+1)}, ErrInvalidCommand, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.in
			err := NormalizeProcess(&in)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, attendu %v", err, tc.want)
			}
			if tc.chk != nil {
				tc.chk(t, in)
			}
		})
	}
}

func TestValidSchedule(t *testing.T) {
	for _, ok := range []string{"0 2 * * *", "*/5 * * * *", "0 9 * * MON-FRI", "0 0 1,15 * *", "@hourly"} {
		if !ValidSchedule(ok) {
			t.Errorf("%q refusé", ok)
		}
	}
	for _, ko := range []string{"", "* * * *", "0 2 * * * *", "@every 5m", "0 2 * * ; rm"} {
		if ValidSchedule(ko) {
			t.Errorf("%q accepté", ko)
		}
	}
}
