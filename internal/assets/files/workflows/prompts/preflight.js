/** @param {{ EXT: * }} input */
function preflightBrief({ EXT }) {
  return 'For each line, run it and report its candidate count and the first candidate as agent/account/model. Report what you find.\n' +
      EXT.map((s) => `claude-code gateway --agent ${s.agent} --fallback agents${s.profile ? ' --profile ' + s.profile : ''}${s.provider ? ' --provider ' + s.provider : ''} --format json </dev/null`).join('\n')
}
