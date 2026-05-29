<script lang="ts">
  import { onMount } from 'svelte';
  import { Settings } from 'lucide-svelte';
  import { readProfileToken } from '$lib/api/profile-token-store';
  import { listProfiles, switchProfile, type ProfileCard } from '$lib/api/profiles';
  import { profileStore } from '$lib/stores/profileStore.svelte';
  import { appState } from '$lib/stores/appState.svelte';
  import PinPad from './PinPad.svelte';

  interface Props {
    /** Called when a profile has been successfully selected and stored. */
    onselect?: (profile: ProfileCard) => void;
  }

  let { onselect }: Props = $props();

  // ── State ────────────────────────────────────────────────────────────────
  let profiles = $state<ProfileCard[]>([]);
  let loadError = $state('');
  let loaded = $state(false);

  // PIN dialog state
  type PinStep = 'exit' | 'entry';
  let pinStep = $state<PinStep | null>(null);
  let pendingProfile = $state<ProfileCard | null>(null);
  let pinError = $state('');
  let pinLoading = $state(false);
  let exitPinCollected = $state('');

  // ── Load profiles ─────────────────────────────────────────────────────────
  onMount(async () => {
    try {
      profiles = await listProfiles();
    } catch {
      loadError = 'Could not load profiles. Check your connection.';
    } finally {
      loaded = true;
    }
  });

  // ── Avatar helpers ────────────────────────────────────────────────────────
  function avatarSrc(p: ProfileCard): string | null {
    if (p.avatarUrl) return p.avatarUrl;
    if (p.avatarPreset) return `/avatars/${p.avatarPreset}.svg`;
    return null;
  }

  function initials(name: string): string {
    return name
      .split(/\s+/)
      .slice(0, 2)
      .map((w) => w[0]?.toUpperCase() ?? '')
      .join('');
  }

  function avatarBg(p: ProfileCard): string {
    if (p.avatarColor) return p.avatarColor;
    // Deterministic colour from display name.
    const colours = [
      '#7C5CBF', '#E07B39', '#D44C10', '#4A7C9B',
      '#2C8B5A', '#C17F24', '#9B3A8C', '#1E7ABB',
    ];
    let h = 0;
    for (let i = 0; i < p.displayName.length; i++) h = p.displayName.charCodeAt(i) + ((h << 5) - h);
    return colours[Math.abs(h) % colours.length];
  }

  // ── Profile selection ─────────────────────────────────────────────────────
  /** Current active profile info (so we know if exit-PIN is needed). */
  const currentProfile = $derived(profileStore.activeProfile);

  async function selectProfile(target: ProfileCard) {
    // Step 1: Does the current profile need an exit PIN?
    if (currentProfile?.isRestricted && currentProfile.hasExitPin) {
      pendingProfile = target;
      exitPinCollected = '';
      pinStep = 'exit';
      return;
    }
    // Step 2: Does the target profile need an entry PIN?
    if (!target.isRestricted && target.hasEntryPin) {
      pendingProfile = target;
      exitPinCollected = '';
      pinStep = 'entry';
      return;
    }
    // No PINs needed — switch directly.
    await doSwitch(target, '', '');
  }

  async function handleExitPin(pin: string) {
    exitPinCollected = pin;
    pinError = '';
    // After collecting exit PIN, check if target also needs entry PIN.
    if (pendingProfile && !pendingProfile.isRestricted && pendingProfile.hasEntryPin) {
      pinStep = 'entry';
    } else {
      await doSwitch(pendingProfile!, pin, '');
    }
  }

  async function handleEntryPin(pin: string) {
    await doSwitch(pendingProfile!, exitPinCollected, pin);
  }

  async function doSwitch(target: ProfileCard, exitPin: string, entryPin: string) {
    pinLoading = true;
    pinError = '';
    try {
      const resp = await switchProfile(
        {
          profileUserId: target.id,
          currentProfilePin: exitPin || undefined,
          targetProfilePin: entryPin || undefined,
        },
        readProfileToken() || undefined
      );
      profileStore.setActiveProfile(resp.profile);
      pinStep = null;
      pendingProfile = null;
      onselect?.(resp.profile);
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : '';
      if (msg.toLowerCase().includes('incorrect pin') || msg.toLowerCase().includes('pin')) {
        pinError = 'Incorrect PIN. Try again.';
      } else {
        pinError = 'Something went wrong. Please try again.';
      }
    } finally {
      pinLoading = false;
    }
  }

  function cancelPin() {
    pinStep = null;
    pendingProfile = null;
    pinError = '';
    exitPinCollected = '';
  }
</script>

<!-- Full-screen overlay
     #404 polish — the picker used to be a centred title on a pure-black
     background, which read as a debug page on first login. Three additions
     here lift it into something that looks intentional:
       1. Soft radial-gradient backdrop so the page isn't a void.
       2. Server name under the title — orients users with multiple Xuva
          servers in their browser history.
       3. "Manage profiles" link in the footer — visible exit to Settings
          so users without an existing profile know where to add one.
     The avatars themselves are unchanged (already 112 px); avatar-preset
     selection on user creation is tracked separately. -->
<div class="fixed inset-0 z-[100] flex flex-col items-center justify-center overflow-hidden bg-background">
  <!-- Backdrop: two radial gradients keyed off the primary palette plus a
       subtle film-grain overlay borrowed from the library hero. Decorative
       only; pointer-events disabled so the buttons below remain clickable. -->
  <div
    aria-hidden="true"
    class="pointer-events-none absolute inset-0 -z-10"
    style="background:
      radial-gradient(55% 65% at 50% 0%, oklch(0.62 0.22 285 / 0.18), transparent 70%),
      radial-gradient(45% 55% at 15% 85%, oklch(0.55 0.18 320 / 0.10), transparent 70%),
      radial-gradient(45% 55% at 85% 85%, oklch(0.55 0.18 240 / 0.10), transparent 70%);
    "
  ></div>
  <div aria-hidden="true" class="grain pointer-events-none absolute inset-0 -z-10 opacity-40"></div>

  <div class="mb-10 text-center">
    <h1 class="text-3xl font-bold tracking-tight text-foreground md:text-4xl">Who's Watching?</h1>
    {#if appState.serverName}
      <p class="mt-2 text-[11px] font-semibold uppercase tracking-[0.28em] text-muted-foreground/80">
        {appState.serverName}
      </p>
    {/if}
  </div>

  {#if !loaded}
    <!-- Loading skeleton -->
    <div class="flex gap-8">
      {#each Array(3) as _, i (i)}
        <div class="flex flex-col items-center gap-3">
          <div class="h-28 w-28 animate-pulse rounded-full bg-surface"></div>
          <div class="h-4 w-20 animate-pulse rounded bg-surface"></div>
        </div>
      {/each}
    </div>
  {:else if loadError}
    <p class="text-red-400">{loadError}</p>
  {:else if profiles.length === 0}
    <p class="text-muted-foreground">No profiles found.</p>
  {:else}
    <div class="flex flex-wrap justify-center gap-6 px-4 md:gap-10">
      {#each profiles as profile (profile.id)}
        {@const src = avatarSrc(profile)}
        {@const bg = avatarBg(profile)}
        {@const isActive = currentProfile?.id === profile.id}
        <button
          type="button"
          onclick={() => selectProfile(profile)}
          class={`group flex flex-col items-center gap-3 transition-transform hover:scale-105 ${isActive ? 'scale-105' : ''}`}
          aria-label={`Select ${profile.displayName}`}
        >
          <!-- Avatar -->
          <div
            class={`relative h-28 w-28 overflow-hidden rounded-full ring-4 transition-all ${
              isActive
                ? 'ring-primary shadow-glow'
                : 'ring-transparent group-hover:ring-primary/50'
            }`}
          >
            {#if src}
              <img src={src} alt={profile.displayName} class="h-full w-full object-cover" />
            {:else}
              <div
                class="flex h-full w-full items-center justify-center text-3xl font-bold text-white"
                style:background-color={bg}
              >
                {initials(profile.displayName)}
              </div>
            {/if}

            <!-- Restricted badge -->
            {#if profile.isRestricted}
              <div class="absolute bottom-1 right-1 rounded-full bg-surface/80 px-1.5 py-0.5 text-[9px] font-semibold text-muted-foreground backdrop-blur-sm">
                Kids
              </div>
            {/if}
          </div>

          <!-- Name -->
          <span class="text-sm font-medium text-muted-foreground transition-colors group-hover:text-foreground">
            {profile.displayName}
          </span>
        </button>
      {/each}
    </div>
  {/if}

  <!-- Manage profiles link — visible exit to Settings for adding or editing
       profiles. Hidden during the loading skeleton so it doesn't flash before
       we know whether profiles exist at all. -->
  {#if loaded && !loadError}
    <a
      href="/settings"
      class="absolute bottom-8 inline-flex items-center gap-2 rounded-full bg-foreground/[0.04] px-4 py-2 text-xs font-medium text-muted-foreground transition-colors hover:bg-foreground/[0.08] hover:text-foreground"
    >
      <Settings class="h-3.5 w-3.5" />
      Manage profiles
    </a>
  {/if}
</div>

<!-- PIN dialogs -->
{#if pinStep === 'exit' && pendingProfile}
  <PinPad
    title="Enter exit PIN"
    subtitle="Enter the PIN to leave {currentProfile?.displayName ?? 'this profile'}"
    error={pinError}
    loading={pinLoading}
    onsubmit={handleExitPin}
    oncancel={cancelPin}
  />
{:else if pinStep === 'entry' && pendingProfile}
  <PinPad
    title="Enter PIN"
    subtitle="Access to {pendingProfile.displayName} is restricted"
    error={pinError}
    loading={pinLoading}
    onsubmit={handleEntryPin}
    oncancel={cancelPin}
  />
{/if}
