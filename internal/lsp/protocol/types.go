package protocol

import (
	"encoding/json"
	"net/url"
	"strings"
)

// Initialize

type InitializeParams struct {
	ProcessID    *int               `json:"processId"`
	RootURI      string             `json:"rootUri"`
	Capabilities ClientCapabilities `json:"capabilities"`
}

type ClientCapabilities struct {
	TextDocument TextDocumentClientCapabilities `json:"textDocument"`
	Window       WindowClientCapabilities       `json:"window,omitempty"`
}

type TextDocumentClientCapabilities struct {
	Definition     GenericCapability `json:"definition,omitempty"`
	References     GenericCapability `json:"references,omitempty"`
	Hover          GenericCapability `json:"hover,omitempty"`
	Implementation GenericCapability `json:"implementation,omitempty"`
	PublishDiagnostics struct {
		RelatedInformation bool `json:"relatedInformation,omitempty"`
	} `json:"publishDiagnostics,omitempty"`
}

type GenericCapability struct {
	DynamicRegistration bool `json:"dynamicRegistration,omitempty"`
}

type WindowClientCapabilities struct {
	WorkDoneProgress bool `json:"workDoneProgress"`
}

type InitializeResult struct {
	Capabilities ServerCapabilities `json:"capabilities"`
}

type ServerCapabilities struct {
	DefinitionProvider     any `json:"definitionProvider,omitempty"`
	ReferencesProvider     any `json:"referencesProvider,omitempty"`
	HoverProvider          any `json:"hoverProvider,omitempty"`
	ImplementationProvider any `json:"implementationProvider,omitempty"`
	DiagnosticProvider     any `json:"diagnosticProvider,omitempty"`
}

// Position and Location

type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

type LocationLink struct {
	TargetURI            string `json:"targetUri"`
	TargetRange          Range  `json:"targetRange"`
	TargetSelectionRange Range  `json:"targetSelectionRange"`
}

// TextDocument params

type TextDocumentIdentifier struct {
	URI string `json:"uri"`
}

type TextDocumentPositionParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
}

type ReferenceParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
	Context      ReferenceContext        `json:"context"`
}

type ReferenceContext struct {
	IncludeDeclaration bool `json:"includeDeclaration"`
}

// TextDocument items (for didOpen)

type TextDocumentItem struct {
	URI        string `json:"uri"`
	LanguageID string `json:"languageId"`
	Version    int    `json:"version"`
	Text       string `json:"text"`
}

type DidOpenTextDocumentParams struct {
	TextDocument TextDocumentItem `json:"textDocument"`
}

// Hover

type HoverResult struct {
	Contents HoverContents `json:"contents"`
	Range    *Range        `json:"range,omitempty"`
}

type HoverContents struct {
	Value string
}

func (h *HoverContents) UnmarshalJSON(data []byte) error {
	var mc MarkupContent
	if err := json.Unmarshal(data, &mc); err == nil && mc.Value != "" {
		h.Value = mc.Value
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		h.Value = s
		return nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(data, &arr); err == nil && len(arr) > 0 {
		var first MarkupContent
		if err := json.Unmarshal(arr[0], &first); err == nil && first.Value != "" {
			h.Value = first.Value
			return nil
		}
		var fs string
		if err := json.Unmarshal(arr[0], &fs); err == nil {
			h.Value = fs
			return nil
		}
	}
	h.Value = string(data)
	return nil
}

type MarkupContent struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// Diagnostics

type PublishDiagnosticsParams struct {
	URI         string       `json:"uri"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

type Diagnostic struct {
	Range    Range  `json:"range"`
	Severity *int   `json:"severity,omitempty"`
	Message  string `json:"message"`
	Source   string `json:"source,omitempty"`
}

const (
	SeverityError   = 1
	SeverityWarning = 2
	SeverityInfo    = 3
	SeverityHint    = 4
)

// Progress

type ProgressParams struct {
	Token json.RawMessage `json:"token"`
	Value json.RawMessage `json:"value"`
}

type WorkDoneProgressValue struct {
	Kind string `json:"kind"`
}

// Workspace Symbol

type WorkspaceSymbolParams struct {
	Query string `json:"query"`
}

type SymbolInformation struct {
	Name     string   `json:"name"`
	Kind     int      `json:"kind"`
	Location Location `json:"location"`
}

var SymbolKindName = map[int]string{
	1: "File", 2: "Module", 3: "Namespace", 4: "Package", 5: "Class",
	6: "Method", 7: "Property", 8: "Field", 9: "Constructor", 10: "Enum",
	11: "Interface", 12: "Function", 13: "Variable", 14: "Constant",
	15: "String", 16: "Number", 17: "Boolean", 18: "Array", 19: "Object",
	20: "Key", 21: "Null", 22: "EnumMember", 23: "Struct", 24: "Event",
	25: "Operator", 26: "TypeParameter",
}

// Helpers

func URIFromPath(absPath string) string {
	u := url.URL{Scheme: "file", Path: absPath}
	return u.String()
}

func PathFromURI(uri string) string {
	u, err := url.Parse(uri)
	if err != nil {
		return strings.TrimPrefix(uri, "file://")
	}
	return u.Path
}
