package memory

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/providers"
)

// routerLink reads the running loopback router address and client secret without starting it.
var routerLink = func(ctx context.Context) (base, secret string, ok bool) {
	st, up := gateway.Running(ctx)
	if !up {
		return "", "", false
	}
	secret, err := gateway.ClientSecret()
	return gateway.Base(st.Port), secret, err == nil
}

// embeddingModelFor picks p's embeddings endpoint from the models it serves.
func embeddingModelFor(ctx context.Context, reg *providers.Registry, p *providers.Provider) (string, error) {
	infos, err := providers.ListModels(ctx, reg, p)
	if err != nil {
		return "", err
	}
	model, ok := pickEmbeddingModel(infos)
	if !ok {
		return "", fmt.Errorf("profile %s serves no embeddings endpoint", p.Name)
	}
	return model, nil
}

// openDatabricksEmbed is profile's embeddings client; credentials come only from providers.Authorize.
func openDatabricksEmbed(ctx context.Context, profile, model string) (*EmbedClient, error) {
	reg, err := providers.Load()
	if err != nil {
		return nil, err
	}
	sel, err := providers.Select(reg, profile) // "" = the agent record, then the registry default
	if err != nil {
		return nil, err
	}
	p := sel.Provider
	if p.Preset != providers.PresetDatabricks || p.Host == "" {
		return nil, fmt.Errorf("profile %s is not a databricks workspace", p.Name)
	}
	if model == "" {
		if model, err = embeddingModelFor(ctx, reg, p); err != nil {
			return nil, err
		}
	}
	ns := p.Name + "|" + strings.TrimRight(p.Host, "/") + "|" + model
	return &EmbedClient{Profile: p.Name, Model: model, Namespace: ns, EmbedFunc: func(ctx context.Context, texts []string) ([][]float32, error) {
		return embedDatabricks(ctx, p, model, texts)
	}}, nil
}

func embedDatabricks(ctx context.Context, p *providers.Provider, model string, texts []string) ([][]float32, error) {
	url := strings.TrimRight(p.Host, "/") + "/serving-endpoints/" + model + "/invocations"
	if base, secret, ok := routerLink(ctx); ok { // the daemon holds the warm credential and upstream connections
		via := base + "/p/" + p.Name + "/" + providers.ProtocolOpenAI + "/" + model + "/invocations"
		vecs, err := PostEmbeddings(ctx, via, texts, func(h http.Header) error { h.Set(gateway.ClientHeader, secret); return nil })
		if err == nil || ctx.Err() != nil {
			return vecs, err
		}
	}
	return PostEmbeddings(ctx, url, texts, func(h http.Header) error { return providers.Authorize(ctx, p, h) })
}
