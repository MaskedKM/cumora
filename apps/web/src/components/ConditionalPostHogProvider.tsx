import { lazy, Suspense, type ReactNode } from 'react'
import { isPostHogConfigured, getPostHogConfig } from '@/lib/observability'
import { isNotificationWindow } from '@/lib/runtime'

// #144b: posthog-js only loads when telemetry is actually configured, so
// self-hosted builds keep the heavyweight client out of the startup path.
const RealProvider = lazy(() =>
  import('posthog-js/react').then((m) => ({ default: m.PostHogProvider })))

export function ConditionalPostHogProvider({ children }: { children: ReactNode }) {
  if (!isPostHogConfigured() || isNotificationWindow) {
    return <>{children}</>
  }

  const { apiKey, options } = getPostHogConfig()
  return (
    // null (not children) as the fallback: rendering children here would
    // unmount/remount the whole app when the provider resolves, resetting
    // every zustand-bootstrapped view. Blocking the first paint on a
    // same-origin chunk for the few ms it takes to load is the cheaper
    // failure mode.
    <Suspense fallback={null}>
      <RealProvider apiKey={apiKey} options={options}>
        {children}
      </RealProvider>
    </Suspense>
  )
}
