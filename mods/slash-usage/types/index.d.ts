// One piece of a usage row: filled already, the next to give, or still open.
export type Part = { text: string; state: 'done' | 'now' | 'todo' }

export type Usage = { name: string; line: string; parts?: Part[] }

declare module 'claude-code' {
  interface PluginState {
    'slash-usage': { active: Usage[] }
  }
}
