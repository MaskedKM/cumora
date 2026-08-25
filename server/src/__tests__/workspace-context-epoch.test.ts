import { test } from 'node:test'
import assert from 'node:assert/strict'
import { commitIfEpochCurrent } from '../../../apps/web/src/stores/contextEpoch'

interface Deferred<T> {
  promise: Promise<T>
  resolve: (value: T) => void
}

function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((done) => { resolve = done })
  return { promise, resolve }
}

test('a late request from the previous workspace cannot overwrite the current workspace', async () => {
  const requestA = deferred<string>()
  const requestB = deferred<string>()
  let contextEpoch = 0
  let stored = ''
  const commitIfCurrent = (request: () => Promise<string>) => commitIfEpochCurrent(
    () => contextEpoch,
    request,
    (value) => { stored = value },
  )

  const loadA = commitIfCurrent(() => requestA.promise)
  contextEpoch += 1
  const loadB = commitIfCurrent(() => requestB.promise)

  requestB.resolve('workspace-b data')
  assert.equal(await loadB, true)
  assert.equal(stored, 'workspace-b data')

  requestA.resolve('workspace-a data')
  assert.equal(await loadA, false)
  assert.equal(stored, 'workspace-b data')
})
