<script lang="ts">
  import { onMount } from 'svelte';
  import "../app.css";
  import { appState } from '$lib/stores/appState.svelte';
  import { profileStore } from '$lib/stores/profileStore.svelte';
  import { syncWatchlistFromServer } from '$lib/stores/watchlistStore.svelte';
  import { listProfiles } from '$lib/api/profiles';
  import WhoIsWatching from '$lib/components/WhoIsWatching.svelte';
  import NavProgress from '$lib/components/NavProgress.svelte';
  import { connectEventStream } from '$lib/api/events';
  import type { ProfileCard } from '$lib/api/profiles';

  let { children } = $props();

  /** Whether the Who's Watching picker should show. */
  let showPicker = $derived(profileStore.showPicker);

  // sessionStorage key: persists across hard navigations within the same tab
  // but resets when the tab is closed — correct "who's watching" semantics.
  const PROFILE_SESSION_KEY = 'xuva-profile-card';

  onMount(() => {
    if (typeof window === 'undefined') return;
    const path = window.location.pathname;
    if (path.startsWith('/setup') || path.startsWith('/signin')) return;

    // Subscribe to backend events for live cache invalidation. The
    // connection is per-tab and idempotent; cleanup tears it down when the
    // layout unmounts (i.e. on full page navigation away).
    const disconnect = connectEventStream();

    // Restore active profile synchronously from sessionStorage so the picker
    // never flashes on subsequent navigations in the same tab.
    try {
      const stored = sessionStorage.getItem(PROFILE_SESSION_KEY);
      if (stored) profileStore.setActiveProfile(JSON.parse(stored) as ProfileCard);
    } catch {
      try { sessionStorage.removeItem(PROFILE_SESSION_KEY); } catch { /* ignore */ }
    }

    // Fire bootstrap, setup-check, and profile discovery in parallel without
    // awaiting — first paint is no longer gated on the network. Each handler
    // updates app state when its response lands.
    void fetch('/api/setup/status').then(async (resp) => {
      if (!resp.ok) return;
      const data = await resp.json() as { requiresSetup?: boolean };
      if (data.requiresSetup) window.location.href = '/setup';
    }).catch(() => { /* server unreachable — stay put */ });

    void fetch('/api/client/bootstrap').then(async (resp) => {
      if (!resp.ok) return;
      const data = await resp.json() as { server?: { name?: string }; features?: { trailers?: boolean } };
      if (data.server?.name) appState.serverName = data.server.name;
      if (data.features?.trailers === false) appState.trailersEnabled = false;
      syncWatchlistFromServer();
    }).catch(() => { /* server unreachable — stay put */ });

    // Skip the picker if a profile was already chosen this tab session.
    if (profileStore.activeProfile) return;

    void listProfiles().then((profiles) => {
      if (profiles.length > 0 && !profileStore.activeProfile) profileStore.openPicker();
    }).catch(() => { /* auth disabled or endpoint unavailable */ });

    return () => { disconnect(); };
  });

  function handleProfileSelected(profile: ProfileCard) {
    profileStore.setActiveProfile(profile);
    try { sessionStorage.setItem(PROFILE_SESSION_KEY, JSON.stringify(profile)); } catch { /* ignore */ }
  }
</script>

<svelte:head>
  <title>{appState.serverName}</title>
</svelte:head>

<NavProgress />

{#if showPicker}
  <WhoIsWatching onselect={handleProfileSelected} />
{:else}
  {@render children()}
{/if}
