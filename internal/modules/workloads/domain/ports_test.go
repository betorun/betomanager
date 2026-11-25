package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestValidateExtraPorts(t *testing.T) {
	p := func(name string, port int) ExtraPort { return ExtraPort{Name: name, Port: port} }
	cases := []struct {
		name   string
		kind   string
		target int
		ports  []ExtraPort
		ok     bool
	}{
		{"aucun", KindWeb, 8080, nil, true},
		{"cron sans port", KindCron, 0, []ExtraPort{}, true},
		{"mailpit", KindWeb, 8025, []ExtraPort{p("smtp", 1025)}, true},
		{"syk-ia grpc", KindWeb, 8080, []ExtraPort{p("grpc", 9090)}, true},
		{"service", KindService, 6379, []ExtraPort{p("metrics", 9121)}, true},
		{"worker", KindWorker, 0, []ExtraPort{p("management", 9000)}, true},
		{"nom de 15 caractères", KindWeb, 8080, []ExtraPort{p("abcdefghijklmno", 9000)}, true},
		{"cinq ports", KindWeb, 8080, []ExtraPort{p("a", 1), p("b", 2), p("c", 3), p("d", 4), p("e", 5)}, true},
		{"cron refusé", KindCron, 0, []ExtraPort{p("smtp", 1025)}, false},
		{"six ports", KindWeb, 8080, []ExtraPort{p("a", 1), p("b", 2), p("c", 3), p("d", 4), p("e", 5), p("f", 6)}, false},
		{"nom vide", KindWeb, 8080, []ExtraPort{p("", 1025)}, false},
		{"nom de 16 caractères", KindWeb, 8080, []ExtraPort{p("abcdefghijklmnop", 1025)}, false},
		{"majuscule", KindWeb, 8080, []ExtraPort{p("SMTP", 1025)}, false},
		{"commence par un chiffre", KindWeb, 8080, []ExtraPort{p("1smtp", 1025)}, false},
		{"finit par un tiret", KindWeb, 8080, []ExtraPort{p("smtp-", 1025)}, false},
		{"nom du port principal", KindWeb, 8080, []ExtraPort{p("http", 1025)}, false},
		{"nom en double", KindWeb, 8080, []ExtraPort{p("smtp", 1025), p("smtp", 1026)}, false},
		{"port 0", KindWeb, 8080, []ExtraPort{p("smtp", 0)}, false},
		{"port trop grand", KindWeb, 8080, []ExtraPort{p("smtp", 65536)}, false},
		{"port principal", KindWeb, 8080, []ExtraPort{p("alt", 8080)}, false},
		{"port principal d'un service", KindService, 6379, []ExtraPort{p("alt", 6379)}, false},
		{"port 80 d'un web", KindWeb, 8080, []ExtraPort{p("alt", 80)}, false},
		{"port 80 d'un worker", KindWorker, 0, []ExtraPort{p("alt", 80)}, true},
		{"port en double", KindWeb, 8080, []ExtraPort{p("a", 1025), p("b", 1025)}, false},
	}
	for _, c := range cases {
		err := ValidateExtraPorts(c.kind, c.target, c.ports)
		if c.ok && err != nil {
			t.Errorf("%s : refusé : %v", c.name, err)
		}
		if !c.ok && !errors.Is(err, ErrInvalidExtraPorts) {
			t.Errorf("%s : accepté (%v)", c.name, err)
		}
	}
}

func TestNormalizeProcess_ExtraPorts(t *testing.T) {
	in := CreateWorkloadRequest{Kind: KindCron, Schedule: "@daily", ExtraPorts: []ExtraPort{{Name: "smtp", Port: 1025}}}
	if err := NormalizeProcess(&in); !errors.Is(err, ErrInvalidExtraPorts) {
		t.Fatalf("cron avec port : %v", err)
	}
	// Web sans port précisé : 8080 par défaut, le port supplémentaire ne doit pas le reprendre.
	in = CreateWorkloadRequest{ExtraPorts: []ExtraPort{{Name: "alt", Port: 8080}}}
	if err := NormalizeProcess(&in); !errors.Is(err, ErrInvalidExtraPorts) {
		t.Fatalf("port principal par défaut repris : %v", err)
	}
	in = CreateWorkloadRequest{Kind: KindWeb, TargetPort: 8025, ExtraPorts: []ExtraPort{}}
	if err := NormalizeProcess(&in); err != nil || in.ExtraPorts != nil {
		t.Fatalf("liste vide : %v %v", err, in.ExtraPorts)
	}
}

func TestWorkloadJSON_ExtraPorts(t *testing.T) {
	spec, _ := json.Marshal(CreateWorkloadRequest{Name: "mailpit", ExtraPorts: []ExtraPort{{Name: "smtp", Port: 1025}}})
	raw, err := json.Marshal(Workload{Name: "mailpit", Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"extra_ports":[{"name":"smtp","port":1025}]`) ||
		!strings.Contains(string(raw), `"internal_addresses":["mailpit:1025"]`) {
		t.Fatalf("fiche : %s", raw)
	}
	// Sans port (ou conteneur antérieur sans spec) : listes vides, jamais null.
	for _, w := range []Workload{{Name: "api"}, {Name: "api", Spec: []byte(`{}`)}, {Name: "api", Spec: []byte(`{"extra_ports":null}`)}} {
		raw, _ := json.Marshal(&w)
		if !strings.Contains(string(raw), `"extra_ports":[]`) || !strings.Contains(string(raw), `"internal_addresses":[]`) {
			t.Fatalf("sans port : %s", raw)
		}
	}
	// Relu par la CLI : les champs reviennent.
	var back Workload
	if err := json.Unmarshal(raw, &back); err != nil || back.ExtraPorts == nil {
		t.Fatalf("relecture : %v %+v", err, back.ExtraPorts)
	}
}
