/**
 * 观测双页共享的轮询刷新 hook(#147 ②):interval + 手动刷新同源,
 * 后台 tab 暂停(desktop 侧既有好实践,admin 侧本件采纳)。refresh 经
 * ref 持有 —— 调用方函数标识变化不重置定时器(切换选中项等交互不再
 * 重排 15s 倒计时相位)。
 */
import { useEffect, useRef } from 'react'

export function usePollingRefresh(
  refresh: () => void,
  intervalMs: number,
  opts: { pauseWhenHidden?: boolean } = {},
): void {
  const refreshRef = useRef(refresh)
  // ref 更新走 effect(render 期写 ref 是并发渲染不推荐的副作用形态);
  // useRef(refresh) 初始化已覆盖挂载前的窗口。
  useEffect(() => { refreshRef.current = refresh })
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
}
