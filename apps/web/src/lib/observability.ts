import type { PostHogConfig, PostHogInterface } from 'posthog-js'
import { isElectron, isNotificationWindow, isWebAppHost } from '@/lib/runtime'

async function getAppVersion(): Promise<string> {
  try {
    if (window.cumora?.update) {
      const info = await window.cumora.update.getAppInfo()
      if (info.version) return info.version
    }
  } catch {
    // Browser/PWA mode has no native app version bridge.
  }
  return 'web'
}

export function getAnalyticsSurface(): 'electron' | 'web' | 'admin' | 'notification' | 'browser' {
  if (isNotificationWindow) return 'notification'
  if (isElectron) return 'electron'
  if (typeof location !== 'undefined' && location.hostname.startsWith('admin.')) return 'admin'
  if (typeof location !== 'undefined' && location.pathname.startsWith('/admin')) return 'admin'
  if (isWebAppHost) return 'web'
  return 'browser'
}

export function isPostHogConfigured(): boolean {
  return !!import.meta.env.VITE_PUBLIC_POSTHOG_KEY
}

export function getPostHogConfig(): { apiKey: string; options: Partial<PostHogConfig> } {
  return {
    apiKey: import.meta.env.VITE_PUBLIC_POSTHOG_KEY || '',
    options: {
      api_host: import.meta.env.VITE_PUBLIC_POSTHOG_HOST || 'https://us.i.posthog.com',
      capture_pageview: false,
      capture_pageleave: false,
      disable_session_recording: false,
      persistence: 'localStorage' as const,
      loaded: (posthogInstance: PostHogInterface) => {
        void getAppVersion().then((version) => {
          posthogInstance.register({
            app_version: version,
            app_surface: getAnalyticsSurface(),
          })
        })
      },
    },
  }
}

/** The posthog-js singleton, loaded on demand (#144b). posthog-js is a
 *  heavyweight client that self-hosted builds (no
 *  VITE_PUBLIC_POSTHOG_KEY) never initialize — returning null there keeps
 *  the chunk off the wire entirely. When configured, the provider's
 *  dynamic import inits this same module singleton, so later callers see
 *  the initialized client. */
export async function getPostHogAsync(): Promise<PostHogInterface | null> {
  if (!isPostHogConfigured()) return null
  const m = await import('posthog-js')
  return m.default
}
