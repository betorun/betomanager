package domain

import (
	"bufio"
	"errors"
	"io"
	"sort"
)

// Lecture de la table des matières d'un dump pg_dump -Fc (format
// « personnalisé »), sans pg_restore : on y lit les schémas et les tables
// que l'import créerait, pour prévenir avant d'importer dans la mauvaise
// base. La table des matières est au début du fichier, avant les données :
// les premiers mégaoctets suffisent. Format : pg_backup_archiver.c
// (ReadHead, ReadToc) et pg_backup_custom.c (_ReadExtraToc).

// Versions du format d'archive (MAKE_ARCHIVE_VERSION).
const (
	archV1_10 = 1<<16 | 10<<8 // tablespace, versions serveur et pg_dump
	archV1_11 = 1<<16 | 11<<8 // section
	archV1_14 = 1<<16 | 14<<8 // méthode d'accès des tables
	archV1_15 = 1<<16 | 15<<8 // algorithme de compression (octet)
	archV1_16 = 1<<16 | 16<<8 // relkind (PostgreSQL 17)
	archMax   = 1<<16 | 16<<8 | 255
)

const (
	tocMaxEntries = 1 << 20
	tocMaxString  = 64 << 20
)

// ErrDumpUnreadable : en-tête ou table des matières illisible.
var ErrDumpUnreadable = errors.New("the dump's table of contents could not be read")

// SchemaTables : tables d'un schéma.
type SchemaTables struct {
	Name   string `json:"name"`
	Tables int    `json:"tables"`
}

// DumpSummary : ce que l'import d'un dump créerait.
type DumpSummary struct {
	// Database : base d'origine du dump.
	Database      string         `json:"database"`
	ServerVersion string         `json:"server_version,omitempty"`
	Schemas       []SchemaTables `json:"schemas"`
	Tables        int            `json:"tables"`
	// Complete : table des matières lue jusqu'au bout (false : fichier
	// tronqué, la liste peut manquer des schémas).
	Complete bool `json:"complete"`
}

type tocReader struct {
	r       *bufio.Reader
	version int
	intSize int
	offSize int
}

func (t *tocReader) byte() (int, error) {
	b, err := t.r.ReadByte()
	return int(b), err
}

func (t *tocReader) int() (int64, error) {
	sign, err := t.byte()
	if err != nil {
		return 0, err
	}
	var v int64
	for i := 0; i < t.intSize; i++ {
		b, err := t.byte()
		if err != nil {
			return 0, err
		}
		if i < 8 {
			v |= int64(b) << (8 * i)
		}
	}
	if sign != 0 {
		v = -v
	}
	return v, nil
}

// str : chaîne (longueur puis octets) ; ok false pour la chaîne NULL (-1).
func (t *tocReader) str() (string, bool, error) {
	n, err := t.int()
	if err != nil {
		return "", false, err
	}
	if n < 0 {
		return "", false, nil
	}
	if n > tocMaxString {
		return "", false, ErrDumpUnreadable
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(t.r, buf); err != nil {
		return "", false, err
	}
	return string(buf), true, nil
}

func (t *tocReader) skipStr(n int) error {
	for i := 0; i < n; i++ {
		if _, _, err := t.str(); err != nil {
			return err
		}
	}
	return nil
}

func (t *tocReader) skipInt(n int) error {
	for i := 0; i < n; i++ {
		if _, err := t.int(); err != nil {
			return err
		}
	}
	return nil
}

// ReadDumpTOC : schémas et tables d'un dump -Fc. Un fichier tronqué au milieu
// de la table des matières donne ce qui a été lu (Complete false) ; un
// en-tête illisible donne ErrNotADump ou ErrDumpUnreadable.
func ReadDumpTOC(r io.Reader) (DumpSummary, error) {
	t := &tocReader{r: bufio.NewReader(r)}
	head := make([]byte, len(dumpMagic))
	if _, err := io.ReadFull(t.r, head); err != nil || !IsCustomDump(head) {
		return DumpSummary{}, ErrNotADump
	}
	var hv [6]int
	for i := range hv { // vmaj, vmin, vrev, intSize, offSize, format
		b, err := t.byte()
		if err != nil {
			return DumpSummary{}, ErrDumpUnreadable
		}
		hv[i] = b
	}
	t.version = hv[0]<<16 | hv[1]<<8 | hv[2]
	t.intSize, t.offSize = hv[3], hv[4]
	// Format 1 = personnalisé ; versions antérieures à PostgreSQL 9.0 refusées.
	if t.version < archV1_10 || t.version > archMax || hv[5] != 1 || t.intSize < 1 || t.intSize > 8 || t.offSize < 1 || t.offSize > 8 {
		return DumpSummary{}, ErrDumpUnreadable
	}
	out := DumpSummary{Schemas: []SchemaTables{}}
	fail := func(err error) (DumpSummary, error) {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return out, nil
		}
		return DumpSummary{}, err
	}

	if t.version >= archV1_15 {
		if _, err := t.byte(); err != nil { // algorithme de compression
			return DumpSummary{}, ErrDumpUnreadable
		}
	} else if err := t.skipInt(1); err != nil { // niveau de compression
		return DumpSummary{}, ErrDumpUnreadable
	}
	if err := t.skipInt(7); err != nil { // date de création (struct tm)
		return DumpSummary{}, ErrDumpUnreadable
	}
	var err error
	if out.Database, _, err = t.str(); err != nil {
		return DumpSummary{}, ErrDumpUnreadable
	}
	if out.ServerVersion, _, err = t.str(); err != nil {
		return DumpSummary{}, ErrDumpUnreadable
	}
	if err := t.skipStr(1); err != nil { // version de pg_dump
		return DumpSummary{}, ErrDumpUnreadable
	}

	count, err := t.int()
	if err != nil {
		return fail(err)
	}
	if count < 0 || count > tocMaxEntries {
		return DumpSummary{}, ErrDumpUnreadable
	}
	tables := map[string]int{}
	for i := int64(0); i < count; i++ {
		ns, desc, tag, err := t.entry()
		if err != nil {
			out.Schemas, out.Tables = schemaList(tables)
			return fail(err)
		}
		switch desc {
		case "SCHEMA":
			if _, ok := tables[tag]; !ok {
				tables[tag] = 0
			}
		case "TABLE":
			tables[ns]++
		}
	}
	out.Schemas, out.Tables = schemaList(tables)
	out.Complete = true
	return out, nil
}

// entry : une entrée de la table des matières (schéma, type, nom).
func (t *tocReader) entry() (ns, desc, tag string, err error) {
	if err = t.skipInt(2); err != nil { // dumpId, hadDumper
		return
	}
	if err = t.skipStr(2); err != nil { // tableoid, oid
		return
	}
	if tag, _, err = t.str(); err != nil {
		return
	}
	if desc, _, err = t.str(); err != nil {
		return
	}
	if t.version >= archV1_11 {
		if err = t.skipInt(1); err != nil { // section
			return
		}
	}
	if err = t.skipStr(3); err != nil { // defn, dropStmt, copyStmt
		return
	}
	if ns, _, err = t.str(); err != nil {
		return
	}
	if err = t.skipStr(1); err != nil { // tablespace
		return
	}
	if t.version >= archV1_14 {
		if err = t.skipStr(1); err != nil { // tableam
			return
		}
	}
	if t.version >= archV1_16 {
		if err = t.skipInt(1); err != nil { // relkind
			return
		}
	}
	if err = t.skipStr(2); err != nil { // owner, withOids
		return
	}
	for { // dépendances, terminées par la chaîne NULL
		var ok bool
		if _, ok, err = t.str(); err != nil {
			return
		}
		if !ok {
			break
		}
	}
	// _ReadExtraToc du format personnalisé : état des données + position.
	if _, err = t.byte(); err != nil {
		return
	}
	for i := 0; i < t.offSize; i++ {
		if _, err = t.byte(); err != nil {
			return
		}
	}
	if desc == "" {
		err = ErrDumpUnreadable
	}
	return
}

func schemaList(m map[string]int) ([]SchemaTables, int) {
	out := make([]SchemaTables, 0, len(m))
	total := 0
	for name, n := range m {
		if name == "" {
			continue
		}
		out = append(out, SchemaTables{Name: name, Tables: n})
		total += n
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, total
}
