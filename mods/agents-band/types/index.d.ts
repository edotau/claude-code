export type Row = { key: string; text: string; isQuiet: boolean; isHead: boolean; details: string[] }
export type Snapshot = { running: number; quiet: number; lost: number; routes: string; rows: Row[]; more: number; at: number; error: string }

declare module 'claude-code' {
  interface PluginState {
    'agents-band': { snapshot: Snapshot | null; isOpen: boolean; expanded: string[]; polledAt: number; hasFiles: boolean }
  }
}
