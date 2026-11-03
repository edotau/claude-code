export type Usage = { name: string; line: string }

declare module 'claude-code' {
  interface PluginState {
    'slash-usage': { active: Usage[] }
  }
}
