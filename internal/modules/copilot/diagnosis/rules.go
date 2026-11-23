package diagnosis

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ── 1. Dépendance attendue sur localhost ──────────────────────────────────

// depService : logiciel attendu derrière un port connu.
type depService struct {
	Name string
	// Template : modèle de service du projet (addons) ; Database : base Postgres.
	Template string
	Database bool
}

// portServices : ports bien connus des dépendances courantes.
var portServices = map[int]depService{
	6379:  {Name: "redis", Template: "redis"},
	5432:  {Name: "postgres", Database: true},
	3306:  {Name: "mysql", Template: "mariadb"},
	7700:  {Name: "meilisearch", Template: "meilisearch"},
	27017: {Name: "mongodb"},
}

var (
	// refusedRe : la connexion a échoué (Node, Python, Go, Java, PHP).
	refusedRe = regexp.MustCompile(`(?i)refused|unable to connect|error 111|errno 111|could not connect|cannot connect|can't connect|failed to connect|failed to establish`)
	// loopbackRes : adresse de bouclage et port, sous ses diverses écritures.
	loopbackRes = []*regexp.Regexp{
		// 127.0.0.1:6379, localhost:5432, localhost/<unresolved>:6379, [::1]:6379, ::1:5432
		regexp.MustCompile(`(?i)(?:\b(?:localhost|127\.0\.0\.1)(?:/(?:<unresolved>|[0-9.]+))?|\[::1\]|(?:^|[\s"'(@/])::1):(\d{2,5})\b`),
		// psycopg : at "localhost" (127.0.0.1), port 5432 failed
		regexp.MustCompile(`(?i)\b(?:localhost|127\.0\.0\.1)"?(?:\s*\([^)]*\))?,\s*port\s+(\d{2,5})\b`),
		// psycopg ancien : on host "localhost" (127.0.0.1) and accepting TCP/IP connections on port 5432?
		regexp.MustCompile(`(?i)host "(?:localhost|127\.0\.0\.1)".*?port (\d{2,5})\b`),
		// urllib3 : HTTPConnectionPool(host='localhost', port=7700)
		regexp.MustCompile(`(?i)host='(?:localhost|127\.0\.0\.1)', port=(\d{2,5})\b`),
	}
)

// loopbackPort : port de bouclage cité par le texte (0 : aucun).
func loopbackPort(s string) int {
	for _, re := range loopbackRes {
		if m := re.FindStringSubmatch(s); m != nil {
			if p, err := strconv.Atoi(m[1]); err == nil {
				return p
			}
		}
	}
	return 0
}

func (a *analysis) localhostDependency() []Finding {
	type hit struct {
		port  int
		lines []string
	}
	var order []int
	hits := map[int]*hit{}
	for i, l := range a.lines {
		if !refusedRe.MatchString(l) {
			continue
		}
		port, used := loopbackPort(l), []string{l}
		if port == 0 { // message sur plusieurs lignes (psycopg ancien)
			end := min(i+3, len(a.lines))
			if p := loopbackPort(strings.Join(a.lines[i:end], " ")); p != 0 {
				port, used = p, a.lines[i:end]
			}
		}
		if _, known := portServices[port]; !known {
			continue
		}
		h, ok := hits[port]
		if !ok {
			h = &hit{port: port}
			hits[port] = h
			order = append(order, port)
		}
		h.lines = append(h.lines, used...)
	}
	var out []Finding
	for _, port := range order {
		h, svc := hits[port], portServices[port]
		f := Finding{Code: CodeLocalhostDependency, Severity: Danger, Evidence: evidence(h.lines...),
			Params: map[string]string{"service": svc.Name, "port": strconv.Itoa(port)}}
		for _, v := range a.in.Workload.Variables {
			if !v.Secret && v.Value != "" && loopbackPort(v.Value) == port {
				f.Params["variable"] = v.Name
				f.Evidence = append(f.Evidence, clip("variable "+v.Name+" = "+v.Value))
			}
		}
		f.Fix = a.attachFor(svc, "", f.Params)
		if addr := a.neighborOnPort(port); addr != "" {
			f.Params["neighbor_address"] = addr
		}
		out = append(out, f)
	}
	return out
}

// attachFor : correction « brancher le service du projet » pour svc, et ce
// que le voisinage en dit (params). variable : nom de variable voulu (vide :
// celui par défaut du service). nil : pas de correction sûre.
func (a *analysis) attachFor(svc depService, variable string, params map[string]string) *Fix {
	w := a.in.Workload
	type candidate struct {
		id, name, address, status string
		attached                  []Attachment
		database                  bool
	}
	var cands []candidate
	known := true
	if svc.Database {
		known = a.in.DatabasesKnown
		for _, d := range a.in.Databases {
			cands = append(cands, candidate{d.ID, d.Name, hostPort(d.Host, d.Port), d.Status, d.Attached, true})
		}
	}
	// Faute de base Postgres, une variable DATABASE_URL peut aussi viser MariaDB.
	template := svc.Template
	if svc.Database && len(cands) == 0 && variable == "DATABASE_URL" {
		template = "mariadb"
	}
	if template != "" {
		known = known && a.in.AddonsKnown
		for _, ad := range a.in.Addons {
			if ad.Template == template {
				cands = append(cands, candidate{ad.ID, ad.Name, hostPort(ad.Host, ad.Port), ad.Status, ad.Attached, false})
			}
		}
	}
	switch {
	case len(cands) == 0 && !known:
		params["in_project"] = "unknown"
		return nil
	case len(cands) == 0:
		params["in_project"] = "false"
		return nil
	case len(cands) > 1:
		names := make([]string, 0, len(cands))
		for _, c := range cands {
			names = append(names, c.name)
		}
		sort.Strings(names)
		params["in_project"] = "true"
		params["candidates"] = strings.Join(names, ",")
		return nil // plusieurs : à la personne de choisir
	}
	c := cands[0]
	params["in_project"] = "true"
	params["project_service"] = c.name
	if c.address != "" {
		params["internal_address"] = c.address
	}
	if c.status != "" {
		params["project_service_status"] = c.status
	}
	for _, at := range c.attached {
		if at.WorkloadID == w.ID {
			params["already_attached"] = "true"
			if at.Variable != "" {
				params["attached_variable"] = at.Variable
			}
			return nil // déjà branché : l'appli doit lire sa variable
		}
	}
	if w.ProjectID == "" || w.ID == "" || c.id == "" {
		return nil
	}
	args := map[string]any{"id": w.ProjectID, "workload_id": w.ID}
	tool := ToolAttachAddon
	if c.database {
		tool, args["databaseID"] = ToolAttachDatabase, c.id
	} else {
		args["addonID"] = c.id
	}
	if variable != "" {
		args["variable"] = variable
	}
	return &Fix{Tool: tool, Args: args}
}

// hostPort : adresse host:port ; un hôte qui porte déjà son port (services :
// « cache:6379 ») est rendu tel quel.
func hostPort(host string, port int) string {
	if host == "" || strings.Contains(host, ":") {
		return host
	}
	if port > 0 {
		return host + ":" + strconv.Itoa(port)
	}
	return host
}

// internalAddress : adresse d'un conteneur dans le réseau privé du projet,
// même règle que workloads/service.InternalURL (vérifiée par un test) :
// web → http://nom (port 80) ; service → nom:port ; worker et cron : aucune.
func internalAddress(n Neighbor) string {
	if n.Name == "" {
		return ""
	}
	switch n.Kind {
	case "", "web":
		return "http://" + n.Name
	case "service":
		if n.Port > 0 {
			return n.Name + ":" + strconv.Itoa(n.Port)
		}
	}
	return ""
}

// neighborOnPort : adresse d'un conteneur « service » du client qui écoute ce port.
func (a *analysis) neighborOnPort(port int) string {
	var found []string
	for _, n := range a.in.Neighbors {
		if n.Kind == "service" && n.Port == port && n.ManagedBy == "" && n.Name != a.in.Workload.Name {
			found = append(found, internalAddress(n))
		}
	}
	sort.Strings(found)
	return strings.Join(found, ",")
}

// ── 2. Nom d'hôte inconnu ──────────────────────────────────────────────────

var unknownHostRes = []*regexp.Regexp{
	regexp.MustCompile(`getaddrinfo (?:ENOTFOUND|EAI_AGAIN) ([A-Za-z0-9][A-Za-z0-9._-]*)`),
	regexp.MustCompile(`lookup ([A-Za-z0-9][A-Za-z0-9._-]*)(?: on [^\s:]+(?::\d+)?)?: (?:no such host|server misbehaving)`),
	regexp.MustCompile(`could not translate host name "([^"\s]+)" to address`),
	regexp.MustCompile(`Error -?\d+ connecting to ([A-Za-z0-9][A-Za-z0-9._-]*):\d+\.\s*(?:Name or service not known|nodename nor servname|Temporary failure in name resolution|No address associated)`),
	regexp.MustCompile(`UnknownHostException:? ([A-Za-z0-9][A-Za-z0-9._-]*)`),
	regexp.MustCompile(`getaddrinfo for ([A-Za-z0-9][A-Za-z0-9._-]*) failed`),
}

func (a *analysis) unknownHost() []Finding {
	var order []string
	lines := map[string][]string{}
	for _, l := range a.lines {
		for _, re := range unknownHostRes {
			m := re.FindStringSubmatch(l)
			if m == nil {
				continue
			}
			host := strings.TrimRight(strings.ToLower(m[1]), ".:")
			if host == "" || host == "localhost" {
				break
			}
			if _, ok := lines[host]; !ok {
				order = append(order, host)
			}
			lines[host] = append(lines[host], l)
			break
		}
	}
	var out []Finding
	for _, host := range order {
		f := Finding{Code: CodeUnknownHost, Severity: Danger, Evidence: evidence(lines[host]...),
			Params: map[string]string{"host": host}}
		if name, addr, exact := a.closestName(host); name != "" {
			if exact {
				f.Params["exists_in_project"] = "true"
			} else {
				f.Params["did_you_mean"] = name
			}
			if addr != "" {
				f.Params["internal_address"] = addr
			}
		}
		out = append(out, f)
	}
	return out
}

// closestName : nom du projet (service, base, conteneur) le plus proche de
// host, s'il l'est assez, et son adresse telle que l'API la donne.
func (a *analysis) closestName(host string) (name, addr string, exact bool) {
	type cand struct{ name, addr string }
	var cands []cand
	for _, ad := range a.in.Addons {
		cands = append(cands, cand{ad.Name, hostPort(ad.Host, ad.Port)})
	}
	for _, d := range a.in.Databases {
		cands = append(cands, cand{d.Name, hostPort(d.Host, d.Port)})
		if d.Host != "" {
			cands = append(cands, cand{d.Host, hostPort(d.Host, d.Port)})
		}
	}
	for _, n := range a.in.Neighbors {
		if n.ManagedBy == "" {
			cands = append(cands, cand{n.Name, internalAddress(n)})
		}
	}
	short := host
	if i := strings.IndexByte(host, '.'); i > 0 {
		short = host[:i]
	}
	best, bestD := cand{}, 1<<30
	for _, c := range cands {
		n := strings.ToLower(c.name)
		if n == "" {
			continue
		}
		if n == host || n == short {
			return c.name, c.addr, true
		}
		d := levenshtein(short, n)
		if d < bestD || d == bestD && c.name < best.name {
			best, bestD = c, d
		}
	}
	if best.name != "" && bestD <= 2 && bestD*2 < len([]rune(short)) {
		return best.name, best.addr, false
	}
	return "", "", false
}

// ── 3. Mémoire épuisée ─────────────────────────────────────────────────────

var heapOOMRe = regexp.MustCompile(`JavaScript heap out of memory|java\.lang\.OutOfMemoryError|MemoryError\b|fatal error: runtime: out of memory|Allowed memory size of \d+ bytes exhausted`)

func (a *analysis) outOfMemory() []Finding {
	s := a.in.State
	var ev []string
	killed := false
	if s.Status == "OOM_KILLED" {
		ev, killed = append(ev, stateLine("status", s.Status)), true
	}
	if s.LastReason == "OOMKilled" {
		line := stateLine("last_termination.reason", s.LastReason)
		if s.HasExitCode {
			line += " (exit code " + strconv.Itoa(s.LastExitCode) + ")"
		}
		ev, killed = append(ev, line), true
	}
	if s.HasExitCode && s.LastExitCode == 137 && !killed {
		for _, e := range a.in.Events {
			if strings.Contains(strings.ToLower(e.Reason+" "+e.Message), "oom") {
				ev, killed = append(ev, stateLine("last_termination.exit_code", "137"), eventLine(e)), true
				break
			}
		}
	}
	var heap []string
	for _, l := range a.lines {
		if heapOOMRe.MatchString(l) {
			heap = append(heap, l)
		}
	}
	if !killed && len(heap) == 0 {
		return nil
	}
	f := Finding{Code: CodeOutOfMemory, Severity: Danger, Evidence: evidence(append(ev, heap...)...),
		Params: map[string]string{}}
	if a.in.Workload.MemoryLimit != "" {
		f.Params["memory_limit"] = a.in.Workload.MemoryLimit
	}
	if !killed {
		f.Params["kind"] = "runtime_heap" // tas du langage, pas le conteneur
		return []Finding{f}
	}
	cur, ok := memMi(a.in.Workload.MemoryLimit)
	if !ok || a.in.Workload.ID == "" {
		return []Finding{f}
	}
	sizes := append([]Size(nil), a.in.Sizes...)
	sort.SliceStable(sizes, func(i, j int) bool { return sizes[i].MemMi < sizes[j].MemMi })
	for _, sz := range sizes {
		if sz.MemMi <= cur {
			continue
		}
		args := map[string]any{"id": a.in.Workload.ID, "memory_limit": strconv.FormatInt(sz.MemMi, 10) + "Mi"}
		if c, ok := cpuMilli(a.in.Workload.CPULimit); ok && sz.CPUMilli > c {
			args["cpu_limit"] = strconv.FormatInt(sz.CPUMilli, 10) + "m"
		}
		f.Params["next_size"] = sz.ID
		f.Params["next_memory_limit"] = args["memory_limit"].(string)
		f.Fix = &Fix{Tool: ToolUpdateWorkload, Args: args}
		break
	}
	return []Finding{f}
}

// cpuMilli : quantité CPU en millièmes (200m, 0.5, 1).
func cpuMilli(q string) (int64, bool) {
	q = strings.TrimSpace(q)
	if strings.HasSuffix(q, "m") {
		n, err := strconv.ParseInt(strings.TrimSuffix(q, "m"), 10, 64)
		return n, err == nil && n > 0
	}
	f, err := strconv.ParseFloat(q, 64)
	return int64(f*1000 + 0.5), err == nil && f > 0
}

// ── 4. Port d'écoute ──────────────────────────────────────────────────────

var (
	listenRe = regexp.MustCompile(`(?i)(listening (?:on|at)|listen on|running on|started (?:server )?on|serving http on|serving on|bound to|development server at|development server \(|\blocal:)\s*:?\s*(?:port(?:\(s\))?:?\s*)?`)
	addrRes  = []*regexp.Regexp{
		regexp.MustCompile(`^\[([0-9a-fA-F:]*)\]:(\d{2,5})\b`),
		regexp.MustCompile(`^\[?(?:(?:https?|tcp)://)?(\[[0-9a-fA-F:]*\]|[A-Za-z0-9.\-]*):(\d{2,5})\b`),
		regexp.MustCompile(`^()(\d{2,5})\b`),
	}
)

type listenObs struct {
	host string
	port int
	line string
	// portOnly : la ligne donne le port, pas l'interface (« Local: » de Next.js, Vite).
	portOnly bool
}

func listenings(lines []string) []listenObs {
	var out []listenObs
	for _, l := range lines {
		loc := listenRe.FindStringSubmatchIndex(l)
		if loc == nil {
			continue
		}
		phrase := strings.ToLower(l[loc[2]:loc[3]])
		rest := l[loc[1]:]
		for _, re := range addrRes {
			m := re.FindStringSubmatch(rest)
			if m == nil {
				continue
			}
			p, err := strconv.Atoi(m[2])
			if err != nil || p <= 0 || p > 65535 {
				break
			}
			out = append(out, listenObs{host: strings.Trim(strings.ToLower(m[1]), "[]"), port: p, line: l, portOnly: phrase == "local:"})
			break
		}
	}
	return out
}

func isLoopback(host string) bool {
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

func (a *analysis) listening() []Finding {
	w := a.in.Workload
	if w.Kind != "" && w.Kind != "web" && w.Kind != "service" {
		return nil // worker et cron n'écoutent aucun port
	}
	obs := listenings(a.lines)
	if len(obs) == 0 {
		return nil
	}
	var out []Finding
	// Interface de bouclage seulement (pour un port, aucune autre interface vue).
	loop, other := map[int][]string{}, map[int]bool{}
	var loopOrder []int
	for _, o := range obs {
		switch {
		case o.portOnly:
		case isLoopback(o.host):
			if _, ok := loop[o.port]; !ok {
				loopOrder = append(loopOrder, o.port)
			}
			loop[o.port] = append(loop[o.port], o.line)
		default:
			other[o.port] = true
		}
	}
	for _, p := range loopOrder {
		if other[p] {
			continue
		}
		out = append(out, Finding{Code: CodeListensOnLocalhost, Severity: Danger, Evidence: evidence(loop[p]...),
			Params: map[string]string{"port": strconv.Itoa(p)}})
	}
	// Port différent du port cible.
	if w.TargetPort <= 0 {
		return out
	}
	seen, ports := map[int]bool{}, []int{}
	var lines []string
	for _, o := range obs {
		if o.port == w.TargetPort {
			return out // l'appli écoute bien le port cible
		}
		if !seen[o.port] {
			seen[o.port] = true
			ports = append(ports, o.port)
		}
		lines = append(lines, o.line)
	}
	f := Finding{Code: CodePortMismatch, Severity: Danger, Evidence: evidence(lines...),
		Params: map[string]string{"target_port": strconv.Itoa(w.TargetPort)}}
	if len(ports) == 1 {
		f.Params["listening_port"] = strconv.Itoa(ports[0])
		if w.ID != "" {
			f.Fix = &Fix{Tool: ToolUpdateWorkload, Args: map[string]any{"id": w.ID, "target_port": ports[0]}}
		}
	} else {
		list := make([]string, 0, len(ports))
		for _, p := range ports {
			list = append(list, strconv.Itoa(p))
		}
		f.Params["listening_ports"] = strings.Join(list, ",")
	}
	return append(out, f)
}

// ── 5. Variable d'environnement manquante ──────────────────────────────────

const envName = `([A-Z][A-Z0-9_]{1,63})`

var (
	missingEnvRes = []*regexp.Regexp{
		regexp.MustCompile(`(?i:missing (?:required )?env(?:ironment)?(?: variable)?)[:\s]+["'` + "`" + `]?` + envName + `\b`),
		regexp.MustCompile(`(?i:env(?:ironment)? var(?:iable)?)\s+["'` + "`" + `]?` + envName + `["'` + "`" + `]?\s+(?i:(?:is\s+)?(?:not set|not defined|undefined|required|missing|must be set))`),
		regexp.MustCompile(`process\.env\.` + envName + `\s+(?i:is (?:not defined|undefined|required|missing))`),
		regexp.MustCompile(`(?i:required key)\s+` + envName + `\s+(?i:missing value)`),
		regexp.MustCompile(`(?i:could not resolve placeholder)\s+'` + envName + `'`),
		regexp.MustCompile(`(?i:failed assertions:)\s*` + envName + `\s+is missing`),
	}
	keyErrorRe = regexp.MustCompile(`^KeyError: '` + envName + `'`)
	// envBlockRe : en-tête d'une liste de variables (envalid, zod / t3-env).
	envBlockRe   = regexp.MustCompile(`(?i)(missing|invalid) environment variables`)
	envEntryRe   = regexp.MustCompile(`^\s*` + envName + `:\s*(.*)$`)
	zodInlineRe  = regexp.MustCompile(envName + `:\s*\[\s*'Required'`)
	zodRequired  = regexp.MustCompile(`(?i)'required'|undefined|required`)
	envBlockSpan = 10
)

// envServices : variables d'adresse que le branchement d'un service pose.
var envServices = map[string]depService{
	"DATABASE_URL": {Name: "postgres", Database: true}, "DATABASE_URI": {Name: "postgres", Database: true},
	"POSTGRES_URL": {Name: "postgres", Database: true}, "POSTGRES_URI": {Name: "postgres", Database: true},
	"POSTGRESQL_URL": {Name: "postgres", Database: true}, "PG_URL": {Name: "postgres", Database: true},
	"REDIS_URL": {Name: "redis", Template: "redis"}, "REDIS_URI": {Name: "redis", Template: "redis"},
	"VALKEY_URL": {Name: "redis", Template: "redis"},
	"MYSQL_URL":  {Name: "mysql", Template: "mariadb"}, "MARIADB_URL": {Name: "mysql", Template: "mariadb"},
	"MEILISEARCH_URL": {Name: "meilisearch", Template: "meilisearch"}, "MEILI_URL": {Name: "meilisearch", Template: "meilisearch"},
	"MEILISEARCH_HOST": {Name: "meilisearch", Template: "meilisearch"}, "MEILI_HOST": {Name: "meilisearch", Template: "meilisearch"},
}

func (a *analysis) missingEnv() []Finding {
	var order []string
	lines := map[string][]string{}
	add := func(name, line string) {
		if _, ok := lines[name]; !ok {
			order = append(order, name)
		}
		lines[name] = append(lines[name], line)
	}
	for i, l := range a.lines {
		for _, re := range missingEnvRes {
			if m := re.FindStringSubmatch(l); m != nil {
				add(m[1], l)
				break
			}
		}
		if m := keyErrorRe.FindStringSubmatch(strings.TrimSpace(l)); m != nil {
			from := max(0, i-6)
			if strings.Contains(strings.Join(a.lines[from:i], "\n"), "environ") {
				add(m[1], l)
			}
		}
		if bm := envBlockRe.FindStringSubmatch(l); bm != nil {
			missing := strings.EqualFold(bm[1], "missing")
			for _, m := range zodInlineRe.FindAllStringSubmatch(l, -1) {
				add(m[1], l)
			}
			for j := i + 1; j < len(a.lines) && j <= i+envBlockSpan; j++ {
				m := envEntryRe.FindStringSubmatch(a.lines[j])
				if m == nil {
					if strings.Trim(strings.TrimSpace(a.lines[j]), "=-{}") == "" {
						continue // cadre de l'encadré
					}
					break
				}
				if missing || zodRequired.MatchString(m[2]) {
					add(m[1], a.lines[j])
				}
			}
		}
	}
	vars := map[string]Variable{}
	for _, v := range a.in.Workload.Variables {
		vars[v.Name] = v
	}
	var out []Finding
	for _, name := range order {
		f := Finding{Code: CodeMissingEnv, Severity: Danger, Evidence: evidence(lines[name]...),
			Params: map[string]string{"name": name}}
		if v, ok := vars[name]; ok {
			f.Params["already_set"] = "true"
			if v.Secret {
				f.Params["secret"] = "true"
			}
		} else if svc, ok := envServices[name]; ok {
			f.Params["service"] = svc.Name
			f.Fix = a.attachFor(svc, name, f.Params)
		}
		out = append(out, f)
	}
	return out
}

// ── 6. Commande de démarrage ───────────────────────────────────────────────

var (
	wrongArchRe = regexp.MustCompile(`exec format error`)
	badStartRes = []*regexp.Regexp{
		regexp.MustCompile(`Error: Cannot find module '([^']+)'`),
		regexp.MustCompile(`ModuleNotFoundError: No module named '([^']+)'`),
		regexp.MustCompile(`Could not import module "([^"]+)"`),
		regexp.MustCompile(`exec(?:: "| )([^\s":]+)"?: (?:executable file not found in \$PATH|no such file or directory)`),
		regexp.MustCompile(`(?:^|\s)(?:/bin/sh|sh|bash|/bin/bash)(?:: \d+)?: ([^\s:]+): (?:not found|No such file or directory)`),
		regexp.MustCompile(`Error: Unable to access jarfile (\S+)`),
		regexp.MustCompile(`Error: Could not find or load main class (\S+)`),
		regexp.MustCompile(`Could not open input file: (\S+)`),
		regexp.MustCompile(`python[0-9.]*: can't open file '([^']+)'`),
	}
)

func (a *analysis) startCommand() []Finding {
	texts := append([]string(nil), a.lines...)
	for _, e := range a.in.Events {
		texts = append(texts, eventLine(e))
	}
	var arch, bad []string
	missing := ""
	for _, l := range texts {
		if wrongArchRe.MatchString(l) {
			arch = append(arch, l)
			continue
		}
		for _, re := range badStartRes {
			if m := re.FindStringSubmatch(l); m != nil {
				bad = append(bad, l)
				if missing == "" {
					missing = m[1]
				}
				break
			}
		}
	}
	var out []Finding
	if len(arch) > 0 {
		f := Finding{Code: CodeWrongArchitecture, Severity: Danger, Evidence: evidence(arch...), Params: map[string]string{}}
		if a.in.Workload.Image != "" {
			f.Params["image"] = a.in.Workload.Image
		}
		out = append(out, f)
	}
	if len(bad) > 0 {
		out = append(out, Finding{Code: CodeBadStartCommand, Severity: Danger, Evidence: evidence(bad...),
			Params: map[string]string{"missing": missing}})
	}
	return out
}

// ── 7. Image introuvable ou refusée ───────────────────────────────────────

var pullMsgRe = regexp.MustCompile(`(?i)manifest unknown|not found|unauthorized|denied|forbidden|failed to pull|pull access|authentication required`)

func (a *analysis) imagePull() []Finding {
	s := a.in.State
	var ev []string
	msgs := ""
	for _, e := range a.in.Events {
		pullReason := e.Reason == "ErrImagePull" || e.Reason == "ImagePullBackOff" || e.Reason == "InvalidImageName"
		failedPull := e.Reason == "Failed" && strings.Contains(strings.ToLower(e.Message), "pull") && pullMsgRe.MatchString(e.Message)
		if pullReason || failedPull {
			ev = append(ev, eventLine(e))
			msgs += " " + strings.ToLower(e.Message)
		}
	}
	if s.Status == "IMAGE_PULL_FAILED" {
		ev = append(ev, stateLine("status", s.Status))
	}
	if s.PullCause != "" {
		ev = append(ev, stateLine("pull_failure.cause", s.PullCause))
	}
	if len(ev) == 0 {
		return nil
	}
	cause := s.PullCause
	if cause == "" {
		switch {
		case strings.Contains(msgs, "unauthorized") || strings.Contains(msgs, "denied") || strings.Contains(msgs, "forbidden") || strings.Contains(msgs, "authentication required"):
			cause = "denied"
		case strings.Contains(msgs, "manifest unknown") || strings.Contains(msgs, "not found"):
			cause = "not_found"
		}
	}
	f := Finding{Code: CodeImagePull, Severity: Danger, Evidence: evidence(ev...), Params: map[string]string{}}
	if a.in.Workload.Image != "" {
		f.Params["image"] = a.in.Workload.Image
	}
	if s.PullRegistry != "" {
		f.Params["registry"] = s.PullRegistry
	}
	if cause != "" {
		f.Params["cause"] = cause
	}
	if cause == "denied" || cause == "denied_or_not_found" {
		// Registre privé externe : l'accès se donne dans l'écran des registres.
		f.Params["screen"] = "/registries"
	}
	return []Finding{f}
}

// ── 8. Sonde de santé ──────────────────────────────────────────────────────

var (
	probeCodeRe = regexp.MustCompile(`statuscode: (\d{3})`)
	probeKindRe = regexp.MustCompile(`^(Readiness|Liveness|Startup) probe`)
	// accessOKRes : ligne d'accès qui montre une réponse 200 sur un chemin.
	accessOKRes = []*regexp.Regexp{
		regexp.MustCompile(`"?(?:GET|HEAD) (/[^\s"?]*)[^\s"]* (?:HTTP/[0-9.]+"? )?200\b`),
		regexp.MustCompile(`\| 200 \|.*\| (?:GET|HEAD)\s+"(/[^"?]*)`),
	}
	healthPathRe = regexp.MustCompile(`(?i)health|ready|live|status|ping`)
)

func (a *analysis) healthCheck() []Finding {
	var ev []string
	code, kind, refused := "", "", false
	for _, e := range a.in.Events {
		if e.Reason != "Unhealthy" && !strings.Contains(strings.ToLower(e.Message), "probe failed") {
			continue
		}
		ev = append(ev, eventLine(e))
		if m := probeCodeRe.FindStringSubmatch(e.Message); m != nil {
			code = m[1]
		}
		if m := probeKindRe.FindStringSubmatch(e.Message); m != nil && kind == "" {
			kind = strings.ToLower(m[1])
		}
		refused = refused || strings.Contains(e.Message, "connection refused")
	}
	if len(ev) == 0 {
		return nil
	}
	w := a.in.Workload
	f := Finding{Code: CodeHealthCheckFailing, Severity: Warn, Evidence: evidence(ev...), Params: map[string]string{}}
	if w.HealthPath != "" {
		f.Params["path"] = w.HealthPath
	}
	if code != "" {
		f.Params["code"] = code
	}
	if kind != "" {
		f.Params["probe"] = kind
	}
	if refused {
		f.Params["reason"] = "connection_refused"
	}
	// L'appli répond 200 sur un autre chemin de santé : le proposer.
	for i := len(a.lines) - 1; i >= 0 && w.ID != "" && code != ""; i-- {
		for _, re := range accessOKRes {
			m := re.FindStringSubmatch(a.lines[i])
			if m == nil || m[1] == w.HealthPath || !healthPathRe.MatchString(m[1]) {
				continue
			}
			f.Params["answers_200_on"] = m[1]
			f.Evidence = append(f.Evidence, clip(a.lines[i]))
			f.Fix = &Fix{Tool: ToolUpdateWorkload, Args: map[string]any{"id": w.ID, "health_path": m[1]}}
			return []Finding{f}
		}
	}
	return []Finding{f}
}

// ── 9. Redémarrages en boucle sans cause trouvée ───────────────────────────

var errorLineRe = regexp.MustCompile(`(?i)error|exception|fatal|panic|traceback|failed|refused`)

func (a *analysis) crashloopUnknown(found []Finding) *Finding {
	s := a.in.State
	if len(found) > 0 || (s.Status != "CRASHLOOP" && s.Restarts < 3) {
		return nil
	}
	var errs []string
	for _, l := range a.lines {
		if errorLineRe.MatchString(l) {
			errs = append(errs, l)
		}
	}
	if len(errs) == 0 && len(a.lines) > 0 {
		errs = a.lines[max(0, len(a.lines)-maxEvidence):]
	}
	if len(errs) == 0 {
		errs = []string{stateLine("status", s.Status) + ", restarts = " + strconv.Itoa(s.Restarts)}
	}
	f := &Finding{Code: CodeCrashloopUnknown, Severity: Warn, Evidence: evidence(errs...),
		Params: map[string]string{"restarts": strconv.Itoa(s.Restarts)}}
	if s.Status != "" {
		f.Params["status"] = s.Status
	}
	if s.LastReason != "" {
		f.Params["last_reason"] = s.LastReason
	}
	if s.HasExitCode {
		f.Params["exit_code"] = strconv.Itoa(s.LastExitCode)
	}
	return f
}

// ── 10. Base du projet arrêtée ─────────────────────────────────────────────

func (a *analysis) databaseStopped() []Finding {
	w := a.in.Workload
	var out []Finding
	for _, d := range a.in.Databases {
		if d.Status != "STOPPED" {
			continue
		}
		variable, attached := "", false
		for _, at := range d.Attached {
			if at.WorkloadID == w.ID {
				attached, variable = true, at.Variable
			}
		}
		if !attached {
			continue
		}
		f := Finding{Code: CodeDatabaseStopped, Severity: Danger,
			Evidence: []string{fmt.Sprintf("database %s: status = %s, attached to this container", d.Name, d.Status)},
			Params:   map[string]string{"database": d.Name}}
		if variable != "" {
			f.Params["variable"] = variable
		}
		if w.ProjectID != "" && d.ID != "" {
			f.Fix = &Fix{Tool: ToolStartDatabase, Args: map[string]any{"id": w.ProjectID, "databaseID": d.ID}}
		}
		out = append(out, f)
	}
	return out
}
