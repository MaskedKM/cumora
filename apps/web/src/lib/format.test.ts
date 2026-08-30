// #147 ② 共享格式化层的行为钉。语体差异是本件的验收点:admin 面
// 紧凑 $ / desktop 面精确 $;token 档位;百分比小数位;命中率零分母;
// 相对时间分部的档位边界。
import { expect, test } from 'vitest'
import {
  fmtUsdCompact, fmtUsdPrecise, fmtTokens, fmtInt, fmtPct,
  cacheHitRate, relativeTimeParts,
} from './format'

test('fmtUsdCompact: K/M 档位与小数位', () => {
  expect(fmtUsdCompact(1.234)).toBe('$1.23')
  expect(fmtUsdCompact(12_400)).toBe('$12.4K')
  expect(fmtUsdCompact(1_234_567)).toBe('$1.23M')
  expect(fmtUsdCompact(-0.5, 4)).toBe('$-0.5000')
  expect(fmtUsdCompact(Number.NaN)).toBe('$0')
})

test('fmtUsdPrecise: 按量级 2/4/6 位小数(BYOA 长尾可读)', () => {
  expect(fmtUsdPrecise(12.345)).toBe('$12.35')
  expect(fmtUsdPrecise(0.0123)).toBe('$0.0123')
  expect(fmtUsdPrecise(0.0000123)).toBe('$0.000012')
})

test('fmtTokens: K/M/B 档位', () => {
  expect(fmtTokens(999)).toBe('999')
  expect(fmtTokens(1_234)).toBe('1.2K')
  expect(fmtTokens(2_345_678)).toBe('2.35M')
  expect(fmtTokens(3_456_789_012)).toBe('3.46B')
  expect(fmtTokens(Number.POSITIVE_INFINITY)).toBe('0')
})

test('fmtInt: en-US 千分位;fmtPct: 比率×100 与小数位', () => {
  expect(fmtInt(1234567)).toBe('1,234,567')
  expect(fmtPct(0.1234)).toBe('12.3%')
  expect(fmtPct(0.1234, 0)).toBe('12%')
})

test('cacheHitRate: cached/(uncached+cached),零分母给 0', () => {
  expect(cacheHitRate(30, 70)).toBeCloseTo(0.7)
  expect(cacheHitRate(0, 0)).toBe(0)
  expect(cacheHitRate(100, 0)).toBe(0)
})

test('relativeTimeParts: 档位边界与 invalid 输入', () => {
  const now = Date.now()
  const at = (msAgo: number) => new Date(now - msAgo).toISOString()
  expect(relativeTimeParts(at(59_000))).toMatchObject({ unit: 'sec', n: 59 })
  expect(relativeTimeParts(at(60_000))).toMatchObject({ unit: 'min', n: 1 })
  expect(relativeTimeParts(at(3_599_000))).toMatchObject({ unit: 'min', n: 59 })
  expect(relativeTimeParts(at(3_600_000))).toMatchObject({ unit: 'hour', n: 1 })
  expect(relativeTimeParts(at(86_400_000))).toMatchObject({ unit: 'day', n: 1 })
  // 未来时间(时钟微抖)夹到 0 秒,不产生负数。
  expect(relativeTimeParts(new Date(now + 5_000).toISOString())).toMatchObject({ unit: 'sec', n: 0 })
  expect(relativeTimeParts('not-a-date')).toBeNull()
})
