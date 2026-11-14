export type Row = { text: string; isQuiet: boolean; isHead: boolean }
export type Snapshot = { running: number; quiet: number; lost: number; routes: string; rows: Row[]; more: number; error: string }

declare module 'claude-code' {
  interface PluginState {
    'agents-band': { snapshot: Snapshot | null; isOpen: boolean }
  }
}
