/** @param {{ a: * }} input */
function looperContextCd({ a }) {
  return a.cwd ? 'Run every command in cwd ' + JSON.stringify(a.cwd) + '. ' : ''
}

/** @param {{ a: * }} input */
function looperContextRules({ a }) {
  return a.rules || 'Never git stash|checkout|reset|restore|clean|commit; never install/deploy/release or edit tool config.'
}
