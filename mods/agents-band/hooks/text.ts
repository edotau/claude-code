export const WIDTH = 80

export const strings = (v: unknown): string[] => (Array.isArray(v) ? v.filter((x): x is string => typeof x === 'string' && x !== '') : [])

export const clip = (s: string): string => (s.length > WIDTH ? `${s.slice(0, WIDTH - 1)}…` : s)

export const age = (ms: number): string => {
  const s = Math.max(0, Math.round(ms / 1000))

  return s < 60 ? `${s}s` : `${Math.floor(s / 60)}m${s % 60}s`
}

// shorten maps each path to its shortest trailing segments that no other path in the set shares.
export const shorten = (paths: string[]): Map<string, string> => {
  const uniq = [...new Set(paths)]
  const tail = (p: string, n: number): string => p.split('/').slice(-n).join('/')
  const out = new Map<string, string>()
  for (const p of uniq) {
    let n = 1
    while (n < p.split('/').length && uniq.some(o => o !== p && tail(o, n) === tail(p, n))) n++
    out.set(p, tail(p, n))
  }

  return out
}

// pack joins names into lines no wider than the band.
export const pack = (names: string[]): string[] => {
  const lines: string[] = []
  for (const n of names) {
    const last = lines.length - 1
    if (last >= 0 && lines[last]!.length + 1 + n.length <= WIDTH - 6) lines[last] += ` ${n}`
    else lines.push(n)
  }

  return lines
}

