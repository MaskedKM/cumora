/**
 * 观测双页共享的轮询刷新 hook(#147 ②):interval + 手动刷新同源,
 * 后台 tab 暂停(desktop 侧既有好实践,admin 侧本件采纳)。refresh 经
 * ref 持有 —— 调用方函数标识变化不重置定时器。
 */
import { useEffect, useRef } from 'react'

export function usePollingRefresh(
  refresh: () => void,
  intervalMs: number,
  opts: { pauseWhenHidden?: boolean } = {},
): () => void {
  const refreshRef = useRef(refresh)
  refreshRef.current = refresh
  const pauseWhenHidden = opts.pauseWhenHidden ?? true

  useEffect(() => {
    if (!intervalMs) return
    const id = window.setInterval(() => {
      // Don't poll when the tab is backgrounded — saves API + battery for
      // a panel the user can't even see.
      if (pauseWhenHidden && typeof document !== 'undefined' && document.visibilityState === 'hidden') return
      refreshRef.current()
    }, intervalMs)
    return () => window.clearInterval(id)
  }, [intervalMs, pauseWhenHidden])

  return refresh
}
