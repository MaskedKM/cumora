/* eslint-env node */
// computeDegradedFromStatus 规则锁(#314):三形态判据 + 未知面恒 null。
// 字段名与 apps/stack/internal/status+supervise 的 json tag 对齐。
const test = require('node:test')
const assert = require('node:assert/strict')
const { computeDegradedFromStatus } = require('./degraded.cjs')

test('单 unit 形态:子进程全活且未熔断 = 不降级', () => {
  assert.equal(computeDegradedFromStatus({
    stackd: { children: [
      { name: 'postgres', running: true, circuitOpen: false },
      { name: 'server', running: true, circuitOpen: false },
    ] },
  }), false)
})

test('单 unit 形态:任一子进程死 = 降级', () => {
  assert.equal(computeDegradedFromStatus({
    stackd: { children: [
      { name: 'postgres', running: true, circuitOpen: false },
      { name: 'server', running: false, circuitOpen: false },
    ] },
  }), true)
})

test('单 unit 形态:熔断开门 = 降级(进程可能被拉回但熔断未合)', () => {
  assert.equal(computeDegradedFromStatus({
    stackd: { children: [{ name: 'daemon', running: true, circuitOpen: true }] },
  }), true)
})

test('旧三 unit 形态:unit 活着 + livez 死 = 降级(8-31 事故形态)', () => {
  assert.equal(computeDegradedFromStatus({
    units: [{ unit: 'cumora-sidecar', load: 'loaded', active: 'active', sub: 'running' }],
    livez: { status: 'fail', detail: 'connect ECONNREFUSED' },
  }), true)
})

test('旧三 unit 形态:unit 活着 + livez ok = 不降级', () => {
  assert.equal(computeDegradedFromStatus({
    units: [{ unit: 'cumora-go', active: 'active' }],
    livez: { status: 'ok', detail: '200' },
  }), false)
})

test('栈未装/全停:无 active unit → null(净机向导期不误报)', () => {
  assert.equal(computeDegradedFromStatus({
    units: [{ unit: 'cumora-go', load: 'not-found', active: 'inactive', sub: 'dead' }],
    livez: { status: 'fail', detail: 'ECONNREFUSED' },
  }), null)
})

test('报告缺字段/非对象 → null(保持上一次判定)', () => {
  assert.equal(computeDegradedFromStatus(null), null)
  assert.equal(computeDegradedFromStatus('oops'), null)
  assert.equal(computeDegradedFromStatus({ units: 'garbage', livez: {} }), null)
  assert.equal(computeDegradedFromStatus({ stackd: { children: [] }, livez: { status: 'ok' } }), null)
})
