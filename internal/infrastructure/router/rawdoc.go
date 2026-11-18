package router

import (
	"fmt"
	"net/http"
	"reflect"
	"strconv"

	"github.com/wI2L/fizz/openapi"
)

// Routes « brutes » (wrapRawHandler : flux de journaux, envoi de fichiers en
// multipart) : fizz ne documente que les handlers tonic, elles manquaient donc
// à la spécification publique — et à tout ce qui s'en sert (console, CLI,
// outils du Copilot). Documented leur donne la même place que les autres.

// RawDoc : description d'une route brute.
type RawDoc struct {
	// Input : structure des paramètres (étiquettes path:/query:), la même
	// que celle du handler.
	Input any
	// Status : code de succès (200 par défaut).
	Status int
	// Produces : type de la réponse de succès. MIMEJSON avec Output, ou un
	// flux texte (MIMETextStream).
	Produces string
	// Output : modèle JSON de la réponse (Produces == MIMEJSON).
	Output any
	// Multipart : champs du corps multipart/form-data, s'il y en a.
	Multipart []FormField
}

// FormField : un champ multipart.
type FormField struct {
	Name        string
	Description string
	File        bool
	Required    bool
}

const (
	MIMEJSON       = "application/json"
	MIMETextStream = "text/plain"
)

// Documented : décrit une route brute dans la spécification.
func (r *Route) Documented(doc RawDoc) *Route {
	r.Raw = &doc
	return r
}

// documentRawRoutes : ajoute au spec les routes brutes décrites, après
// l'enregistrement de toutes les routes (même parcours que registerGroup).
func documentRawRoutes(gen *openapi.Generator, groups []*RouteGroup) {
	var walk func(g *RouteGroup, absPath, tag string, internal bool)
	walk = func(g *RouteGroup, absPath, tag string, internal bool) {
		if g.tag.name != "" {
			tag = g.tag.name
		}
		for _, r := range g.Routes {
			if r.Raw == nil {
				continue
			}
			if err := addRawOperation(gen, r, buildFullPath(absPath, r.Path), tag, internal); err != nil {
				panic(fmt.Sprintf("documentation de %s %s : %v", r.Method, r.Path, err))
			}
		}
		for _, child := range g.Groups {
			walk(child, absPath+child.Path, tag, internal || child.internal)
		}
	}
	for _, g := range groups {
		walk(g, g.Path, g.tag.name, g.internal)
	}
}

func addRawOperation(gen *openapi.Generator, r *Route, fullPath, tag string, internal bool) error {
	doc := r.Raw
	if doc.Input == nil {
		return fmt.Errorf("RawDoc.Input manquant")
	}
	status := doc.Status
	if status == 0 {
		status = http.StatusOK
	}
	oi := &openapi.OperationInfo{ID: r.ID, StatusCode: status, StatusDescription: http.StatusText(status)}
	if oi.ID == "" {
		oi.ID = generateOperationID(r.Method, fullPath)
	}
	for _, opt := range docOptions(r, internal) {
		opt(oi)
	}

	var out reflect.Type
	if doc.Produces == MIMEJSON && doc.Output != nil {
		out = reflect.TypeOf(doc.Output)
	}
	op, err := gen.AddOperation(fullPath, r.Method, tag, reflect.TypeOf(doc.Input), out, oi)
	if err != nil {
		return err
	}
	if op == nil {
		return nil
	}
	code := strconv.Itoa(status)
	if doc.Produces == MIMETextStream {
		resp := op.Responses[code]
		if resp == nil || resp.Response == nil {
			resp = &openapi.ResponseOrRef{Response: &openapi.Response{Description: http.StatusText(status)}}
			op.Responses[code] = resp
		}
		resp.Response.Content = map[string]*openapi.MediaTypeOrRef{
			MIMETextStream: {MediaType: &openapi.MediaType{Schema: &openapi.SchemaOrRef{Schema: &openapi.Schema{
				Type: "string", Description: "Flux texte, une ligne par entrée (HTTP chunked)",
			}}}},
		}
	}
	if len(doc.Multipart) > 0 {
		props := map[string]*openapi.SchemaOrRef{}
		var required []string
		for _, f := range doc.Multipart {
			s := &openapi.Schema{Type: "string", Description: f.Description}
			if f.File {
				s.Format = "binary"
			}
			props[f.Name] = &openapi.SchemaOrRef{Schema: s}
			if f.Required {
				required = append(required, f.Name)
			}
		}
		op.RequestBody = &openapi.RequestBody{Required: true, Content: map[string]*openapi.MediaType{
			"multipart/form-data": {Schema: &openapi.SchemaOrRef{Schema: &openapi.Schema{
				Type: "object", Properties: props, Required: required,
			}}},
		}}
	}
	return nil
}
