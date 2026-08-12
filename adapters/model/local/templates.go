package local

import (
	"context"
	"fmt"

	"proactive-interaction-engine/internal/application/port"
)

type Templates struct {
	values map[string]string
}

func NewTemplates() Templates {
	return Templates{values: map[string]string{
		"welcome.return": "你回来啦。",
	}}
}

func (t Templates) Realize(ctx context.Context, request port.UtteranceRequest) (port.Utterance, error) {
	if err := ctx.Err(); err != nil {
		return port.Utterance{}, err
	}
	text, ok := t.values[request.TemplateID]
	if !ok {
		return port.Utterance{}, fmt.Errorf("template %s not found", request.TemplateID)
	}
	return port.Utterance{Text: text, UsedFallback: true, ModelVersion: "local-templates.v1"}, nil
}
