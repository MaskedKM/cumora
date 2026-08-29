import { StrictMode, lazy, Suspense } from 'react'
import { createRoot } from 'react-dom/client'
import { App } from './App'
import { ConditionalPostHogProvider } from './components/ConditionalPostHogProvider'
import { isPostHogConfigured } from './lib/observability'
import { isElectron, isNotificationWindow } from './lib/runtime'

// #144b: the tracker statically imports posthog-js/react's usePostHog, so
// it rides the same lazy chunk as the provider — and is skipped entirely
// (never rendered) when telemetry is unconfigured.
const PostHogAppTracker = lazy(() =>
  import('./components/PostHogAppTracker').then((m) => ({ default: m.PostHogAppTracker })))
const telemetryEnabled = isPostHogConfigured() && !isNotificationWindow
import { bootNative, isNativePlatform } from './lib/native'
import './styles/globals.css'

if (isElectron) document.body.classList.add('electron')
if (isNativePlatform()) document.body.classList.add('native', `native-${typeof window !== 'undefined' && (window as { Capacitor?: { getPlatform?: () => string } }).Capacitor?.getPlatform?.() || ''}`)

void bootNative()

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <ConditionalPostHogProvider>
      {telemetryEnabled ? (
        <Suspense fallback={null}>
          <PostHogAppTracker />
        </Suspense>
      ) : null}
      <App />
    </ConditionalPostHogProvider>
  </StrictMode>,
)
