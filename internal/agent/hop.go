package agent

import (
	"cmp"
	"context"
	"slices"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/models"
)

// hopFamily is the model family hop d runs: its concrete model's, else its agent's own, "" when neither is known.
func hopFamily(d Dispatch) string {
	if d.Model != "" && !IsAuto(d.Model) {
		return models.FamilyName(d.Model)
	}
	return agentid.Routing(d.agentName()).ModelFamily
}

// hopMoves: a failure at hop from that a later hop of c answers (unavailable, or a policy refusal to another family).
func (c Chain) hopMoves(ctx context.Context, from int, req RunOptions, res wire.Result, err error) bool {
	if from < 0 || from >= len(c.Dispatches) {
		return false
	}
	d := c.Dispatches[from]
	if dispatchUnavailableContext(ctx, d.Agent, res, err) {
		return true
	}
	in := refusedHop{disp: d, req: req, res: res, err: err}
	ran := refusedModel(d, req, res)
	if !policyRefused(in, ran) || !fallsThroughContext(ctx, d.Agent) || explicitModel(ctx, in) {
		return false
	}
	return c.otherFamilyAfter(from, models.FamilyName(ran))
}

// otherFamilyAfter reports a hop after from running a family other than refused.
func (c Chain) otherFamilyAfter(from int, refused string) bool {
	return refused != "" && slices.ContainsFunc(c.Dispatches[from+1:], func(d Dispatch) bool {
		f := hopFamily(d)
		return !d.quotaOverflow && f != "" && f != refused // the overflow hop runs on quota failures only
	})
}

// skipsRefusedFamily: hop d runs the family a policy refusal at prev came from, so it would be refused again.
func skipsRefusedFamily(prev, d Dispatch, req RunOptions, res wire.Result, err error) bool {
	if !retry.IsPolicyRefusal(err) {
		return false
	}
	refused := models.FamilyName(refusedModel(prev, req, res))
	return refused != "" && hopFamily(d) == refused
}

// refusedModel is the model hop d ran: the result's, else d's own concrete one, else the request's pick.
func refusedModel(d Dispatch, req RunOptions, res wire.Result) string {
	for _, m := range []string{res.Model, d.Model, ranModel(d, req, res)} {
		if m != "" && !IsAuto(m) {
			return m
		}
	}
	return ""
}

// autoBeatsCell: a typed or pinned auto replaces the cell's model on its row, never a pi runner hop's picked model.
func autoBeatsCell(out, req RunOptions, disp Dispatch) bool {
	if out.CellModel || disp.runFor != "" {
		return false
	}
	return out.AutoModel || req.Model == "" && pinsAuto(out.Values, cmp.Or(req.Agent, engineName(disp)))
}
