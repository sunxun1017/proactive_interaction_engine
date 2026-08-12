package port

import "context"

type UtteranceRequest struct {
	TemplateID    string
	Style         string
	InteractionID string
	TraceID       string
}

type Utterance struct {
	Text         string
	UsedFallback bool
	ModelVersion string
}

// TextRealizer may use a model, but callers must supply a deadline and a local
// template fallback. It never receives an ActionDriver.
type TextRealizer interface {
	Realize(context.Context, UtteranceRequest) (Utterance, error)
}
