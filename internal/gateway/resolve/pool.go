package resolve

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/textutil"
)

// PoolMembers is the rotation pool a rotation pick drew from; nil for any other pick.
func PoolMembers(r agent.ModelRoute, choice *agent.AutoChoice) []agent.PoolMember {
	if choice.Criterion != "rotation" {
		return nil
	}
	return agent.RotationMembers(r.Agent, r.Profile, choice.Tier, time.Now())
}

// PoolLine is one pool member: a marker, the model, then its hold or last pick; at most 80 columns.
func PoolLine(m agent.PoolMember) string {
	mark, note := " ", "never picked"
	switch {
	case m.Held:
		mark, note = "x", "held until "+m.HeldUntil.Local().Format("Jan 2 15:04")
	case m.Next:
		mark = ">"
	}
	if !m.Held && m.LastPicked != nil {
		note = "picked " + m.LastPicked.Local().Format("Jan 2 15:04")
	}
	line := fmt.Sprintf("  %s %s  %s", mark, m.Model, note)
	if len(line) > 80 {
		line = line[:79] + "…"
	}
	return line
}

// poolAgents are the five agents the pool table walks.
var poolAgents = append([]string{agentid.Claude}, agent.DefaultAgents...)

// poolTiers are the auto tiers in print order, keyed by the alias TaskTier returns.
var poolTiers = []struct{ name, alias string }{{"fast", models.Haiku}, {"standard", models.Sonnet}, {"deep", models.Opus}}

// PoolRow is one table row; json reuses the auto block's pool member fields.
type PoolRow struct {
	Row        string             `json:"row"`
	Agent      string             `json:"agent"`
	Tier       string             `json:"tier"`
	Size       int                `json:"size"`
	Next       string             `json:"next"`
	Held       int                `json:"held"`
	LastPicked *time.Time         `json:"last_picked,omitempty"`
	Note       string             `json:"note,omitempty"`
	Pool       []agent.PoolMember `json:"pool"`
}

// Tiers is the tier names task selects, in print order; every tier for no brief.
func Tiers(task string) []string {
	alias, _ := agent.TaskTier(task)
	var tiers []string
	for _, t := range poolTiers {
		if alias == "" || t.alias == alias {
			tiers = append(tiers, t.name)
		}
	}
	return tiers
}

// PoolRows resolves every rotation row x agent x tier through the single-agent path, sorted agent, row, tier.
func PoolRows(ctx context.Context, reg *providers.Registry, tiers []string, tools bool, fallback string, now time.Time) []PoolRow {
	var out []PoolRow
	for _, p := range allRows(reg) {
		if !router.RotationRow(p) {
			continue
		}
		for _, name := range poolAgents {
			if agent.VaultAgentIsClaude(name) || !agent.CanServe(name, p) {
				continue
			}
			for _, tier := range tiers {
				if row, ok := poolCell(ctx, reg, name, p, tier, tools, fallback, now); ok {
					out = append(out, row)
				}
			}
		}
	}
	slices.SortStableFunc(out, func(a, b PoolRow) int {
		return cmp.Or(strings.Compare(a.Agent, b.Agent), strings.Compare(a.Row, b.Row), tierRank(a.Tier)-tierRank(b.Tier))
	})
	return out
}

func tierRank(tier string) int {
	return slices.IndexFunc(poolTiers, func(t struct{ name, alias string }) bool { return t.name == tier })
}

// poolCell is name's auto pick on p at tier; false when p cannot run name at all (claude on a login row).
func poolCell(ctx context.Context, reg *providers.Registry, name string, p *providers.Provider, tier string,
	tools bool, fallback string, now time.Time) (PoolRow, bool) {
	row := PoolRow{Row: router.AccountOf(p), Agent: name, Tier: tier}
	q, err := QueryForRoute(ctx, name, agent.AutoModel, route.Route{Profile: p.Name}, "")
	if err != nil {
		row.Note = "cannot run"
		return row, true
	}
	q.Task, q.Tools, q.Fallback = "Tier: "+tier, tools, cmp.Or(fallback, q.Fallback)
	r, choice, err := FirstHop(ctx, reg, q, q.Task)
	switch {
	case errors.Is(err, agent.ErrPoolExhausted):
		row.Note, row.Pool = "exhausted", agent.RotationMembers(name, p.Name, tier, now)
	case err != nil && agent.VaultAgentIsClaude(name) && errors.Is(err, agent.ErrModelRoute):
		return row, false
	case err != nil:
		row.Note = "cannot run"
	case choice != nil:
		row.Next, row.Pool = r.Model, PoolMembers(r, choice)
		if choice.Criterion == "rotation" && len(row.Pool) == 0 { // tierRotation stepped: the row serves nothing here
			row.Note, row.Pool = "→ standard", agent.RotationMembers(name, p.Name, "standard", now)
		}
	default:
		row.Next = r.Model
	}
	return tallyPool(row, agent.VaultAgentIsClaude(name)), true
}

// tallyPool fills the held count, newest pick and size from the pool, and why an empty pool is empty.
func tallyPool(row PoolRow, claude bool) PoolRow {
	for _, m := range row.Pool {
		if m.Held {
			row.Held++
		}
		if m.LastPicked != nil && (row.LastPicked == nil || m.LastPicked.After(*row.LastPicked)) {
			row.LastPicked = m.LastPicked
		}
	}
	row.Size = len(row.Pool)
	if row.Size == 0 && row.Note == "" {
		row.Note = map[bool]string{true: "tier model", false: "no listing"}[claude]
	}
	return row
}

// RenderPoolTable prints the table; width > 0 (a terminal) clips NOTE first, then NEXT, so no line passes width.
func RenderPoolTable(w io.Writer, rows []PoolRow, width int, now time.Time) {
	head := [8]string{"ROW", "AGENT", "TIER", "POOL", "NEXT", "HELD", "LAST", "NOTE"}
	cells := [][8]string{head}
	for _, r := range rows {
		last := "-"
		if r.LastPicked != nil {
			last = agent.ShortDur(now.Sub(*r.LastPicked))
		}
		next := r.Next
		if strings.HasPrefix(r.Row, router.RouteDatabricks) { // ROW already says it: the prefix only costs the NOTE its room
			next = strings.TrimPrefix(next, "databricks-")
		}
		cells = append(cells, [8]string{r.Row, r.Agent, r.Tier, strconv.Itoa(r.Size), next, strconv.Itoa(r.Held), last, r.Note})
	}
	var widths [8]int
	for _, c := range cells {
		for i, s := range c {
			widths[i] = max(widths[i], textutil.DisplayWidth(s))
		}
	}
	if width > 0 {
		fixed := 7 + widths[0] + widths[1] + widths[2] + widths[3] + widths[5] + widths[6] // 7 column gaps
		widths[7] = max(min(widths[7], width-fixed-widths[4]), 0)
		widths[4] = max(min(widths[4], width-fixed-widths[7]), 0)
	}
	for _, c := range cells {
		var line [8]string
		for i, s := range c {
			line[i] = textutil.PadCells(textutil.TruncateCells(s, widths[i], "…"), widths[i], i == 3 || i == 5)
		}
		fmt.Fprintln(w, strings.TrimRight(strings.Join(line[:], " "), " "))
	}
}
