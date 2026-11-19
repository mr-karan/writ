package ui

// These are the CLI's documents, as docs/json-output.md defines them, read
// only for the fields the pages show. The API's own documents are the
// server package's types.

// docDiag is a diagnostic of a CLI document.
type docDiag struct {
	Severity string `json:"severity"`
	Class    string `json:"class"`
	File     string `json:"file"`
	Line     int    `json:"line"`
	Col      int    `json:"col"`
	Message  string `json:"message"`
	Fix      *struct {
		Message string `json:"message"`
	} `json:"fix"`
	// Adopted is set on the diagnostic that the page's adopt note words,
	// which the list then shows as one short line.
	Adopted bool `json:"-"`
}
